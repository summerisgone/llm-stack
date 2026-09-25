# 0018: Repowise as a shared codebase-intelligence service -- web UI on its own host `repowise.<site>` behind gateway OIDC, MCP through pat-service

## Status

Proposed on 2026-09-25. Implementation started the same day; repowise is
not deployed on the cluster. Written from the repository and from the
repowise sources at tag `v0.53.0` (commit `6699171`). Every assumption
about Envoy Gateway v1.8.1, the site proxy's wildcard host and repowise in
workspace mode is listed under "Verification stages" and must be closed
before this is accepted; what has been checked so far is under
"Verification log".

Precondition (section 6a): embeddings run on the GPU, with VRAM taken from
the live LLM engine. That depends on [ADR 0013](0013-embeddings-api-bge-m3.md)
stage V3 and extends its GPU mode to the ninfer engine; until it holds,
`REPOWISE_ENABLED` stays `false`.

Builds on [ADR 0017](0017-web-search-mcp-openserp-kagent.md) (the pat-service
`/mcp/` route, the Hermes profile entry, the Open WebUI tool connection) and
amends its section 6 on who owns the Open WebUI tool-server ConfigMap
(section 7 below). Makes a scoped exception to
[ADR 0001](0001-path-routing-on-one-origin.md): the repowise UI is a second
public origin, `repowise.<site>`, and the `edge` Gateway matches that one
host by `Host` on the existing listener (section 3). Follows
[ADR 0002](0002-one-owner-per-object.md) (one owner per object) and
[ADR 0004](0004-tls-terminates-at-the-site-proxy.md) (TLS at the site
proxy).

## Context

Repowise (AGPL-3.0, Python/FastAPI API + Next.js UI) indexes git
repositories into a dependency graph, git analytics, generated docs and
architectural decisions, and serves them to people through a web UI and to
agents through MCP. We want one shared instance ("system", not per user)
with company repositories indexed. People reach it in the browser through
SSO; Hermes agents (ADR 0014) and Open WebUI use it as an MCP tool.

What the repowise sources say (v0.53.0, read, not run):

- Auth. There is one shared secret, `REPOWISE_API_KEY`, checked as
  `Authorization: Bearer <key>` on every `/api/*` router
  (`packages/server/src/repowise/server/deps.py`, `verify_api_key`). Without
  the key only loopback peers are served; forwarded headers are
  deliberately not trusted. There are no users, no roles and no OIDC.
- UI to API. The browser calls relative `/api/...`
  (`NEXT_PUBLIC_REPOWISE_API_URL`, default empty, baked in at build time),
  and Next middleware rewrites `/api/*`, `/health` and `/metrics` to
  `REPOWISE_API_URL` (`packages/web/src/middleware.ts`). The browser takes
  the bearer key from `localStorage` (`repowise_api_key`, set on the
  settings page) (`packages/web/src/lib/api/client.ts`).
- Base path. `packages/web/next.config.ts` sets no `basePath`, and the UI
  assumes it owns the root of its origin: `/`, `/_next/*`, `/repos/*`,
  `/api/*`. On the main origin `/` and `/api/*` belong to Open WebUI. A
  `basePath` build fixes `next/link` and `/_next` but not the code that
  builds root-absolute URLs itself: `next/image` sources with
  `images.unoptimized` (`components/layout/brand-logo.tsx`),
  `window.location.href = "/repos/..."` (`components/docs/docs-viewer.tsx`),
  a `^/repos/` match on `location.pathname`
  (`components/layout/context-drawer-provider.tsx`) and
  `new URL(href, location.origin)` (`packages/ui/src/docs/docs-reader.tsx`).
  Each upgrade would need that audit again.
- The upstream `docker/Dockerfile` sets
  `ENV NEXT_PUBLIC_REPOWISE_API_URL=http://localhost:7337` before `next
  build` (an `ENV`, so `--build-arg` cannot override it), so its UI calls
  the API on the viewer's own `localhost` and works only on the machine
  that runs it. Any remote deployment needs its own build with that value
  empty (relative `/api`, through the Next middleware). The image also sets
  `REPOWISE_DB_URL=sqlite+aiosqlite:////data/wiki.db`, which puts a
  workspace into "shared database mode"; per-repo databases need it empty.
