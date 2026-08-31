# 0014: Hermes in the cluster -- curated base profile in the repo, per-user PVC layers, bounded worker slots with idle eviction

## Status

Proposed on 2026-09-15. Not implemented. Refines
[ADR 0009](0009-cloud-hermes-fleet-per-user-profiles.md) (also Proposed):
it settles 0009's open items 1 and 2 (SQLite placement, adapter contract)
and changes one thing 0009 states literally -- a "worker" is not a
pre-started pod that a profile PVC is mounted into (see "Workers" below).
It adds a second PAT issuance path next to the SSO one from
[ADR 0003](0003-private-ai-gateway-and-pats.md), scoped to the broker
(section 8). Nothing else is superseded. Every Hermes-internal behaviour
this ADR relies on is listed as an assumption under stage V1 and must be
confirmed against the pinned Hermes release before implementation.

## Context

- The goal is a **curated catalog** of Hermes skills and settings, reached
  by users through Open WebUI, not a free-for-all of per-laptop installs.
  The catalog is maintained in this repository and reviewed like any other
  change.
- Each user keeps personal state: memories, session history (`state.db`),
  personal skills, personal settings. That state must survive restarts,
  catalog updates and redeploys.
- A catalog change must reach **every** user, including users who are not
  active at the moment of the change.
- A user can see which catalog skills exist and switch off the ones they do
  not want, for example when their profile is first created.
