# 0013: Embeddings API -- bge-m3 as its own release, CPU by default, GPU as an opt-in memory loan from the LLM

## Status

Proposed on 2026-09-13. Not deployed. Written from the repository only: the
cluster was unreachable at the time (`127.0.0.1:41755` refused), so every
live-state assumption below is listed under "Verification stages" and must
be closed before this is accepted. Nothing in this ADR supersedes an earlier
one; the GPU mode is a deliberate, scoped exception to the GPU exclusivity
described in [ADR 0007](0007-inference-engines-as-helm-releases.md).

## Context

External systems need vector embeddings for retrieval over two corpora:
source code and Russian-language documentation. They must authenticate with
the same personal access tokens (`sk-...`) that already gate `/v1` for chat,
through the same public origin ([ADR 0001](0001-path-routing-on-one-origin.md),
[ADR 0003](0003-private-ai-gateway-and-pats.md)).

What the repository already gives, and what it does not:

- **The public path already carries it.** The edge `api` rule
  (`helm/airgap-stack/templates/routes-external.yaml`) forwards every
  `/v1` prefix to pat-service, and `pat-service` proxies any `/v1/*` path to
  the private AI Gateway after PAT validation
  (`pat-service/cmd/pat-service/main.go`, `proxy`). A `POST /v1/embeddings`
  with a valid PAT reaches the gateway today with no code change.
  `observeSession`/`recordUsage` act only on `/chat/completions`, so an
  embeddings request produces no QoS spend and no `patsvc_*` usage metric.
- **The private route would send it to the wrong place.** `AIGatewayRoute/llmd`
  rule `openai` has no match and forwards every model name to the llm-d EPP.
  Without a dedicated rule, embeddings calls would enter the GPU LLM's flow
  control queue, count against `concurrency-detector.maxConcurrency`, and
  fail at the engine.
- **The rate limit is sized for humans.** `llmd-per-user-limit` is 60
  requests/minute per `X-User-Id` on the whole `llmd` route
  (`inference.perUserRateLimitPerMinute`). A bulk indexing job from an
  external system exhausts that in seconds and would also starve the same
  user's chat traffic.
- **The GPU has no free memory.** vLLM runs at `gpuMemoryUtilization: 0.94`
  on the 32 GiB RTX 5090; 0.95 crash-loops against a hard ~30.2 GiB
  driver-side ceiling (CONTEXT.md, 2026-09-07). `maxModelLen: 131072` was
  chosen as the largest context that still fits the KV pool at that
  utilization.
- **The node has CPU headroom** on paper: 16 CPU / 48 GiB (CONTEXT.md,
  2026-09-03). Actual free requests are unknown.
- **Only one CUDA stack is proven on this card** under the WSL2 legacy
  prestart hook ([ADR 0005](0005-legacy-nvidia-runtime-hook.md)): the
  `llm-stack/vllm-qwen38-nvfp4:0.27.1` image (SM 12.0) and the pinned
  `SGLANG_IMAGE`.

## Decision

### Model

`BAAI/bge-m3`, pinned to one Hugging Face revision recorded in
`models/manifest.yaml` with hashes in `models/checksums.txt`. Served under
the model name `bge-m3`. Only the dense 1024-dimension output is exposed
(the OpenAI embeddings API has no shape for bge-m3's sparse or multi-vector
outputs). Input limit 8192 tokens per item.

Chosen because it is multilingual with strong Russian coverage and has a long
input window that fits whole code files and long documentation sections in
one chunk. It is **not** a code-specialized model; that is the main quality
risk and has its own verification gate below.

The model name is part of the public contract. Vectors from different models
or revisions are not comparable, so the name never becomes an alias for
"current", and a model change is a new name served alongside the old one.

### Workload: one chart, two accelerator modes

A new Helm release `helm/embeddings-inference` (`make embeddings-up` /
`make embeddings-down`), following ADR 0007's shape: its own values file,
outside the `airgap-stack` release, weights on a read-only PV/PVC
`embeddings-bge-m3-model` (added to `GPU_OBJECTS` in the Makefile and to
`GPU_KEEP_OUT` in `scripts/helm-render`).

`values.yaml` carries `accelerator: cpu | gpu`:

- **`cpu` (default).** Hugging Face Text Embeddings Inference, CPU image,
  pinned by digest in `versions.lock.env` as `EMBEDDINGS_CPU_IMAGE`. No
  RuntimeClass, no GPU resource, explicit CPU/memory `requests` and
  `limits`. Installed by `make stack-up`, because it cannot contend with the
  LLM engines.
