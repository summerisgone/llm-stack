# ninfer (pilot)

Third-party inference engine ([github.com/Neroued/ninfer](https://github.com/Neroued/ninfer)),
serving the same Qwen3.8-27B NVFP4 checkpoint family as `vllm-inference` and
`sglang-inference`, wired in per
[docs/adr/0006-pluggable-inference-backends.md](../../docs/adr/0006-pluggable-inference-backends.md)
and
[docs/adr/0015-third-party-engine-metrics-contract.md](../../docs/adr/0015-third-party-engine-metrics-contract.md).
Task brief: `~/agent/ninfer-pilot-task.md`.

**Pilot status, not a replacement for vLLM/SGLang.** Deployed as its own
in-cluster Deployment (`helm/ninfer-inference`), bypassing llm-d's EPP
fair-share queue -- it cannot join EPP itself, see "EPP / fair-share" below.

**Its own model name since ADR 0019: `qwen-3.8-27b-ninfer`.** Like
llama.cpp/externalApi, ninfer has its own exact-match rule (`openai-ninfer`
in `llmd.yaml`) to its direct backend; `qwen-3.8-27b` always stays on the
llm-d EPP pool. Clients opt in by sending `model: qwen-3.8-27b-ninfer`, and
the gaps below ("Compatibility gaps", no EPP fair-share) apply only to them.
On one GPU it answers only while `NINFER_REPLICAS` is 1 and the pool's
replicas are 0 (`make engines-up`).

**Ran live on this cluster from 2026-09-19** under the canonical name (the
retired `inference.ninfer.live` switch).
`ninfer-qwen38` is `Running` (2/2: engine + `jsonl_exporter` sidecar),
`usage.prompt_tokens`/`completion_tokens` confirmed non-zero on
`POST /v1/chat/completions` with `model: qwen-3.8-27b` (MTP speculative
decoding active: `draft_n_accepted` 21/27 on one sample request),
`up{job="ninfer"}` is `1` in Prometheus, the shared route rule's
`backendRefs` resolves to `ninfer-openai`, and scaling `sglang-qwen38` to
`replicas: 1` alongside it left sglang `Pending` on
`Insufficient nvidia.com/gpu` -- not an OOM crash. Risk 1 verified mitigated
for real, not just by chart inspection.

Unlike `deploy/llamacpp` and `deploy/sglang-qwen38`, there is no `run`/`smoke`
script here: ninfer runs only as the `helm/ninfer-inference` k8s Deployment
(Risk 1 below is why).

## Why this pilot exists

A manual side-by-side against the live `sglang-qwen38` deployment (same
NVFP4 checkpoint, same RTX 5090) found two things worth a real pilot:

- ninfer runs MTP speculative decoding with a compressed draft head
  (`--lm-head-draft`, ~0.33 GiB) where sglang's config has speculative
  decoding disabled (the uncompressed draft head costs ~4.7 GiB; see
  `helm/sglang-inference/values.yaml` and ADR-0010).
- Under 3 concurrent long-context agent-style sessions (40k+ tokens each),
  sglang's radix cache evicted and forced full re-prefills 5 times out of 30
  turns (TTFT spikes to 30s+); ninfer's prefix cache held for all 30 turns.

Numbers are from one manual comparison session, not a benchmark publication
-- directional, re-verify against this actual deployment.

## Deploying

```sh
# 1. One-time: get the image onto the cluster. Not published upstream, and
#    versions.lock.env's ninfer-image.yml CI path is unverified against a
#    real GHCR push from this repo's Actions permissions -- what's actually
#    running today was built directly on the WSL2/k3d host from the exact
#    commit pinned in NINFER_UPSTREAM_REF (already the load-test checkout at
#    ~/projects/ninfer-loadtest/ninfer on that host) and loaded straight into
#    containerd, no registry round-trip:
docker build -t ghcr.io/summerisgone/ninfer:latest ~/projects/ninfer-loadtest/ninfer
k3d image import ghcr.io/summerisgone/ninfer:latest -c llm-stack
#    (run on the WSL2 host itself -- native x86_64 build, avoids emulating a
#    CUDA/CMake build over qemu from an arm64 dev machine)

# 2. Qwen3.8-27B NVFP4 converted to ninfer's own artifact format (its
#    weight-conversion pipeline, upstream docs/weight-conversion.md -- NOT
#    the same file as vllm-qwen38-nvfp4-model's safetensors checkpoint) is a
#    single file, not a per-engine directory like vllm/embeddings. Its
#    checksum is in models/checksums.txt (model qwen3_8_27b_nvfp4.ninfer).

# 3. Upload it to MinIO from the GPU node that holds it (the k3d mock
#    mounts the host's model store at /var/lib/models); the engine pod's
#    model-cache initContainer fetches and verifies it (ADR 0019)
scripts/models-upload qwen3_8_27b_nvfp4.ninfer

# 4. Set NINFER_API_KEY in .env, then
make helm-up      # creates the ninfer-api-key Secret + BackendSecurityPolicy
# 5. .env: NINFER_REPLICAS=1 and the pool's VLLM_REPLICAS/SGLANG_REPLICAS=0
make engines-up   # helm/ninfer-inference
```

`inference.ninfer.enabled` wires the Backend/AIServiceBackend, the
`ninfer-api-key` Secret and the `openai-ninfer` rule.

`inference.ninfer.modelName` in `helm/airgap-stack/values.yaml` and
`inference.servedModelName` in `helm/ninfer-inference/values.yaml` are both
`qwen-3.8-27b-ninfer`. Keep the two in sync (ninfer's own `--model-id` must equal the route's
match value or requests get rejected as a model-id mismatch, per upstream
`docs/serving.md`).

**Only one GPU on this node.** Do not run `ninfer-up` at the same time as
`vllm-up`/`sglang-up` at `replicas: 1` each -- see Risk 1. Because
`live: true` also makes ninfer the thing `qwen-3.8-27b` clients actually
hit, treat flipping it the same as the vLLM<->SGLang swap procedure in
[docs/operations/inference-backends.md](../../docs/operations/inference-backends.md):
an explicit, deliberate step, not a background toggle.

## Verifying inference works

```sh
# Directly against the Service (bypassing the gateway):
kubectl -n airgap-ai-stack port-forward svc/ninfer-qwen38 18080:18080 &
curl -s http://127.0.0.1:18080/health
curl -s http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $NINFER_API_KEY" \
  -d '{"model":"qwen-3.8-27b","messages":[{"role":"user","content":"Say hi in five words."}],"max_tokens":32}' \
  | python3 -m json.tool
```

Confirm the response has non-zero `usage.prompt_tokens` and
`usage.completion_tokens` (`usage.prompt_tokens_details.cached_tokens` is
also present -- ninfer exceeds ADR-0015's Tier 1 mandatory bar). Then confirm
GPU exclusivity:

```sh
kubectl -n airgap-ai-stack scale deployment/sglang-qwen38 --replicas=1
kubectl -n airgap-ai-stack scale deployment/ninfer-qwen38 --replicas=1
kubectl -n airgap-ai-stack get pods -l 'app.kubernetes.io/name in (sglang-qwen38,ninfer-qwen38)'
# expect: one Running, one Pending (device plugin refuses to co-schedule
# both nvidia.com/gpu:"1" requests) -- NOT one OOM-killed (exitCode 137).
```

## Metrics: what's wired and what isn't

ADR-0015's three-tier contract:

| Tier | Status | Detail |
|---|---|---|
| 1 -- OpenAI `usage` | **Confirmed live, zero code changes** | `POST /v1/chat/completions` returns `usage.prompt_tokens`/`completion_tokens`/`prompt_tokens_details.cached_tokens`. pat-service's `recordUsage` is already engine-agnostic; ninfer's traffic feeds `patsvc_*_tokens_total` and cost accounting the same as vLLM/SGLang. |
| 2 -- Prometheus scrape target | **Wired**, natively | `GET /metrics` on the API port (`:18080`), added upstream in `abb7f14f` (2026-10-05) and included in `NINFER_UPSTREAM_REF`. It sits behind the same `--api-key` as inference. |
| 3 -- engine-native inference metrics | **Wired**, on ninfer's own metric names | See below. |

### Scrape and auth

Prometheus scrapes `ninfer-qwen38:18080/metrics` under `job: ninfer`
(`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`), which
gives Tier 2's `up{job="ninfer"}` from the same scrape as Tier 3. ninfer has
no flag to leave `/metrics` unauthenticated, so the job sends
`Authorization: Bearer` from `credentials_file`: the `ninfer-api-key` Secret
(`NINFER_API_KEY` in `.env`, created by `make helm-up`) mounted into the
Prometheus pod at `/etc/prometheus-secrets/ninfer` (`optional: true`, so
Prometheus starts without it). Prometheus does not expand env vars in scrape
auth, so the file mount is the only way in. The chart's NetworkPolicy admits
Prometheus on `:18080`. With that key Prometheus could also send inference
requests.

### What the dashboards use

Upstream's metric table is in ninfer's `docs/serving.md` ("Live metrics").
`config/grafana/dashboards/ninfer.json` and the ninfer series in
`fair-share.json` use:

- `ninfer_time_to_first_token_seconds` -- histogram, including preparation
  and queueing.
- `ninfer_request_duration_seconds`, `ninfer_request_queue_seconds` --
  histograms of settled requests.
- Per-token latency (fair-share "TPOT") -- ninfer has no per-token histogram,
  so it is an average: `(request_duration_sum - ttft_sum) /
  ninfer_decode_tokens_total` (decode tokens exclude the first one).
- `ninfer_{prompt,prompt_tokens_cached,generation}_tokens_total` -- counters,
  updated as tokens commit, not at request completion.
- `ninfer_requests_{running,waiting,prefilling,decode_ready,paused}` --
  gauges.
- `ninfer_device_kv_used_pages / ninfer_device_kv_capacity_pages` and
  `ninfer_host_context_used_bytes / ninfer_host_context_capacity_bytes` --
  KV occupancy as a ratio.
- `ninfer_requests_total{outcome}` (`completed`, `cancelled`, `failed`,
  `rejected`), `ninfer_response_failures_total`, `ninfer_engine_ready`.

Counters reset when the server restarts. Nothing maps these onto vLLM/SGLang
names (ADR-0015 keeps `vllm.json` a closed contract).

## Known risks

### Risk 1 -- GPU is a single shared resource (mitigated)

A bare `docker run --gpus all` container is invisible to k8s's
`nvidia.com/gpu` accounting -- during testing this OOM-killed the live
`sglang-qwen38` pod (`exitCode 137`). **Mitigation: ninfer only runs as the
`helm/ninfer-inference` in-cluster Deployment**, requesting
`nvidia.com/gpu: "1"` exactly like `helm/sglang-inference` and
`helm/vllm-inference`. With one GPU registered on `k3d-llm-stack-agent-0`,
scaling both ninfer and sglang to `replicas: 1` leaves one `Pending`, per the
verification steps above. Do not add a `deploy/ninfer/run` host-Docker
script -- that would reopen this exact risk.

### Risk 2 -- cold-prefill head-of-line blocking (accepted for pilot scope, not fully mitigated)

Under `--max-concurrency 3` with 3 sessions holding 40k+ token growing
contexts, a new cold long-context request blocked two other sessions'
already-cached, near-instant turns for ~24-29s -- FIFO admission has no
priority for cache hits.

Checked against ninfer's own source, not assumed:

- **`docs/maintainer/engine-architecture.md` §5.2 confirms FIFO ownership is
  never reordered by resource state** ("资源条件不能反向改变 FIFO 所有权"). A
  later request may only "backfill" ahead of a blocked FIFO head when the
  Program can prove the head still gets full root admission once the
  borrower's reservation ends -- this is a resource-safety escape valve, not
  a cache-hit fast path, and does not use any "borrower finishes sooner"
  timing assumption. **There is no admission-priority knob for cache hits in
  the current binary** -- confirmed against `docs/maintainer/resource-scheduling-and-context-cache.md`
  and `serve_options.cpp`'s full flag list, not left unverified.
- `--prefill-chunk` (default 1024, must be a positive multiple of 128) is a
  real, present flag that chunks a large prefill into smaller admitted
  units, and the architecture doc's §5.3 scheduling rules explicitly protect
  existing decode work from prefill starvation. A smaller value is the most
  likely lever to shrink the blocking window -- **not tested in this pilot**.
- This pilot's own sizing (`--kv-dtype fp8`, `--max-concurrency 3` ->
  250,304-token shared KV pool at a 180224-token ceiling) fits one
  full-ceiling request plus headroom, not three -- which independently
  limits how often a concurrent request has enough spare capacity to
  backfill at all, regardless of prefill chunking. Right-sizing
  `--kv-capacity`/`--max-concurrency` against the pilot's real concurrent
  session count is the other lever, also not tested here.

**Decision: accepted for pilot scope.** Do not route production multi-tenant
agent traffic through ninfer until `--prefill-chunk` tuning and KV-capacity
right-sizing have been tried against this deployment's real traffic and, if
neither closes the gap, an admission-priority request has been filed
upstream ([github.com/Neroued/ninfer](https://github.com/Neroued/ninfer)).
Track this decision here, not by rediscovering it from a latency spike.

## Compatibility gaps (not just EPP fair-share)

These gaps are visible to clients of `qwen-3.8-27b-ninfer` only; clients
that need them stay on `qwen-3.8-27b`.

### No constrained/JSON-mode output at all

ninfer accepts only `response_format: {"type":"text"}` (or the field
omitted). Confirmed directly against upstream `docs/serving.md`: *"NInfer
does not apply defaults, enforce required properties, perform recursive
JSON Schema validation, or use constrained decoding."* Any
`{"type":"json_object"}` or `{"type":"json_schema", ...}` request gets an
explicit HTTP 400 identifying the field, e.g.:

```
this response_format requires constrained output, which NInfer cannot
guarantee; only {"type":"text"} is available
```

**Observed 2026-09-19:** Hermes agent's auxiliary title generation hit this
exact error while ninfer was live. vLLM and SGLang both support guided/
constrained decoding, so this specific failure mode did not exist before
ninfer became reachable under `qwen-3.8-27b`. Whether a given client feature
degrades gracefully (as Hermes agent's title generation appears to, per the
"Auxiliary" framing in its own warning) or hard-fails is up to that client,
not something this deployment controls. **No fix available on ninfer's
side** -- this is a binary capability gap, not a missing flag. Before
`live: true` is treated as more than a pilot toggle, inventory which
production clients rely on JSON-mode `response_format` and decide whether
that is acceptable while ninfer is the live engine.

Also rejected outright by ninfer (same source, same reason -- no code fix
possible): nonzero `logit_bias`, requested `logprobs`, `strict: true`,
required/named tool choice, and non-empty legacy `functions`. Most
coding-agent traffic does not use these; `response_format` json-mode is the
one confirmed to actually bite in practice so far.

## EPP / fair-share (explicit non-goal, and not just unverified)

`core-metrics-extractor.defaultEngine` (`config/llmd/router-nvfp4-values.yaml`)
is **not** set to `ninfer`, and ninfer is **not** routed through llm-d's EPP
-- its `openai-ninfer` rule (`llmd.yaml`) goes straight to its backend, and
its pods carry no `llm-d.ai/model` label, so the EPP pool never selects them. Two independent reasons,
not one:

- Whether the installed llm-d chart recognizes any `defaultEngine` value
  besides `sglang`/`vllm` is unverified; per ADR-0015, that requires probing
  the running EPP pod's accepted config directly (the way ADR-0012 probed
  `allow-experimental-plugins`), not assuming a value is safe.
- Even a recognized value would not make EPP's scoring meaningful for
  ninfer: `router-nvfp4-values.yaml`'s `metrics-data-source` plugin polls
  the live engine's `/metrics` to feed `queue-scorer`,
  `kv-cache-utilization-scorer`, and `utilization-detector`. ninfer's
  `/metrics` uses its own `ninfer_*` names (and needs its API key), none of
  which the extractor knows -- EPP would have no telemetry it can read to
  score or queue against it, not just an unrecognized engine-type string.

Until llm-d's EPP itself grows a ninfer-compatible metrics path, ninfer's
traffic (even while `live: true`) does not get ADR-0008/ADR-0012's
fair-share or priority-band guarantees -- same as llama.cpp today, just
under a name that used to imply otherwise.
