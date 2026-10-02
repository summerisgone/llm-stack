# Engines

[Русский](README.ru.md) | [Handbook index](../README.md)

The engine is the process that runs the model on the GPU. vLLM and SGLang are
first-class: their pods are members of the `qwen-3.8-27b` pool behind llm-d
EPP and get queueing and fair share. ninfer cannot join EPP and serves its own
model name, `qwen-3.8-27b-ninfer`. llama.cpp and external OpenAI-compatible
APIs are extra model names too. This page covers the pool, the engine
replicas and the one command that applies them;
[adding an engine](adding-an-engine.md) covers the rest.

**Contents**

- [Engine matrix](#engine-matrix)
- [How a first-class engine is wired](#how-a-first-class-engine-is-wired)
- [Engine replicas](#engine-replicas)
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
| ninfer | `qwen-3.8-27b-ninfer` | `helm/ninfer-inference`, `make ninfer-up` | no, direct route | JSONL log re-exported by a sidecar (`ninfer_*`) | pilot |
| llama.cpp | `llamacpp-local` | host Docker, `deploy/llamacpp` | no | none | optional, off |
| external API | `external-api` | not managed | no | none | optional, off |
| bge-m3 (embeddings) | `bge-m3` | `helm/embeddings-inference`, `make embeddings-up` | no, own route | native | on |

Every engine pod requests `nvidia.com/gpu: 1` and runs on a GPU worker
(`node-role/inference=true`,
[ADR 0019](../../adr/0019-inference-plane-gpu-worker-nodes.md)). With one GPU,
one engine pod runs at a time.

## How a first-class engine is wired

```
AIGatewayRoute llmd, rule openai-qwen38nvfp4 (model == qwen-3.8-27b)
  -> AIServiceBackend llmd-qwen-test-openai            (never an engine directly)
  -> EPP: InferencePool selector llm-d.ai/model=qwen-3.8-27b, port 8000
  -> any vLLM or SGLang pod with that label, llm-d.ai/engine-type=vllm|sglang
```

Two labels on the pod, both set by the engine chart, make it a pool member:

1. **`llm-d.ai/model`** (the chart's `inference.servedModelName`): EPP's
   `router.modelServers.matchLabels` in `config/llmd/router-nvfp4-values.yaml`
   selects on it, on port 8000 for every engine.
2. **`llm-d.ai/engine-type`**: EPP's `core-metrics-extractor` reads it to
   know the metric names for queue depth and KV use. Without it EPP assumes
   vLLM, SGLang metrics do not parse and the endpoint goes stale. vLLM and
   SGLang pods can therefore share the pool.

Engines run without an API key. The NetworkPolicy `inference-pool-members`
(`helm/airgap-stack/templates/llmd.yaml`) admits only the EPP pod and
Prometheus to port 8000 of any pod with `llm-d.ai/model`.

ninfer is not a pool member: rule `openai-ninfer` (model ==
`qwen-3.8-27b-ninfer`) goes straight to its `AIServiceBackend`, with its
API key injected by the gateway and no EPP queueing or fair share.

## Engine replicas

Capacity is replicas per engine, set in `.env`:

```sh
# .env
VLLM_REPLICAS=0
SGLANG_REPLICAS=1
NINFER_REPLICAS=0
```

```sh
make engines-up
```

`engines-up` runs, in this order:

1. scales the GPU embeddings pod to 0 (the LLM engine must start first,
   [ADR 0013](../../adr/0013-embeddings-api-bge-m3.md));
2. applies the engines going to 0 and waits for their pods to go;
3. applies the others with `make <engine>-up` and waits until they are Ready
   (model load takes a few minutes);
4. `make llmd-up`;
5. `make embeddings-up` and scales embeddings back to 1.

`make <engine>-up` alone applies that engine's values with its replica
count from `.env`. With one GPU keep the total at 1: `qwen-3.8-27b` answers
while vLLM or SGLang has a replica, `qwen-3.8-27b-ninfer` while ninfer has
one. Moving the GPU between vLLM and SGLang changes nothing for clients;
moving it to ninfer leaves `qwen-3.8-27b` without endpoints, so warn users
first. With more GPUs, vLLM and SGLang replicas add up in one pool.

## Changing an engine's settings

Launch flags are values, not manifests: `helm/vllm-inference/values.yaml`
(`maxModelLen`, `maxNumSeqs`, `gpuMemoryUtilization`, ...) and
`helm/sglang-inference/values.yaml` (`contextLength`, `maxRunningRequests`,
`memFractionStatic`, ...). Apply with `make vllm-up` / `make sglang-up`
while that engine has replicas; only its pods restart. What each value does and
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

On EPP's `:9090/metrics`, `llm_d_epp_ready_endpoints` equals the vLLM plus
SGLang replicas (0 while only ninfer runs, which EPP does not serve), and
`llm_d_epp_flow_control_stale_endpoints` stays 0.

## Where it lives

- Charts: `helm/vllm-inference`, `helm/sglang-inference`,
  `helm/ninfer-inference`, `helm/embeddings-inference`
- Replicas: `Makefile` (`VLLM_REPLICAS`, `SGLANG_REPLICAS`,
  `NINFER_REPLICAS`, `engines-up`)
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
