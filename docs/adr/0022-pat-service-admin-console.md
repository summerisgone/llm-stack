# 0022: PAT service administration for users, quotas and inference

## Status

Proposed on 2026-10-03. This is an implementation plan; the administration
console, enforced quotas and deployment worker described below do not exist
yet. The current-state findings come from repository code, not a live
cluster inspection.

Extends [ADR 0003](0003-private-ai-gateway-and-pats.md) with an administrative
surface and a private Keycloak-authenticated inference entry point. Extends
[ADR 0008](0008-per-user-fair-share.md) with durable token quotas before
scheduling; EPP retains queueing, fairness and saturation decisions.
Amends the runtime replica configuration of
[ADR 0007](0007-inference-engines-as-helm-releases.md) and
[ADR 0019](0019-inference-plane-gpu-worker-nodes.md) once managed switching
is enabled. Helm release ownership and routing by model remain unchanged.

## Context

- `pat-service/web/src/main.jsx` is a React self-service dashboard. Its Go
  backend already authenticates through `pat-dashboard` OIDC and saves
  Keycloak realm roles in a signed session cookie. `ai-user` and `ai-admin`
  can sign in; there is no separate admin authorization middleware.
- `pat-service/cmd/pat-service/usage.go` reads `qos_events` in `pat-db` for
  daily tokens, cost and sessions. `QOS_MONTHLY_LIMIT` is a display-only
  monetary limit, not an enforced per-user token budget. Event insertion
  currently happens after the response and can fail without failing the call.
- Sessions are chain-hash inference conversations, not Keycloak login
  sessions ([ADR 0011](0011-pat-session-key-langfuse-tracing.md)). Existing
  session duration is the span between event timestamps. Request start,
  first output and last output timestamps are not persisted.
- PAT clients and agents pass through PAT service. Direct Open WebUI chat
  reaches the private AI Gateway with the user's Keycloak token and bypasses
  PAT accounting. Its EPP fallback band is 0, below `demoted` (1).
- vLLM, SGLang and ninfer are engines, not model names. vLLM and SGLang
  can serve the same model pool simultaneously. ninfer has a separate model
  name and direct route, outside EPP. A single "current engine" flag would
  misrepresent this topology.
- Engine releases are currently applied by Make targets, with replica
  counts from `.env`. An independent UI that patches Deployments would
  conflict with Helm and the next deployment.
- Node and GPU exporters feed the application Prometheus; Envoy metrics
  live in the gateway add-ons Prometheus. Grafana and Langfuse remain
  operator services without a public route.

## Decision

### 1. One console inside PAT service, available to `ai-admin`

Add `${URL_PREFIX}/admin` to the existing dashboard (`/platform/admin` on
the remote profile) and `${URL_PREFIX}/api/admin/*` for its API. Internal Go
routes remain prefix-free, following the existing edge rewrite. The local
profile uses its existing PAT origin. No additional public host is needed.

`ai-admin` in the configured Keycloak realm is the application administrator
role. A Keycloak management role, username `admin`, or role from another
realm does not implicitly grant access. Assign the existing `ai-admin` role
to the intended accounts in Keycloak. Do not maintain a second role database
or add password management to PAT service.

- The backend authorizes every admin read and write. Anonymous API calls
  receive 401; authenticated non-admins receive 403. Hiding navigation is
  only presentation. Ordinary users retain their own tokens and usage pages.
- Browser administration uses the OIDC session, never a PAT, including a PAT
  owned by an administrator. Add a session/capabilities endpoint so the UI
  can decide whether to show "Administration".
- Verify OIDC signature, issuer, intended client/audience and expiry; review
  the existing JWT validator as part of this boundary. Continue PKCE and
  secure, HttpOnly session cookies. Mutations require CSRF protection and
  a same-origin check, use POST/PUT/DELETE, and never execute on GET.
- Check current account enabled state and effective `ai-admin` membership
  through a dedicated, read-only Keycloak directory client. Admin reads may
  cache this result for at most 60 seconds; mutations and execution of a
  queued deployment operation require a fresh check. On Keycloak failure,
  deny the privileged action with 503 rather than trust indefinitely stale
  roles. A role removal prevents new changes; an already executing operation
  may finish or recover safely and remains attributed to its initiator.
- The directory client is scoped to the application realm and user/role
  reads; it cannot grant roles, reset passwords or administer Keycloak.
  Its credentials stay in a Kubernetes Secret, never in the browser.

