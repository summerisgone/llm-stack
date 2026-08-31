# Architecture

Decisions are recorded in [docs/adr](../adr/README.md); this page is the
current shape of the system.

## Request paths

```
[Internet]
  https://<public-origin>
    -> site reverse proxy (terminates TLS)
      -> Envoy Gateway `edge`, listener `external`
        -> /sso      Keycloak
        -> /platform PAT dashboard (pat-service)
        -> /v1       pat-service, then the private AI Gateway
        -> /         Open WebUI

[Cluster only]
  Open WebUI ─┐
              ├─> ai-gateway-private (ClusterIP) -> llm-d EPP -> vLLM or SGLang
  pat-service ┘

[Operator only, host network]
  grafana-nodeport, langfuse-nodeport
```

Public routing is selected by path only; no route matches on `Host`, and no
workload uses the host's own name as an endpoint. Workload-to-workload traffic
uses Kubernetes service DNS.

## Components

| Component | Role |
| --- | --- |
| Envoy Gateway | the public `edge` Gateway and the private AI Gateway |
| Envoy AI Gateway | OpenAI-schema translation, model routing, GenAI tracing |
| Keycloak | the identity provider; realm `ai-stack`, under the `/sso` path |
| Open WebUI | the browser client; SSO-only, roles from Keycloak |
| pat-service | PAT self-service dashboard and the public `/v1` reverse proxy; identity and session labelling only, no queue (see "Queueing and fairness" below) |
| llm-d EPP | endpoint picker: queue, KV-cache and prefix-cache aware routing |
| vLLM | the default model server, its own Helm release |
| SGLang | swap-in second engine on the same checkpoint, its own Helm release, reached through llm-d EPP exactly like vLLM |
| Langfuse | GenAI traces, prompts and answers |
| Prometheus + Grafana | metrics and dashboards for Envoy, vLLM and llm-d |
| OTEL Collector | receives spans, drops Envoy transport spans, exports to Langfuse |
| CloudNativePG, Altinity ClickHouse, Redis, MinIO operators | all persistent state |

Application workloads run in `airgap-ai-stack`; controllers and operators have
their own namespaces. `make down` scales Deployments to zero and leaves PVCs
in place.

## Identity through the request

Two entry points produce the same downstream identity:

- **Open WebUI** forwards the signed-in user as `X-User-Name` / `X-User-Id`
  and calls the gateway with the user's own Keycloak token. Envoy validates
  the token and derives the identity for the rate limit; a header alone
  asserts nothing.
- **pat-service** validates the `sk-…` token, resolves its owner, and calls
  the gateway with its own service-account JWT plus the resolved headers.
  Inbound `X-User-*` headers are stripped at the edge first.

The AI Gateway maps those headers onto GenAI spans, so Langfuse shows traces
by user, with the SSO subject and PAT id as metadata. Tokens issued before the
username was recorded fall back to the stable SSO subject.

## Queueing and fairness: who owns what

This boundary is settled. A fairness, queueing or priority problem is fixed in
llm-d EPP configuration, never by building a scheduler in `pat-service`.

**`pat-service` owns identity and labels.** It validates the `sk-...` PAT,
resolves its owner, derives the chain-hash session key
([ADR 0011](../adr/0011-pat-session-key-langfuse-tracing.md)), and attaches the
headers that describe the request: `x-llm-d-inference-objective` (the band
name) and `X-Llm-D-Inference-Fairness-Id` (the Keycloak `sub`). It holds no
queue, runs no limiter and no semaphore, and makes no admission or dispatch
decision -- every request it authorises is proxied immediately.
`pat-service/internal/qos` is bookkeeping that produces a label: band
assignment from warmth, streak and spend (`session.go` `assignBand`) and
post-hoc cost accounting in Valkey (`RecordCost`). It is not a scheduler.

**llm-d EPP owns when a request runs.** The flow-control queues, the priority
bands provisioned from `router.inferenceObjectives`, fairness between users
inside a band (`flowControl.defaultPriorityBand` in
`config/llmd/router-nvfp4-values.yaml`), the saturation signal that gates
dispatch, and endpoint selection. Per-user request rate limits belong to Envoy
AI Gateway, also not to `pat-service`.

Three reasons this does not get re-litigated:

- [ADR 0008](../adr/0008-per-user-fair-share.md) v2 did propose the opposite
  ("queue and fairness both move to `pat-service`") and was superseded at v4.
  The only evidence for EPP being incapable was `inferencePool.create: false`
  in the EPP values, which meant no `InferencePool` existed for EPP to resolve
  `InferenceObjective` bands against -- a stack configuration gap, not a build
  limitation. Stage 1B proved the same image dispatches per band once the
  `InferencePool` exists.
