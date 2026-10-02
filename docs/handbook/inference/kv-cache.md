# KV cache

[Русский](kv-cache.ru.md) | [Handbook index](../README.md)

The KV cache is the GPU memory that holds each running conversation's
attention state. Its size decides how long a context can be, how many
sequences run at once, and how often a returning conversation gets its prefix
for free. This page explains the levers each engine exposes and how the rest
of the stack (EPP, pat-service bands) uses the cache.

**Contents**

- [Why it matters here](#why-it-matters-here)
- [The model makes it special](#the-model-makes-it-special)
- [vLLM](#vllm)
- [SGLang](#sglang)
- [ninfer](#ninfer)
- [How the stack uses the cache](#how-the-stack-uses-the-cache)
- [Trading context for concurrency](#trading-context-for-concurrency)
- [What to watch](#what-to-watch)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Why it matters here

One 32 GB card holds the 27B model weights and whatever is left becomes the
KV pool. vLLM at `gpuMemoryUtilization: 0.94` logs 19.1 GiB for the weights and
7.6 GiB of KV cache, which is 230,104 tokens in FP8. Coding agents send
long, growing prompts: each step resends the whole history. Two effects follow:

- **Prefix reuse is the main speed-up.** If the previous step's prefix is
  still cached, only the new tail is computed. That is why pat-service marks
  a session `warm` for 120 s after its last dispatch and EPP serves `warm`
  first ([queues](queues-and-fair-share.md)).
- **The pool, not compute, is the bottleneck.** EPP's saturation signal
  treats 80 % KV utilisation as saturated.

## The model makes it special

`qwen-3.8-27b` (architecture `Qwen3_5ForConditionalGeneration`) is a hybrid:
16 full-attention layers and 48 linear-attention (Mamba-style) layers. A
reusable prefix therefore has two parts, attention KV and Mamba state, and
engines cache the Mamba part differently and less maturely. Most surprises
below come from that.

## vLLM

Values under `inference` in `helm/vllm-inference/values.yaml`:

| Value | Setting | Why |
| --- | --- | --- |
| `gpuMemoryUtilization` | `0.94` | 0.95 does not start on this card (about 30.2 GiB is ever free); 0.94 is one step back |
| `maxModelLen` | `131072` | per-request context; vLLM only requires one full-length sequence to fit |
| `maxNumSeqs` | `2` | sequences decoded at once: the admission ceiling |
| `maxNumBatchedTokens` | `8192` | prefill chunk per scheduler step |
| `kvCacheDtype` | `fp8` | halves KV memory against BF16 |
| `prefixCaching` | `true` | reuse of identical prefixes; for this model vLLM switches the Mamba cache to `align` mode on its own |
| `kvOffloadingSizeGiB` / `kvOffloadingBackend` | `8` / `native` | evicted KV blocks spill to a `/dev/shm` buffer, so a preempted session reloads instead of recomputing; `dshmSizeGiB` (12) must exceed it |

At 131072 the pool holds about 230 K tokens, i.e. 1.76 full-length contexts:
two maximum-length sessions at once would need about 12 % more, which the CPU
offload absorbs by preemption plus reload. Offload does **not** let one
sequence exceed the GPU pool. The measurements and the tuning history are in
[vllm-inference.md](../../operations/vllm-inference.md).

## SGLang

Values under `inference` in `helm/sglang-inference/values.yaml`:

| Value | Setting | Why |
| --- | --- | --- |
| `contextLength` | `180224` | per-request context; also advertised as `max_model_len` in `/v1/models` |
| `memFractionStatic` | `"0.90"` | weights plus KV pool share of GPU memory |
| `maxRunningRequests` | `3` | admission ceiling |
| `maxMambaCacheSize` | `12` | Mamba state slots |
| `mambaRadixCacheStrategy` | `extra_buffer_lazy` | how Mamba state joins the radix (prefix) cache |
| `chunkedPrefillSize` | `2048` | prefill chunk |
| `kvCacheDtype` | `auto` | model dtype |

KV stays on the GPU. HiCache (a host-RAM tier) and CPU weight offload crash
on this hybrid checkpoint with SGLang 0.5.19 on the RTX 5090; ADR 0010 keeps
them off until an upstream fix passes the tests listed there
([ADR 0010](../../adr/0010-sglang-hicache-kv-offload.md),
[study](../../operations/sglang-hicache-study-2026-09-07.md)).

## ninfer

ninfer has its own prefix cache (responses report
`prompt_tokens_details.cached_tokens`) but no Prometheus `/metrics` EPP could
read, so EPP cannot see its KV state and the route bypasses EPP
([engines](../engines/adding-an-engine.md#ninfer)). Its memory settings are in
`helm/ninfer-inference/values.yaml`.

## How the stack uses the cache

| Component | Uses | How |
| --- | --- | --- |
| pat-service | warmth | `warm` band for sessions dispatched within `QOS_WARM_TTL_SECONDS` |
| EPP flow control | utilisation | `utilization-detector`: saturated at `kvCacheUtilThreshold` 0.8 |
| EPP scheduler | utilisation, prefixes | `kv-cache-utilization-scorer`, `prefix-cache-scorer` (only matters with several replicas) |
| embeddings on GPU | the leftover memory | `helm/embeddings-inference` `gpu.gpuMemoryBudgetGiB` (3) is carved out of what the LLM engine leaves; raising it means lowering `gpuMemoryUtilization` / `memFractionStatic` in the same change ([ADR 0013](../../adr/0013-embeddings-api-bge-m3.md)) |

## Trading context for concurrency

The pool is fixed by the card; the knobs only divide it:

- **Longer context** (`maxModelLen` / `contextLength`) lets one request be
  bigger but fewer full-size requests fit at once; vLLM then preempts, SGLang
  queues.
- **More slots** (`maxNumSeqs` / `maxRunningRequests`) raise throughput for
  short prompts, and make long prompts preempt each other.
- **More memory for the pool** (`gpuMemoryUtilization`,
  `memFractionStatic`) is capped by what the card really frees and by the
  embeddings budget.

Change one value, apply it with the engine's own `make <engine>-up`
(`make engines-up` if replicas change too), and compare the metrics below
under real load. The procedure is in [tuning](../configuration/tuning.md#engine-capacity).

## What to watch

| Question | vLLM | SGLang |
| --- | --- | --- |
| KV pool in use | `vllm:kv_cache_usage_perc` | `sglang:token_usage` |
| Prefix hits | `vllm:prefix_cache_hits_total` / `vllm:prefix_cache_queries_total` | `sglang:cache_hit_rate` |
| Requests waiting in the engine | `vllm:num_requests_waiting` | `sglang:num_queue_reqs` |
| Preemptions, offload traffic | `vllm:num_preemptions_total`, `vllm:kv_offload_total_bytes_total`, `vllm:kv_offload_cpu_cache_usage_perc` | - |
| Cached prompt tokens per user | `patsvc_cached_prompt_tokens_total` stays 0: neither vLLM 0.27.1 nor SGLang 0.5.19 report `cached_tokens` in `usage` (ninfer does) | same |

Dashboards: vLLM / vLLM, llm-d / Performance and KV cache, QoS / Fair share
for SGLang ([observability](../observability/README.md)).

## Where it lives

- `helm/vllm-inference/values.yaml`, `helm/sglang-inference/values.yaml`,
  `helm/ninfer-inference/values.yaml`
- Flag mapping: `helm/<engine>-inference/templates/inference.yaml`
- Measurements: [vllm-inference.md](../../operations/vllm-inference.md),
  `tests/inference/sglang-hicache-results-*.md`

## Related

- Previous: [Queues and fair share](queues-and-fair-share.md). Next: [Engines](../engines/README.md)
- [ADR 0010](../../adr/0010-sglang-hicache-kv-offload.md), [ADR 0013](../../adr/0013-embeddings-api-bge-m3.md)