- MCP. `repowise mcp --transport streamable-http` is a separate process
  (default port 7338, endpoint `/mcp`). The HTTP transport has **no
  authentication** (`_warn_on_open_bind` in `mcp_server/_server.py`, pending
  upstream issue #1400). On a `0.0.0.0` bind the SDK's DNS-rebinding Host
  check is turned off. In workspace mode one MCP server serves all repos.
- LLM and embeddings. LLM provider `openai` honours `OPENAI_BASE_URL`; the
  OpenAI embedder honours `OPENAI_BASE_URL` and `REPOWISE_EMBEDDING_DIMS` /
  `REPOWISE_EMBEDDING_DECLARED_DIMS`. Index, search and most MCP tools need
  no LLM; doc generation, `update` with docs, UI chat and the MCP
  `get_answer` tool do.
- Repositories. Repowise reads local git checkouts. The server's 15-minute
  "polling fallback" (`server/scheduler.py`) compares each checkout's local
  `HEAD` with the last synced commit; it never runs `git fetch`, and it
  walks only the repositories of the primary database, not every workspace
  member. Something outside repowise has to fetch. GitLab push webhooks
  (`/api/webhooks/gitlab`, `REPOWISE_GITLAB_WEBHOOK_TOKEN`) are optional.
- Telemetry is on by default and anonymous; `REPOWISE_TELEMETRY_DISABLED=1`
  and `DO_NOT_TRACK=1` turn it off.
- Upstream `docker/Dockerfile` runs API and UI in one container
  (`docker/entrypoint.sh`), with SQLite on `/data`.

What the repository already has:

- Envoy Gateway v1.8.1 (`versions.lock.env`) with one external listener
  (`external`, `helm/airgap-stack/templates/gateway.yaml`) and path routes
  in `helm/airgap-stack/templates/routes-external.yaml`. The site proxy
  terminates TLS for the main origin and forwards to that listener
  (ADR 0004). A wildcard DNS name and certificate for the site's domain
  are available.
  Envoy Gateway's `SecurityPolicy` is used for JWT only today (`llmd.yaml`,
  `embeddings.yaml`); gateway OIDC is not used anywhere yet.
- Keycloak clients are provisioned by make targets (`provision-grafana-oidc`,
  `provision-pat-oidc`); the realm role `ai-user` gates Open WebUI,
  `/platform` and pat-service `/mcp/`.
- pat-service `/mcp/web-search/` (`pat-service/cmd/pat-service/mcp.go`):
  PAT or Keycloak token, server-derived identity headers, per-user rate
  limit, `patsvc_mcp_calls_total`. The prefix and upstream are hard-coded to
  web-search.
- Hermes agents may reach only DNS and pat-service
  (`k8s/hermes/networkpolicy.yaml`); the profile has one MCP entry,
  `mcp_servers.web-search`, locked in `locked-keys.yaml` and removed by
  hermes-sync when web search is off.
- Open WebUI reads `TOOL_SERVER_CONNECTIONS` from the ConfigMap
  `openwebui-tool-servers`, which today is owned by the `web-search`
  release.

## Decision

### 1. Workload: its own opt-in release, `helm/repowise`

- A new chart `helm/repowise`, release `repowise` in `airgap-ai-stack`
  (same namespace as pat-service, so NetworkPolicies select by pod label as
  in 0017), installed by `make repowise-up` / `make repowise-down` only when
  `REPOWISE_ENABLED=true` (default `false`, `.env.example`). Air-gapped
  installs leave it off: the repositories are fetched from the internet.
