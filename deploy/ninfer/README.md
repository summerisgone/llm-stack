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

**Seamless swap under the canonical model name, not its own.** Unlike
llama.cpp/externalApi, ninfer does not advertise a `ninfer-*` model id.
`inference.ninfer.live` (`helm/airgap-stack/values.yaml`) toggles whether
the one shared `openai-qwen38nvfp4` route rule (`llmd.yaml`) points at
ninfer's direct backend or at llm-d EPP (vLLM/SGLang) -- clients keep
sending `model: qwen-3.8-27b` unchanged across the swap, same as a
vLLM<->SGLang engine change. The cost of that seamlessness is real, not
free: see "Compatibility gaps" below, in addition to losing EPP fair-share.

**Live on this cluster as of 2026-09-19** (`inference.ninfer.live: true`).
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
#    single file, not a per-engine directory like vllm/embeddings: confirmed
#    live at /var/lib/models/qwen3_8_27b_nvfp4.ninfer (the existing
#    /var/lib/models bind mount already projects it in from
#    /home/llm-stack/models/qwen3_8_27b_nvfp4.ninfer on the WSL2 host -- no
#    extra mount step was needed). ninfer-model-volume.yaml's hostPath
#    matches this exact file, type File, not Directory.

# 3. Create the PVC (adds to gpu-objects-up's GPU_OBJECTS list)
make gpu-objects-up

# 4. Set NINFER_API_KEY in .env (same pattern as SGLANG_API_KEY), then
make helm-up      # creates the ninfer-api-key Secret + BackendSecurityPolicy
make ninfer-up    # helm/ninfer-inference
```

`inference.ninfer.enabled` wires the Backend/AIServiceBackend and the
`ninfer-api-key` Secret (deployed, reachable, but not yet the one clients
hit). `inference.ninfer.live` is the separate switch that actually points
the canonical `qwen-3.8-27b` route rule at it:

```sh
helm upgrade --install airgap-stack helm/airgap-stack \
  --namespace airgap-ai-stack --values helm/airgap-stack/values.yaml \
  --set inference.ninfer.enabled=true --set inference.ninfer.live=true
