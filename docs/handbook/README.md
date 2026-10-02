# llm-stack handbook

[Русский](README.ru.md)

## Contents

| # | Page | What you learn |
| --- | --- | --- |
| 1 | [System overview](overview.md) | components, namespaces, the three request paths, identity, repository ownership |
| 2 | [Open WebUI](openwebui.md) | what users get, connections, sign-in and roles, what to expect |
| 3 | [PAT service](pat-service.md) | tokens, session stitching, band labelling, accounting, what it does not do |
| 4 | [Inference](inference/README.md) | the path from gateway to GPU, who decides what, bypass paths, timeouts |
| 4.1 | [Queues and fair share](inference/queues-and-fair-share.md) | EPP bands, per-user fairness, saturation, the graduated ceiling |
| 4.2 | [KV cache](inference/kv-cache.md) | GPU memory, prefix reuse, engine settings, context vs concurrency |
| 5 | [Engines](engines/README.md) | the `qwen-3.8-27b` pool, engine replicas, `make engines-up`, embeddings |
| 5.1 | [Adding an engine](engines/adding-an-engine.md) | ninfer, llama.cpp, external APIs, the metrics contract |
| 6 | [Observability](observability/README.md) | metrics sources, dashboards by question, logs, alerts |
| 6.1 | [Langfuse traces](observability/langfuse.md) | what a trace holds, users and sessions |
| 6.2 | [Data and retention](observability/data-and-retention.md) | what is stored where, retention, deleting a user's data |
| 7 | [Configuration](configuration/README.md) | `.env`, Helm values, versions, which target applies what |
| 7.1 | [Tuning limits and queues](configuration/tuning.md) | rate limits, EPP, pat-service coefficients, engine capacity, from metrics |
| 8 | [Agents](agents/README.md) | agent-broker, runtimes, profiles, tokens, gVisor |
| 8.1 | [Managing agents](agents/managing.md) | deploy, catalog, keys, offboarding, adding a runtime |
| 9 | [MCP servers](mcp/README.md) | installed servers, the pat-service door, Open WebUI and agents, policies |
| 9.1 | [Repowise](mcp/repowise.md) | repowise operations |
| 9.2 | [Adding an MCP server](mcp/adding-a-server.md) | the steps and rollout order |
| 10 | [Operations](operations.md) | cluster access, deploys, smoke tests, users, known failures and gaps |
| 11 | [Contributing to the docs](contributing.md) | where a change goes, page template, English and Russian, docs-check |

## Glossary

| Term | Meaning | Defined in |
| --- | --- | --- |
| PAT | personal access token `sk-...`, the credential for `/v1` | [PAT service](pat-service.md#token-lifecycle) |
| agent key | a 7-day PAT with `issued_by = agents`, used by the user's agents | [Agents](agents/README.md#tokens) |
| EPP | llm-d endpoint picker: the queue and scheduler in front of the engine | [Queues and fair share](inference/queues-and-fair-share.md) |
| band | an EPP priority class: `warm` 10, `normal` 5, `demoted` 1, fallback 0 | [Queues and fair share](inference/queues-and-fair-share.md#bands-there-are-four-not-one) |
| fairness id | the key EPP shares a band by: the user's Keycloak `sub` | [Queues and fair share](inference/queues-and-fair-share.md#fairness-inside-a-band) |
| session key | pat-service's chain-hash id of one conversation, `sess-...` | [PAT service](pat-service.md#session-stitching) |
| saturation | EPP's 0..1 load signal from engine queue depth and KV use | [Queues and fair share](inference/queues-and-fair-share.md#saturation-when-epp-holds-requests-back) |
| engine replicas | how many pods of each engine run (`VLLM_REPLICAS`, `SGLANG_REPLICAS`, `NINFER_REPLICAS`); vLLM and SGLang pods serve `qwen-3.8-27b` | [Engines](engines/README.md#engine-replicas) |
| runtime | an agent kind: Hermes, pi or opencode | [Agents](agents/README.md#runtimes) |
| slot | one of K running agent pods the broker allows | [Agents](agents/README.md#agent-broker) |
| profile | a user's agent home on a PVC | [Agents](agents/README.md#profiles-and-the-catalog) |
| catalog | the curated skills and settings rendered into every profile | [Agents](agents/README.md#profiles-and-the-catalog) |
| MCP door | pat-service `/mcp/<name>/`, the only way to an MCP server | [MCP servers](mcp/README.md#the-door-pat-service-mcp) |

## Other documentation

| Where | What |
| --- | --- |
| [README.md](../../README.md) | the stack in one page, charts, profiles, deploy commands |
| [AGENTS.md](../../AGENTS.md) | repository rules for humans and coding agents |
| [docs/architecture](../architecture/README.md) | architecture reference |
| [docs/adr](../adr/README.md) | decisions and their history (not edited after acceptance) |
| [docs/install](../install/README.md) | installing on a new site |
| [docs/operations](../operations/README.md) | runbooks |
| [docs/security](../security/README.md) | trust boundaries, credential inventory |
| [docs/clients](../clients/README.md) | connecting clients (coding tools, SDKs) |
| [docs/airgap](../airgap/README.md) | air-gap bundle status |