- In Open WebUI a user chooses per chat between talking to the model
  directly (today's path) and talking to their Hermes agent. The agent is
  an addition to the model selector, not a replacement for direct chat.
- The number of worker slots `K` is smaller than the number of active users
  `N`. Compute is the scarce resource (node CPU/RAM; inference itself is
  already one shared engine with fair-share per
  [ADR 0008](0008-per-user-fair-share.md)), so an agent that is not used
  must give its slot back.
- Constraints from the platform:
  - A Kubernetes Pod's volumes are fixed at creation. A PVC cannot be
    attached to an already-running pod, so "a free worker picks up a user's
    PVC" is not implementable literally.
  - Storage on the reference host is local disk on one k3d node
    (`ReadWriteOnce`). SQLite on a RWO local volume opened by exactly one
    process is safe; 0009's SQLite-on-network-storage concern applies only
    if storage later becomes NFS/RWX.
  - Open WebUI admits only users with the Keycloak role `ai-user` or
    `ai-admin` (`OAUTH_ALLOWED_ROLES`). Its one OpenAI connection
    (`OPENAI_API_BASE_URL`, the private AI Gateway) uses
    `OPENAI_API_CONFIGS` `auth_type: system_oauth`: every request carries
    the signed-in user's Keycloak access token, and the gateway derives the
    user from that token, not from the `X-User-Id` header Open WebUI also
    forwards (`k8s/base/applications.yaml`). Models in the selector are
    whatever that connection's `/v1/models` returns
    (`BYPASS_MODEL_ACCESS_CONTROL: "true"`).
  - pat-service issues PATs only to a browser SSO session
    (`POST /api/tokens` behind `requireSession`,
    `pat-service/cmd/pat-service/main.go`); there is no machine path to
    issue a token for a user today.
  - Air-gapped installs: everything the catalog needs ships as a pinned
    image in `versions.lock.env`; no runtime fetch from a skills hub.

## Decision

### 1. The base profile lives in the repository

`config/hermes/base-profile/` is the single source of the catalog. Its
layout mirrors a Hermes profile directory (exact file names confirmed in
V1):

```
config/hermes/base-profile/
  config.yaml          # base settings: model endpoint, toolsets, limits
  SOUL.md              # base persona / system instructions
  skills/<skill-name>/SKILL.md (+ scripts, references)
  catalog.yaml         # per-skill metadata: required, default_enabled
  locked-keys.yaml     # config keys users may not override
  CATALOG_VERSION      # written by the build, not by hand
```

Rules:

- Every skill in the catalog is reviewed in a merge request like code.
  Skills that run scripts are treated as code executed with the user's
  agent credentials.
- `catalog.yaml` marks each skill `required` (cannot be switched off, e.g.
  skills the platform itself depends on) or optional with
  `default_enabled: true|false`. A skill missing from `catalog.yaml` is
  optional and enabled by default. The build fails if `catalog.yaml` names
  a skill that does not exist.
- No secrets in the base profile. Endpoints and credentials are injected
  per pod (section 8).
- `make hermes-catalog` builds `llm-stack/hermes-catalog:<git-sha>`, a
  minimal image containing only this directory, and records it as
  `HERMES_CATALOG_IMAGE` in `versions.lock.env`. The version pin, not the
  branch head, is what the cluster runs, so a catalog rollout is one
  reviewable commit and a rollback is reverting it.

### 2. A user profile is two layers: catalog (replaced) + personal (kept)

Each user has one PVC `hermes-profile-<id>`, where `<id>` is the first 16
hex characters of `sha256(keycloak sub)` (a stable, DNS-safe name that does
not expose the `sub`). Inside it:

| Path in PVC | Owner | On catalog update |
| --- | --- | --- |
| `catalog/` (copy of base profile) | catalog | replaced whole |
| `catalog-selection.yaml` (switched-off catalog skills) | user | untouched; entries for removed or now-required skills ignored |
| `skills/` personal skills | user | untouched |
| `config.user.yaml` personal overrides | user | untouched; locked keys ignored |
| `memories/`, `state.db`, sessions, logs | user | untouched |
| `.catalog-version`, `.catalog-seen` | sync step | rewritten |

The effective profile Hermes sees is assembled at pod start:

- **Skills.** Hermes sees the catalog skills that are enabled for this user
  plus everything in `skills/`. Enabled means: `required`, or not listed in
  `catalog-selection.yaml` as disabled (for `default_enabled: false`
  skills: explicitly enabled there). Delivery is via multiple skill
  directories if the pinned release supports them, otherwise per-skill
  symlinks from `skills/` into `catalog/skills`; in both cases the sync
  step exposes only enabled skills, so selection does not depend on any
  Hermes feature. On a name collision the **catalog skill wins** and the
  personal one is reported in the sync log. A user who wants to change a
  catalog skill forks it under a new name; they never edit `catalog/`.
- **Config.** `catalog/config.yaml` deep-merged with `config.user.yaml`;
  keys listed in `locked-keys.yaml` (model provider and endpoint, enabled
  toolsets, sandbox/network settings, scheduler off) always take the
  catalog value. The merged file is written to the path Hermes reads; the
  user never edits that generated file directly.
- **Persona.** `catalog/SOUL.md` is the base; an optional personal addendum
  is appended, not substituted.

Removing a skill from the catalog removes it from every user at their next
start. That is intended: the catalog is curated, not advisory.

### 3. Skill selection: visible and switchable from the chat

No separate settings page is added: it would need a new public path and a
new SSO client, and the user already has the chat in front of them. Selection
is therefore done with chat commands in the `hermes-agent` conversation,
**handled by the broker itself** and never sent to the model:

| Command | Effect |
| --- | --- |
| `/skills` | Lists catalog skills: name, one-line description (from `SKILL.md`), state (on / off / required), and marks skills new since the user last looked |
| `/skills off <name>...` | Adds to disabled list; refused for `required` skills |
| `/skills on <name>...` | Removes from disabled list |
| `/skills reset` | Returns to catalog defaults |

- **On profile creation** (first request, empty PVC) the broker answers the
  first message with the catalog list and the commands above before
  starting the agent, so the user makes the choice up front; sending any
  normal message accepts the defaults.
- **On catalog update** the next reply after an agent start that brought
  new skills is prefixed with a short "new in catalog: ..." notice listing
  the new optional skills and how to switch them off (`.catalog-seen`
  tracks what was already announced).
- The broker never mounts user PVCs (they are RWO and belong to the
  agent). It reads the catalog list from its own read-only copy of
  `HERMES_CATALOG_IMAGE` and the user's current state from the agent's
  last sync report (a pod annotation). A selection change is passed to the
  **next agent start** as a pod annotation and written to
  `catalog-selection.yaml` by the init step (section 4). The broker then
  (re)starts the user's agent right away -- after the user's other
  in-flight streams finish -- so the change takes effect on the next
  message. A selection command therefore costs a slot like a normal
  message.

### 4. Catalog sync happens at every agent start

Every agent pod has an init container running `HERMES_CATALOG_IMAGE` that:

1. Applies a pending selection change from the pod annotation, if any, to
   `catalog-selection.yaml`.
2. Copies the base profile into `catalog.new/` on the PVC.
3. Atomically swaps `catalog.new/` -> `catalog/` (rename), so a crash
   mid-copy leaves the previous catalog intact.
4. Regenerates the merged config, the enabled skill set, and
   `.catalog-version`, and reports enabled/disabled skills and
   `.catalog-seen` back as a pod annotation for the broker.
5. On first start (empty PVC) also creates the empty personal layer.

Because agents are short-lived (section 5), "sync at start" is enough to
reach every user:

- Inactive users get the new catalog the next time they start an agent.
- Running agents are marked stale when `HERMES_CATALOG_IMAGE` changes and
  are restarted **at their next idle point**, never mid-request. Upper
  bound on propagation to an active user: their idle timeout, or
  `maxAgentLifetime` (default 8h) for a user who is never idle.
- `make hermes-catalog-rollout FORCE=1` restarts stale agents immediately
  for urgent fixes (e.g. withdrawing a harmful skill), accepting that
  in-flight requests are cut.

### 5. Workers are bounded slots, not pre-started pods

Since a PVC cannot be attached to a running pod, the pool is expressed as
**`K` concurrency slots** for per-user agent pods:

- A worker is a pod created for exactly one user, mounting only that user's
  PVC, alive only while the user is active. Pod lifetime is one activation;
  profile lifetime is the PVC's.
- `K` is enforced twice: by the broker (section 6) as its scheduling rule,
  and by a `ResourceQuota` in the agent namespace (`pods: K`, plus CPU and
  memory totals) as the backstop if the broker has a bug.
- Cold start cost is kept low instead of pre-warming pods: the Hermes image
  is pre-pulled on the node, the init sync copies only the (small) catalog,
  and agent start time is a measured budget (V2, target p95 < 10s).
- This also closes 0009's SQLite question for this deployment: one RWO PVC
  is mounted by at most one pod, so `state.db` has exactly one writer on
  local disk. The broker guarantees at most one pod per user PVC; a pod is
  deleted and confirmed gone before another one for the same PVC is
  created.

Rejected alternatives:

- **Pre-started generic workers that fetch the profile from object storage
  (MinIO) on attach and push it back on release.** Avoids the mount
  problem, but moves `state.db` over the network on every activation, adds
  a lost-update window if a worker dies before upload, and makes the
  profile's truth a sync protocol instead of a volume.
- **One long-lived pod per user (`StatefulSet`).** Needs `N` pods' worth of
  memory for `K` users' worth of work; already rejected in 0009.
- **All PVCs mounted into every worker.** Breaks user isolation: a tool
  call in one user's agent could read another user's memories.

### 6. The broker: Open WebUI's only path to an agent

A new component `hermes-broker` (single replica, Deployment in the agent
namespace) sits between Open WebUI and the agents.

**Front: model or agent, chosen in the Open WebUI selector.**

- Open WebUI gets a **second OpenAI connection** next to the existing one:
  `OPENAI_API_BASE_URLS` = private AI Gateway `;` broker, and
  `OPENAI_API_CONFIGS` sets `auth_type: system_oauth` on both. The selector
  then lists the gateway's models (e.g. `qwen-3.8-27b`, direct chat, exactly
  as today) and the broker's `hermes-agent` side by side; the user picks
  one per chat, and switching back to the model needs nothing from the
  broker. The change is made in `k8s/base/applications.yaml` and flows into
  the chart through `scripts/helm-render` (ADR 0002).
