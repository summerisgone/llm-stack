# Adding an engine

[Русский](adding-an-engine.ru.md) | [Handbook index](../README.md)

Three ways to connect an engine other than vLLM and SGLang, from least to most
integrated, with ninfer and llama.cpp as the worked examples. Read
[engines](README.md) first for how the first-class engines are wired.

**Contents**

- [Pick the pattern](#pick-the-pattern)
- [Pattern A: extra model name (llama.cpp, external API)](#pattern-a-extra-model-name-llamacpp-external-api)
- [Pattern B: in-cluster engine under its own name (ninfer)](#pattern-b-in-cluster-engine-under-its-own-name-ninfer)
- [Pattern C: first-class behind EPP](#pattern-c-first-class-behind-epp)
- [Metrics contract](#metrics-contract)
- [ninfer](#ninfer)
- [Strata](#strata)
- [llama.cpp](#llamacpp)
- [Checklist](#checklist)
- [Related](#related)

## Pick the pattern

| Pattern | Model name | Queue and fair share | Needs from the engine | Example |
| --- | --- | --- | --- | --- |
| A. extra model name | its own (`llamacpp-local`) | no | OpenAI API | llama.cpp, external API |
| B. in-cluster engine, own name | its own (`qwen-3.8-27b-ninfer`) | no | OpenAI API | ninfer |
| C. behind EPP, pool member | `qwen-3.8-27b` | yes | OpenAI API, Prometheus `/metrics` in a shape EPP understands | vLLM, SGLang |

The deciding question for C is whether llm-d's `core-metrics-extractor` knows
the engine (built in: `vllm`, `sglang`, `trtllm-serve`, `triton-tensorrt-llm`,
`triton`, `atom`) or can be taught its metric names through `engineConfigs`.
If not, EPP cannot read queue depth and KV use, marks the endpoint stale and
fails closed.

## Pattern A: extra model name (llama.cpp, external API)

The gateway gets a `Backend` + `AIServiceBackend` pair and one exact-match rule
on a new model name ([ADR 0006](../../adr/0006-pluggable-inference-backends.md)).
Templates are already in `helm/airgap-stack/templates/inference-backends.yaml`
and `llmd.yaml`, switched by values:

```yaml
# helm/airgap-stack/values.yaml
inference:
  llamacpp:
    enabled: true            # the route exists; nothing is started
    host: host.k3d.internal  # host-run container, or a Service DNS name
    port: 8090
    modelName: llamacpp-local
```

Then `make helm-up`, add the model id to Open WebUI's connection `0`
`model_ids` if people should see it ([Open WebUI](../openwebui.md#model-connections)),
and put an upstream key, if the engine needs one, in `.env`
(`EXTERNAL_API_KEY`). The same JWT check and per-user rate limit apply; there
is no queue in front of it. For a new engine type, copy the llamacpp blocks
under a new `inference.<name>` key.

## Pattern B: in-cluster engine under its own name (ninfer)

Pattern A for an engine that runs as an in-cluster GPU Deployment: a chart
like `helm/ninfer-inference` (GPU request, `nodeSelector` and toleration for
the GPU workers, no `llm-d.ai/model` label), a Makefile `<name>-up` with
`--set replicas=$(<NAME>_REPLICAS)` and a case in `engines-up`, and in
`helm/airgap-stack` a `Backend`/`AIServiceBackend` pair plus one exact-match
rule on its own model name. For ninfer that is `inference.ninfer.enabled`
and `inference.ninfer.modelName: qwen-3.8-27b-ninfer`, rule `openai-ninfer`
in `llmd.yaml` ([ADR 0019](../../adr/0019-inference-plane-gpu-worker-nodes.md)).
It does not take over `qwen-3.8-27b`: that would turn fair share off for
every client of the pool.

The price for its own clients: no bands, no per-user fairness, and whatever
API gaps the engine has (ninfer refuses JSON-mode `response_format`). On one
GPU it answers only while the pool has 0 replicas.

## Pattern C: first-class behind EPP

For an engine EPP understands, the new engine's pods join the
`qwen-3.8-27b` pool:

1. An engine chart like `helm/sglang-inference`: `Recreate`,
   `nvidia.com/gpu: 1`, the GPU worker `nodeSelector` and toleration, port
   8000, no API key, pod labels `llm-d.ai/model: <served name>` and
   `llm-d.ai/engine-type: <type>`.
2. Makefile: `<NAME>_REPLICAS`, `ENGINE_DEPLOYMENT_<name>`, a `<name>-up`
   target with `--set replicas=$(<NAME>_REPLICAS)`, and its cases in
   `engines-up`.
3. A Prometheus scrape job (pod discovery, port 8000) and dashboards (below).
4. Test it with replicas 1 and the others 0, then mixed if there are GPUs
   for it: `llm_d_epp_ready_endpoints` counts every member.

## Metrics contract

[ADR 0015](../../adr/0015-third-party-engine-metrics-contract.md) splits what an
engine must provide into three tiers:

| Tier | Requirement | Gives |
| --- | --- | --- |
| 1, mandatory | OpenAI `usage` in responses (`prompt_tokens`, `completion_tokens`; `cached_tokens` optional) | pat-service token and cost accounting, dashboard usage |
| 2, mandatory | a Prometheus scrape target, even if only `up` | "is it alive" in Grafana |
| 3, optional | engine-native request metrics (TTFT, queue, KV) | engine dashboards, one panel at a time |

Scrape jobs live in
`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml` (static
targets with `namespace=airgap-ai-stack`); dashboards in
`config/grafana/dashboards/` loaded by `make monitoring-up`
([observability](../observability/README.md)).

## ninfer

- Chart `helm/ninfer-inference` (engine plus a `jsonl_exporter.py` sidecar),
  image `NINFER_IMAGE` built from `NINFER_UPSTREAM_REF`, model file
  `qwen3_8_27b_nvfp4.ninfer` from MinIO through the node model cache (its
  own converted format, not the vLLM checkpoint).
- API key: `NINFER_API_KEY` in `.env` -> Secret `ninfer-api-key` (created by
  `make helm-up`), used by both the server and the gateway.
- Metrics: no native `/metrics`; the sidecar turns the request log into
  `ninfer_*` series on `:9400` (Prometheus job `ninfer`, dashboard
  `ninfer.json`). Tier 3 is partial: no per-token latency, KV as raw pages.
- Strengths measured in the pilot: MTP speculative decoding with a small
  draft head and a prefix cache that held under long concurrent agent
  sessions. Details and risks: [deploy/ninfer](../../../deploy/ninfer/README.md).

## Strata

Qwen3.8-Flash-Next IQ3_S, model name `qwen-3.8-flash-next`, a MoE model
that runs from one GPU plus system RAM. Pattern A: a host Docker container
(`deploy/strata/run`, `config/strata/`) on `host.k3d.internal:8095`, routed
through `inference.externalApi` with `EXTERNAL_API_KEY`.

- It holds the GPU outside Kubernetes: every engine replica stays 0 while it
  runs, and `STRATA_ON_HOST=1` lets GPU embeddings start after it.
- One request at a time behind its own FIFO; no Prometheus metrics.
- Pods reach `host.k3d.internal` only through the CoreDNS alias from
  `deploy/vllm-qwen38-nvfp4/k3d-host-dns` (k3s drops k3d's NodeHosts entry
  when nodes change); the same applies to llama.cpp.
- Measured through the cluster: prefill 2.3-4.8K tok/s, decode 83-116 tok/s.
- Measured on the host: prefill 3.7-5.4K tok/s, decode 136-152 tok/s.
- `helm/strata-inference` runs it as pattern B (MinIO weights, `strata_*`
  metrics sidecar). Off here: the pod got too little RAM for the experts the
  GPU does not hold and ran 5-6x slower.
  Details: [deploy/strata](../../../deploy/strata/README.md).

## llama.cpp

A host Docker container (`deploy/llamacpp/run`, profile
`config/llamacpp/qwen-gguf.env`), reached from the cluster at
`host.k3d.internal:8090`. Not deployed: it needs a real, checksummed GGUF
file in `MODEL_GGUF`. It is invisible to Kubernetes GPU accounting, so keep
`N_GPU_LAYERS=0` (CPU) unless no in-cluster engine holds the GPU. No metrics
are scraped today. Details: [deploy/llamacpp](../../../deploy/llamacpp/README.md).

## Checklist

- [ ] OpenAI-compatible chat completions, streaming and `usage` verified directly against the engine
- [ ] Chart or host run documented; image pinned in `versions.lock.env`
- [ ] Route: pattern A or B values and rule, or pattern C pool labels
- [ ] Upstream key in `.env` and a `BackendSecurityPolicy`, if needed
- [ ] Open WebUI `model_ids` updated when a new model name appears
- [ ] Prometheus scrape job and at least an `up` panel
- [ ] `make verify`, the engine's smoke, `make llmd-nvfp4-smoke`
- [ ] This page and [engines](README.md) updated in both languages

## Related

- Previous: [Engines](README.md). Next: [Observability](../observability/README.md)
- Runbook: [inference-backends.md](../../operations/inference-backends.md)
