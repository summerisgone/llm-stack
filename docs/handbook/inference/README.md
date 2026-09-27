# Inference

[Русский](README.ru.md) | [Handbook index](../README.md)

How a model request travels from the gateway to the GPU, and who decides what
on the way. Two deeper pages follow: [queues and fair share](queues-and-fair-share.md)
(who waits, in which order) and [KV cache](kv-cache.md) (what the GPU memory
holds and how to trade context for concurrency).

**Contents**

- [The path](#the-path)
- [Who decides what](#who-decides-what)
- [How the engines are organised](#how-the-engines-are-organised)
- [Paths that bypass EPP](#paths-that-bypass-epp)
- [Timeouts and limits on the path](#timeouts-and-limits-on-the-path)
- [Where it lives](#where-it-lives)
- [Related](#related)

## The path

```
client (Open WebUI | pat-service)
  -> ai-gateway-private (Envoy AI Gateway, envoy-gateway-system)
       SecurityPolicy llmd-jwt: Keycloak JWT required
       BackendTrafficPolicy llmd-per-user-limit: requests/minute per X-User-Id
       AIGatewayRoute llmd: rule by model name (x-ai-eg-model)
  -> AIServiceBackend llmd-qwen-test-openai
  -> llm-d EPP pod (llmd-qwen-test-epp): Envoy sidecar + ext-proc
       flow control: enqueue per band and per user, dispatch when not saturated
       scheduling: score endpoints, pick one
  -> engine pod (vLLM :8000 or SGLang :30000), OpenAI API
```

The model name clients send, `qwen-3.8-27b` (`inference.modelName`), is the
same whichever engine is live. The gateway routes by that name; EPP knows
which engine pods to dispatch to from its `modelServers` selector, which
`make llmd-up` derives from `INFERENCE_ENGINE`
([engines](../engines/README.md#switching-the-live-engine)).

The AI Gateway also emits one GenAI span per request (prompt, answer, model,
tokens, user, session) that ends up in Langfuse
([observability](../observability/langfuse.md)).

## Who decides what

| Decision | Owner | Configured in |
| --- | --- | --- |
| Is the caller allowed at all | AI Gateway (JWT), pat-service (PAT) | `helm/airgap-stack/templates/llmd.yaml`, Keycloak |
| How many requests per minute per user | AI Gateway rate limit | `inference.perUserRateLimitPerMinute` |
| Which band and fairness id a request carries | pat-service (label only) | `QOS_*` env |
| When a request is dispatched, in which order | llm-d EPP flow control | `config/llmd/router-nvfp4-values.yaml` `flowControl` |
| Which engine pod gets it | llm-d EPP scheduler | same file, `schedulingProfiles`, `modelServers` |
| How many sequences run at once on the GPU, context length, KV memory | the engine | `helm/<engine>-inference/values.yaml` |

pat-service never holds a request back; see
[PAT service: what it does not do](../pat-service.md#what-it-does-not-do).

## How the engines are organised

- **One GPU, one engine.** vLLM, SGLang and ninfer each request
  `nvidia.com/gpu: 1` with `strategy: Recreate`; a second engine stays
  `Pending` instead of fighting for the card. Only the embeddings server
  shares the GPU, outside scheduler accounting, with a fixed memory budget.
- **Each engine is its own Helm release** (`helm/vllm-inference`,
  `helm/sglang-inference`, `helm/ninfer-inference`), so a launch flag change
  restarts only that pod ([ADR 0007](../../adr/0007-inference-engines-as-helm-releases.md)).
- **Weights are on a read-only host-path PV** (`/var/lib/models` in the k3d
  node), shared by all engines; no weights in images.
- **One replica.** EPP's scorers choose between endpoints; with one pod there
  is nothing to choose, so on this site EPP's value is the queue, not the
  routing. The scorers matter the day a second replica or GPU appears.
- **Concurrency is the engine's.** vLLM runs `inference.maxNumSeqs` (2)
  sequences at once, SGLang `inference.maxRunningRequests` (3). Everything
  above that waits: first inside the engine, and once EPP sees saturation,
  in EPP's queue where bands and fairness apply.

Engine details, switching and adding engines: [engines](../engines/README.md).

## Paths that bypass EPP

| Traffic | Why | Consequence |
| --- | --- | --- |
| `qwen-3.8-27b` with `INFERENCE_ENGINE=ninfer` | ninfer has no `/metrics` EPP can read ([ADR 0015](../../adr/0015-third-party-engine-metrics-contract.md)) | the route points straight at ninfer: no bands, no fairness, only the per-user rate limit |
| `llamacpp-local`, `external-api` (when enabled) | their own model names and route rules ([ADR 0006](../../adr/0006-pluggable-inference-backends.md)) | same: no queue in front of them |
| `bge-m3` embeddings | separate route `embeddings`, own rate limit | not queued with chat |

## Timeouts and limits on the path

| Where | Value | Set in |
| --- | --- | --- |
| Edge `/v1` request timeout | 10 min | `helm/airgap-stack/templates/routes-external.yaml` |
| AI Gateway rule request and stream idle timeout | 10 min | `helm/airgap-stack/templates/llmd.yaml` |
| EPP queue TTL | 10 min (`flowControl.defaultRequestTTL`) | `config/llmd/router-nvfp4-values.yaml` |
| Request body buffer on the model route | 4 MiB | `llmd.yaml` `llmd-per-user-limit` |
| Per-user rate limit | 60 / min | `inference.perUserRateLimitPerMinute` |
| Per-IP limit on `/v1` at the edge | 60 / min | `routes-external.yaml` `edge-api-ip-rate-limit` |

How to change them from what the metrics show: [tuning](../configuration/tuning.md).

## Where it lives

- Gateway routes and policies: `helm/airgap-stack/templates/llmd.yaml`,
  `inference-backends.yaml`, `embeddings.yaml`; values `inference.*`
- EPP: `config/llmd/router-nvfp4-values.yaml`, `make llmd-up`
- Engines: `helm/*-inference/values.yaml`, `make engine-up`
- Runbooks: [inference-backends.md](../../operations/inference-backends.md),
  [vllm-inference.md](../../operations/vllm-inference.md)

## Related

- Previous: [PAT service](../pat-service.md). Next: [Queues and fair share](queues-and-fair-share.md)
- [ADR 0008](../../adr/0008-per-user-fair-share.md) fair share,
  [ADR 0012](../../adr/0012-graduated-band-ceiling-over-strict-priority.md)
  band ceiling, [ADR 0010](../../adr/0010-sglang-hicache-kv-offload.md) KV offload