- The broker's `GET /v1/models` returns only `hermes-agent`, with a
  description that it is the personal agent. Model ids must not collide
  with gateway models; the connection's `prefix_id` is the fallback if
  they ever do.
- **Identity comes from the token, not a header.** The broker validates the
  Keycloak access token Open WebUI sends (issuer, signature via realm JWKS,
  expiry, role `ai-user` or `ai-admin`) and takes `sub` and
  `preferred_username` from it. `X-User-Id` / `X-User-Name` are ignored. A
  `NetworkPolicy` admitting only the Open WebUI pod stays as defence in
  depth, not as the boundary.
- Rejected: putting `hermes-agent` behind the private AI Gateway as a rule
  keyed on model name ([ADR 0006](0006-pluggable-inference-backends.md)).
  That gateway is also the target of pat-service, so the agent would become
  reachable with any PAT from outside, and agent turns (minutes, many tool
  calls) would run under the `llmd` route's per-user request rate limit,
  request timeout and body processing, all sized for single LLM calls.
- The broker is not exposed on any public path
  ([ADR 0001](0001-path-routing-on-one-origin.md) routes are unchanged).

**Per request.**

1. Validate the bearer token; resolve `<id>` from its `sub`. Invalid or
   missing token, or no allowed role -> 401/403, nothing started.
