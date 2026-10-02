# 0020: DeepSeek Harness (dsh) as a fourth agent runtime, with a per-user web UI on its own host behind gateway OIDC and agent-broker

## Status

Proposed on 2026-10-03. Implemented in the repository; not deployed on the
cluster yet. Written from the repository and from the DeepSeek Harness
sources at npm `@deepseek-ai/dsh@0.2.0-rc.2` (read, not run). The open
points are listed under "Verification".

Extends [ADR 0014](0014-hermes-curated-catalog-and-worker-slots.md) (broker,
slots, profiles, catalog) with a runtime and with a second path into an
agent: the browser. Makes a second scoped exception to
[ADR 0001](0001-path-routing-on-one-origin.md), after
[ADR 0018](0018-repowise-codebase-intelligence.md): the dsh web UI is its own
public origin, matched by `Host` on the existing `edge` listener. Follows
[ADR 0002](0002-one-owner-per-object.md) and
[ADR 0004](0004-tls-terminates-at-the-site-proxy.md).

## Context

DeepSeek Harness (`dsh`) is a coding agent with a browser UI (`dsh web`), a
one-shot mode and an ACP v1 server (`dsh --profile acp`). We want it next to
Hermes, pi and opencode as a model in Open WebUI, and its own web UI for the
same user, next to Open WebUI.

What the dsh sources say:

- **One operator per process.** Everyone admitted to a `dsh web` process
  shares its sessions, workspace and shell. A shared instance would give
  every user everyone else's shell.
- **Root path only.** Browser cookies are `Path=/` and bound to the request's
  `host:port`; the login redirect goes to `./`. A path prefix on the main
  origin is not supported.
- **Own browser authentication.** Each process mints a launch token; `GET
  /?token=` sets a signed cookie (30 days, secret in
  `$DSH_HOME/.credentials.yaml`). No `Authorization` header is accepted.
- **Trust fence.** `Host` must be loopback or in `trustedHosts`
  (`--trusted-host`), and `Origin`, when sent, must equal `Host`.
- **Bind.** The CLI refuses `--host 0.0.0.0`; the `webserver` config row
  accepts it.
- **Provider.** OpenAI-compatible endpoints through `llm-pi-ai`
  (`apiKeyEnv`, `baseURL`, `compat`); MCP over streamable HTTP with headers.
  Telemetry and session-log upload to DeepSeek are on by default.

## Decision

1. **Runtime.** `dsh` is an agent-adapter runtime (image target `dsh`,
   `DSH_IMAGE`, model `dsh-agent`). `agent-adapter/dsh.mjs` drives
   `dsh --profile acp` over stdio, one ACP session per Open WebUI chat
   (`session/new`, `session/resume`), and auto-allows permission requests:
   the pod is the sandbox (gVisor, NetworkPolicy), so dsh runs with
   `DSH_PERMISSION_MODE=danger-full-access` and `DSH_TELEMETRY_MODE=DISABLED`.
2. **Profile.** agent-sync renders `<home>/dsh`: a home-level
   `cordis.patch.yml` (provider `pat` = pat-service with the user's agent
   PAT, the catalog skills, the MCP servers, session-log upload off) that wins
   over the profile's own patch, plus `acp.patch.yml`, `web.patch.yml` and
   `AGENTS.md`. The workspace is `<home>/workspace` on the profile PVC.
3. **One pod per user serves both.** The same `dsh-agent-<id>` pod also runs
   `dsh web` on `:3080` (all interfaces, `--trusted-host` = the public host).
   Chat and browser share the user's `DSH_HOME` and workspace; slots, idle
   eviction and the 2 GiB memory limit (`DSH_MEMORY_LIMIT`) apply to it.
4. **Routing.** `DSH_PUBLIC_ORIGIN` is a second site-proxy rule to the same
   edge listener (like repowise). HTTPRoute `dsh-web`
   (`helm/airgap-stack/templates/dsh-web.yaml`) matches that host and sends
   everything to agent-broker's web proxy (`agents/agent-broker:8081`,
   ReferenceGrant in `k8s/agents`). Route timeout off (cold start,
   WebSocket). ClientTrafficPolicy `edge-external-site-proxy` trusts the
   site proxy as one hop, so Envoy keeps its `X-Forwarded-Proto: https`:
   Envoy's OIDC builds the post-login redirect from it, and with `http` the
   browser returns without the session cookies and loops through Keycloak.
5. **Authentication.** SecurityPolicy `dsh-web`: Envoy OIDC with Keycloak
   client `dsh-web` (`make provision-dsh-oidc`), `forwardAccessToken`. The
   broker verifies the token like a chat request (`ai-user` / `ai-admin`),
   routes by its subject to that user's pod, starting it through the slots
   if needed, and proxies HTTP and WebSocket. Upstream it sets `Host` to the
   public authority, drops `Authorization` and every cookie but dsh's own.
   Routing never depends on a dsh cookie.
6. **dsh's own login.** When dsh answers a GET with 401 and the browser sent
   no dsh cookie, the broker fetches the launch token from agent-adapter
   (`GET /web-token`, bearer `API_SERVER_KEY`), redeems it upstream and
   returns dsh's `Set-Cookie` with a 303 to the same URL. The token never
   reaches the browser.
7. **Slots.** Each HTTP request holds the slot for its duration; a WebSocket
   releases it after the upgrade, so an open tab does not pin a slot. An
   evicted pod drops the socket; the next request starts it again.

## Consequences

- Users get a personal dsh in Open WebUI and in the browser with one login,
  one PAT and one quota identity; nothing about dsh is shared between users.
- A second host to maintain at the site proxy; the certificate must cover it.
- `ResourceQuota` memory grows to K x 2 GiB plus the broker.
- dsh `0.2.0-rc.2` is a release candidate: ACP and patch formats may change;
  the version is pinned in `agent-adapter/Dockerfile`.
- Whether ACP chats and web sessions show up in each other's lists is not
  promised; they share storage, not a protocol.

## Verification

1. dsh starts under gVisor with a read-only root and `danger-full-access`;
   both processes stay under 2 GiB.
2. `web.patch.yml` binds `0.0.0.0:3080`; a request with the public `Host`
   passes the trust fence; WebSocket works through Envoy and the broker.
3. Envoy Gateway v1.8.1 forwards the access token and refreshes it; the
   issuer matches the broker's `OIDC_ISSUER`.
4. The token exchange returns a cookie bound to the public authority (with
   port when the origin has one).
5. A dsh chat in Open WebUI streams and resumes after a pod restart; MCP tools
   appear when their flags are on.
