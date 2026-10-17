# 0025: Hosted and remote inference APIs as a provider catalog, routed by model name next to the EPP pool

## Status

Proposed on 2026-10-17. Chart wiring implemented
(`helm/airgap-stack/templates/inference-api.yaml`, `inference.api` in
`helm/airgap-stack/values.yaml`), off by default: no provider is enabled
and no model is listed. Rendered and checked against the Envoy AI Gateway
v1.1.0 and Envoy Gateway v1.8.1 CRD schemas (the versions in
`versions.lock.env`); nothing has run on a cluster yet. The gates under
"Verification" are open.

Revised the same day after a review of obleth's model, endpoint and
fairshare design (obleth.com/docs: deployments, inference gateways,
reliability, pool sizes, fairshare engine). obleth makes the same split as
this stack: the gateway decides who sends and when, and the inference
gateway behind it decides which replica serves. Its one admission queue
covers every model, hosted or self-hosted. From that review: alternative D
became stage 2 (the next step), session affinity and a header allowlist in
pat-service were added to it, and fallback to a different model was ruled
out (decision 2). Stage 2 is not implemented.

Extends [ADR 0006](0006-pluggable-inference-backends.md) (model-name rules
on the private AI Gateway) from one hand-written block per backend to a
values-driven catalog, and leaves the EPP pool of
[ADR 0019](0019-inference-plane-gpu-worker-nodes.md) and the queue of
[ADR 0008](0008-per-user-fair-share.md) / [ADR 0012](0012-graduated-band-ceiling-over-strict-priority.md)
unchanged.

## Context

- Every backend that is not the pool (llama.cpp, `externalApi`, ninfer,
  Strata) is a copy-pasted Backend/AIServiceBackend/Secret/policy block in
  `inference-backends.yaml` plus a rule in `llmd.yaml`. There is exactly one
  generic slot, `externalApi`, and it is in use (Strata on the GPU host).
  Adding OpenAI, Anthropic, Gemini or another site's cluster means editing
  two templates, and every external API would also need TLS, which no
  existing block sets.
- `AIGatewayRoute` holds at most 15 rules, one reserved for the
  controller; `llmd` has 5 today.
- The throughput machinery is llm-d EPP: flow control (admission on engine
  KV/queue saturation, per-user round-robin fairness, the `warm`/`normal`/
  `demoted` bands) and load/prefix-aware endpoint scoring. All of it works on
  pods in an `InferencePool` and on their `/metrics`. A hosted API has
  neither: there is nothing to score and no saturation signal except the
  provider's own 429s.
- pat-service forwards identity headers to the gateway for EPP fairness and
  Langfuse (`X-User-Id`, `X-User-Name`, `X-Pat-Token-Id`, `X-Session-Key`,
  `X-Llm-D-Inference-*`), and passes through what Open WebUI forwards
  (`X-OpenWebUI-User-Email`, `-User-Role`, `-Chat-Id`, ...). The gateway's
  inbound `Authorization` is pat-service's own Keycloak token. Inside the
  cluster that is fine; sent to a third party, every one of them is a leak.
- pat-service exposes only `/v1/*`, and its ledger reads OpenAI-shaped
  `usage`. Per the Envoy AI Gateway v1.1.0 endpoint table,
  `/v1/chat/completions` translates to OpenAI, Azure OpenAI, AWS Bedrock,
  GCP Vertex AI and GCP Anthropic; the native `Anthropic` schema serves only
  `/anthropic/v1/messages`.

## Decision

1. **A provider catalog in values.** `inference.api.providers.<name>`
   is one upstream API: `schema` (+ `prefix`/`version`), `host`/`port`,
   `tls`, `auth` (`none`, `APIKey`, `AnthropicAPIKey`, `AzureAPIKey`) and
   `apiKeyEnv`, the `.env` key holding the secret. Each enabled provider
   renders `Backend`, `AIServiceBackend` and, with auth, a Secret
   `api-<name>-key` and a `BackendSecurityPolicy`, all named `api-<name>`.
   `tls: true` verifies against the system CAs with SNI = host. The cloud
   IAM auth types (`AWSCredentials`, `GCPCredentials`,
   `AzureCredentials`) are not wired; adding one is a branch in the
   template, not a new design.