2. If the last user message is a `/skills` command, handle it (section 3)
   and answer directly; no slot is used unless a restart follows.
3. If the user's agent pod is Ready, proxy to it (streaming passthrough),
   mapping the Open WebUI chat to a Hermes session (header name confirmed
   in V1), and update last activity.
4. If not running and a slot is free: create the PVC if absent (and answer
   with the onboarding list, section 3), ensure the user's credential
   (section 8), create the pod, wait for Ready, proxy.
5. If no slot is free: evict the **least recently used idle** agent
   (section 7), then continue as in 4.
6. If no agent is idle: hold the request in a FIFO wait queue for up to
   `slotWaitTimeout` (default 60s), returning a short status message into
   the chat stream so the user sees why; on timeout answer with an
   OpenAI-shaped 503 and `Retry-After`.

**State.** The broker keeps no database. Pod existence and phase come from
the Kubernetes API (pods labelled `hermes.llm-stack/user-id=<id>`); last
activity is written to a pod annotation at most once a minute, so a broker
restart rebuilds its view from the cluster. In-flight request counts are in
memory; after a broker restart every pod is treated as busy for one
`idleTimeout` rather than evicted blindly.

**RBAC.** The broker's ServiceAccount may create/delete Pods, create PVCs,
and create/update Secrets named `hermes-cred-*` **only in the agent
namespace**. It never deletes PVCs and has no access outside the namespace.

### 7. Idle definition and eviction

An agent is **idle** when both hold:

- no in-flight request through the broker (including open streams), and
- last activity older than `idleTimeout` (default 15 min).

Eviction is `delete pod` with `terminationGracePeriodSeconds` long enough
for Hermes to flush and close `state.db` (measured in V1; default 30s). A
busy agent is never evicted to make room; the waiting request queues
instead. Independently of slot pressure, the broker stops agents idle for
`idleShutdown` (default 30 min) so capacity is returned even when nobody is
waiting, and stops stale agents (section 4) at their first idle moment.

**Background agents are not supported.** An agent exists only to serve a
user's requests; nothing runs without one. Hermes scheduler/cron features
are switched off through `locked-keys.yaml`, and a long-running tool
process dies with its pod on eviction. This is a design choice, not a
temporary gap: it is what makes "idle" well-defined and `K` a hard bound.

### 8. Agent pod: identity, inference, sandbox

**Inference credential: the broker issues a PAT per user.**

- pat-service gets an internal endpoint `POST /internal/hermes-tokens` on a
  **separate listener** (its own port, not routed by any Gateway or
  HTTPRoute), admitted by `NetworkPolicy` only from the broker and
  authenticated with a shared secret from a chart-owned Secret. Request:
  `subject`, `owner_name`. The endpoint does not accept a name, lifetime or
  scope from the caller: it always creates a token named `hermes-agent`
  with a fixed short lifetime (`HERMES_PAT_TTL_DAYS`, default 7) and, in
  the same transaction, revokes that subject's previous `hermes-agent`
  token.