#### Mandatory audit for every mutating action

Every administrative action that can change state must produce durable audit
events, whether initiated from the UI, its API or the managed operator CLI.
This includes quota/default/override creation, updates and removal, inference
profile switches, replica changes, operation cancellation, retries, rollback
and reconciliation. New mutating endpoints must adopt the same audit wrapper
before they can be enabled. Ordinary application logs are not the audit log.

Store append-only events in `pat-db` with these fields:

- Event ID and server-generated UTC timestamp; request ID, operation ID and
  parent operation ID where applicable.
- Initiator `(issuer, sub)` and a display-name snapshot; executing service
  identity for asynchronous work, without replacing the human initiator.
- Action, target type/ID, source (`ui`, `api`, `cli`, `worker`), reason and
  expected/applied configuration revision.
- Sanitized before/after values or a structured diff. Distinguish requested
  changes from observed applied state for external operations.
- Outcome (`requested`, `denied`, `started`, `succeeded`, `failed`,
  `cancelled`, `rolled_back`, `manual_intervention_required`) and a bounded,
  sanitized error code/description when relevant.

Record rejected mutation attempts as well, including authorization/CSRF
failures, validation failures and revision conflicts. An unauthenticated
attempt has an unknown actor; never trust a user-supplied identity for audit
attribution. Do not record tokens, cookies, credentials, raw request bodies,
prompt/answer content or secret-bearing Helm values.

For local database changes, commit the mutation and its audit event in the
same transaction. For Helm/external changes, durably record the authorized
intent and queued operation together before any side effect, then append
execution and outcome events. If the initial audit write fails, return 503
and make no change. If audit storage fails after an external action begins,
do not report success or start another mutation: retain the pending operation
and reconcile its actual state and missing outcome when storage recovers.
The worker must recover this sequence after a crash without duplicate changes
or fabricated success records; retries share correlation IDs but have their
own attempt events.

Expose a read-only "Audit log" screen and paginated admin API, filterable by
time, actor, action, target, outcome and operation ID, with before/after detail
and links to operation progress. Application database roles may insert/read
audit events but cannot update/delete them. No console/API action can erase
or rewrite history, including for `ai-admin`. Retention cleanup uses a separate
maintenance role under the operator-managed retention policy below; include
the audit log in backups and restore checks.

### 2. Screens and user management

| Screen | Required content and actions |
| --- | --- |
| Overview | Requests and tokens in the selected period, active users, quota rejections, available model pools, unhealthy nodes, load and temperature summary |
| Users | Searchable, paginated Keycloak user directory, including users with no requests; enabled state, roles, last inference activity, tokens, cost, effective limits and remaining budget |
| User details | Usage timeline, prompt/cached/output tokens, requests and outcomes, session count and list, usage time, TTFT, tok/s, TPOT; filter by time, model and source; edit token quota rules |
| Inference | Model/checkpoint and revision, engine and version, desired/ready replicas, pool membership, serving state, node/GPU placement; preview and request a supported configuration switch |
| Nodes | CPU/GPU node topology, engine pods, GPU allocation and tensor-parallel groups, model cache readiness, health and hardware metrics |
| Envoy routes | Read-only graph and table of listeners, routes, policies, model matches, rewrites, backends, EPP and endpoints; reconciliation status and failures |
| Operations | Configuration changes, progress, validation failures and recovery results, linked to audit events |
| Audit log | Read-only journal of every mutating action and rejected attempt; actor, time, target, before/after values and outcome, with filters and operation links |

Key users by `(issuer, sub)`, never by mutable username or email. Join the
directory to local usage without losing history for renamed or deleted
accounts; display deleted users as historical subjects. Existing rows can
be backfilled with the deployment's configured issuer. A recreated account
with a new subject does not inherit the previous account's quota.

User management in this ADR means usage and quota administration. Account
creation, deletion, passwords and role assignment remain in Keycloak. Cost
is informational at the configured model prices and currency; it is separate
from token quotas and EPP scheduler cost units.

### 3. Token quotas use explicit rolling windows

An administrator adds one or more rules: a duration and a non-negative
integer token ceiling. The duration input accepts `1ч`, `8ч`, `5д`, with
presets for all three; `1h`, `8h`, `5d` are equivalent. Store validated integer
seconds: 3600, 28800 and 432000. A day is exactly 24 hours, independent of
timezone and daylight saving. Reject zero/negative durations, fractions,
ambiguous strings and overflow; initially allow 1 hour through 90 days.

