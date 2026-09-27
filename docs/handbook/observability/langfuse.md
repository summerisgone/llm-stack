# Langfuse traces

[Русский](langfuse.ru.md) | [Handbook index](../README.md)

Langfuse holds one trace per model call that went through the private AI
Gateway: the prompt, the answer, the model, token counts, latency, and who
asked. It is the place to answer "what did this user or agent actually send,
and what came back". Because it holds conversation content, treat access to
it as access to user data.

**Contents**

- [Where traces come from](#where-traces-come-from)
- [What a trace contains](#what-a-trace-contains)
- [Users and sessions](#users-and-sessions)
- [Reading traces](#reading-traces)
- [What is not traced](#what-is-not-traced)
- [Troubleshooting](#troubleshooting)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Where traces come from

```
ai-gateway-private
  EnvoyProxy tracing (service ai-gateway-private): HTTP spans, 100 % sampled  --\
  GatewayConfig ai-gateway-tracing (service ai-gateway-genai): GenAI spans     ---> OTEL Collector
                                                                                    filter: drop service.name == ai-gateway-private
                                                                                    export: Langfuse /api/public/otel (basic auth)
```

- The GenAI spans come from the AI Gateway's external processor using
  OpenInference conventions, `always_on`, with
  `OPENINFERENCE_HIDE_INPUTS/OUTPUTS=false`: inputs and outputs are captured,
  streaming included.
- The Collector drops Envoy's own transport spans so each request appears
  once, and authenticates to Langfuse with `LANGFUSE_PUBLIC_KEY` /
  `LANGFUSE_SECRET_KEY` from `.env`. Envoy never sees those keys.
- `tests/telemetry/genai-smoke.sh` proves the chain: a streaming and a
  non-streaming call must land in ClickHouse with input, output, model and
  user, and no Envoy HTTP span may be exported.

## What a trace contains

| Langfuse field | Source |
| --- | --- |
| Input, output | `input.value`, `output.value` (OpenInference) |
| Model | `llm.model_name` (`qwen-3.8-27b`, or `llamacpp-local` and so on) |
| Tokens | `llm.token_count.prompt` / `completion` / `total` |
| Latency | span duration |
| User | `X-User-Name` -> `langfuse.user.id` |
| Metadata `user_subject` | `X-User-Id` (Keycloak `sub`) |
| Metadata `pat_token_id` | `X-Pat-Token-Id` (PAT traffic only) |
| Session | `X-Session-Key` or `agent-session-id` -> `session.id` |

The header-to-attribute mapping is one line in `config/ai-gateway/values.yaml`
(`controller.spanRequestHeaderAttributes`), applied by the `aieg` release
(`make gateway-up`); per-gateway tracing settings are in
`helm/airgap-stack/templates/llmd.yaml`.

## Users and sessions

- **Browser chats** (Open WebUI) carry the user from Open WebUI's forwarded
  headers but no session: each call is its own trace.
- **PAT and agent traffic** carries the user and pat-service's session key, so
  all steps of one agent run form one Langfuse session with summed cost,
  tokens and latency ([session stitching](../pat-service.md#session-stitching)).
- Agents call with the user's own agent PAT, so their traces are attributed
  to the user, with `pat_token_id` of the agent key.
- Tokens issued before usernames were recorded show the `sub` instead of a
  name; a new PAT fixes later traces.

## Reading traces

| Pattern | Meaning | Next step |
| --- | --- | --- |
| Large `prompt_tokens`, small `completion_tokens`, repeating | agent resending history | check prefix hits in Grafana; `warm` band share in QoS / Fair share |
| Latency grows, tokens do not | waiting in EPP or engine queue | QoS / Cluster load, EPP queue duration |
| Many short sessions from one PAT | client does not resend history, or edits it | expected for some tools; sessions are per conversation prefix |
| Errors with `response_format` | JSON mode against ninfer | [engines](../engines/README.md) |

## What is not traced

- Calls that do not pass `ai-gateway-private`: the agent-broker's own
  onboarding answers, MCP tool calls (counted in `patsvc_mcp_calls_total`
  instead), embeddings (separate route; token counts only in pat-service).
- Traffic sent to an engine directly (port-forwards, in-pod tests).

## Troubleshooting

| Symptom | Check |
| --- | --- |
| No new traces | `kubectl -n airgap-ai-stack logs deploy/otel-collector`; Langfuse keys in `.env` match the project (Settings -> API keys) |
| Traces without input/output | the `GatewayConfig` was not applied: `make helm-up`; old traces cannot be repaired |
| Duplicated traces | the Collector filter for `ai-gateway-private` spans is missing |
| No sessions | pat-service Valkey errors, or `x-session-key` mapping lost from `config/ai-gateway/values.yaml` |

## Where it lives

- Langfuse web and worker: `k8s/base/applications.yaml`; stores in
  `k8s/base/databases.yaml` (Postgres `langfuse-db`, ClickHouse
  `langfuse-clickhouse`, Valkey `langfuse-valkey`) and
  `k8s/base/object-storage.yaml` (MinIO bucket `langfuse`)
- Collector: `k8s/base/observability.yaml`
- Tracing config: `helm/airgap-stack/templates/llmd.yaml`,
  `config/ai-gateway/values.yaml`
- More detail: [operations README](../../operations/README.md#ai-gateway-traces-in-langfuse),
  [config/langfuse](../../../config/langfuse/README.md)

## Related

- Previous: [Observability](README.md). Next: [Data and retention](data-and-retention.md)
- [ADR 0011](../../adr/0011-pat-session-key-langfuse-tracing.md)