- **`gpu` (opt-in).** Runs on the RTX 5090 next to the live LLM engine,
  paid for by lowering the LLM's memory reservation:
  - Pod uses `runtimeClassName: nvidia` with `NVIDIA_VISIBLE_DEVICES=all`
    and **does not request `nvidia.com/gpu`** -- the same way the device
    plugin pod itself reaches the card. Requesting the extended resource
    would leave it `Pending` behind vLLM; enabling device-plugin
    time-slicing instead would make `nvidia.com/gpu` countable twice and
    let vLLM and SGLang schedule together, undoing ADR 0007's guarantee.
    The LLM engines keep their exclusive `nvidia.com/gpu: 1`; only the
    embeddings pod sits outside scheduler accounting, and only in this mode.
  - Engine: TEI's CUDA image if it passes stage V3 on SM 12.0 under the
    legacy hook; otherwise vLLM's pooling runner on the already-proven
    `llm-stack/vllm-qwen38-nvfp4:0.27.1` image. The API contract (below) is
    the same either way; the choice is recorded in `versions.lock.env`
    (`EMBEDDINGS_GPU_IMAGE`) when V3 closes.
  - Memory is a budget written down in values, not a hope: the chart value
    `gpuMemoryBudgetGiB` states what the embeddings pod may use (bounded by
    max batch tokens and client batch size), and the same change lowers
    `helm/vllm-inference` `inference.gpuMemoryUtilization` (or SGLang's
    static memory fraction) by at least that amount, re-checking that
    `maxModelLen` still fits one sequence. Both values move in one commit.
  - Start order is fixed: LLM engine first, embeddings second. vLLM's
    startup check requires `utilization x total` to be free at start, so an
    embeddings pod that grabbed memory first can crash-loop the LLM. After
    any GPU pod restart, the embeddings pod is restarted after the LLM is
    Ready. `make embeddings-up` refuses `accelerator: gpu` unless the LLM
    Deployment is Ready.

Switching modes is `make embeddings-up` with the changed value, plus
`make vllm-up` (or `make sglang-up`) for the memory reduction or its
reversal. The gateway route does not change between modes: the Service name
and port are identical.

### Routing

- `helm/airgap-stack/templates/inference-backends.yaml`: `Backend
  embeddings` and `AIServiceBackend embeddings-openai` (OpenAI schema,
  prefix `/v1`) under `.Values.inference.embeddings.enabled`, per
  [ADR 0006](0006-pluggable-inference-backends.md). Direct backend, never
  through the llm-d EPP: EPP's queue, saturation detector and fair-share
  bands model the LLM's slots, and embeddings calls would corrupt all three.
- A **separate `AIGatewayRoute/embeddings`** on the same private Gateway,
  with an exact `x-ai-eg-model: bge-m3` match, instead of a rule inside
  `llmd`. Reason: it gets its own `SecurityPolicy` (same Keycloak JWT
  provider as `llmd-jwt`) and its own `BackendTrafficPolicy` -- a per-user
  rate limit sized for batch indexing
  (`inference.embeddings.perUserRateLimitPerMinute`), a request buffer
  sized to `maxClientBatchSize x 8192` tokens, and a short request timeout
  (60s) -- so embedding traffic cannot consume a user's chat allowance and
  vice versa. If stage V2 shows EAG cannot give a second route precedence
  over `llmd`'s match-less catch-all, fall back to a rule inside `llmd` and
  accept the shared rate limit, raising it.
- No new public path. External systems call
  `https://<origin>/v1/embeddings` with a PAT; pat-service stays the only
  public entry.

### API contract for external systems

- `POST /v1/embeddings`, OpenAI request/response shape: `model: "bge-m3"`,
  `input` string or array of strings, `encoding_format: "float"`.
  Response vectors are 1024-dimensional and L2-normalized (cosine similarity
  equals dot product).
- `GET /v1/models` lists `bge-m3` alongside the chat model.
- Limits documented and enforced server-side: max items per request
  (`maxClientBatchSize`), max 8192 tokens per item (over-length input is an
  error, not silently truncated, unless the engine cannot do that -- then
  truncation is documented), per-user requests/minute.
- Errors in OpenAI error shape; 401 for invalid/revoked PAT, 429 with
  `X-RateLimit-*` headers.
- Russian and English client docs in `docs/clients/README.md`: curl and
  Python examples, chunking guidance for code (by function/class, with the
  file path prefixed to the chunk) and for Russian docs (by heading), and
  the rule that stored vectors must record `model` so a future model change
  is detectable.

### pat-service