| Example input | Meaning |
| --- | --- |
| `1ч` / 100000 tokens | At most 100000 accounted and reserved tokens in the rolling last hour |
| `8ч` / 500000 tokens | An additional ceiling over the rolling last eight hours |
| `5д` / 2000000 tokens | An additional ceiling over the rolling last 120 hours |

These numbers illustrate the UI, not deployment defaults. All enabled rules
apply simultaneously. Use a platform default policy with explicit per-user
overrides, each keyed by window duration; an override replaces the default
for that duration and other defaults still apply. Removing an override
restores inheritance. An explicit unlimited override disables that window;
zero blocks token-consuming requests. Initially no limits are configured.

Count `prompt_tokens + completion_tokens`, including cached input exactly
once (cached tokens are a subset of prompt tokens). Embeddings consume their
input tokens; multiple choices and reasoning output must be included in the
model adapter's accounting contract. Rules apply across all models, PATs,
agent keys and interactive requests of the same user, including admins.
MCP calls have their existing call-rate policy; underlying model inference
consumes the relevant user's or service account's token budget once.

The UI shows the effective policy, inheritance, consumed/reserved/remaining
tokens, percentage, and the next expected release of budget for every
window. There is no midnight reset for a rolling window. Lowering a limit
below current consumption blocks new requests immediately; existing admitted
requests finish. Editing a policy uses optimistic version checks, returns
409 for stale edits, and writes its audit record in the same transaction.
Admission and policy edits share a serialization protocol, including changes
to inherited defaults; a stale cached policy cannot admit a new request after
an edit commits. Persist the applied policy revision with each reservation.

### 4. Enforce quotas before inference, with durable reservations

Use Postgres `pat-db` as the authoritative quota store, sharing identity and
request accounting with PAT service. Prometheus, Langfuse and the existing
short-lived QoS Valkey keys are not quota ledgers.

For each request, atomically lock the user's quota state, read the effective
policy and recent charges, and reserve an upper bound on its token usage
against every applicable window. Committed charges count in `(now-window,
now]` by server-side finalization time. All in-flight reservations count
regardless of age, so a long request cannot outlive its reservation and gain
free capacity. Use database time consistently across replicas.

The reservation includes prompt tokens after the actual model chat template
plus the enforced maximum output for every choice. Use a pinned tokenizer
and template or a verified conservative bound from the model profile. Apply
a documented server output cap when the client omits it. Do not silently
reduce a requested output cap to fit the remaining budget. Unsupported
multimodal/tokenization cases cannot claim hard-quota support: reject a
limited request before dispatch until its adapter supplies a safe bound.

This is a capability gate per model/profile, not an assumption that the
current response-only usage parser already provides admission control.

- A successful final usage record atomically replaces the reservation with
  actual usage and returns the unused budget. Finalization has a unique
  request/attempt ID and is idempotent across restarts and repeated delivery.
- Admission failures consume no tokens. After dispatch, cancellation,
  partial SSE and missing usage are not free: retain the reservation until
  reconciliation; if actual usage cannot be recovered, finalize the upper
  bound as an explicitly estimated charge. A reservation lease expiring
  never means zero usage. Do not persist prompts just to recover accounting.
- Persist pending admission before sending upstream. Recover abandoned
  reservations after a crash, with a bounded reconciliation deadline and
  audited corrections. If finalization storage fails, the durable reservation
  still protects the budget. A post-response best-effort insert is insufficient.
- Retries that execute inference again need separate reservations and
  charges. Disable opaque gateway retries on quota-controlled generation
  unless every attempt can be bounded and accounted for. Repeated client
  requests are new consumption unless an explicit idempotency contract applies.
- A rejected reservation returns an OpenAI-compatible 429 with error code
  `quota_exceeded`, violated windows and available budget. Supply a conservative
  `Retry-After` only when budget expiry makes it calculable; identify requests
  whose bound exceeds the full limit, for which waiting alone will not help.
- If policy/admission storage is unavailable, return 503 before dispatch.
  Optional telemetry failures do not block inference. Quota admission does
  not add a second scheduling queue or alter EPP's four priority bands.

### 5. Close the Open WebUI accounting bypass

Platform-wide per-user limits cannot ship while direct chat bypasses the
ledger. Add a private inference entry point in PAT service for validated
Keycloak user access tokens. Keep public `/v1` PAT-only per ADR 0003; do not
enable browser-cookie inference or publicly route the private entry point.