2. **Models are separate from providers.** `inference.api.models[]` maps
   a public model name to one or more targets `{provider, model, priority,
   weight}`. `model` becomes `modelNameOverride`, so clients use stable
   names (`claude-sonnet`) while the upstream name changes. `priority`
   gives failover (0 first), `weight` splits load within one priority
   (two accounts, two regions). A target on a disabled provider is
   dropped, a model with no target left is not routed, so a provider is
   switched off by one flag.

   **Every target of a model serves the same model.** `model` may differ
   between targets only because providers name the same model differently
   (`claude-sonnet-4-5` on Anthropic, `claude-sonnet` on another site).
   Failover never swaps in a different model: a client that asked for
   `gpt-5` must not silently get `gpt-5-mini` or a local Qwen, which would
   change quality, tool and JSON-mode behaviour, context window and price
   in a way the client cannot see. Switching models is the client's
   choice, made by asking for another public name. obleth draws the same
   line (failover only between endpoints of one model). The chart cannot
   check this, since names differ across providers; it is a review rule
   for `inference.api.models`.
3. **Own routes, never inside `llmd`.** Models render as exact-match rules
   on `AIGatewayRoute/inference-api-<n>`, 14 per route, split
   automatically. They carry no catch-all and never point at the EPP
   backend, so the pool, `default` and the catch-all keep their meaning.
   This is the shape `embeddings.yaml` already uses (ADR 0013): a second
   route whose header-matching rules outrank `llmd`'s match-less catch-all.
   A public name that is already served in-cluster (`qwen-3.8-27b`,
   `default`, any enabled local backend's name) fails the render instead of
   racing the other route.
4. **Same entry boundary.** `SecurityPolicy/inference-api-jwt` admits only
   pat-service's client-credentials token, like `llmd-jwt`: every API call
   goes through pat-service's PATs, quotas and ledger
   ([ADR 0022](0022-pat-service-admin-console.md) section 5). The
   NetworkPolicy on the gateway pod is unchanged.
5. **Nothing identifying leaves.** Each provider's `AIServiceBackend`
   removes `inference.api.privateHeaders` and, unless `auth: APIKey`
   overwrites it, `Authorization`. This is a denylist and therefore the
   second layer only: a header a client starts sending tomorrow passes it.
   The first layer, an allowlist in pat-service, is stage 2c.
6. **Resilience and budget on the route** (`BackendTrafficPolicy/inference-api`):
   the same 4Mi request buffer and per-user request limit as `llmd`;
   retries on connect failure, reset and 429/5xx with jittered backoff and
   `numAttemptsPerPriority: 1`, so a retry moves to the next priority;
   passive health (outlier ejection) so a failing provider's traffic shifts
   without waiting for retries; optional `circuitBreaker` as a
   per-provider concurrency cap with an Envoy-side queue; optional
   `tokensPerUserPerHour`, a per-user total-token budget charged from the
   response's usage (`llmRequestCosts` on the routes).
7. **Native Anthropic schemas are refused** at render time with a pointer
   to the provider's OpenAI-compatible endpoint, since no client path
   through pat-service reaches `/anthropic/v1/messages`.

The existing `externalApi`, `llamacpp`, `ninfer` and `strata` blocks are
not migrated in this change; `externalApi` can become a catalog entry once
the catalog has run.

## What replaces EPP for API traffic

EPP stays where it works: the in-cluster pool. For API targets, each EPP
function maps to something else, or to nothing:

| EPP function (pool) | API equivalent in this decision | Gap |
| --- | --- | --- |
| Admission on KV/queue saturation (`utilization-detector`) | Provider 429 + retry/backoff; optional `circuitBreaker` cap and queue | Envoy's queue is FIFO and blind to users; closed by stage 2a |
| Per-user fairness inside a band | Per-user request rate and token budget | A limit, not fair queueing: one user can still fill the cap; closed by stage 2a |
| Priority bands (`warm`/`normal`/`demoted`) | None | Become weights in stage 2a |
| Load-aware pick (queue, KV scorers) | `weight` + outlier ejection | No load signal from the provider |
| Prefix-cache affinity (`prefix-cache-scorer`) | Provider-side prompt caching (OpenAI automatic, Anthropic `cache_control`) | Weight-splitting a model across accounts splits its cache too; closed by stage 2b |
| Failover | `priority` targets + retries, same model only | -- |

## Stage 2: admission, affinity and headers in pat-service (next step)

Not implemented. Modeled on obleth's fairshare engine and endpoint
selection, adapted to what pat-service already has (session tracker,
bands, ledger, Valkey). The model list, pool sizes and targets come from
`inference.api`: the chart renders them into a ConfigMap
(`pat-service-api-models`, chart-owned per ADR 0002) that the pat-service
Deployment in `k8s/base` mounts as optional, so a stack without API models
behaves exactly as today.

### 2a. Fair admission queue for API models

- **A pool per public API model**, `maxInFlight` slots (per model, default
  from `inference.api`), sized to what the provider account sustains, not
  to replicas. A slot is held from admission until the response stream
  ends, so one slot covers every gateway retry and failover attempt of
  that request.
- **Only API models.** Requests for the EPP pool, `default` and the other
  in-cluster names are not queued in pat-service: EPP is their admission.
  Queueing them twice would hide saturation from EPP and add a second wait
  before the one that has the engine metrics. This is obleth's split too
  (it sizes its pool to ready replicas and leaves replica choice to the
  inference gateway).
- **Order by served tokens over weight.** Each user keeps a served
  counter in tokens (prompt + completion, from the ledger's usage), and
  the next free slot goes to the waiting user with the lowest
  served/weight. Shares are ceilings, not reservations: an idle slot is
  lent to whoever waits, nothing is preempted, and a user returning after
  a quiet spell has the counter raised to the pool's current minimum so
  idle time is not banked as credit.
- **Bands become weights,** not strict priority, in line with ADR 0012's
  graduated ceiling: `warm`/`normal`/`demoted` from the session tracker
  multiply the user's weight (starting point 4/2/1, to be measured).
  Weights only matter when the pool is full.
- **Bounded wait.** A request that waits longer than the admission timeout
  gets 503 with `Retry-After`; a request whose client disconnected while
  queued is dropped uncharged. The per-user token budget stays a hard 429
  before the queue.
- **Replicas.** One pat-service replica today: slots are in memory. With
  more, slot counts move to Valkey with a heartbeat per replica so a
  crashed replica's slots expire; if Valkey is unreachable each replica
  takes `ceil(maxInFlight / replicas)`, never unbounded admission.
- **Visible.** Queue depth, wait time and slots in use per model and per
  user on pat-service's `/metrics`, and in the admin console next to the
  EPP flow-control panels.

This replaces `circuitBreaker` as the main concurrency control for API
models; the circuit breaker stays as a safety net set above the pool.

### 2b. Session affinity across a model's targets

When a model's priority-0 targets are split by `weight` (several accounts
or regions), each provider's prompt cache sees only its share of a
session. obleth's `session_hash` mode keeps a conversation on one endpoint
by hashing its session id. Here:

- The chart renders, for each such model, one extra rule per weighted
  target, matching `x-ai-eg-model` **and** `X-Api-Target: <provider>`.
  The pinned target is priority 0 there and the model's other targets
  follow at priority 1, so failover still works. Two header matches
  outrank the model's plain rule (the same Gateway API precedence as
  decision 3).
