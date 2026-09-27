# Adding an engine

[Русский](adding-an-engine.ru.md) | [Handbook index](../README.md)

Three ways to connect an engine other than vLLM and SGLang, from least to most
integrated, with ninfer and llama.cpp as the worked examples. Read
[engines](README.md) first for how the first-class engines are wired.

**Contents**

- [Pick the pattern](#pick-the-pattern)
- [Pattern A: extra model name (llama.cpp, external API)](#pattern-a-extra-model-name-llamacpp-external-api)
- [Pattern B: live engine under the canonical name (ninfer)](#pattern-b-live-engine-under-the-canonical-name-ninfer)
- [Pattern C: first-class behind EPP](#pattern-c-first-class-behind-epp)
- [Metrics contract](#metrics-contract)
- [ninfer](#ninfer)
- [llama.cpp](#llamacpp)
- [Checklist](#checklist)
- [Related](#related)

## Pick the pattern

| Pattern | Model name | Queue and fair share | Needs from the engine | Example |
| --- | --- | --- | --- | --- |
| A. extra model name | its own (`llamacpp-local`) | no | OpenAI API | llama.cpp, external API |
| B. live engine, direct route | `qwen-3.8-27b` | no | OpenAI API, same model | ninfer |
| C. behind EPP | `qwen-3.8-27b` | yes | OpenAI API, Prometheus `/metrics` in a shape EPP understands | vLLM, SGLang |

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

## Pattern B: live engine under the canonical name (ninfer)

The engine takes over `qwen-3.8-27b` by retargeting the existing rule
(`openai-qwen38nvfp4` and the catch-all `openai` in `llmd.yaml`) to its own
backend. Clients see no change; EPP is skipped. For ninfer this is
`inference.ninfer.enabled` (objects exist) plus `inference.ninfer.live`
(route points at it), and `make engine-up` sets `live` from
`INFERENCE_ENGINE=ninfer`. A second engine of this kind would need its own
`live` flag in the same two rules and its own case in the Makefile's
`ENGINE_DEPLOYMENT_*` and `HELM_ENGINE_SETS`.

The price: no bands, no per-user fairness, and whatever API gaps the engine
has (ninfer refuses JSON-mode `response_format`).

## Pattern C: first-class behind EPP

For an engine EPP understands:

1. An engine chart like `helm/sglang-inference`: one replica, `Recreate`,
   `nvidia.com/gpu: 1`, the shared model PVC, pod labels
   `app.kubernetes.io/name: <deployment>` and `llm-d.ai/engine-type: <type>`.
2. Makefile: `ENGINE_DEPLOYMENT_<name>`, `EPP_PORT_<name>`, add it to
   `ENGINES` and to `EPP_ENGINE`, and a `<name>-up` target.
3. If it needs an upstream key, a Secret plus `BackendSecurityPolicy` on
   `llmd-qwen-test-openai` like `sglang-api-key` in `llmd.yaml`.
4. A Prometheus scrape job and dashboards (below).
5. Test the switch both ways with `make engine-up`.

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
  `ninfer-model-volume.yaml` (its own converted format, not the vLLM
  checkpoint).
- API key: `NINFER_API_KEY` in `.env` -> Secret `ninfer-api-key` (created by
  `make helm-up`), used by both the server and the gateway.
- Metrics: no native `/metrics`; the sidecar turns the request log into
  `ninfer_*` series on `:9400` (Prometheus job `ninfer`, dashboard
  `ninfer.json`). Tier 3 is partial: no per-token latency, KV as raw pages.
- Strengths measured in the pilot: MTP speculative decoding with a small
  draft head and a prefix cache that held under long concurrent agent
  sessions. Details and risks: [deploy/ninfer](../../../deploy/ninfer/README.md).

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
- [ ] Route: pattern A values, pattern B `live` flag, or pattern C EPP wiring
- [ ] Upstream key in `.env` and a `BackendSecurityPolicy`, if needed
- [ ] Open WebUI `model_ids` updated when a new model name appears
- [ ] Prometheus scrape job and at least an `up` panel
- [ ] `make verify`, the engine's smoke, `make llmd-nvfp4-smoke`
- [ ] This page and [engines](README.md) updated in both languages

## Related

- Previous: [Engines](README.md). Next: [Observability](../observability/README.md)
- Runbook: [inference-backends.md](../../operations/inference-backends.md)