- `personal_access_tokens` gets a column `issued_by`
  (`user` | `hermes-broker`, default `user`). The user's PAT dashboard shows
  broker-issued tokens labelled as such; the user can revoke them.
- The broker stores the token in Secret `hermes-cred-<id>` with its expiry,
  and mints a new one when the Secret is missing or expires within 24h,
  before creating the pod. The pod mounts the Secret; the token value never
  appears in pod spec, logs or the PVC.
- Validation of broker-issued tokens is the existing `proxy` path, so
  inference carries the user's `sub` as `X-User-Id` into fair-share
  ([ADR 0008](0008-per-user-fair-share.md)) and Langfuse
  ([ADR 0011](0011-pat-session-key-langfuse-tracing.md)), and rate limits
  apply to the user, not to a shared agent identity.
- Revoking the `hermes-agent` token in the dashboard stops the running
  agent's inference, but the broker issues a new one on the next start --
  it is not a way to disable the agent for a user. Access to the agent is
  controlled by the Keycloak role (Open WebUI admission); on offboarding
  `scripts/hermes-profile-delete` revokes the token and deletes the Secret
  and PVC.
- Rejected: one shared service PAT for all agents (collapses every agent
  into one fair-share flow and one Langfuse user); asking each user to
  create a PAT and paste it (breaks the "no setup" goal and leaves tokens
  in chat history).

**Sandbox.** Non-root, read-only root filesystem except the PVC and an
`emptyDir` workdir, no ServiceAccount token, `NetworkPolicy` egress only
to pat-service's public `/v1` listener and the systems 0009 allows
(GitLab, read-only token). Agent pods cannot reach the internal token
listener.

**Resources.** Explicit requests/limits per agent pod; `K` is derived from
node allocatable minus the existing stack, not chosen freely (V2).

**PVC.** Fixed size (default 2Gi) with a usage metric; retention is
independent of activity. PVCs are deleted only by
`scripts/hermes-profile-delete <user>`, never by the broker.

### 9. Ownership ([ADR 0002](0002-one-owner-per-object.md))

- The `airgap-stack` chart owns: agent namespace, broker Deployment and
  Service, RBAC, `ResourceQuota`, `NetworkPolicy`s, broker config
  (`K`, timeouts, `HERMES_IMAGE`, `HERMES_CATALOG_IMAGE`), the broker <->
  pat-service shared Secret and pat-service's internal listener Service.
  The Open WebUI connection list stays where Open WebUI's env is owned
  today (`k8s/base/applications.yaml`, rendered into the chart).
- The broker owns, at runtime: agent Pods, user PVCs, and
  `hermes-cred-*` Secrets. They carry
  `app.kubernetes.io/managed-by: hermes-broker` and are excluded from
  `scripts/helm-render` drift checks and from Helm ownership, so
  `helm upgrade` never deletes a user's profile.
- The repository owns the catalog content; the cluster only ever sees it
  through the pinned catalog image.

## Consequences

- One reviewed place defines what every Hermes agent can do; a merge plus
  a version pin changes it for all users, with a bounded and known
  propagation time.
- Personal memories, sessions, skills and skill selection survive catalog
  updates, agent eviction and redeploys, because they never live in the
  catalog layer.
- Direct model chat in Open WebUI is unchanged; the agent is one more entry
  in the model selector, chosen per chat. A chat started with the model
  and switched to `hermes-agent` mid-way is handed to the agent as plain
  history, not as an agent session.
- Users choose among optional catalog skills without leaving the chat, and
  are told when new ones arrive. They cannot switch off `required` skills,
  edit catalog skills or override locked config keys: those are reapplied
  at every start.
- Capacity is explicit: at most `K` agents run; the `K+1`-th active user
  waits for an idle agent or gets a clear 503. Nobody holds a slot while
  not using it.
- Every return after idle pays a cold start (pod create + catalog sync +
  Hermes boot). This is the price of `K < N`; it is measured, not assumed.
- No agent work happens without a user request. Scheduled tasks and
  long-running background jobs are not available through this platform.