```text
PAT clients / agent keys -> public PAT /v1 -----------+
                                                    +-> shared identity,
Open WebUI -> private PAT entry point, Keycloak JWT --+   quota reservation,
                                                        usage capture
                                                        -> private AI Gateway
                                                        -> EPP or direct backend
                                                        -> model engine
```

Both entry points call the same admission/accounting implementation. Derive
the owner only from verified credentials, strip supplied identity/priority
headers, and set the gateway headers server-side. Configure token audience
and caller/network restrictions explicitly for the private entry point.
Reuse the established OIDC token-forwarding pattern; never substitute a
shared PAT for all WebUI users.

Restrict AI Gateway/engine inference ingress so normal clients and WebUI
cannot retain a parallel bypass; preserve required controller, EPP, metrics
and readiness traffic. Inventory embeddings, internal service calls and
external backends before enabling enforcement. Shared service PAT usage is
attributed to that service account, not falsely to an end user. Agent-broker
interaction stays outside this proxy; its model calls already use agent PATs.

Routing direct WebUI chat through this path also gives it PAT-derived
session identity and warm/normal/demoted bands. This is an intentional
change from fallback band 0, requiring fair-share regression tests. Preserve
the EPP namespace anchor and the actual backend topology from ADR 0002;
the `InferencePool` does not become the `AIGatewayRoute` backend.

During migration, label all usage views with coverage (`PAT only` or
`all configured inference paths`) and cutover time. Historical WebUI usage
cannot be reconstructed from PAT rows. Enable global quotas only after
every configured inference path passes the bypass tests.

### 6. Define statistics before adding charts

Extend the request ledger with server timestamps (received, dispatched,
first output, last output, finished), outcome, source, model/profile revision,
request/attempt IDs and usage quality (`actual`, `estimated`, `unknown`).
Keep session and token references and optional trace correlation. Record
upstream engine/pod only when reliably observed; never guess the selected
engine from a mixed pool. No prompt/answer content is required.

| Metric | Definition |
| --- | --- |
| Token use | Prompt, cached subset, output and total; actual and estimated charges separately, reservations separately |
| Utilization | Request count, active requests, errors/cancellations/quota rejections, budget used %, and activity over time; not a claim of per-user GPU utilization |
| Sessions | Distinct nonempty server-derived inference session IDs with activity in the selected interval; show requests without a session separately, not as one shared session |
| Usage time | Union of the user's request intervals clipped to the selected period, avoiding double-counting concurrency; separately show summed request duration and session wall-clock span |
| TTFT | Time from request receipt at PAT service to first meaningful output delta (text, reasoning or tool-call output); excludes role-only/empty SSE events, includes waiting and network time |
| tok/s | Per-request output tokens divided by receipt-to-last-output time, labelled end-to-end output rate; aggregate as sum(tokens)/sum(eligible durations), not average of averages |
| TPOT | Observed `(last_output - first_output)/(output_tokens - 1)` for complete streams with more than one output token; a proxy-level estimate, not engine-native per-token latency |

SSE events are not tokens. A chunk may contain multiple tokens; buffered
streams make the observed TPOT approximate. Zero-duration, one-token,
non-streaming, cancelled or usage-missing responses have null TTFT/TPOT
where they cannot be measured, with the reason and sample count. Keep engine
native TTFT/TPOT histograms as separate, explicitly scoped metrics. Do not
assign a pool-wide histogram to an individual user.

Show p50/p95 for valid per-request TTFT and TPOT samples, with counts and
coverage; never average percentiles across nodes. Time filters, UTC storage
and the displayed timezone must be explicit. Old rows retain tokens/cost but
unknown timing; do not backfill artificial zeros. Existing chain sessions
remain approximate conversations with their documented TTL semantics.

Initially retain detailed request data for 180 days, daily usage summaries
and admin audit for 365 days, configurable by the operator. Never purge
unsettled reservations or data needed for the longest active quota window
(up to 90 days). The UI reports the available history. Use indexed queries
and bounded pagination; keep long analytics queries away from admission
transactions. This requires a purge/aggregation job, not just UI filters.

### 7. Inference inventory and controlled switching

Show served model name, checkpoint/hash, quantization, engine/version, Helm
release, desired versus observed configuration, ready endpoints and node/GPU
placement. Support several active models and several engines per pool.
Include unrecognized engines as read-only inventory; the first managed
adapters cover vLLM, SGLang and ninfer.