- pat-service, which already derives the session key (`X-Session-Key`,
  ADR 0011), hashes it over the weighted targets and sets
  `X-Api-Target`. No session key: no header, plain weighted rule.
- `X-Api-Target` joins `privateHeaders`.
- Within one provider, pat-service may also set that provider's own
  cache-routing hint from the same hash where one exists (OpenAI's
  `prompt_cache_key`), as it already rewrites bodies for
  `stream_options.include_usage`.

Models with a single priority-0 target need neither.

### 2c. Header allowlist in pat-service

pat-service's `copyRequestHeaders` drops a fixed list today and passes
everything else from the client to the gateway (and so to a provider).
It becomes an allowlist for every forwarded request: `Content-Type`,
`Accept` and the few client headers the OpenAI API defines (`OpenAI-Beta`),
nothing else. pat-service then sets its own headers as now. The gateway's
`privateHeaders` denylist stays as the second layer, for the headers
pat-service sets itself. Before the list is fixed, the header names
clients actually send are recorded with `QOS_LOG_CLIENT_SHAPE=true`
(pat-service logs header names, not values), across Open WebUI, the
agents and a PAT client.

## Alternatives for API traffic

Ordered by cost. A is what this ADR implements.

- **A. Gateway-native knobs (implemented).** Everything above, no new
  component. Gives failover, budget and isolation; does not give fair
  queueing or bands.
- **B. EPP at the remote end.** When the "API" is another site running
  this stack (own GPUs, own llm-d), point a provider at that site's `/v1`
  with a service PAT. Its EPP schedules on real engine metrics. The cost
  is that the remote sees one tenant (the service PAT), because identity is
  stripped here; carrying a hashed fairness id across is possible but a
  separate, explicit decision. This is the recommended path for any
  self-hosted capacity outside this cluster.