```

`inference.ninfer.modelName` in `helm/airgap-stack/values.yaml` and
`inference.servedModelName` in `helm/ninfer-inference/values.yaml` are both
`qwen-3.8-27b` -- the same name vLLM/SGLang use, not a ninfer-specific one.
Keep the two in sync (ninfer's own `--model-id` must equal the route's
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
| 2 -- Prometheus scrape target | **Wired**, via a sidecar, not ninfer itself | ninfer has no `/metrics` route at all -- checked directly against `src/serve/*.cpp` and the endpoint table in upstream `docs/serving.md`, not assumed. `up{job="ninfer"}` comes from Prometheus scraping the exporter sidecar below, not ninfer's own port. |
| 3 -- engine-native inference metrics | **Partially wired**, via the same sidecar, on ninfer's own metric names | See below. Not everything vLLM/SGLang's dashboards chart has an equivalent. |

### The adapter: `jsonl_exporter.py`

ninfer's only structured signal is `--request-log-jsonl`
(`ninfer_serve_request_log` schema v21: `server_start`, `request_start`,
`request_rejected`, `request_done`, `request_error`, `throughput` events --
confirmed against upstream `src/serve/request_log.h`/`.cpp`). There is no
`--metrics`-style flag to enable a native exposition; this is a fixed gap in
the current ninfer binary, not a missing config flag.

`helm/ninfer-inference/files/jsonl_exporter.py` runs as a second container in
the same pod as `ninfer-serve` (sharing an `emptyDir` at `/var/log/ninfer` --
it needs pod-local file access, so it is a true sidecar container, not a
separate Deployment the way `gpu-exporter-sglang` is), tails the JSONL file,
and re-exposes it as Prometheus text format on `:9400/metrics`. Prometheus
scrapes that port under `job: ninfer`
(`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`), which
satisfies Tier 2's minimum bar (`up{job="ninfer"}`) from the same scrape as
Tier 3 -- a separate blackbox-exporter probe of `/health` was judged
redundant and skipped.

What it exposes, and what it deliberately does not claim:

- `ninfer_request_ttft_seconds{_bucket,_sum,_count}` -- real histogram, from
  `request_done.timings_seconds.ttft`. Feeds a TTFT p50/p95 panel like
  vLLM/SGLang's.
- `ninfer_request_decode_seconds{_bucket,_sum,_count}` -- from
  `request_done.timings_seconds.decode`. **This is the whole request's
  decode-phase duration, not a per-token figure.** ninfer has no native
  per-token (inter-token-latency/TPOT) timing field, unlike
  `vllm:inter_token_latency_seconds`. Do not chart this as if it were TPOT.
- `ninfer_{prompt,generation,prefix_cache_hit_tokens}_total` -- counters, from
  `request_done.result.*`.
- `ninfer_num_requests_{running,waiting,prefilling,decode_ready}` -- gauges,
  from `throughput.scheduler.*` (emitted every `--log-stats-interval-ms`,
  default 5s).
- `ninfer_kv_cache_{device_main,device_backend}_pages` /
  `ninfer_kv_cache_host_bytes` -- gauges, from
  `throughput.context_cache.occupancy.*`. **Raw occupied-unit counts, not a
  percentage.** This pilot has not independently confirmed that
  `server_start`'s `memory.kv_capacity_page_groups` and `throughput`'s
  `device_main_kv_pages` share the same page-group unit, so no
  `kv_cache_usage_perc`-equivalent ratio is computed or charted.

Not built: nothing forces a match against the vLLM/SGLang metric names in
ADR-0015's Tier 3 table -- `config/grafana/dashboards/ninfer.json` uses
ninfer's own metric names throughout, and does not touch `vllm.json`/
`fair-share.json` (ADR-0015 keeps those a closed contract).

## Known risks

### Risk 1 -- GPU is a single shared resource (mitigated)

A bare `docker run --gpus all` container is invisible to k8s's
`nvidia.com/gpu` accounting -- during testing this OOM-killed the live
`sglang-qwen38` pod (`exitCode 137`). **Mitigation: ninfer only runs as the
`helm/ninfer-inference` in-cluster Deployment**, requesting
`nvidia.com/gpu: "1"` exactly like `helm/sglang-inference` and
`helm/vllm-inference`. With one GPU registered on `k3d-llm-stack-server-0`,
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

`inference.ninfer.live: true` makes ninfer answer for the same model name
real clients already use, so gaps here are user-visible the moment it is
flipped, not scoped to a `ninfer-*` opt-in name anymore.

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
-- `inference.ninfer.live` only retargets the AI Gateway route rule
(`llmd.yaml`), never the EPP `modelServers` block. Two independent reasons,
not one:

- Whether the installed llm-d chart recognizes any `defaultEngine` value
  besides `sglang`/`vllm` is unverified; per ADR-0015, that requires probing
  the running EPP pod's accepted config directly (the way ADR-0012 probed
  `allow-experimental-plugins`), not assuming a value is safe.
- Even a recognized value would not make EPP's scoring meaningful for
  ninfer: `router-nvfp4-values.yaml`'s `metrics-data-source` plugin polls
  the live engine's `/metrics` to feed `queue-scorer`,
  `kv-cache-utilization-scorer`, and `utilization-detector`. ninfer has no
  `/metrics` route at all (see `deploy/ninfer/README.md`'s Tier 2 section) --
  EPP would have zero real telemetry to score or queue against it, not just
  an unrecognized engine-type string.

Until llm-d's EPP itself grows a ninfer-compatible metrics path, ninfer's
traffic (even while `live: true`) does not get ADR-0008/ADR-0012's
fair-share or priority-band guarantees -- same as llama.cpp today, just
under a name that used to imply otherwise.