Switching selects an operator-approved profile from a versioned catalog:
model identity, engine chart and pinned image, compatible GPUs, memory and
tensor-parallel requirements, replica counts, tokenizer/usage capabilities,
and existing route/model name. Models and hashes come from `models/`; image
versions come from `versions.lock.env`. No arbitrary image, URL, shell
command, launch flags, YAML or remote model download is accepted from the UI.

Changing engine within a pool and activating a different served model are
distinct operations. Keep the pool's public model identity stable when
switching vLLM/SGLang. Switching to ninfer shows its separate model name and
lack of EPP fairness; it cannot silently replace the shared pool route.
Only predeclared profiles/routes can be activated. Adding a new route or
model profile remains a reviewed repository configuration change.

Use an asynchronous deployment worker separate from the PAT HTTP process.
PAT service persists typed operations; the worker consumes them and invokes
the same pinned Helm charts and deployment sequence used by the CLI.
The HTTP process gets read-only topology access and no general deployment
credentials. The worker is trusted infrastructure with permissions required
for the named engine releases, their Helm metadata and dependencies; audit
those permissions explicitly, since Helm access is broader than a scale-only
permission. No browser-facing Kubernetes proxy or pod exec is provided.

#### One authority for runtime configuration

In managed mode, persist a versioned `inference_desired_state` in Postgres:
approved profile IDs and replica counts only. The worker is the sole normal
writer of engine releases. `.env` initializes this state once; it must not
silently overwrite subsequent admin changes. Chart values continue owning
the full workload definition; approved runtime overrides supply only the
selected profile and capacity.

Refactor `make engines-up`, individual engine up/down targets and the engine
step of `make stack-up` to use this same operation API and revision lock in
managed mode. Existing unmanaged installations keep today's Make behavior
until explicitly migrated. The operator CLI uses an authenticated Keycloak
admin session and the same authorization checks; a PAT does not authorize
deployment. Unattended deployment credentials are a separate future decision.
Export desired state for backup and recovery.
An operator recovery path must fence the worker first, run the same validated
Helm workflow, import the resulting revision and then release the fence.
Unexpected release drift blocks new switches pending reconciliation.

This explicitly changes `.env` from the ongoing replica authority to a
bootstrap source in managed mode. Do not implement independent Deployment
patching or a UI-only replica store that the next `stack-up` would undo.
Helm remains the single owner of its Kubernetes objects (ADR 0002).

#### Operation lifecycle

1. **Plan:** validate catalog revision, GPU placement and capacity, model
   cache/hash, routes, quota support and current Helm revisions. Show the
   before/after diff, affected models/users, expected interruption and
   recovery target. The admin confirms this concrete plan.
2. **Serialize:** accept an idempotency key and expected revision; lock the
   affected pool and shared GPU allocation. Conflicting operations return
   409. The initial implementation may use one cluster-wide operation lock.
   Under the lock, revalidate the confirmed plan, permissions and observed
   resources before mutation; a changed plan requires a fresh preview.
3. **Drain:** stop admission to affected models, let active requests finish
   to a deadline, and expose progress. Return retryable 503 to new requests.
   Forced termination is an explicit operation option with accounting for
   partial calls; other pools keep serving.
4. **Apply:** on one GPU, stop the old engine and confirm resource release
   before starting the new one. Include GPU embeddings in capacity checks.
   With spare independent GPUs, validate a new endpoint before retiring the
   old one. Respect tensor-parallel groups rather than counting pods as GPUs.
5. **Verify:** wait for rollout readiness, model identity, inference probe,
   usage contract and EPP endpoint/metrics health where applicable. Only then
   reopen admission and mark the new revision active.
6. **Recover:** on failure, restore the previous profile through Helm in
   GPU-safe order and verify it. Expose `failed`, `rolled_back` or
   `manual_intervention_required` accurately. A failed rollback keeps the
   affected model unavailable; it must not appear successful.

Persist every phase so worker restart resumes/reconciles observed state
instead of launching a duplicate switch. Cancel only before mutation or
through the same recovery workflow. Probes use a dedicated service identity,
with their consumption visible as operational usage.

### 8. Nodes, Envoy structure and hardware summary

