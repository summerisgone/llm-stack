# 0011: PAT session key drives Langfuse session grouping for agent traces

## Status

Accepted and deployed. `helm upgrade aieg` and a `pat-service` rebuild/rollout
have been applied to the remote cluster, and
`tests/telemetry/genai-smoke.sh` passes live: two requests sharing an
`X-Session-Key` land in one Langfuse `session.id`, a third with a different
key gets its own. It exists because the fair-share work
(ADR 0008) already gives `pat-service` a reliable server-side session identity
for every chat request, and that identity is currently invisible to Langfuse:
each inference request lands as an isolated, session-less trace even though it
belongs to a multi-request agent run.

## Context

`pat-service` already computes a per-request session id for `/v1/chat/completions`
via the chain-hash fingerprint in `internal/qos` (ADR 0008, Stage 3 step 1) and
forwards it upstream as `X-Session-Key: sess-<hex>` on the request it sends to
the AI Gateway (`cmd/pat-service/main.go:617-619`). Requests whose message
history accumulates (the ordinary shape of an agent mid-task: each tool hop
re-sends the prior turns) chain-match to the **same** session id; a request
with a fresh context mints a new one.

The AI Gateway's external processor emits a GenAI span per request using
OpenInference conventions and maps trusted request headers onto span
attributes via `config/ai-gateway/values.yaml` `controller.spanRequestHeaderAttributes`:

    agent-session-id:session.id,
    x-user-name:langfuse.user.id,
    x-user-id:langfuse.trace.metadata.user_subject,
    x-pat-token-id:langfuse.trace.metadata.pat_token_id

The Collector forwards those GenAI spans to Langfuse over OTLP/HTTP
(`k8s/base/observability.yaml`), dropping the Envoy transport spans. Langfuse
groups every observation carrying the same `session.id` into one session,
aggregating cost, latency and token usage across the member traces — no
server-side session object needs to be created first.

**The gap is one header.** PAT's `X-Session-Key` is not mapped to `session.id`,
so every request becomes its own session-less trace. The `agent-session-id`
mapping exists for clients that volunteer their own session header, but
nothing in the stack sets it today, and it is not the PAT session key.

## Decision

1. **Map PAT's session key to the Langfuse session id.** Add
   `x-session-key:session.id` to `controller.spanRequestHeaderAttributes` in
   `config/ai-gateway/values.yaml`. Every GenAI span whose request carried a
   PAT session then gets `session.id = sess-<hex>`, and Langfuse groups all
   traces of one agent run into one session. This is the whole mechanism the
   task is asking for, and it is a pure config change — no Go code, no PAT
   rebuild.

2. **Make PAT authoritative on the `/v1` path.** Add `agent-session-id` to the
   client-spoofing strip list in `copyRequestHeaders`
   (`cmd/pat-service/main.go:980-990`), the same treatment `X-Session-Key`
   already gets. Because both `agent-session-id` and `x-session-key` would map
   to `session.id`, a client-supplied `agent-session-id` forwarded through PAT
   could otherwise override the server-derived key (Envoy collapses duplicate
   attribute keys, last-write-wins). Stripping it mirrors ADR 0003's principle:
   on the PAT path the session identity is derived server-side, never asserted
   by the caller.

3. **Keep `agent-session-id:session.id` for the Open WebUI route.** That path
   does not go through PAT, so it keeps its own session source; the two routes
   share the same controller config but never both carry a session header in
   practice once step 2 strips the client header on the PAT path.

4. **Do not have PAT POST sessions to Langfuse.** Langfuse auto-creates a
   session from any trace that carries a `sessionId`; a dedicated
   `POST /api/public/sessions` adds only custom session-level metadata that is
   not already on the traces. Since each trace already carries `user.id` and
   `metadata.pat_token_id` via the existing header mapping, session-level
   aggregation inherits them. Skipping the POST keeps PAT free of Langfuse
   credentials and off the hot path. If session-level metadata is ever wanted,
   it can be added later without changing the grouping mechanism.

## Consequences

- **Tiny change surface**: one config line plus one entry in the strip list.
  `make verify` does not cover the `aieg` Helm release; apply with
  `helm upgrade aieg ... --values config/ai-gateway/values.yaml` and re-run
  the telemetry smoke test.
- All `/v1` chat requests whose chains stitch into one PAT session now appear
  as one Langfuse session, with per-session aggregated cost, latency and
  tokens, and per-trace user and PAT identity already attached.
- **Inherent limitation of the fingerprint**: the chain-hash only stitches
  sessions where each request re-sends its prior messages. Independent
  sub-agent calls or fresh-context requests mint a new session key and become
  a separate Langfuse session. That is the designed behaviour of ADR 0008, not
  a regression. If a per-run tool-call DAG (agent → tool → LLM nesting) is
  wanted, that needs agent-side (Hermes) instrumentation and is out of scope
  here — this ADR delivers session-level grouping of the underlying LLM
  generations, which is what the PAT session marker can provide.
- Session ids are opaque `sess-<hex>`. Making them human-readable in Langfuse
  (e.g. mapping to a friendlier id, or adding
  `x-session-key:langfuse.trace.metadata.session_key` alongside) is optional
  follow-up, not required for grouping.
- No new credential exposure: PAT never learns Langfuse keys because it does
  not POST.

## Implementation

- `config/ai-gateway/values.yaml`: added `x-session-key:session.id` to
  `controller.spanRequestHeaderAttributes`.
- `pat-service/cmd/pat-service/main.go` `copyRequestHeaders`: added
  `Agent-Session-Id` to the client-spoofing strip list alongside
  `X-Session-Key`.
- `tests/telemetry/genai-smoke.sh`: extended with a session-grouping check —
  two requests sharing one `X-Session-Key` must land in the same Langfuse
  `session.id`, a third with a different key must get its own. Sent directly
  to the AI Gateway with the Gateway's own OIDC client credentials, same as
  the existing checks in this script, so it verifies the config mapping
  (step 1), not PAT's own header handling.
- `pat-service/cmd/pat-service/main_test.go`
  `TestCopyRequestHeadersStripsLLMDFairnessControls`: extended to assert
  `Agent-Session-Id` is stripped, since that only happens inside PAT's own
  proxy handler and isn't reachable from the gateway-direct shell smoke test.

## Deployment record

- `helm upgrade aieg oci://docker.io/envoyproxy/ai-gateway-helm --version
  v1.1.0 --values config/ai-gateway/values.yaml` — revision 4, deployed.
- `make pat-deploy` — rebuilt and rolled out `pat-service` with the
  `Agent-Session-Id` strip.
- `tests/telemetry/genai-smoke.sh` run against the live cluster: all three
  checks (two streaming variants, session grouping) passed.