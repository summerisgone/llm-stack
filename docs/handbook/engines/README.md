# Engines

[Русский](README.ru.md) | [Handbook index](../README.md)

The engine is the process that runs the model on the GPU. vLLM and SGLang are
first-class: they sit behind llm-d EPP and get queueing and fair share.
ninfer can take the same model name but bypasses EPP. llama.cpp and external
OpenAI-compatible APIs are extra model names. This page covers the
first-class engines and the one command that switches between all three
in-cluster engines; [adding an engine](adding-an-engine.md) covers the rest.

**Contents**

- [Engine matrix](#engine-matrix)
- [How a first-class engine is wired](#how-a-first-class-engine-is-wired)
- [Switching the live engine](#switching-the-live-engine)
- [Changing an engine's settings](#changing-an-engines-settings)
- [Embeddings](#embeddings)
- [Checks after a change](#checks-after-a-change)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Engine matrix

| Engine | Model name | Release, command | Behind EPP | Metrics | Status |
| --- | --- | --- | --- | --- | --- |
| vLLM 0.27.1 | `qwen-3.8-27b` | `helm/vllm-inference`, `make vllm-up` | yes | native `/metrics` (`vllm:*`) | default |
| SGLang 0.5.19 | `qwen-3.8-27b` | `helm/sglang-inference`, `make sglang-up` | yes | native `/metrics` (`sglang:*`) | first-class alternative |
| ninfer | `qwen-3.8-27b` | `helm/ninfer-inference`, `make ninfer-up` | no, direct route | JSONL log re-exported by a sidecar (`ninfer_*`) | pilot |
| llama.cpp | `llamacpp-local` | host Docker, `deploy/llamacpp` | no | none | optional, off |
| external API | `external-api` | not managed | no | none | optional, off |
| bge-m3 (embeddings) | `bge-m3` | `helm/embeddings-inference`, `make embeddings-up` | no, own route | native | on |

All in-cluster engines mount the same read-only model PV and request
`nvidia.com/gpu: 1`, so only one can run.

## How a first-class engine is wired

```
AIGatewayRoute llmd, rule openai-qwen38nvfp4 (model == qwen-3.8-27b)
  -> AIServiceBackend llmd-qwen-test-openai            (never an engine directly)
  -> EPP: InferencePool selector app.kubernetes.io/name=<engine deployment>, port 8000|30000
  -> engine pod, labelled llm-d.ai/engine-type=vllm|sglang
```

Three things must agree for EPP to dispatch to an engine, and
`make engine-up` sets all three from one variable:

1. **The selector and port** in EPP's `router.modelServers`
   (`LLMD_ENGINE_SETS` in the Makefile, passed to `make llmd-up`).
2. **The engine-type label** on the pod (`llm-d.ai/engine-type`, set by the
   engine chart). EPP's `core-metrics-extractor` reads it to know the metric
   names for queue depth and KV use; without it EPP assumes vLLM, the metrics
   do not parse and the endpoint goes stale.
3. **The upstream key.** SGLang requires a bearer token. With
   `inference.sglang.enabled=true` (set by `make helm-up` when
   `INFERENCE_ENGINE=sglang`) the airgap-stack chart creates the
   `sglang-api-key` Secret from `SGLANG_API_KEY` and a
   `BackendSecurityPolicy` that injects it on the EPP path. vLLM needs none.

The gateway rule itself never changes between vLLM and SGLang. For ninfer the
same rule is pointed at ninfer's own backend (`inference.ninfer.live=true`).

## Switching the live engine

The live engine is one variable in `.env`:

```sh
# .env
INFERENCE_ENGINE=sglang        # vllm | sglang | ninfer
```

```sh
make engine-up
```

`engine-up` runs, in this order:

1. scales the GPU embeddings pod to 0 (the LLM engine must start first,
   [ADR 0013](../../adr/0013-embeddings-api-bge-m3.md));
2. scales the other engines to 0 and waits for their pods to go;
3. `make helm-up`: routes `qwen-3.8-27b` (to EPP, or straight to ninfer) and
   creates the engine's API key Secret;
4. `make <engine>-up` and waits until the engine is Ready (model load takes
   a few minutes);
5. `make llmd-up`: points EPP at the engine;
6. `make embeddings-up` and scales embeddings back to 1.

Clients keep sending `qwen-3.8-27b`; requests fail during the switch, so warn
users first. `helm-up` and `llmd-up` read `INFERENCE_ENGINE` every time they
run, so keep `.env` equal to what is running: a plain `make helm-up` with a
stale value re-routes the model name. A one-off
`make engine-up INFERENCE_ENGINE=vllm` works for that invocation only.
`make stack-up` brings up the engine named in `.env` the same way.

Committed values (`inference.sglang.enabled`, `inference.ninfer.live`,
`router.modelServers`) are render defaults for vLLM; they no longer record
which engine a site runs.

## Changing an engine's settings

Launch flags are values, not manifests: `helm/vllm-inference/values.yaml`
(`maxModelLen`, `maxNumSeqs`, `gpuMemoryUtilization`, ...) and
`helm/sglang-inference/values.yaml` (`contextLength`, `maxRunningRequests`,
`memFractionStatic`, ...). Apply with `make vllm-up` / `make sglang-up`
while that engine is live; only its pod restarts. What each value does and
how they trade against each other: [KV cache](../inference/kv-cache.md).
When the GPU embeddings server is on, restart it after the engine
(`kubectl -n airgap-ai-stack rollout restart deploy/embeddings-bge-m3`).

`make verify` does not lint the engine charts; template them by hand:
`helm template x helm/vllm-inference >/dev/null`.

## Embeddings

`bge-m3` serves `/v1/embeddings` through its own route and rate limit
(`inference.embeddings.*`, 120 requests per minute per user). In
`accelerator: gpu` mode (`helm/embeddings-inference/values.yaml`) it borrows
`gpu.gpuMemoryBudgetGiB` of GPU memory outside Kubernetes GPU accounting, so
the LLM engine's memory share must leave that much free and the start order
is fixed: engine first. `accelerator: cpu` has no such constraint. repowise
needs the GPU mode ([ADR 0013](../../adr/0013-embeddings-api-bge-m3.md),
[ADR 0018](../../adr/0018-repowise-codebase-intelligence.md)).

The 3 GiB budget was measured with ninfer as the live engine (embeddings
peak about 2.2 GiB). vLLM at `gpuMemoryUtilization: 0.94` takes 29.9 of the
card's 31.8 GiB, leaving about 1.9 GiB: both start and answer (verified), but
heavy embedding load under vLLM can run the card out of memory. Lower
`gpuMemoryUtilization` before such load, or use `accelerator: cpu` while vLLM
is live.

## Checks after a change

```sh
kubectl -n airgap-ai-stack get pods -l 'app.kubernetes.io/name in (vllm-qwen38-nvfp4,sglang-qwen38,ninfer-qwen38,embeddings-bge-m3)' -L llm-d.ai/engine-type
kubectl -n airgap-ai-stack get aigatewayroute llmd -o jsonpath='{range .spec.rules[*]}{.name}{" -> "}{.backendRefs[0].name}{"\n"}{end}'
make llmd-nvfp4-smoke      # PAT issue, inference, rate limit, revoke (STACK_BASE_URL set)
```

On EPP's `:9090/metrics`, `llm_d_epp_ready_endpoints` is 1 for vLLM or
SGLang (0 with ninfer, which EPP does not serve), and
`llm_d_epp_flow_control_stale_endpoints` stays 0.

## Where it lives

- Charts: `helm/vllm-inference`, `helm/sglang-inference`,
  `helm/ninfer-inference`, `helm/embeddings-inference`
- Switch logic: `Makefile` (`INFERENCE_ENGINE`, `engine-up`,
  `LLMD_ENGINE_SETS`, `HELM_ENGINE_SETS`)
- Gateway rules: `helm/airgap-stack/templates/llmd.yaml`,
  `inference-backends.yaml`
- Images: `versions.lock.env`; the vLLM image is built on the GPU host
  (`deploy/vllm-qwen38-nvfp4`)
- Runbooks: [inference-backends.md](../../operations/inference-backends.md),
  [vllm-inference.md](../../operations/vllm-inference.md),
  [deploy/sglang-qwen38](../../../deploy/sglang-qwen38/README.md)

## Related

- Previous: [KV cache](../inference/kv-cache.md). Next: [Adding an engine](adding-an-engine.md)
- [ADR 0006](../../adr/0006-pluggable-inference-backends.md),
  [ADR 0007](../../adr/0007-inference-engines-as-helm-releases.md),
  [ADR 0015](../../adr/0015-third-party-engine-metrics-contract.md)