- **C. EPP in front of an API through shim pods.** An `InferencePool` of
  N small proxy pods, each one "slot" against a provider or an account,
  with request-count saturation (`concurrency-detector`, which this stack
  used before ADR 0012) instead of engine metrics. That would bring flow
  control, round-robin fairness and the priority bands to API traffic,
  and the approximate prefix scorer could keep a session on one account's
  cache. Unknowns: whether EPP accepts pods without vLLM/SGLang metrics, and
  N extra pods to keep alive. An experiment, not a plan.
- **D. Fair queue in pat-service (adopted as stage 2).** Brings fairness
  and bands to API models in code this repo owns, without touching EPP.
  obleth documents a complete version of this design, which settles the
  open questions (unit of fairness, behaviour on a full pool, replicas). Chosen
  over C because it needs no extra pods and no EPP behaviour that has not
  been probed.
- **E. LiteLLM (or similar) as the API tier behind the gateway.** Brings
  RPM/TPM-aware routing across keys, fallbacks and an exact or semantic
  response cache. Duplicates what Envoy AI Gateway and pat-service already
  do (keys, accounting, limits), adds a hop and a second place where keys
  live, and some of its admin features are enterprise-licensed. Not
  adopted.
- **F. Response caching.** Not in Envoy AI Gateway. Low hit rate for
  agent traffic. If it is ever wanted, obleth's shape is the one to copy:
  exact match only, keyed by user + model + body so no answer crosses
  users, short TTL, `200` responses only. It would sit in pat-service.
  Not planned.
- **G. Per-user provider keys (BYOK).** `BackendSecurityPolicy.credentialOverride.fromRequestHeaders`
  takes the upstream credential per request from a trusted header; with
  [ADR 0024](0024-per-user-secrets-and-credential-gateway.md) the key
  could come from OpenBao through pat-service. Out of scope here; the
  catalog does not block it.

## Consequences

- Turning a provider on sends prompts outside the company. The air-gapped
  profile simply leaves `inference.api` empty. The gateway's Envoy pods
  (`envoy-gateway-system`) need egress to the provider's host on 443; a site
  that only allows egress through an HTTP proxy is not covered.
- Keys come from `.env` into Secrets, like every other runtime secret. An
  enabled provider with an empty key fails the render.
- Open WebUI shows a model only if it is listed in
  `OPENAI_API_CONFIGS` `model_ids`
  (`k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml`); adding
  an API model means editing that list in the same change.
- pat-service prices a model it has no entry for at `_default`
  (`pat-service-pricing`, `k8s/base/applications.yaml`). Each API model
  needs its price there.
- The admin console's route view (`pat-service/cmd/pat-service/topology.go`)
  already walks every `AIGatewayRoute` and shows an FQDN outside the
  cluster as `external`.
- Rule names are `model-<index>`; reordering `models` renames rules, which
  only changes Envoy cluster names in metrics.

## Verification

Open, in this order, on a cluster with one provider and one model enabled:

1. `GET /v1/models` through pat-service lists the API model; a request for
   it reaches the provider, and `qwen-3.8-27b`/`default`/an unknown name
   still reach EPP (precedence over the `llmd` catch-all).
2. Point a provider at a header-echo endpoint: none of `privateHeaders`
   and no Keycloak `Authorization` arrive; with `auth: APIKey` the
   provider key does.
3. TLS from the gateway pod to the public host with
   `wellKnownCACertificates: System`.
4. Streaming and non-streaming requests produce a pat-service ledger row
   with token counts.
5. A 503 from the priority-0 target is retried on the priority-1 target;
   ejection after `consecutive5XxErrors` moves traffic without retries.
6. With `tokensPerUserPerHour` set, the counter is charged from the
   response (metadata namespace `io.envoy.ai_gateway`, key
   `llm_total_token`, as rendered) and the next request past the budget
   gets 429.

Stage 2, once built:

7. Two users on one API model with `maxInFlight` 2: a user sending 10
   concurrent requests does not delay the other's single request beyond
   one slot release; a `warm` session overtakes `normal` only while the
   pool is full.
8. A request queued past the admission timeout gets 503 with
   `Retry-After`; a disconnected queued request leaves no ledger charge.
9. Requests for `qwen-3.8-27b` never wait in pat-service's queue.
10. With two weighted targets, requests of one session hit one provider;
    with that provider failing, they fail over to the other.
11. A client header outside the allowlist does not reach a header-echo
    provider.
