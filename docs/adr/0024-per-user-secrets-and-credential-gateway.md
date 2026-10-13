# 0024: Per-user secrets for agents: OpenBao behind a credential gateway, secrets never enter the agent

## Status

Proposed on 2026-10-13. Not implemented. Written from the repository and from
the Open WebUI `v0.12.0` sources (the tag pinned in `versions.lock.env`; the
cluster still runs `v0.11.4`, `k8s/base/applications.yaml`).

Verification run on 2026-10-14 with local prototypes (no cluster): results
and the points still open are under "Verification"; decisions 3, 4, 8 and
the stages were corrected from what it found.

Extends [ADR 0014](0014-hermes-curated-catalog-and-worker-slots.md) (agent
pods, profiles, egress through pat-service) and
[ADR 0021](0021-openwebui-pipe-and-acp-agent-integration.md) (permission
round trips). Reuses the pat-service proxy pattern of
[ADR 0017](0017-web-search-mcp-openserp-kagent.md) and
[ADR 0018](0018-repowise-codebase-intelligence.md). Follows
[ADR 0002](0002-one-owner-per-object.md) and
[ADR 0003](0003-private-ai-gateway-and-pats.md).

## Context

The goal is a full server-side development environment: a user's agent
(Hermes, dsh, opencode, pi) clones and pushes to the user's GitLab/GitHub
repositories, opens merge requests, and reads or drafts the user's mail. That
needs per-user third-party credentials. Today there are none:

- **What an agent pod holds.** Secret `agent-cred-<id>` with `API_SERVER_KEY`
  and `INFERENCE_KEY` (the user's pat-service PAT), delivered as env vars
  (`agent-broker/cmd/agent-broker/pods.go`). Profiles reference the PAT by
  env placeholder, but the agent process has it in its environment, so any
  agent shell can print it.
- **Egress.** Agent pods reach DNS and pat-service only
  (`k8s/agents/networkpolicy.yaml`). The `repo-reader` skill's `git clone`
  cannot work: no route and no credentials.
- **No secret manager.** No Vault, OpenBao, external-secrets, sealed-secrets
  or SOPS. Runtime secrets are `.env` rendered into Secret `airgap-runtime`.
  pat-service stores PATs as HMAC, nothing reversible.
- **Where text goes.** Agent transcripts live on the profile PVC; the Pipe
  copies up to 4000 chars of each tool output into the Open WebUI chat
  (`config/openwebui/agent_pipe.py`); AI Gateway GenAI spans carry prompts and
  tool results to Langfuse through an OTel collector with no redaction
  processor (`k8s/base/observability.yaml`). Anything that reaches the model
  context is persisted in three places.
- **pat-service's main mux is public.** HTTPRoute rule `platform`
  (`helm/airgap-stack/templates/routes-external.yaml`) rewrites
  `/platform/*` to `/` on port 8080, so every route on that mux, `/mcp/`
  included, is reachable from outside as `/platform/<route>`. Only the
  private inference listener (`pat-service/cmd/pat-service/private.go`) has
  no HTTPRoute.

### Can Open WebUI store the secrets?

Partly, and not for agents. What `v0.12.0` has:

| Mechanism | Storage | Who can use the secret | Fit |
| --- | --- | --- | --- |
| Tool/Function `UserValves` | `user.settings.tools.valves[<id>]`; Fernet with a key derived from `WEBUI_SECRET_KEY` only if `ENABLE_VALVE_ENCRYPTION=true` (default `False`, `backend/open_webui/env.py`, `utils/valves.py`) | Python tools running inside the Open WebUI process, passed as `__user__['valves']` | No. Plaintext by default; any admin-installed tool runs in-process with DB access; no audit, no scoping, no rotation; agent pods never see it |
| OAuth sessions (`oauth_session` table) | Fernet, `OAUTH_SESSION_TOKEN_ENCRYPTION_KEY` (defaults to `WEBUI_SECRET_KEY`), with refresh | Login provider (Keycloak, `system_oauth`) and tool servers with `auth_type: oauth_2.1` / `oauth_2.1_static`, stored as provider `mcp:<server_id>` (`models/oauth_sessions.py`, `utils/tools.py`) | Only for tool calls Open WebUI makes itself. A real per-user OAuth token store, but OAuth-only (no IMAP password, no deploy key), one key shared with session signing, and out of reach of agent pods |
| Connection `key` / custom headers | Admin config, one value for all users | Everyone | No: not per-user |

So Open WebUI can hold per-user OAuth tokens for its own tool servers, but it
is not a secret store for agents, and an agent that is handed a token will
eventually write it into a transcript.

### The real problem

A store alone does not stop leaks. If the secret reaches the agent process
(env, file, tool result), the model can read it, echo it, put it in a commit
or a URL, and it lands in the transcript, the Open WebUI chat and Langfuse.
The only robust rule is structural: **the agent never holds a third-party
secret; a trusted component outside the agent uses it on the agent's
behalf.**

## Decision

1. **Store: OpenBao.** An opt-in Helm release `helm/openbao` (Raft storage on
   a PVC, single replica first; static-key auto-unseal, key in its own Secret
   that is excluded from PVC backups -- see Consequences). KV v2 at
   `secret/users/<id>/<connector>`, where `<id>` is the same sha256-of-`sub`
   prefix the broker uses. Audit device on. Policies by Kubernetes auth role:
   only the credential consumers in 3 and 5 can read, each only its own
   connector path. Users and admins have no read policy: a stored secret can
   be replaced or deleted, never read back through any UI or API.
2. **Connecting an account happens outside the chat.** The platform UI of
   pat-service ([ADR 0022](0022-pat-service-admin-console.md)) gets a
   "Connections" page: OAuth authorization-code flow for GitLab (self-hosted
   OAuth application) and GitHub (GitHub App user-to-server tokens, which
   expire and refresh); a form for credentials that have no OAuth
   (IMAP/SMTP app password, fine-grained PAT as fallback). The callback or
   form writes straight to OpenBao; the browser and Open WebUI never see a
   refresh token. Scopes are minimal per connector and shown on the page.
   Disconnect deletes the entry and revokes at the provider where it can.
3. **Use: `cred-gateway`, behind pat-service.** A new small service in its
   own namespace `credentials`, next to OpenBao. pat-service gets a third
   listener (like the private inference port: no HTTPRoute, NetworkPolicy
   ingress from `agents` only) serving `/cred/<connector>/...`. It must not
   be a route on the 8080 mux, which is public under `/platform/`. The
   listener authenticates the
   agent's PAT as it does for `/mcp/`, strips `Authorization` and cookies,
   sets `X-User-Id`, and forwards to `cred-gateway`. The gateway, reachable
   only from pat-service (NetworkPolicy), reads the user's token from
   OpenBao, refreshes it if needed (one refresh per user and connector at a
   time, new pair written back before use: a GitHub refresh invalidates the
   old access and refresh token, so two concurrent refreshes lose the
   connection), injects it, and forwards to an allowlist
   of upstream hosts and paths per connector. Responses are stripped of
   `Authorization`, `Set-Cookie` and `WWW-Authenticate`; upstream error
   bodies pass through a secret-pattern scrubber. Agent egress stays
   "pat-service only".
4. **Git.** agent-sync renders a git config into the profile:
   `url."<pat-service>/cred/gitlab/".insteadOf "https://<gitlab-host>/"`
   (same for GitHub) and a credential helper that answers with the PAT
   (or nothing, once 6 lands). Remotes in `.git/config` therefore never
   contain a third-party token. Smart HTTP only; SSH is not offered. The
   allowlist is `info/refs`, `git-upload-pack` and `git-receive-pack`. Git
   LFS is off in stage 1: its batch API answers with an `href` and a
   `header` map the client applies itself, so storage URLs and their
   short-lived auth would reach the agent and bypass the gateway; enabling
   LFS needs the gateway to rewrite those actions to itself.
5. **APIs and mail through MCP.** GitLab/GitHub API actions and mail are MCP
   servers behind pat-service `/mcp/<name>/` (ADR 0017/0018 pattern). Each
   server is a credential consumer with its own OpenBao role and reads the
   token per request by `X-User-Id`. `mail-mcp` exposes list/search/read/
   draft/send; IMAP/SMTP credentials never leave it. Tools return content,
   never credentials.
6. **The PAT leaves the agent container.** The agent pod gets an egress
   sidecar that holds `INFERENCE_KEY` (separate container and uid, no shared
   env or volume with it) and adds it to requests to pat-service. The agent
   process talks to `localhost` and has no token to print. A leaked
   transcript then contains no usable credential at all.
7. **Write actions need consent.** `git push`, MR creation and `send` are
   classified as write operations by the gateway and the MCP servers; on the
   ADR 0021 Pipe path they require a permission round trip, and runtimes
   without permission support get read-only scopes. dsh's
   `danger-full-access` does not extend to these operations.
8. **Defence in depth against leaks into sessions.** Even with 3-6, users
   paste secrets and upstreams echo them:
   - a repo-owned Open WebUI inlet/outlet Filter scans user messages and Pipe
     tool outputs for known patterns (`glpat-`, `ghp_`, `github_pat_`,
     `sk-`, private-key blocks, gitleaks rule set) and redacts before
     the model sees them, telling the user to use Connections instead. An
     inlet Filter does not protect the chat DB: Open WebUI writes the
     user message to the chat (`main.py`: `insert_new_chat` /
     `update_chat_by_id`) before `process_chat_payload` runs the filters,
     and the browser saves its own history copy. The Filter therefore also
     overwrites the stored user message with the redacted text (it runs
     in-process and gets the chat and message ids);
   - the OTel collector gets a `redaction` processor with the same
     patterns as `blocked_values` and `allow_all_keys: true` (without it
     the processor deletes every attribute). It masks span attributes only,
     not span events; AI Gateway v1.1.0 puts prompts and outputs in
     attributes (`llm.input_messages.*`, `llm.output_messages.*`), so
     Langfuse does not store them;
   - gateway and MCP access logs record user, connector, method and path,
     never headers or bodies.
9. **Open WebUI's own stores are not used for third-party credentials.**
   Repo-owned tools do not declare secret `UserValves`; tool servers keep
   `system_oauth`, not `oauth_2.1` to third parties. `ENABLE_VALVE_ENCRYPTION`
   is set to `true` so that admin valves that do hold keys are not plaintext.
10. **Network.** `cred-gateway`, the MCP servers and OpenBao get their own
    NetworkPolicies; only the gateway and the MCP servers have egress, each
    to its connector's hosts. On an air-gapped site connectors to external
    hosts (github.com, public mail) are disabled by configuration.

### Stages

1. OpenBao, Connections page, the pat-service `/cred/` listener,
   `cred-gateway`, GitLab read over HTTPS, the Filter (including the
   stored-message overwrite) and the collector redaction.
2. PAT sidecar (6); GitLab write with permission round trip.
3. GitHub App connector; GitLab/GitHub MCP; LFS through the gateway.
4. `mail-mcp`.

## Alternatives considered

- **Open WebUI `UserValves` / `oauth_session`.** See Context: in-process,
  OAuth-only or plaintext by default, and unreachable for agent pods.
- **Per-user Kubernetes Secret in agent env** (extend `agent-cred-<id>`).
  Simplest, but the agent holds the secret and will print it; rejected for
  third-party credentials.
- **external-secrets / sealed-secrets / SOPS.** They deliver secrets into
  Kubernetes Secrets, i.e. back into the pod; they solve GitOps storage of
  platform secrets, not per-user runtime secrets.
- **Keycloak identity brokering with stored tokens** (GitLab/GitHub as IdPs,
  "Store tokens"; V1 `GET /realms/<realm>/broker/<alias>/token` with the
  `broker` `read-token` role, or V2 behind
  `--features=identity-brokering-api:v2`, confidential clients with "Allow
  retrieve external tokens"). Gives per-user OAuth tokens without a new
  store, but no non-OAuth credentials (mail), token lifetime and refresh
  depend on the provider, and in V1 any client with `read-token` in scope
  gets the raw token. Can become a token source for `cred-gateway` later.
- **HashiCorp Vault.** Same model as OpenBao, but under the Business Source
  License; OpenBao is the MPL-2.0 fork.
- **Encrypted table in pat-service** (envelope encryption with a key from
  `airgap-runtime`). No new stateful component, but no audit device, no
  per-consumer policies, and the key sits next to the database in every
  backup. Acceptable fallback if OpenBao is not wanted on small sites; the
  gateway reads through an interface so the store can be swapped.

## Consequences

- Agents can work on the user's repositories and mail with the user's
  identity, and a transcript, chat or trace does not contain a usable
  third-party credential unless a user pasted it and the patterns missed it.
- New stateful component (OpenBao) with its own backup and unseal procedure;
  losing it loses connections, not data (users reconnect). An air-gapped
  site has no KMS, so auto-unseal is OpenBao's static-key seal (key from
  file or env, 32 bytes); its own docs warn it is only as safe as where the
  key is injected. With the key in a Kubernetes Secret, a cluster admin can
  unseal and read everything; what it buys is that a stolen PVC or backup
  without that Secret is useless. Manual Shamir unseal is the alternative
  if an operator can be present at every restart.
- GitHub user tokens live 8 hours, refresh tokens 6 months; GitLab access
  tokens 2 hours by default. A connection unused for longer than the
  refresh-token lifetime needs the user to connect again.
- pat-service becomes the single entry for agent egress to third parties;
  its availability now affects git as well as inference.
- The agent can still act as the user through the gateway while the pod
  runs: a prompt-injected agent can read whatever the scopes allow. Scopes,
  write consent (7) and audit bound this; the gateway does not prevent it.
- Pattern-based redaction is best effort; unknown secret formats pass.

## Verification

Run on 2026-10-14 on a workstation with Docker, `gh` and the pinned
upstream sources; no cluster. "Done" means the mechanism was exercised or
read in the pinned source; "open" items need the cluster or the real
implementation.

| # | Check | Result |
| --- | --- | --- |
| 1 | OpenBao unseal without a KMS | Done (docs): 2.7.x lists a Static Key seal, key by `file://` or `env://`, 32 bytes, with an explicit warning. Consequence recorded above. Open: Raft on the remote storage class |
| 2 | Policies: writer cannot read, consumer reads only its connector | Done: OpenBao 2.7.1 dev server. `connections-writer` (`create`,`update` on `secret/data/users/*`, `delete` on metadata) can put and delete but read is denied; `gitlab-consumer` (`read` on `secret/data/users/+/gitlab`) reads gitlab, is denied mail and list; a `default` token is denied. Open: Kubernetes auth roles in the cluster |
| 3 | Git through an injecting gateway, no token on the agent side | Done against GitHub with a ~70-line Python prototype on the host holding the token, and a tokenless container as the agent with `insteadOf`: clone and fetch of a private repo, push and delete of a temporary branch on a public repo; path outside the allowlist 404; without the gateway clone and push fail with no credentials. Token found in 0 files of the clone, `.git` and the gateway log. Open: self-hosted GitLab, large packs; LFS deferred (decision 4) |
| 4 | `/cred/` unreachable from outside | Done (code): a route on the 8080 mux would be public as `/platform/cred/...`; decision 3 now requires a separate listener. Open: test once the listener exists |
| 5 | Sidecar keeps the PAT out of the agent container | Done under runc: sidecar with the key, agent sharing only the network namespace (`--user 10000 --read-only`): key in agent env 0, in agent `/proc/*/environ` 0, request through `localhost` works. Agent pods do not set `shareProcessNamespace`. Open: the same under gVisor on the remote overlay |
| 6a | Open WebUI Filter keeps a pasted token out of `webui.db` | Done (source v0.12.0): an inlet Filter alone does not; the message is stored before filters run. Decision 8 corrected. Open: Filter that overwrites the stored message, and the v0.11.4 to v0.12.0 upgrade |
| 6b | Collector masks the token before Langfuse | Done: `otel/opentelemetry-collector-contrib:0.114.0` (the deployed version) with `redaction` masked `glpat-`/`ghp_` in `llm.*_messages.*.message.content` (`redaction.masked.count=2`); the same token in a span event passed unmasked. AI Gateway v1.1.0 sets message content as attributes (`internal/tracing/openinference`) |
| 7 | Token refresh | Done (docs): GitHub App user token 8 h, refresh 6 months, refresh invalidates the old pair; GitLab access token 2 h by default, lifetime configurable by the admin. Gateway refresh lock added to decision 3. Open: run against both providers |