- One pod, `replicas: 1`, `strategy: Recreate`, one `ReadWriteOnce` PVC for
  checkouts and per-repo `.repowise/` indexes under `/data/workspace`.
  - init container `workspace`: clones missing repos; the first run does
    `repowise init --all --yes` on the workspace, later runs
    `repowise workspace add` for repos new to the list;
  - `app`: the image's entrypoint (API on 7337, UI on 3000), working
    directory the workspace so the API starts in workspace mode;
  - `mcp`: the same image, `repowise mcp <workspace> --transport
    streamable-http --host 0.0.0.0 --port 7338`;
  - `sync`: every 15 minutes fetches each repo, resets it to the fetched
    branch head and, if any head moved, runs `repowise update --workspace
    --yes` (Context: the server's own poll never fetches).
  All read the same workspace on the PVC, with `REPOWISE_DB_URL` empty
  (per-repo databases). PostgreSQL (repowise's `postgres` extra) is not
  adopted: one pod, no HA requirement.
- Image. Built in this repo's CI (`.github/workflows/repowise-image.yml`)
  from the upstream `docker/Dockerfile` at the pinned tag
  (`REPOWISE_UPSTREAM_REF`), unmodified source, with one override: the
  `NEXT_PUBLIC_REPOWISE_API_URL` line is rewritten to `""` before the build
  (Context). Pushed as `REPOWISE_IMAGE` and pinned by digest in
  `versions.lock.env`, like `WEB_SEARCH_MCP_IMAGE`. Bumping the tag is a
  rebuild plus V1.
- Environment: `REPOWISE_API_KEY` from a Secret generated at install,
  `REPOWISE_TELEMETRY_DISABLED=1`, `DO_NOT_TRACK=1`,
  `REPOWISE_SKIP_EDITOR_SETUP=1`, no third-party provider keys.

### 2. Which repositories, and who sees them

- The repository list is a chart value (`repos: [{alias, url, branch}]`).
  The `workspace` init container clones each into the workspace over HTTPS
  and runs the workspace init non-interactively; the `sync` container keeps
  them current (section 1). No webhooks.
- First stage: **public repositories only**. No git credentials exist in
  the release. Egress is port 443 to public addresses (the same
  `egressExcept` private and cluster ranges as `helm/web-search`) plus
  pat-service and DNS. A NetworkPolicy cannot restrict by hostname, so the
  pod can reach any public HTTPS host; that is accepted for a pod holding
  no credentials but the API key and the service PAT.
- Second stage: the company GitLab. It is external to this stack, reached
  over HTTPS like the public hosts, with a read-only token (the same kind
  ADR 0014 section 8 plans for agents) in a Secret used only by the init
  container and the poll. Push webhooks from GitLab would need a public
  route to `/api/webhooks/gitlab`; they are not added, polling stays. This
  stage is a values change plus the Secret, and is decided by the
  admission rule below, not by a new ADR.
- Repowise has no per-user authorization, so **every signed-in `ai-user`
  sees every indexed repository**, in the UI and through MCP. Only
  repositories readable by all `ai-user` holders are indexed. This is the
  admission rule for the `repos` list, recorded in the chart values
  comment and the runbook. Public repositories satisfy it trivially; it
  starts to matter with GitLab. Per-team instances are the answer if it
  stops holding (out of scope).

### 3. Web UI on its own host, `repowise.<site>`

The UI gets its own origin instead of a path on the main one, so it owns
`/` there and runs as upstream built it; no `basePath`.

- Site proxy: a second rule for the repowise origin
  (`REPOWISE_PUBLIC_ORIGIN` in `.env`, wildcard certificate), forwarding to
  the same backend and listener as the main origin, on the same public
  port. `X-Forwarded-Proto` as for the main origin (ADR 0004).
- The rule preserves `Host`. Observed 2026-09-25: requests to the
  repowise host arrive at the `external` listener with that host in
  `:authority` and, with no repowise route yet, fall through to the Open
  WebUI catch-all (`edge-test-external`).
- Cluster: no new listener or port. The `repowise` release owns an
  `HTTPRoute` attached to `edge`, `sectionName: external`, with
  `hostnames` set to the host of `REPOWISE_PUBLIC_ORIGIN` (chart value
  `publicOrigin`, set by `make repowise-up`) and rules to the UI Service
  on port 3000. `edge-test-external` has no `hostnames`, so under Gateway API
  precedence the repowise host is served by the repowise route only:
  `/sso`, `/platform`, `/v1` and Open WebUI are not reachable on it, and
  the main host never reaches repowise. `routes-external.yaml` is not
  edited.
- When the public port is not the scheme default, the authority carries
  it. Envoy must match it against a port-less `hostnames` entry; V1
  confirms that on v1.8.1, and if it does not, the proxy rule is changed
  to send `Host` without the port rather than the route matching on a
  port.
- The API and MCP ports get no public route. `/api/*` on the repowise host
  reaches the API only through the Next middleware, behind section 4.
- The ADR 0001 exception is this one host, and it is the `Host`
  matching ADR 0001 avoided. ADR 0001's reason was a proxy that rewrote
  `Host`; this proxy has been observed not to, and a failure fails
  closed: a rewritten `Host` lands on Open WebUI's login, never on
  repowise without SSO. Everything else keeps routing by path. Absolute
  callback URLs stay explicit (section 4); the Keycloak issuer stays on
  the main origin (`/sso`), so token `iss` does not change.

The local-mac profile already routes by host (ADR 0001) and gets
`repowise.localhost`.

### 4. SSO: Envoy Gateway OIDC, the shared key injected at the edge

- A `SecurityPolicy` on the repowise route with `oidc` against the
  `ai-stack` realm, a new confidential client `repowise` provisioned by
  `make provision-repowise-oidc` (explicit redirect URL
  `<REPOWISE_PUBLIC_ORIGIN>/oauth2/callback`, logout
  path `/logout`, web origin the repowise host). Cookies are host-only on
  the repowise host, so they never reach Open WebUI or pat-service, and
  theirs never reach repowise.
- Authorization: the access token from the OIDC cookie (stored unencrypted,
  `disableTokenEncryption`, under a fixed cookie name) is validated as a
  JWT and the realm role is checked. Envoy Gateway's authorization rules
  match methods, not paths, so the route has two named rules, each with its
  own `SecurityPolicy` (same OIDC client and cookies):
  - rule `interactive`, paths `^/api/repos/[^/]+/(chat|blast-radius)(/.*)?$`
    (enumerated from the pinned `packages/api-client`): `ai-user` and
    `ai-admin` may use GET, POST, PATCH and DELETE. Chat conversations are
    shared by everyone, since repowise has no users;
  - rule `read`, everything else: `ai-user` and `ai-admin` get GET and HEAD;
  - new realm role `repowise-admin`: every method on both rules (settings,
    provider keys, repo add/delete, sync and full re-index).
  Everything else is denied at the gateway. This is what stops any user
  from changing provider settings or starting an LLM-heavy full re-index,
  which the shared key alone would allow.
- An `HTTPRouteFilter` with `credentialInjection` (`overwrite: true`,
  Secret key `credential` = `Bearer <REPOWISE_API_KEY>`) sets the
  `Authorization` header on every request after OIDC. The browser never
  holds the key; the Next middleware forwards the header to the API.
  Whatever a browser puts in `localStorage` is overwritten.
- The UI container accepts traffic only from the gateway's Envoy pods
  (NetworkPolicy), so the injected-key path is the only way in.

pat-service is not used as the UI's auth proxy: it would take on a second
web app's traffic and cookies for a problem the gateway already solves
(see Alternatives).

### 5. MCP: `/mcp/repowise/` in pat-service

pat-service's single `/mcp/web-search/` becomes a table of MCP upstreams,
`/mcp/<name>/` -> URL, each with its own enabled flag
(`WEB_SEARCH_ENABLED`, `REPOWISE_ENABLED`); a disabled name returns 404 as
today. The table is config, not code; web-search behaviour is unchanged.

- Credentials, identity headers, header stripping: exactly as ADR 0017
  section 3. Repowise ignores the identity headers; attribution lives in
  pat-service logs and metrics.
- `patsvc_mcp_calls_total` gains a `server` label; the rate-limit key
  becomes per `(user, server)`, same `MCP_CALLS_PER_MINUTE` default.
- The `mcp` container's NetworkPolicy admits pat-service only. That is the
  whole authentication story for the MCP port, because repowise's HTTP
  transport has none; if upstream #1400 adds a key, it is set as well.
- Tool set: the default set of the pinned version, recorded in V2. Tools
  that call the LLM (`get_answer` at least) run under repowise's own PAT
  (section 6), not the caller's.

### 6. LLM and embeddings

- LLM: provider `openai`, `OPENAI_BASE_URL` = pat-service `/v1`, key = a
  PAT owned by a Keycloak service user `svc-repowise`, issued once and kept
  in a Secret. All repowise LLM use (doc generation, UI chat,
  `get_answer`) is one fair-share identity in ADR 0008 terms, with the
  lowest priority band. Per-user attribution of those calls is given up
  (see Consequences).
- Embeddings: the `embeddings-inference` release (ADR 0013, Deployment
  `embeddings-bge-m3`, model `bge-m3`), reached through the same base URL
  and PAT via the gateway route `openai-bge-m3`.
  `REPOWISE_EMBEDDER=openai`, model `bge-m3`,
  `REPOWISE_EMBEDDING_DECLARED_DIMS=1024` (declares the width without ever
  sending `dimensions`), `OPENAI_EMBEDDING_TIMEOUT=60`. There is no local-embedder fallback: an
  index built with a different embedder would have to be rebuilt on
  switching.
- The initial full index with docs runs once per repo on install. It can
  also run with `--no-prose` (no LLM) if V5 shows the GPU cost is too high;
  docs are then generated on demand.

### 6a. Precondition: embeddings on the GPU, VRAM carved from the LLM

Repowise embeds every file, symbol and page on the initial index and on
every update. On the CPU mode deployed today (`accelerator: cpu`, TEI
`cpu-1.9.3`, 2 CPU) that competes with Keycloak and pat-service for the
node's cores (ADR 0013 V4) and makes a full index slow. Therefore
**`embeddings-inference` in `accelerator: gpu` mode is a precondition of
`REPOWISE_ENABLED=true`**, not an optimization:

- `make repowise-up` fails unless the `embeddings-inference` release has
  `accelerator: gpu` and `embeddings-bge-m3` has one Ready replica. The
  release runs at `replicas: 1` for as long as repowise is enabled.
- This requires ADR 0013 stage V3 closed: GPU engine chosen (TEI CUDA on
  SM 12.0, else vLLM pooling runner) and `EMBEDDINGS_GPU_IMAGE` pinned,
  vectors matching CPU mode, peak embeddings VRAM measured into
  `gpuMemoryBudgetGiB`. bge-m3 has 568M parameters (about 1.1 GiB of fp16
  weights); the budget is weights plus the activation peak at
  `maxClientBatchSize` x `maxBatchTokens`, plus a margin, as measured.
- ADR 0013 carves the budget out of vLLM's `gpuMemoryUtilization` or
  SGLang's static memory fraction. The live engine is now ninfer
  (`ninfer-inference`, `ninfer-qwen38`, `kvCapacity: auto`,
  `maxContext: 180224`, `maxConcurrency: 3`, `kvDtype: fp8`; with `auto`
  the KV pool was 250,304 tokens, `deploy/ninfer/README.md`). For ninfer
  the budget is taken by replacing `kvCapacity: auto` with an explicit
  capacity that leaves `gpuMemoryBudgetGiB` free. The reduced value and
  `gpuMemoryBudgetGiB` change in one commit, as ADR 0013 requires for
  vLLM and SGLang. Whichever engine is live, that engine's knob is the
  one that moves.
- Floor: the reduced KV pool must still hold one request at `maxContext`
  (180224 tokens) plus the headroom the ninfer pilot sized for. If it
  cannot, `maxContext` is lowered and the new value is recorded here and
  in the engine's values; chat context is what pays for repowise.
- ADR 0013's rules for the GPU mode apply unchanged: the embeddings pod
  uses `runtimeClassName: nvidia` without requesting `nvidia.com/gpu`, the
  LLM engine keeps its exclusive `nvidia.com/gpu: 1`, and the LLM starts
  first, embeddings second. `make embeddings-up` refuses `gpu` unless the
  LLM Deployment is Ready.
- Turning repowise off does not return the VRAM by itself; reverting to
  `accelerator: cpu` and `kvCapacity: auto` is the ADR 0013 reversal,
  done deliberately.
- Values taken (2026-09-25, VG in progress). GPU engine: TEI's CUDA image
  for SM 12.0, `text-embeddings-inference:120-1.9.3` (same version as the
  CPU image). ninfer `--kv-capacity` is in tokens (64 per page group,
  2,245,632 bytes per group at fp8): `auto` gave 250,304 tokens (3,911
  groups) and 1.27 GB free after start; the explicit `196608` gives 3,072
  groups, 3.16 GB free after start per ninfer and 28,882 of 32,607 MiB used
  per `nvidia-smi`. It still holds one `maxContext` request (180,224) plus
  16,384 tokens. `gpuMemoryBudgetGiB: 3` is a paper value until VG measures
  the TEI peak.

### 7. Agents and Open WebUI

- Hermes: `mcp_servers.repowise` in `config/hermes/base-profile/config.yaml`,
  URL `.../mcp/repowise/`, header `Authorization: Bearer
  ${HERMES_INFERENCE_KEY}`, added to `locked-keys.yaml`. hermes-sync's
  web-search special case becomes a map from MCP entry to enable flag, and
  `REPOWISE_ENABLED` reaches the sync container the same way as
  `WEB_SEARCH_ENABLED` (hermes-images ConfigMap). Agent egress is
  unchanged.
- Open WebUI: an MCP tool connection to pat-service `/mcp/repowise/` with
  `auth_type: system_oauth` and `access_grants` `user *` / `read`, as in
  ADR 0017 section 6.
- Ownership change (amends ADR 0017 section 6). `TOOL_SERVER_CONNECTIONS`
  is one JSON list, so two releases cannot each own the ConfigMap. The
  `openwebui-tool-servers` ConfigMap moves to the `airgap-stack` chart and
  renders one entry per enabled MCP server from values set by `helm-up`
  from `.env`. `web-search` and `repowise` releases no longer create it;
  `websearch-up/-down` and `repowise-up/-down` still restart Open WebUI.

### 8. Operations

- Probes on the UI's `/health` (proxied by the Next middleware) and the
  API's `/health`.
- The API's `/metrics` scraped by the in-cluster Prometheus if its format
  is Prometheus text (V6); otherwise liveness only.
- Runbook `docs/operations/repowise.md`: adding a repository (admission
  rule of section 2), rotating the GitLab token (second stage),
  `REPOWISE_API_KEY` and the service PAT, re-index, upgrade (rebuild
  with the API-URL override), the site-proxy rule for the repowise host.

## Alternatives considered

| Option | Why not chosen |
| --- | --- |
| `/repowise` on the main origin with a `basePath` build | Needs code patches in at least four places that build root-absolute URLs (Context), re-audited on every upgrade; the wildcard host costs one proxy rule instead |
| A second public port on the main hostname | A new port to publish on the site proxy and firewall; the wildcard host reuses the existing public port |
| Dedicated `edge` listener for the repowise host, no `Host` matching | The host forwarding that brings the listener port to the Gateway is outside this repository and would need a second port by hand; `Host` is preserved, so matching it on the existing listener needs nothing new below the Gateway |
| Operator-only NodePort, like Grafana and Langfuse | The UI is for all users, with SSO |
| Gateway prefix rewrite `/repowise` -> `/` without a `basePath` build | The UI's HTML references absolute `/_next/*` and calls `/api/*`, both of which land on Open WebUI |
| oauth2-proxy in front of the UI | A new component and cookie secret for what Envoy Gateway's `SecurityPolicy` already does |
| pat-service as the UI's auth proxy (its `/platform` session) | pat-service grows a second web app's traffic, cookies and a proxy for SSE; its outage would also take the UI down |
| Users enter `REPOWISE_API_KEY` in the UI settings page | Shares the one admin key with every user |
| Repowise MCP reached from agents directly, trusted by NetworkPolicy | No attribution, rate limit or revocation; agents' egress widens (same as 0017) |
| stdio MCP inside each Hermes pod | Every agent pod would need the index, repo checkouts and LLM credentials |
| Per-user repowise instances | Duplicate indexes and LLM spend per user; nothing in the current scope needs per-user repositories |
| PostgreSQL backend | One pod, one writer; SQLite on the PVC is what upstream runs and tests |

## Consequences

- Chat users and agents get repository-aware tools without cloning
  anything; people get a browsable, generated wiki behind the same SSO.
- The repository list becomes an access-control decision: whatever is
  indexed is readable by every `ai-user`, including through agents.
- We build and serve an AGPL-3.0 application from unmodified upstream
  source with one build-time env override; every upgrade is a rebuild
  plus V1. The build recipe lives in this repository.
- A second public origin exists. It needs the site-proxy rule and the
  wildcard certificate kept valid. The `edge` Gateway now matches on
  `Host` for one route; any future host-based route is a new decision,
  not a precedent.
- Repowise LLM use shares the GPU with chat under one low-priority
  identity. A full re-index is a large batch job; the admin role gates it.
- Enabling repowise permanently shrinks the LLM's KV pool by the
  embeddings budget (section 6a): fewer concurrent long requests, and
  possibly a lower `maxContext`. Embeddings leave the CPU, so indexing
  does not load the node that runs Keycloak and pat-service.
- The embeddings pod sits outside scheduler GPU accounting (ADR 0013), so
  the budget is enforced by values and start order, not by Kubernetes.
- pat-service is on the path of every MCP call; its outage stops repowise
  tools as well as search and inference.
- The UI depends on Envoy Gateway's OIDC filter: the first use of it in
  the stack, so its behaviour on logout and token refresh is new to us.
- Fetched repository content enters agent context. It is company code,
  not web content, but a repo can still contain text that reads as
  instructions; the ADR 0017 guidance in `SOUL.md` applies.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Our image keeps upstream's baked `http://localhost:7337` API URL | UI loads, every API call fails in the browser | V0 greps the built bundle for `localhost:7337`; V1 browser network log |
| Envoy does not match an `:authority` that carries the public port against the port-less hostname | Repowise host keeps landing on Open WebUI | V1; proxy rule sends `Host` without the port |
| A later site-proxy change rewrites `Host` | Repowise unreachable (lands on Open WebUI), no exposure | V1 check kept in the runbook; the smoke test hits both hosts |
| Proxy rule sends the wrong port, or omits `X-Forwarded-Proto` | Repowise host serves Open WebUI, or OIDC redirects break | V1 checks both hosts; the rule is in the runbook |
| `credentialInjection` or OIDC-plus-JWT authorization not available as assumed on v1.8.1 | No SSO, or no read/admin split | V1; fallback for the split is the UI read-only for everyone and admin actions only through port-forward |
| MCP HTTP transport has no auth | Anyone in the namespace can call tools | NetworkPolicy admits pat-service only; V4 negative tests |
| An `ai-user` sees a repo they should not | Code disclosure inside the company | Admission rule of section 2; runbook; per-team instances if needed |
| Initial index with docs saturates the GPU | Chat latency during indexing | Lowest fair-share band; `--no-prose` first; V5 measures |
| Three processes on one SQLite workspace (API, MCP, sync) | Lock errors or stale reads | V2 runs concurrent sync and MCP calls; fallback is MCP served from the API process if upstream supports it, or a second read-only replica of the index |
| The server's polling fallback and the `sync` container both start an update of the primary repo after a fetch | Duplicate work, possibly lock contention | V2 observes a fetch cycle; the update is incremental and idempotent per state.json |
| Upstream breaks the build override or the MCP contract between releases | Failed upgrade | Pinned digest; V1 and V2 re-run on every bump |
| bge-m3 width or `dimensions` handling mismatches repowise | Search errors or silent bad vectors | V3 |
| ADR 0013 V3 does not close on SM 12.0 (neither TEI CUDA nor vLLM pooling works) | No GPU embeddings, so no repowise | Precondition holds: repowise is not enabled on CPU embeddings |
| ninfer's explicit `kvCapacity` does not map to VRAM as assumed, or ninfer grabs free memory beyond it | Embeddings or LLM OOM, crash loop after restart | VG measures actual VRAM of both processes; start order LLM first; reversal proven |
| Reduced KV pool cannot hold one `maxContext` request | Long chat and agent requests rejected | Floor rule of section 6a: lower `maxContext` explicitly, recorded |
| Embedding bursts during indexing slow LLM decode on the shared card | Chat TPOT regresses while indexing | VG measures TTFT/TPOT under index load; index batch size and concurrency capped from the numbers |

## Verification stages

Each stage has an exit condition. A failed gate stops the stages after it.

### VG -- GPU embeddings precondition (before any other stage on the cluster)

- ADR 0013 V3 closed: GPU engine and `EMBEDDINGS_GPU_IMAGE` recorded,
  `gpuMemoryBudgetGiB` from measured peak, vectors match CPU mode.
- ninfer with an explicit `kvCapacity`: the unit of the value, the KV pool
  it produces and the VRAM ninfer actually holds (`nvidia-smi` via
  `gpu-exporter`) recorded; free VRAM after start >= `gpuMemoryBudgetGiB`.
- One request at `maxContext` still admitted; otherwise the lowered
  `maxContext` recorded.
- Both processes survive: LLM restart with embeddings running (the guard
  or runbook prevents the bad order), embeddings restart with LLM running.
- LLM TTFT/TPOT under sustained embedding load equal to an initial
  repowise index, against the no-embeddings baseline.
- Reversal: `accelerator: cpu` and `kvCapacity: auto` restore the old KV
  pool.

Exit: `kvCapacity`, `gpuMemoryBudgetGiB`, the resulting KV pool and
`maxContext`, and the TTFT/TPOT delta recorded in section 6a; reversal
proven.

### V0 -- repository and render (no cluster)

- `helm template` / `helm lint` of `helm/repowise` clean with
  `REPOWISE_ENABLED` both ways; `make verify` includes them.
- pat-service unit tests for the MCP upstream table: unknown and disabled
  names 404, web-search unchanged, `server` label present.
- The image builds in CI with `NEXT_PUBLIC_REPOWISE_API_URL=""`; the
  built bundle contains no `localhost:7337`; `REPOWISE_IMAGE` digest pinned.
- `helm template` of `helm/repowise` renders the `HTTPRoute` with
  `sectionName: external` and `hostnames` from `publicHost`.

### V1 -- UI on `repowise.<site>` behind gateway OIDC

- `REPOWISE_PUBLIC_ORIGIN` is served by the repowise route (Envoy
  `route_name`), including when `:authority` carries the public port;
  `/sso`, `/platform`, `/v1` on the repowise host return 404; the main
  origin still reaches Open WebUI; the wildcard certificate is valid for
  the repowise host.
- Every top-level UI page, a repo page, docs, graph and a running job's
  live stream work; every browser request goes to the repowise host
  (network log), none to `localhost`.
- Login through Keycloak, logout, token refresh after expiry; a user
  without `ai-user` gets 403.
- `credentialInjection` on v1.8.1 sets the header; a request with a
  forged `Authorization` gets the injected one.
- Enumerate the `POST` endpoints a read-only user needs; `ai-user` cannot
  reach settings, provider, repo-mutating and re-index endpoints;
  `repowise-admin` can.

Exit: sections 3 and 4 finalized, or the fallback of the risk table
recorded.

### V2 -- MCP through pat-service

- Tool list of the pinned version recorded; which tools call the LLM.
- A Hermes turn and an Open WebUI chat each call a repowise tool through
  `/mcp/repowise/`; `patsvc_mcp_calls_total{server="repowise"}` counts
  them.
- MCP calls during a running sync: no errors from the shared SQLite.

Exit: sections 5 and 7 finalized.

### V3 -- LLM and embeddings through pat-service

- Doc generation and `get_answer` work against pat-service `/v1` with the
  service PAT; the calls appear under `svc-repowise` in fair share.
- bge-m3 embeddings through `embeddings-bge-m3` in GPU mode: which dims
  setting works; search results are sensible on one known repo; embedding
  throughput during the initial index.
- `make repowise-up` refuses with `accelerator: cpu` or with no Ready
  embeddings replica.

### V4 -- boundaries

- `mcp` port reachable from pat-service only; UI port from the gateway
  only; API port from neither.
- Revoked PAT -> 401 on `/mcp/repowise/`; token without `ai-user` -> 403.
- No outbound traffic to private or cluster ranges except pat-service and
  DNS; no traffic to repowise telemetry endpoints (egress logs over a full
  index and a poll cycle).

Exit: all pass twice.

### V5 -- capacity

- Time, GPU tokens and chat TTFT impact of the initial index with docs for
  the configured repos; the same with `--no-prose`. Decide which one the
  chart runs.
- Memory and CPU of `app` and `mcp` at steady state; PVC size per repo.

### V6 -- operations

- Probes, `/metrics` format, a Grafana panel for MCP calls by server.
- Runbook `docs/operations/repowise.md`.
- Client docs (Russian and English): what is indexed, that everything
  indexed is visible to everyone.

Exit: ADR moved to Accepted.

## Verification log

2026-09-25, before any repowise object is on the cluster:

- VG, partial. TEI publishes `120-1.9.x` (SM 12.0) images; pinned
  `120-1.9.3` by digest. ninfer redeployed with `kvCapacity: "196608"`:
  `kv_capacity_mode: explicit`, KV pool 196,608 tokens, numbers in section
  6a. Not yet done: `make embeddings-up` in GPU mode, peak VRAM, vectors
  versus CPU mode, restarts, TTFT/TPOT under load, reversal.
- V0. pat-service tests for the MCP table pass (disabled and unknown names
  404, per-server rate limit, `server` label, `/mcp/repowise/` mapped to
  upstream `/mcp` because `/mcp/` answers 307). hermes-sync and
  hermes-broker tests pass (the broker's pre-existing flaky
  `TestSlotsLRUEvictionAndQueue` fails with and without these changes).
  `helm lint` clean; `helm template` of `helm/repowise` applied with
  `kubectl apply --dry-run=server` against the cluster's CRDs: `HTTPRoute`
  with named rules, `HTTPRouteFilter` `credentialInjection`, both
  `SecurityPolicy` objects (oidc, jwt from cookie, authorization with
  `sectionName`) accepted. Image built locally for amd64 from `v0.53.0`
  with the override: the only `localhost:7337` strings left in the client
  bundle are the settings page's input placeholder and the webhook field's
  initial state; the CI check excludes exactly those two.
- Workspace behaviour, run locally on an arm64 build of the same tag (the
  amd64 build segfaults in lancedb under Rosetta emulation, not
  representative; the arm64 build needs `gcc` and drops the
  `tree-sitter-gdscript` grammar, test-only), mock embedder, `--no-prose`,
  three public repos: the chart's `workspace.sh` inits the workspace and
  later adds a new repo; the API with `REPOWISE_DB_URL=""` and the
  workspace as working directory lists all three repos, 401 without the
  key, 200 with it through the Next middleware on port 3000; MCP
  streamable HTTP serves 11 tools (`get_answer`, `get_change_risk`,
  `get_context`, `get_dead_code`, `get_health`, `get_overview`,
  `get_risk`, `get_symbol`, `get_why`, `list_repos`, `search_codebase`);
  `sync.sh` fetches, resets and runs `repowise update --workspace --yes`.
  `/metrics` is Prometheus text and needs no key. `update` writes
  `.vscode/` files into the checkouts regardless of
  `REPOWISE_SKIP_EDITOR_SETUP` (untracked, harmless for fetch and reset).

## Implementation order

0. GPU embeddings: ADR 0013 V3, then the ninfer `kvCapacity` reduction
   and `accelerator: gpu` in one commit (VG). Nothing below is enabled on
   the cluster before VG exits.
1. `helm/repowise` with the built image, git init container and
   NetworkPolicies; no public route (V0).
2. pat-service MCP upstream table; Hermes entry and hermes-sync map;
   tool-server ConfigMap moved to `airgap-stack` (V2).
3. LLM and embeddings wiring (V3).
4. Host-matched route, Keycloak client and `SecurityPolicy` (V1), then
   V4. The site-proxy rule and wildcard certificate are in place
   (2026-09-25).
5. V5; index mode and values committed.
6. V6; status to Accepted.

Out of scope: per-team or per-user instances, indexing repositories not
readable by all `ai-user` holders, kagent-native agents (ADR 0017 section
4), editor integrations for developer workstations (they can use the same
`/mcp/repowise/` with a PAT later).
