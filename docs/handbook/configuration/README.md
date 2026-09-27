# Configuration

[Русский](README.ru.md) | [Handbook index](../README.md)

Where every setting lives and how it reaches the cluster. There are four
layers: `.env` (secrets and site values, not committed), Helm values
(committed behaviour), `versions.lock.env` (pinned versions) and a few
service env blocks in manifests. The `make` target that applies each is part
of the answer. Turning metrics into new values is [tuning](tuning.md).

**Contents**

- [The four layers](#the-four-layers)
- [.env keys](#env-keys)
- [How .env reaches the cluster](#how-env-reaches-the-cluster)
- [Helm values](#helm-values)
- [Service settings in manifests](#service-settings-in-manifests)
- [versions.lock.env](#versionslockenv)
- [Which make target applies what](#which-make-target-applies-what)
- [Site-specific values](#site-specific-values)
- [Where it lives](#where-it-lives)
- [Related](#related)

## The four layers

| Layer | Holds | Committed | Rule |
| --- | --- | --- | --- |
| `.env` | credentials, public origin, feature flags, live engine | no (`.env.example` is the template) | one key per setting; never put a secret anywhere else |
| Helm values (`helm/*/values.yaml`, `config/*/values.yaml`) | behaviour: limits, routes, engine flags, tool servers | yes | change here, apply with the chart's target |
| `versions.lock.env` | every image and chart version or digest | yes | never inline a version in a recipe or manifest |
| Env in manifests (`k8s/base/*.yaml`) | service tunables without a values key (pat-service `QOS_*`, Open WebUI, broker) | yes | edit the base, `make helm-up` renders it into the chart |

## .env keys

From `.env.example` (every key there must be set on a real site) plus the optional keys the Makefile reads.

| Group | Keys | Used by |
| --- | --- | --- |
| Langfuse bootstrap and client | `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`, `LANGFUSE_HOST`, `LANGFUSE_INIT_*` | Langfuse first start, OTEL Collector export |
| pat-service | `PAT_HASH_KEY`, `PAT_COOKIE_KEY` (32+ bytes each; rotating the hash key kills every PAT), `PAT_GATEWAY_CLIENT_SECRET` | pat-service, Keycloak client `pat-gateway` |
| Open WebUI | `WEBUI_SECRET_KEY` (keep stable), `OPENWEBUI_AUTOMATIONS_PAT` | Open WebUI sessions, the Automations connection |
| Engines | `INFERENCE_ENGINE` (`vllm`, `sglang`, `ninfer`), `SGLANG_API_KEY`, `NINFER_API_KEY`, `EXTERNAL_API_KEY` | `make engine-up`, `helm-up`, `llmd-up` |
| Feature flags | `WEB_SEARCH_ENABLED`, `REPOWISE_ENABLED` | pat-service, Open WebUI tools, agents, `stack-up` ([MCP](../mcp/README.md)) |
| Site | `STACK_BASE_URL` (required on the remote profile), `GRAFANA_BASE_URL`, `REPOWISE_PUBLIC_ORIGIN` | Makefile origin substitution, OIDC |
| repowise | `REPOWISE_API_KEY`, `REPOWISE_OIDC_CLIENT_SECRET`, `REPOWISE_PAT` | `make repowise-up` |
| Agents (optional) | `PI_IMAGE`, `OPENCODE_IMAGE` override the pins; empty leaves the runtime out | `make agents-up` |
| Operator access (optional) | `WSL_SSH_HOST`, `WSL_SSH_PORT` | `make k3s-tunnel`, image loading |

## How .env reaches the cluster

```
.env --(Makefile -include)--> make variables: STACK_BASE_URL, INFERENCE_ENGINE, *_ENABLED, ...
  |
  +--(scripts/helm-env-values)--> helm/airgap-stack/runtime.secret.yaml (gitignored)
          `runtimeEnvironment:` every KEY=VALUE
       --(make helm-up)--> Secret airgap-runtime  --envFrom/secretKeyRef--> workloads
                      \--> derived Secrets: sglang-api-key, ninfer-api-key, external-api-key
                      \--> ConfigMap openwebui-tool-servers (entries whose flag is "true")
  +--(make agents-up)--> ConfigMap agents/agent-images (images + MCP flags)
  +--(make repowise-up)--> Secrets repowise, repowise-credential, repowise-oidc
```

- Every `.env` key lands in the `airgap-runtime` Secret, whether a workload
  reads it or not.
- On the local-mac profile `make up` creates `airgap-runtime` directly from
  `.env`.
- A changed Secret value does not restart pods. After `make helm-up`, restart
  the consumers (`kubectl -n airgap-ai-stack rollout restart deploy/<name>`)
  unless the chart rolled them.

## Helm values

| File | Key settings | Apply |
| --- | --- | --- |
| `helm/airgap-stack/values.yaml` | `inference.modelName`, `inference.perUserRateLimitPerMinute` (60), `inference.<backend>.enabled`, `inference.embeddings.*` (120/min), `routing.*` (edge port, paths, NodePorts), `oidc.*`, `openwebuiToolServers` | `make helm-up` |
| `helm/vllm-inference/values.yaml` | `inference.maxModelLen`, `maxNumSeqs`, `gpuMemoryUtilization`, `kvCacheDtype`, `prefixCaching`, `kvOffloadingSizeGiB`, resources | `make vllm-up` |
| `helm/sglang-inference/values.yaml` | `inference.contextLength`, `maxRunningRequests`, `memFractionStatic`, Mamba settings | `make sglang-up` |
| `helm/ninfer-inference/values.yaml` | ninfer launch settings | `make ninfer-up` |
| `helm/embeddings-inference/values.yaml` | `accelerator` (`cpu` or `gpu`), `gpu.gpuMemoryBudgetGiB`, batch limits | `make embeddings-up` |
| `config/llmd/router-nvfp4-values.yaml` | EPP bands, flow control, plugins, scorer weights | `make llmd-up` |
| `helm/web-search/values.yaml`, `helm/repowise/values.yaml` | MCP server workloads | `make websearch-up`, `make repowise-up` |
| `config/gateway-addons/values.yaml` | Grafana (OIDC, datasource, dashboards, alert), Loki, Tempo | `make monitoring-up` |
| `config/ai-gateway/values.yaml` | GenAI span header mapping | `make gateway-up` |
| `config/gateway/remote-wsl-values.yaml` | Envoy Gateway controller | `make gateway-up` |

Values set by the Makefile on top of the files: the public origin
(`oidc.publicBaseURL`, `oidc.externalIssuer` from `STACK_BASE_URL`) and the
live engine (`inference.sglang.enabled`, `inference.ninfer.live`,
`router.modelServers.*` from `INFERENCE_ENGINE`).

## Service settings in manifests

Kept as env in `k8s/base/applications.yaml` and rendered into the chart:

| Service | Settings | Reference |
| --- | --- | --- |
| pat-service | `QOS_*` band and cost coefficients, `MCP_CALLS_PER_MINUTE`, `AGENT_PAT_TTL_DAYS`, pricing ConfigMap `pat-service-pricing` | [PAT service](../pat-service.md), [tuning](tuning.md#pat-service-qos-coefficients) |
| Open WebUI | OIDC, roles, connections, feature permissions | [Open WebUI](../openwebui.md) |
| agent-broker | `AGENT_SLOTS`, `IDLE_TIMEOUT`, runtime class, issuer (`k8s/agents/broker.yaml`) | [agents](../agents/README.md) |

Defaults of pat-service settings that are not in the manifest come from
`loadConfig` in `pat-service/cmd/pat-service/main.go`.

## versions.lock.env

The Makefile `include`s it. Groups: core images (Envoy, Keycloak, Open WebUI,
Postgres, ClickHouse, Valkey, MinIO, Langfuse, OTEL, Prometheus, Grafana);
vLLM and llm-d (chart version and digests, EPP image and digest); optional
engines (SGLang, llama.cpp, ninfer); embeddings; pat-service; Helm chart
versions of the operators and gateways; agents (`HERMES_IMAGE`,
`AGENT_CATALOG_IMAGE`, `AGENT_BROKER_IMAGE`, `PI_IMAGE`, `OPENCODE_IMAGE`);
web search; repowise. An upgrade is a change here plus the target that
deploys it.

## Which make target applies what

| Target | Applies |
| --- | --- |
| `make stack-up` | everything of the remote profile, in order (gateways, operators, GPU objects, `engine-up`, web search if enabled, provisioning) |
| `make helm-up` | `airgap-stack`: routes, rate limits, rendered workloads, `airgap-runtime`; then Keycloak client provisioning |
| `make engine-up` | switch or (re)start the live engine ([engines](../engines/README.md#switching-the-live-engine)) |
| `make vllm-up`, `sglang-up`, `ninfer-up`, `embeddings-up` | one engine release |
| `make llmd-up` | the EPP release |
| `make monitoring-up` | Grafana, dashboards, Loki, Tempo |
| `make gateway-up` | Envoy Gateway, AI Gateway, then `monitoring-up` |
| `make operators-up` | CNPG, ClickHouse, Redis, MinIO operators |
| `make agents-up` | agent-broker and `agent-images` |
| `make websearch-up`, `make repowise-up` | MCP servers (restart Open WebUI) |
| `make verify` | renders and validates everything, docs included, without a cluster |
| `make helm-diff` | `helm-up` as a dry run |

## Site-specific values

For a new site, [docs/install](../../install/README.md#values-that-are-site-specific)
lists every value that must be reviewed (origin, node name, model directory,
NodePorts, realm). Site values go in `.env` or site overlays, never as literal
hosts in tracked files; `scripts/docs-check` fails on origins from `.env`
found in the docs.

## Where it lives

- `.env.example`, `scripts/helm-env-values`, `Makefile`, `versions.lock.env`
- Credential inventory and rotation: [docs/security](../../security/README.md#credential-inventory)

## Related

- Previous: [Data and retention](../observability/data-and-retention.md). Next: [Tuning](tuning.md)
