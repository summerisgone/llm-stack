# PAT service

[Русский](pat-service.ru.md) | [Handbook index](README.md)

`pat-service` is the stack's own Go service. It turns a personal access token
(`sk-...`) into a verified identity, labels each model request with a session
and a priority band, proxies MCP tool calls, and serves the self-service
dashboard. It deliberately does **not** schedule anything: queueing and
fairness belong to llm-d EPP.

**Contents**

- [What it does](#what-it-does)
- [What it does not do](#what-it-does-not-do)
- [Endpoints](#endpoints)
- [Token lifecycle](#token-lifecycle)
- [Session stitching](#session-stitching)
- [Priority band labelling](#priority-band-labelling)
- [Usage and cost accounting](#usage-and-cost-accounting)
- [State it keeps](#state-it-keeps)
- [Failure modes](#failure-modes)
- [Where it lives](#where-it-lives)
- [Related](#related)

## What it does

1. **Authenticates programs.** A PAT is looked up by its HMAC hash
   (`PAT_HASH_KEY`); revoked or expired tokens are refused on the next
   request. The owner (Keycloak `sub` and username) becomes the identity.
2. **Swaps the credential.** The request continues to the private AI Gateway
   with pat-service's own client-credentials JWT (Keycloak client
   `pat-gateway`), never with the PAT. Client-supplied identity headers are
   stripped first and replaced by trusted ones.
3. **Stitches sessions.** It derives a stable session key from the message
   history, so every step of an agent run shares one `X-Session-Key`
   (Langfuse session, EPP warmth).
4. **Labels priority.** It picks the EPP band (`warm`, `normal`, `demoted`)
   and the fairness id (the owner's `sub`) and sends them as headers.
5. **Accounts usage.** Tokens and cost per request go to Prometheus counters
   and to the `qos_events` table the dashboard reads.
6. **Proxies MCP.** `/mcp/<name>/` authenticates the caller, rate-limits tool
   calls and forwards to the MCP server with identity headers
   ([MCP](mcp/README.md)).
7. **Issues agent keys.** `/api/agent-token` mints a short-lived PAT for the
   user's agents and writes it where their pods read it
   ([agents](agents/README.md#tokens)).

## What it does not do

This boundary is settled ([architecture](../architecture/README.md#queueing-and-fairness-who-owns-what),
[ADR 0008](../adr/0008-per-user-fair-share.md)); do not build around it.

- **No queue, no inference limiter, no admission decision.** Every
  authorised model request is proxied at once. When a request should wait,
  EPP makes it wait.
- **No per-user request rate limit on `/v1`.** That is the AI Gateway's
  `BackendTrafficPolicy` keyed on `X-User-Id`. The one limiter pat-service
  runs is for MCP tool calls.
- **No enforcement of a monthly budget.** `QOS_MONTHLY_LIMIT` is shown on the
  dashboard only.
- **It does not see Open WebUI chat traffic.** Open WebUI calls the gateway
  directly, so browser chats have no session key and no band. That is one
  reason a scheduler here would be wrong: it would arbitrate part of the load.
- **It does not store prompts or answers.** Those are in Langfuse.
- **It is not an identity provider.** Users, roles and passwords live in
  Keycloak; the dashboard itself signs in through Keycloak (client
  `pat-dashboard`).

## Endpoints

| Path | Who calls it | What it does |
| --- | --- | --- |
| `/platform/` (public, prefix stripped) -> `/` | browser | dashboard: create, list and revoke tokens, usage charts, recent sessions, "Agent inference key" |
| `/auth/login`, `/auth/callback`, `/auth/logout` | browser | PKCE OIDC; requires `ai-user` or `ai-admin` |
| `/api/tokens` (GET, POST), `/api/tokens/{id}` (POST = revoke) | dashboard | token CRUD; name 1-100 chars, expiry 1-365 days, default 90 |
| `/api/agent-token` (POST) | dashboard | issue the user's agent key |
| `/api/usage/daily`, `/api/usage/sessions`, `/api/usage/limit` | dashboard | usage from `qos_events` |
| `/v1/*` (public) | programs, agents, Open WebUI Automations | the PAT -> JWT proxy to the AI Gateway |
| `/mcp/<name>/` (cluster only) | agents, Open WebUI tool calls | MCP proxy |
| `/healthz` | kubelet | database ping |
| `:9090/metrics` (cluster only) | Prometheus | `patsvc_*` metrics |

## Token lifecycle

- **Issue.** On `/platform`. The full token is shown once; the database keeps
  only the HMAC hash, a 15-character prefix for display, owner, name,
  expiry and `issued_by` (`user` or `agents`).
- **Use.** `Authorization: Bearer sk-...` on `https://<origin>/v1`. Several
  tokens of one user share that user's limits: limits count the owner, not
  the token.
- **Revoke.** On `/platform`; effective on the next request. Revoked rows
  stay in the table with `revoked_at` set.
- **Expire.** At `expires_at`. Agent keys expire after `AGENT_PAT_TTL_DAYS`
  (7); issuing a new one revokes the previous agent key.
- **Rotating `PAT_HASH_KEY` invalidates every stored token.** Treat it as a
  mass revocation.

Operators can issue a token for a user from the command line with
`scripts/kc-pat-issue` (see [scripts/README.md](../../scripts/README.md)).

## Session stitching

Agents and coding tools resend the whole conversation on every step. For
`/v1/chat/completions` pat-service hashes the conversation as a chain
([ADR 0011](../adr/0011-pat-session-key-langfuse-tracing.md),
`internal/qos/chain.go`):

```
c0 = SHA256(model | tools | message[0])
ci = SHA256(c(i-1) | message[i])
```

Each link is stored in Valkey (`qos:chain:<sub>:<link>`, TTL
`QOS_SESSION_TTL_SECONDS`, 1800 s). A new request looks up its links newest
first: if any is known, it belongs to that session; if none is, a new
`sess-<24 hex>` is minted. The key is sent upstream as `X-Session-Key` and
mapped by the AI Gateway to the Langfuse `session.id`, so one agent run is one
Langfuse session. A client-supplied `Agent-Session-Id` is stripped so it
cannot override the server-derived key.

Consequences:

- Two users never share a session: the owner's `sub` is part of the key.
- A conversation idle for longer than the TTL starts a new session.
- Only bodies up to `QOS_FINGERPRINT_MAX_BODY_BYTES` (4 MiB) are hashed; the
  lookup has a `QOS_FINGERPRINT_TIMEOUT_MS` (200 ms) budget. Any failure
  degrades to "no session", never to a failed request.
- `patsvc_session_match_total{result}` shows how often requests matched.

## Priority band labelling

`internal/qos/session.go` `assignBand` picks one of three EPP bands per
request; the full queue model is in
[queues and fair share](inference/queues-and-fair-share.md).

| Band | When | Priority |
| --- | --- | --- |
| `demoted` | the owner sent more than `QOS_DEMOTE_AFTER_STEPS` (8) consecutive requests in the same session (the streak resets when they switch session), or the owner's recent spend exceeds the lowest spend among other active users by more than `QOS_SPEND_DEMOTE_THRESHOLD` (5.0 cost units in `QOS_SPEND_WINDOW_SECONDS`, 600 s) | 1 |
| `warm` | the session was dispatched within `QOS_WARM_TTL_SECONDS` (120 s), so its KV cache is probably still resident | 10 |
| `normal` | everything else, and any Valkey error | 5 |

Demotion is checked first. The band goes out as
`X-Llm-D-Inference-Objective`, the owner's `sub` as
`X-Llm-D-Inference-Fairness-Id`.

## Usage and cost accounting

Two separate numbers, for two purposes:

| | Scheduler cost units | Money |
| --- | --- | --- |
| Formula | `QOS_COST_ALPHA_PER_TOKEN` x uncached prompt tokens + `QOS_COST_BETA_PER_TOKEN` x completion tokens | per-1K prices from `QOS_PRICING_FILE` (ConfigMap `pat-service-pricing`), prompt, cached and completion |
| Used by | spend-based demotion (Valkey `qos:spend:<sub>`) | dashboard, `qos_events.cost_amount` |
| Metric | `patsvc_cost_units_total` | - |

Usage comes from the response: for streams pat-service adds
`stream_options.include_usage` and reads the final usage chunk. Embeddings
are counted (`patsvc_embedding_tokens_total`, `qos_events` band
`embeddings`) but never affect bands.

## State it keeps

| Store | Content | Lifetime |
| --- | --- | --- |
| Postgres `pat-db` (CNPG), table `personal_access_tokens` | token hash, prefix, owner, name, dates, `issued_by` | forever; revoked rows stay |
| same, table `qos_events` | one row per chat or embeddings response: owner, session, model, band, tokens, cost, token id | forever (no purge job) |
| Valkey `envoy-ratelimit-valkey` | session chains, streaks, dispatch times, spend, active users, MCP rate windows | TTLs of 2 min to 30 min |
| Kubernetes Secret `agents/agent-cred-<id>` | the user's current agent key | until reissued or the profile is deleted |

See [data and retention](observability/data-and-retention.md) for the
whole-stack picture.

## Failure modes

| Symptom | Likely cause | Check / fix |
| --- | --- | --- |
| Every PAT fails at once with 401 | pat-service's cached gateway JWT expired early | gateway Envoy log `Jwt_is_expired`; `kubectl -n airgap-ai-stack rollout restart deploy/pat-service`; compare the `pat-gateway` client's access-token lifespan in Keycloak with the realm file |
| One token gets 401 | revoked, expired, or `PAT_HASH_KEY` changed | dashboard shows the token state |
| 503 on `/v1` or `/mcp` | database unreachable | `kubectl -n airgap-ai-stack get cluster pat-db` |
| No sessions in Langfuse for PAT traffic | Valkey down (fails open) or the AI Gateway header mapping lost | `patsvc_session_match_total`; `config/ai-gateway/values.yaml` |
| Agent key button fails | pat-service RBAC in `agents` namespace | `kubectl -n agents get role pat-service-agent-key` |

## Where it lives

- Code: `pat-service/cmd/pat-service/` (`main.go` routes and config,
  `mcp.go`, `agents.go`, `usage.go`), `pat-service/internal/qos/`,
  dashboard `pat-service/web/`
- Deployment and env: `k8s/base/applications.yaml` (rendered into
  `helm/airgap-stack`); image `PAT_SERVICE_IMAGE` in `versions.lock.env`,
  built by CI (`.github/workflows/pat-service-image.yml`)
- Keycloak clients `pat-dashboard`, `pat-gateway`: `make provision-pat-oidc`
- Tests: `cd pat-service && go test ./...`; boundary smoke:
  `make pat-smoke`

## Related

- Previous: [Open WebUI](openwebui.md). Next: [Inference](inference/README.md)
- [ADR 0003](../adr/0003-private-ai-gateway-and-pats.md) private gateway and
  PATs, [ADR 0011](../adr/0011-pat-session-key-langfuse-tracing.md) session
  key, [ADR 0008](../adr/0008-per-user-fair-share.md) fair share
- [Configuration and tuning](configuration/tuning.md#pat-service-qos-coefficients)