Build a read-only topology from Kubernetes Nodes, workloads, Services,
EndpointSlices, Gateway API/AI Gateway resources and EPP discovery/status.
Show CPU/control-plane versus inference nodes, labels/taints relevant to
placement, pool -> engine pod -> node -> GPU relationships, replica health
and tensor-parallel groups. Unknown placement stays unknown.

The Envoy view follows the actual resource references:

```text
edge listener -> HTTPRoute -> PAT service / Open WebUI
PAT service -> private AI Gateway -> model-matching AIGatewayRoute
  -> AIServiceBackend / Backend -> llm-d routing and EPP -> engine endpoints
  -> direct backend -> ninfer or another separately named model
```

Display path/host/model matches, rewrite and timeout settings, authentication
and rate policies, accepted/resolved conditions, observed generations, and
endpoint readiness. Distinguish Kubernetes desired configuration, controller
acceptance and observed data-plane health; do not label CRDs as proof that
Envoy loaded them. If effective configuration is needed, use a private
collector that reads only fixed diagnostic endpoints and returns a sanitized
projection of listeners/routes/clusters and their versions. Its absence is
shown as "effective configuration unavailable". Never expose Envoy's admin
port, raw secret-bearing dumps, arbitrary URLs or route mutation in the UI.

Backend adapters query the two existing Prometheus sources with fixed,
bounded queries. The browser receives typed results, not credentials or a
general-purpose PromQL proxy.

| Hardware signal | Source and presentation |
| --- | --- |
| Load average | node-exporter `node_load1`, `node_load5`, `node_load15`, alongside CPU count and normalized load; do not present load as CPU utilization |
| CPU, RAM, disk | Existing node-exporter counters/gauges, rates and capacity; node table plus cluster totals where aggregation is meaningful |
| CPU/board temperature | Available hwmon/thermal-zone sensors with sensor labels, in Celsius; unavailable sensors display N/A |
| GPU utilization, VRAM, temperature and power | Current `gpu-exporter` (`gpu_temperature_celsius` for temperature); a DCGM adapter on nodes using that exporter per ADR 0019 |

Deduplicate by physical host/GPU identity where k3d nodes share hardware.
Show the hottest sensor with its host/GPU, rather than hiding hot hardware
behind an average temperature. WSL2 metrics describe the available Linux/GPU
view; missing Windows host CPU sensors are N/A, not zero. Record observation
time and mark values stale after two expected scrape intervals. Missing
Prometheus/exporters degrades these panels without blocking quota management.

### 9. Data, API and repository boundaries

Planned durable entities in `pat-db`: versioned quota policies and overrides,
request attempts/usage and reservations, admin audit events, inference desired
state and operations. Extend or migrate `qos_events` into the request ledger;
do not keep two independent billable event stores. Database migrations are
additive first, with a tested recovery procedure before enabling enforcement.

Planned admin API groups: users and usage, quota policy preview/update,
inference inventory and plan/execute/status, node metrics, route topology and
audit. All list/time-range endpoints have pagination and bounded ranges;
responses include source timestamps, coverage and partial-error status.

| Area | Implementation owner |
| --- | --- |
| UI, admin authorization, quota API, shared admission/accounting | `pat-service/web/`, `pat-service/cmd/pat-service/`, new internal packages as needed |
| User directory/OIDC provisioning | `scripts/provision-pat-oidc`, realm configuration and component Secrets |
| Deployment worker and reusable engine apply workflow | New worker component and shared deployment scripts; Make targets delegate to it in managed mode |
| Approved profile catalog | New configuration under `config/`; references existing engine chart values, `models/` and `versions.lock.env` |
| Application workloads and component RBAC | `k8s/base`, rendered into `helm/airgap-stack`; no hand edits to generated copies |
| Engine workloads | `helm/vllm-inference`, `helm/sglang-inference`, `helm/ninfer-inference` |
| Routes and site integration | Remote `helm/airgap-stack/templates`; local `k8s/overlays/local-mac`; GPU prerequisites/site patches remain with the remote overlay |
| Node/engine telemetry | Existing exporters and Prometheus configuration; adapters reuse their sources |

## Delivery plan and acceptance gates

Each stage is independently reviewable. Read-only screens may ship before
write operations; their status must not imply that quotas or switches work.

Every stage enabling mutations has an audit acceptance gate: successful,
denied, invalid and conflicting requests are attributable; an unavailable
audit store prevents new changes; transaction rollback leaves no successful
change event; worker crashes/retries preserve intent and reconcile outcomes;
secrets are redacted; administrators cannot edit/delete audit records. Verify
the journal filters, operation correlation and backup/restore coverage.