- pat-service gains a second issuance path. Whoever holds the broker's
  shared secret and can reach the internal listener can obtain an
  inference token for any subject; this is the broker's main privilege and
  is contained by network policy, fixed token shape and short lifetime.
- `/skills` becomes a reserved prefix in the `hermes-agent` chat; a user
  message starting with it never reaches the model.
- Open WebUI remains a question-answer UI without tool progress (0009);
  this ADR does not change that, and the broker's contract lets the web
  dashboard be added later against the same agents.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Pinned Hermes cannot read skills from two directories | Layering needs symlinks | V1 checks; symlink fallback defined in section 2 |
| Hermes config has no clean override layering | Locked keys enforced only by generated file | Generated merged config is the only file Hermes reads; V1 confirms Hermes does not rewrite it at runtime |
| Hermes lets the agent install or enable skills at runtime | Selection and curation bypassed until next start | V1 checks; disable skill-install tooling via `locked-keys.yaml`; sync resets on every start |
| Hermes does not flush `state.db` on SIGTERM within grace period | Lost or corrupted session history on eviction | V1 kill test under load; grace period set from measurement; SQLite integrity check in init step |
| Two pods for one PVC overlap on agent restart | Two writers on `state.db` | Broker waits for pod deletion before create; RWO + single node; V4 race test |
| Cold start too slow | Users see long waits after idle | V2 budget; pre-pulled image; tune `idleTimeout` up if `K` allows |
| `K` too small for real usage | Frequent 503/queueing | Broker metrics: slot occupancy, queue wait, evictions/hour; `K` revisited from data |
| Open WebUI v0.11 does not apply `system_oauth` to a second connection | Broker receives no user token; agent unusable | V3 checks first; fallback is trusting `X-User-Id` with NetworkPolicy as the boundary, recorded as a weaker exception |
| Forged identity reaching the broker | Access to another user's profile and a PAT in their name | Identity only from a validated Keycloak token; NetworkPolicy as second layer; V3 negative tests |
| Keycloak access token expires during a long agent turn | Nothing (validated at request start) or a cut stream if re-validated | Validate once per request; V3 runs a turn longer than the token lifetime |
| Broker shared secret leaks | Tokens can be minted for any subject | Internal listener reachable only from broker pod; fixed name and 7-day lifetime; `issued_by` visible in dashboard; secret rotation documented in runbook |
| User loses Keycloak role while holding a valid broker token | Token valid up to 7 days | Agent unreachable without Open WebUI admission; token sits only in a Secret mounted by that user's agent; offboarding script revokes it |
| Harmful or broken skill merged into catalog | Reaches all users at next start | Review required for `config/hermes/**`; `required` flag reviewed separately; forced rollout for withdrawal; catalog smoke test in CI |
| Personal skill shadowed by a new catalog skill of the same name | User's skill disappears | Catalog wins by rule; sync log + "new in catalog" notice names the shadowed skill |
| Broker restart loses in-flight counts | Busy agent evicted | All pods treated busy for one `idleTimeout` after restart |
| PVC growth (sessions, logs) | Node disk pressure | Fixed PVC size, usage metric, alert at 80% |
| Helm or `make` cleanup deletes runtime PVCs | Loss of all user profiles | Runtime objects outside Helm ownership; `hermes-profile-delete` is the only delete path; V4 checks `helm uninstall` leaves PVCs |

## Verification stages

A failed gate stops the stages after it.

### V1 -- Hermes behaviour on the pinned release (no cluster)

Confirm, and record the answers here:

- Profile directory layout and how the home/profile path is selected
  (environment variable or flag).
- Multiple skill directories supported, or symlinks honoured; how a skill's
  description is read from `SKILL.md`.
- Whether the agent can install/enable skills at runtime, and how to turn
  that off.
- Config file name, override mechanism, whether Hermes rewrites config at
  runtime.
- OpenAI-compatible API server mode: streaming, session selection header,
  concurrent requests on one profile.
- SIGTERM handling: time to flush `state.db`; `PRAGMA integrity_check`
  after kill during an active tool call.
- How to disable scheduler/cron features.

Exit: each item answered with the Hermes version; sections 2, 3, 4 and 7
adjusted if an answer differs.