No change is required for correctness. One scoped change is made for
accounting: `recordUsage` also reads `usage.prompt_tokens` from
non-streaming `/v1/embeddings` responses into a new
`patsvc_embedding_tokens_total{user,model}` and counts requests in
`patsvc_requests_total` with a `band` of `embeddings`. Embedding tokens do
**not** feed `qos.Tracker.RecordCost`: they do not consume the LLM's GPU
slots in `cpu` mode, and in `gpu` mode their cost is already paid by the
memory loan, not by queue time.

### Observability

- Prometheus job `embeddings-bge-m3` in
  `k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`.
- Grafana panels: requests/s, p50/p95 latency, batch size, queue, tokens per
  user, CPU (cpu mode) or GPU memory by process (gpu mode, from the existing
  `gpu-exporter`).
- AI Gateway traces to Langfuse as for chat; confirm embeddings spans do not
  export full input text if that is too large (V6).

## Consequences

- External systems get embeddings with no new credential type, no new
  public route and no new identity path; revoking a PAT stops both chat and
  embeddings at once.
- `cpu` mode costs nothing on the GPU and cannot affect LLM latency or
  context length, at the price of throughput and indexing time.
- `gpu` mode trades LLM KV capacity (usable context and/or concurrency) for
  embedding throughput. It is an operational decision taken by changing two
  values together, never a default.
- In `gpu` mode the Kubernetes scheduler no longer sees every GPU consumer.
  Memory arbitration is by budget and start order only; an operator who
  raises one number without lowering the other gets an OOM or a crash-loop.
- `make verify` does not cover engine charts; `helm/embeddings-inference`
  needs `helm template` by hand, same as the vLLM and SGLang charts.
- The model name `bge-m3` becomes a compatibility promise to external
  systems for as long as they keep stored vectors.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| EAG v1.1.0 does not route `/v1/embeddings` by model or does not advertise it in `/v1/models` | Design does not work as written; traffic falls into the EPP catch-all | V2 is the first gate; nothing is built before it passes |
| Second `AIGatewayRoute` loses precedence to `llmd` catch-all | Embeddings reach EPP, fail | V2 checks it live; fallback is a rule inside `llmd` |
| bge-m3 retrieval quality on code is too low | External systems build on poor search | V5 quality gate with a repo-derived eval set before announcing the API |
| CPU mode latency/throughput too low for indexing | Indexing jobs take hours, timeouts | V4 measures; batch limits and rate limit tuned from it; `gpu` mode is the escape hatch |
| CPU contention with Keycloak, ClickHouse, pat-service, Postgres | Login and PAT validation slow down under bulk indexing | Hard CPU `limits`; V4 runs load while probing `/healthz` and SSO latency |
| `gpu` mode: embeddings pod allocates before the LLM | vLLM start check fails, LLM crash-loops, chat is down | Fixed start order, `make embeddings-up` guard, runbook after reboot |
| `gpu` mode: activation spikes exceed the budget | CUDA OOM in either process | Budget derived from measured peak at max batch x 8192 tokens (V3), not from weight size |
| `gpu` mode: reduced `gpuMemoryUtilization` no longer fits `maxModelLen` | vLLM refuses to start or context shrinks | V3 re-runs vLLM's startup check and records the new max context |
| `gpu` mode: TEI CUDA image does not run on SM 12.0 under the legacy hook | GPU mode unavailable with TEI | Fallback to vLLM pooling runner on the proven image |
| Embedding and LLM compete for GPU compute | LLM TTFT/TPOT regress during indexing | V3 measures TTFT/TPOT with concurrent embedding load; dashboard panel |
| PATs are owned by people; an external system's token dies with the owner's account | Integration breaks on offboarding | Documented; dedicated Keycloak users for integrations (`scripts/kc-user-add`) |
| Large batch payloads exceed request buffer | 413 at the AI Gateway ext-proc (seen before at ~32KiB, CONTEXT.md 2026-09-06) | Buffer on the embeddings route sized to max batch; V2 sends a max-size batch |
| Full input text exported to Langfuse traces | Trace store growth, code content in traces | V6 checks span size; hide inputs for this route if needed |
| Model later replaced | All stored vectors incompatible | New model served under a new name in parallel; old one retired on an announced date |

## Verification stages

Each stage has an exit condition. A failed gate stops the stages after it.

### V0 -- repository and render (no cluster)

- `make verify` clean with `inference.embeddings.enabled: true`.
- `helm template embeddings-inference helm/embeddings-inference -n airgap-ai-stack`
  clean for both `accelerator: cpu` and `accelerator: gpu`.
- `go test ./...` and `go vet ./...` in `pat-service`.

### V1 -- live cluster baseline