| Stage | Deliverable | Acceptance |
| --- | --- | --- |
| 1. Identity and admin shell | Admin middleware, capability endpoint, directory, navigation and audit foundation | `ai-admin` succeeds; ordinary/no-role/foreign-realm/expired sessions and all PATs fail admin access; role removal and disabled accounts take effect within the stated bound; CSRF and direct URL/API access tested |
| 2. Usage and topology | User details, timing ledger, node/route views and metric adapters | Synthetic streams prove token/time formulas; old/partial data is labelled; users with no activity appear; multi-node/mixed-engine fixtures render correctly; no secrets in API results |
| 3. Shared accounting path | Private JWT entry point, WebUI migration, network restrictions and coverage marker | Same subject in PAT, WebUI and agent traffic resolves to one ledger; embeddings/direct backends included; forged headers and direct gateway/engine bypass fail; session/fair-share regression passes |
| 4. Enforced quotas | Duration editor, default/override policies, atomic reservations, reconciliation, retention | `1ч`, `8ч`, `5д` parse correctly; simultaneous windows, inheritance, zero/unlimited and edits work; parallel PAT replicas cannot overspend the validated bound; cancellations, missing usage, retries, crash recovery and DB failure preserve reservations; cleanup preserves quota history |
| 5. Managed switching | Catalog, worker, desired-state migration, CLI integration, plan confirmation and operation UI | vLLM/SGLang/ninfer scenarios, one-GPU exclusion, mixed pools, spare-node rollout, embeddings budget, worker restart, duplicate/conflicting requests, stale plans and failed rollback tested; `stack-up` preserves admin-selected state |
| 6. Operational rollout | Runbooks, backups/restore, recovery fencing, EN/RU handbook updates | Restore retains policies, open reservations and deployment revisions; air-gap bundle includes worker/UI/catalog dependencies; operators can diagnose unavailable sensors and blocked operations |

Place boundary tests under `tests/auth`, `tests/qos`, `tests/inference` and
`tests/telemetry`, documenting how to run them. Use integration tests with
Postgres and fake clocks for concurrency/window boundaries and fake engine
streams for accounting; use an inference simulator before real GPU switches.
Run `cd pat-service && go test ./...`, the frontend build and relevant smoke
tests. Routing changes require both deployment profiles and
`make pat-smoke` / `make llmd-nvfp4-smoke`; endpoints come from
`scripts/lib-endpoints.sh`. Manifest changes require `make verify`, explicit
engine chart templates and client dry-runs. A real one-GPU switch and recovery
is a release gate for write controls, not replaced by a read-only mock.

## Consequences and alternatives

- PAT service becomes the user accounting boundary for interactive inference
  as well as PAT clients. Durable admission adds a database dependency and
  token-bound validation overhead; benchmark both before enforcement rollout.
- Reservations can temporarily deny a request that would have produced fewer
  tokens. This is the deliberate price of a bounded hard quota. Display-only
  limits or post-response checks are simpler but allow concurrent overspend.
- A separate privileged worker and one runtime configuration authority add
  operational work, but keep normal PAT HTTP handling away from deployment
  credentials and prevent Helm/CLI/UI configuration drift.
- Reusing Grafana alone would not provide quota administration or controlled
  switching. It remains the detailed operations tool; the console provides
  focused summaries without publicly exposing it or Langfuse.
- Per-user statistics use the request ledger. Prometheus remains appropriate
  for infrastructure and aggregate engine behavior, with existing label
  cardinality monitored rather than adding session/request labels.
- Building a second Kubernetes dashboard or a Keycloak admin clone is outside
  this decision. Route editing, arbitrary model installation and account
  lifecycle administration remain operator workflows.

## Implementation questions to resolve at the relevant stage

1. Validate tokenizer/template and output-bound support for each approved
   model, including reasoning, multi-choice and any multimodal input. Block
   that profile's hard-quota gate until the bound is demonstrated.
2. Verify minimal Keycloak directory permissions and effective-role reads
   for the pinned deployment, including composite roles and disabled users.
3. Measure request-ledger contention and retention volume; add rollups or
   partitioning when measurements justify them, without changing quota semantics.
4. Validate the worker's exact Helm/Kubernetes permission set and fencing
   mechanism, and the private Envoy diagnostic collector against the pinned
   releases. Administrative switching stays disabled until this is tested.