### V2 -- capacity and cold start

- Node allocatable minus current stack requests -> per-agent requests/limits
  -> `K`.
- Cold start p50/p95 (pod create -> first token) with image pre-pulled,
  empty PVC and populated PVC.

Exit: `K`, resources and timeouts committed with the numbers.

### V3 -- access, isolation, credentials

- Open WebUI with two connections: a non-admin SSO user sees both the
  gateway model and `hermes-agent` in the selector; the broker receives a
  valid Keycloak bearer token on `/v1/models` and `/v1/chat/completions`.
- Direct model chat still goes through the gateway unchanged
  (`make smoke`), and never touches the broker.
- Open WebUI user A and user B each reach only their own agent (memory
  written by A not visible to B).
- Broker with a forged `X-User-Id` and no/invalid/expired token -> 401;
  token without `ai-user`/`ai-admin` -> 403; request from a non-Open-WebUI
  pod -> refused by NetworkPolicy.
- `hermes-agent` is not reachable through the public `/v1` with a PAT.
- Request to pat-service's internal listener from an agent pod, Open WebUI
  pod and through the public origin -> refused; wrong secret from the
  broker pod -> 401.
- Internal endpoint ignores caller-supplied name/lifetime; second mint
  revokes the first token.
- Broker-issued token shown as `hermes-agent` / `issued_by=hermes-broker`
  in the user's dashboard; revoking it fails the running agent's next
  inference with 401 and the next start re-mints.
- Agent pod: no ServiceAccount token, egress to anything but allowed
  targets refused; token absent from pod spec, logs and PVC.
- Inference from agent pods shows distinct per-user `fairness_id` and
  Langfuse user.
- `go test ./...` and `go vet ./...` in `pat-service`.

### V4 -- lifecycle and selection

- `K+1` users: LRU idle agent evicted, new user served; with all `K` busy,
  the next request queues then gets 503 with `Retry-After`.
- Eviction during idle keeps `state.db` intact; session resumes after
  restart.
- New user: first message returns the onboarding list; `/skills off` on an
  optional skill -> agent does not have it; on a `required` skill ->
  refused; `/skills reset` -> defaults.
- `/skills off` while the agent is running restarts it only after the
  user's other streams finish; the old and new pods never overlap on the
  same PVC.
- Catalog update: inactive user gets new version on next start with a
  "new in catalog" notice once; active user gets it after next idle;
  `FORCE=1` restarts immediately; personal skills, memories and selection
  unchanged; removed catalog skill gone; skill made `required` is on
  despite an old "off" entry.
- Broker restart with active streams: no agent evicted.
- `helm upgrade` and `helm uninstall` of `airgap-stack` leave user PVCs and
  `hermes-cred-*` Secrets.

Exit: `scripts/hermes-smoke-test` covers V3-V4 and passes twice.

## Implementation order

1. V1 against the pinned Hermes release; adjust this ADR.
2. `config/hermes/base-profile/` with a minimal catalog (one required, one
   optional skill), `catalog.yaml`, `make hermes-catalog`,
   `HERMES_IMAGE` / `HERMES_CATALOG_IMAGE` pins.
3. Init sync container (selection-aware) and a manually created agent pod
   on a test PVC.
4. pat-service: `issued_by` column, internal listener and
   `POST /internal/hermes-tokens`, dashboard label; tests.
5. `hermes-broker`: request path, credential Secrets, slots, LRU eviction,
   idle shutdown, `/skills` commands, onboarding and "new in catalog"
   notices.
6. Chart objects: namespace, RBAC, quota, network policies, internal
   listener Service, shared Secret; second Open WebUI connection in
   `k8s/base/applications.yaml`. `make verify`.
7. V2-V4; broker metrics and Grafana panels; runbook in
   `docs/operations/hermes.md`; user notes (including `/skills`) in
   `docs/clients/README.md`.
8. Status to Accepted.

Out of scope: background and scheduled agents, a web settings page for
skill selection, web dashboard front-end, messenger channel, RWX/NFS
storage, the `local-mac` profile.