- Record node allocatable vs. summed requests/limits for CPU and memory.
- Record current GPU memory use by the live LLM engine and its
  `gpuMemoryUtilization` / `maxModelLen`.
- Record current TTFT/TPOT p50/p95 from `llm_d_epp_*` for comparison in V3/V4.

Exit: numbers written into this ADR.

### V2 -- gateway contract

With the engine running in `cpu` mode and the routes deployed:

- `POST /v1/embeddings` with a PAT through the public origin -> 200, 1024
  dimensions, L2 norm = 1.0.
- AI Gateway access log shows the request on `AIGatewayRoute/embeddings`,
  not `llmd`; EPP `/metrics` request counters unchanged.
- `GET /v1/models` lists both `qwen-3.8-27b` and `bge-m3`.
- Chat still routes to EPP (`scripts/pat-smoke-test`).
- No PAT -> 401; revoked PAT -> 401; over-limit -> 429 on embeddings while
  the same user's chat is not limited.
- Max-size batch (`maxClientBatchSize` items x 8192 tokens) -> 200, no 413.
- Over-length item -> documented behavior (error or truncation).
- Open WebUI chat model selector does not show `bge-m3`.

Exit: all assertions pass in `scripts/embeddings-smoke-test`, run twice.

### V3 -- GPU mode

- Engine choice: TEI CUDA image starts on the RTX 5090 under
  `RuntimeClass/nvidia` and returns correct vectors; otherwise vLLM pooling
  runner. Record the choice.
- Peak GPU memory of the embeddings process at max batch x 8192 tokens,
  measured with `nvidia-smi` via `gpu-exporter` -> sets
  `gpuMemoryBudgetGiB` (peak + margin).
- vLLM restarted at the reduced `gpuMemoryUtilization`: starts, reports max
  concurrency for `maxModelLen`; record the new usable context.
- Order test: restart LLM with embeddings already running -> must be
  prevented by the runbook/guard; document the observed failure once.
- Vectors from GPU mode match CPU mode (cosine >= 0.999 on a fixed sample).
- LLM TTFT/TPOT under sustained embedding load vs. V1 baseline.
- Reversal: `accelerator: cpu`, restore `gpuMemoryUtilization`, LLM back to
  its V1 context.

Exit: budget, new context limit and TTFT/TPOT delta recorded here; reversal
proven.

### V4 -- CPU capacity

- Throughput (items/s, tokens/s) and latency p50/p95 at batch sizes 1, 8,
  32 with code-sized and doc-sized inputs.
- Same load while measuring SSO login, pat-service `/healthz` and PAT
  validation latency.
- Decide `maxClientBatchSize`, CPU `limits` and
  `perUserRateLimitPerMinute` from these numbers.

Exit: values committed with the measurements they came from.

### V5 -- retrieval quality

- Eval set of at least 50 query -> relevant chunk pairs each for: code from
  this repository (Go, shell, YAML; queries in Russian and English) and
  Russian documentation.
- Metrics: recall@5 and MRR@10.
- Thresholds agreed with the external system owners before the run; below
  threshold on code -> this ADR is revisited (code-specific model served
  under a second name) before the API is announced for code.

Exit: results recorded, go/no-go per corpus.

### V6 -- operations

- Prometheus target up, Grafana panels populated,
  `patsvc_embedding_tokens_total` matches `usage.prompt_tokens` of a real
  response.
- Langfuse trace for an embeddings call present; span size acceptable.
- Full `make stack-up` from scratch brings embeddings up in `cpu` mode with
  no manual step; after a host reboot the documented recovery order works
  in `gpu` mode.
- Air-gap: image digest and model hashes present; install works with no
  outbound network.

Exit: runbook in `docs/operations/inference-backends.md`, client docs in
`docs/clients/README.md`, ADR moved to Accepted.

## Implementation order

1. Model weights, manifest, checksums, PV/PVC; image pins (V0).
2. `helm/embeddings-inference` with `accelerator: cpu`; Makefile targets.
3. V1 baseline, then routes and policies in `airgap-stack`; V2.
4. `scripts/embeddings-smoke-test`, `make embeddings-smoke`, included in
   `smoke-nogpu` for `cpu` mode; `tests/inference/README.md`.
5. pat-service accounting, Prometheus job, Grafana panels.
6. V4 and V5; client docs; announce to external systems.
7. `accelerator: gpu` path and V3 -- independent of 1-6, only when a need
   for GPU throughput is shown by V4.
8. V6; status to Accepted.

Out of scope: the `local-mac` profile, Open WebUI RAG switching to this
endpoint, bge-m3 sparse and multi-vector outputs.