- EPP already measures per-flow queue depth, wait time and token usage, and
  reads streaming responses to do it. `pat-service`'s own cost accounting
  parses non-streaming JSON usage only, so a scheduler there would start from
  worse data than EPP already has.
- `pat-service` cannot see all the traffic. Open WebUI calls the private AI
  Gateway directly and never passes through `pat-service`, so a queue there
  would arbitrate a subset of the load while EPP arbitrates all of it.

Cross-band starvation, which no in-band fairness policy can reach, is
[ADR 0012](../adr/0012-graduated-band-ceiling-over-strict-priority.md) -- also
an EPP configuration change.

## Observability

Every span the Collector receives is exported to Langfuse using the
`LANGFUSE_*` credentials from the `airgap-runtime` Secret; Envoy never sees
them. HTTP tracing on the `EnvoyProxy` gives request timing;
`GatewayConfig/ai-gateway-tracing` turns on GenAI tracing in the AI Gateway's
external processor using OpenInference conventions, so Langfuse gets input,
output, model and token counts. The Collector drops the AI Gateway's own
transport spans so each request appears once.

Prometheus scrapes vLLM and the llm-d EPP with `namespace=airgap-ai-stack`
attached, which is what the upstream vLLM and llm-d dashboards filter on.

## Inference profile

One vLLM replica per GPU, `strategy: Recreate`, exclusive by node selection —
two replicas would load a second copy of the model into the same card. Every
in-cluster engine requests and limits `nvidia.com/gpu: 1`, so on this
one-GPU node the scheduler itself arbitrates: a second engine stays `Pending`
rather than silently contending for the card. The
llm-d EPP's admission concurrency is derived from the vLLM Deployment's
declared capacity (`--max-num-seqs`), so those two numbers move together. The
model is mounted read-only from a static `Retain` PV backed by a host
directory, so weights stay outside container images.

The engines are separate Helm releases (`helm/vllm-inference`,
`helm/sglang-inference`), so their launch flags are values and changing one
restarts only that pod — see
[ADR 0007](../adr/0007-inference-engines-as-helm-releases.md). Measured
throughput and the exact launch flags for the reference site are in
[docs/operations/vllm-inference.md](../operations/vllm-inference.md).

## Additional backends and model routing

vLLM is the always-on default, but not the only engine that can answer the
canonical `qwen-3.8-27b` model: SGLang is a swap-in replacement, reached the
same way, through llm-d EPP (see "Inference profile" above and
[ADR 0008](../adr/0008-per-user-fair-share.md) "SGLang portability"). Which
one EPP dispatches to is `config/llmd/router-nvfp4-values.yaml`'s
`router.modelServers` block, never a gateway routing change — the
`AIGatewayRoute/llmd` rule for `qwen-3.8-27b` always points at EPP.

llama.cpp and an external OpenAI-compatible API (the same role LM Studio
plays for the `local-mac` profile) are wired differently: each is its own
`Backend` / `AIServiceBackend` pair plus one exact-match rule on a
*different* model name (`x-ai-eg-model`), toggled independently through
`helm/airgap-stack/values.yaml` `inference.*.enabled`. Traffic to those model
names bypasses llm-d entirely — no queue, no fair-share
(`TASK-qos-fair-share.md` §7.6). A client picks a backend by request `model`
name, same as it already picks `qwen-3.8-27b`. See
[docs/adr/0006](../adr/0006-pluggable-inference-backends.md) for the
mechanism and
[docs/operations/inference-backends.md](../operations/inference-backends.md)
for how to bring one up or switch engines.

SGLang runs in the cluster as its own Helm release (`helm/sglang-inference`,
`make sglang-up`), mounting the same read-only model PVC under the same
`nvidia` RuntimeClass, and is reached over Service DNS at
`sglang-qwen38.airgap-ai-stack.svc.cluster.local:30000`. llama.cpp is still a
host Docker container (`deploy/llamacpp`), reached at
`host.k3d.internal:8090` — k3d's own DNS alias for the host's Docker bridge
gateway; `deploy/sglang-qwen38` keeps the same host path for bring-up and
benchmarking.

This host has one GPU: vLLM and SGLang cannot serve concurrently. Enabling
`inference.<name>.enabled` for llama.cpp/external-api only advertises the
model at the gateway; it never starts or stops the engine behind it, and
`make stack-up` brings up vLLM only — bringing up SGLang instead is always a
manual `make sglang-up` plus the EPP switch above.
