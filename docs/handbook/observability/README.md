# Observability

[Русский](README.ru.md) | [Handbook index](../README.md)

Three signals, three places: **metrics** in Prometheus, shown in Grafana;
**traces** of every model call (prompt, answer, user, session) in Langfuse;
**logs** of containers in Loki through Grafana. This page says what emits what,
how to reach it, and which dashboard answers which question. The trace model
is on [Langfuse](langfuse.md); what is stored and for how long is on
[data and retention](data-and-retention.md).

**Contents**

- [Map](#map)
- [Reaching the UIs](#reaching-the-uis)
- [Metrics sources](#metrics-sources)
- [Dashboards by question](#dashboards-by-question)
- [Logs](#logs)
- [Alerts](#alerts)
- [Gaps](#gaps)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Map

```
engines, EPP, pat-service, node, GPU --scrape 15s--> Prometheus (airgap-ai-stack)
                                                          |  datasource "vLLM Prometheus"
AI Gateway GenAI spans --OTLP--> OTEL Collector --> Langfuse (web + worker; Postgres, ClickHouse, MinIO, Valkey)
                                                          |
container logs --fluent-bit--> Loki (monitoring)          v
Envoy metrics --> Prometheus (monitoring, eg-addons)  Grafana (monitoring, eg-addons)
```

There are two Prometheus instances: the application one in `airgap-ai-stack`
(a plain Deployment, all LLM metrics) and the one bundled with the Envoy
Gateway add-ons in `monitoring` (Envoy metrics). The operator Grafana is the
add-ons one in `monitoring`; it reads both. A second, older Grafana
Deployment in `airgap-ai-stack` is not the one to use.

## Reaching the UIs

Neither Grafana nor Langfuse has a public route. They are exposed as
NodePorts on the GPU host (`routing.nodePorts` in
`helm/airgap-stack/values.yaml`: Grafana 32030, Langfuse 32031), reached from
the operator network. The site's URLs are `GRAFANA_BASE_URL` and the Langfuse
public URL patch (`k8s/overlays/remote-wsl-vllm-nvfp4/langfuse-public-url-patch.yaml`).

| UI | Sign-in | Who |
| --- | --- | --- |
| Grafana | Keycloak SSO only (login form off); `ai-admin` is Grafana Admin, everyone else Viewer | operators, team leads |
| Langfuse | its own accounts; the first admin from `LANGFUSE_INIT_USER_*` in `.env` | operators only: it shows prompts and answers |
| Prometheus | no UI exposed; `kubectl -n airgap-ai-stack port-forward svc/prometheus 9090` | ad-hoc PromQL |

## Metrics sources

Scrape jobs of the application Prometheus
(`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`), all with
`namespace=airgap-ai-stack`:

| Job | Target | Metric prefix | Tells you |
| --- | --- | --- | --- |
| `vllm-qwen38-nvfp4` | `:8000/metrics` | `vllm:` | engine queue, KV use, TTFT, throughput, prefix hits |
| `sglang-qwen38` | `:8000/metrics` | `sglang:` | same for SGLang |
| `ninfer` | sidecar `:9400` | `ninfer_` | ninfer request log re-exported |
| `embeddings-bge-m3` | `:80` | text-embeddings-inference metrics | embeddings server |
| `llmd-epp` | EPP `:9090` | `llm_d_epp_` (and deprecated `inference_extension_`) | queues per band, saturation, per-user TTFT, ready and stale endpoints |
| `pat-service` | `:9090` | `patsvc_` | per-user requests by band, tokens, cost, sessions, MCP calls |
| `node` | node-exporter | `node_` | host CPU, memory, disk, network |
| `gpu-exporter` (DaemonSet on GPU workers) | `:9400` | `gpu_` | GPU utilisation, memory, temperature (`nvidia-smi`; DCGM does not work on WSL2) |

pat-service metrics carry `user` (the Keycloak `sub`); EPP metrics carry
`fairness_id` (the same `sub`, or `default-flow` for Open WebUI traffic).
Resolve a `sub` to a name in Keycloak or in Langfuse.

Not scraped by this Prometheus: Envoy (the envoy-gateway dashboards read the add-ons chart's own Prometheus),
agent-broker (no metrics yet), web-search-mcp, Langfuse, ClickHouse,
kube-state-metrics and cAdvisor (no per-pod resource panels).

## Dashboards by question

Folders in Grafana, loaded from `config/grafana/dashboards/` by
`make monitoring-up`:

| Question | Dashboard (folder / name) |
| --- | --- |
| Is everything up? | cluster-monitor / Cluster monitor (`up{job}` for every target, node view) |
| Is the GPU busy, hot, full? | system-state / System state |
| What limit or queue knob should I change? | QoS / Cluster load (built for this) |
| Who uses how much, with what latency? | QoS / User activity (per-user tokens, TTFT, cost, outcomes, stale endpoints) |
| Do sessions stitch, which bands are assigned? | QoS / Fair share |
| Engine internals: running, waiting, KV, TTFT | vLLM / vLLM; llm-d / vLLM overview or SGLang overview |
| EPP queues, request sizes, scheduling latency | llm-d / Inference Gateway, Failure and saturation, Diagnostic drill-down, Performance and KV cache |
| ninfer | ninfer |
| Gateway traffic, 429s, upstream latency | envoy-gateway (from the add-ons chart) |

The llm-d prefill/decode dashboard stays empty: this site does not split
prefill and decode.

## Logs

fluent-bit ships container logs to Loki in `monitoring`; query them in
Grafana Explore with the Loki datasource. Retention is the chart default
([data and retention](data-and-retention.md)). For quick checks `kubectl
logs` is faster:

```sh
kubectl -n airgap-ai-stack logs deploy/pat-service --tail=100
kubectl -n airgap-ai-stack logs deploy/llmd-qwen-test-epp -c epp --tail=100
kubectl -n agents logs deploy/agent-broker --tail=100
```

## Alerts

Three Grafana alert rules are provisioned (`config/gateway-addons/values.yaml`,
`grafana.alerting`):

| Rule | Fires when | Meaning |
| --- | --- | --- |
| `adr0012-stale-endpoints` (critical) | `max_over_time(llm_d_epp_flow_control_stale_endpoints[5m]) > 0` | EPP cannot read an engine's metrics and has stopped dispatching to it |
| `adr0019-no-ready-endpoints` (critical) | `max_over_time(llm_d_epp_ready_endpoints[5m]) < 1`, or no data | the `qwen-3.8-27b` pool has had no ready pod for 5 minutes; expected only while its replicas are deliberately 0 |
| `adr0019-metrics-errors` (warning) | EPP poll or extract errors grew over 10 minutes, for 5 minutes | a pool member's `/metrics` is unreachable or unparsable (GPU worker down, missing `llm-d.ai/engine-type`, NetworkPolicy) |

No contact
point is configured in the repository; set one in Grafana if someone should
be paged.

## Gaps

- No retention is set for Prometheus, Loki, Tempo, Langfuse or its stores:
  everything runs on defaults ([data and retention](data-and-retention.md)).
- No agent-broker metrics; agent activity is visible only in pat-service
  metrics (as the user's agent PAT) and in logs.
- `patsvc_cost_units_total` panels were built when only non-streaming usage
  was parsed; pat-service now also reads streaming usage, so older data
  undercounts coding-agent traffic.
- SGLang 0.5.19 exports `sglang:*`, but the vendored
  llm-d / SGLang overview dashboard queries `sglang_*`, so its panels stay
  empty; QoS / Fair share uses the right names.

## Where it lives

- Prometheus: `k8s/base/observability.yaml`, scrape config
  `k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`
- Grafana, Loki, Tempo: `config/gateway-addons/values.yaml`, `make monitoring-up`
- Dashboards and their provenance: `config/grafana/dashboards/`
  ([README](../../../config/grafana/dashboards/README.md))
- OTEL Collector and Langfuse: `k8s/base/observability.yaml`,
  `k8s/base/applications.yaml`
- Telemetry test: [tests/telemetry](../../../tests/telemetry/README.md)

## Related

- Previous: [Adding an engine](../engines/adding-an-engine.md). Next: [Langfuse](langfuse.md)
- [Tuning from metrics](../configuration/tuning.md)
