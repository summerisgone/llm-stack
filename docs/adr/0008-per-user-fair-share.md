# 0008: Per-user fair share of the 2-slot inference deployment

## Status

Proposed (draft, v4). **Superseded core decision**: the "Decision" section
below (queue and per-user/session fairness both live in `pat-service`) was
`TASK-qos-fair-share.md` v2's conclusion, driven by Stage 1's finding that
this EPP build exposed no per-flow concurrency cap and no request-scoped
priority. Stage 1B (below) found that conclusion was a **stack
configuration gap, not a build limitation**: `inferencePool.create: false`
in `config/llmd/router-nvfp4-values.yaml` meant no `InferencePool` existed
for EPP to resolve `InferenceObjective` bands against. With
`inferencePool.create: true` and the matching GAIE/llm-d CRDs installed,
this exact EPP build (commit `b1bf63da5e9a52dc8815264809d00f45f5b5e966`,
unchanged) dynamically provisions a priority band per `InferenceObjective`
and dispatches per the `x-llm-d-inference-objective` header, verified live
end-to-end. **v4's role split (queue and priority bands in llm-d;
`pat-service` only computes session state and enriches headers) replaces
v2's "everything moves to pat-service."** The Decision section's prose
below still describes the superseded v2 design and needs a full rewrite at
Stage 3; treat "Stage 1B" as the current source of truth for what EPP does
and does not do until that rewrite lands.

Stage 0 (measurement), Stage 1 (EPP capability investigation), Stage 1B
(InferencePool/priority bands enablement) and Stage 2 (prefix caching) are
complete; Stage 3 **step 1 only** (chain-hash session matching, observation
mode — computed, recorded in Valkey, exported as metrics, but making no
admission decision) is implemented and validated against live traffic.
**Not yet accepted** — Stage 3 steps 2–5 (assigning the `InferenceObjective`
header from session state, cost-based band modulation, the hard slot
ceiling if measurement shows it's needed: the actual per-request wiring
this ADR now needs to specify under the v4 split) have not started, pending
confirmation, per the task's own gate.

**The role split is closed and does not get re-opened.** `pat-service` does
authentication, PAT tokens, session keys and the headers derived from them;
queues, priority bands, fairness and dispatch are llm-d EPP's, and per-user
rate limits are Envoy AI Gateway's. It is written up as current-state, not as
a decision under review, in
[docs/architecture/README.md](../architecture/README.md) "Queueing and
fairness: who owns what" -- read that rather than the superseded Decision
prose below.

## Stage 1B: InferencePool and priority bands, revisited

Re-ran the Q1/Q2 probes from "Q1–Q4, answered against the running EPP
build" below after `TASK-qos-fair-share.md` v4 pointed at a specific,
checkable cause: `x-llm-d-inference-objective` doesn't carry a priority
directly, it names an `InferenceObjective` object that EPP resolves in its
`InferencePool`'s namespace (llm-d docs), and this stack never created an
`InferencePool` (`config/llmd/router-nvfp4-values.yaml`
`inferencePool.create: false`, `Makefile`'s own comment: "plain Envoy
Gateway Backends, so no GAIE InferencePool backend resource is registered").
`kubectl get crd | grep infer` confirmed **empty** — matching Stage 1's
original finding — but that's a missing CRD, not a missing capability.

**Version check, not guesswork**: the deployed EPP image
(`LLMD_EPP_IMAGE_MANIFEST_DIGEST`, commit `b1bf63da5e9a52dc8815264809d00f45f5b5e966`)
is 780 commits ahead of upstream tag `v0.9.0` and 143 ahead of `v0.10.0`
(`gh`/GitHub compare API, `llm-d/llm-d-router`), i.e. newer than the release
the task doc said first documented these headers — no image/chart bump
needed. Its `config/crd/kustomization.yaml` at that exact commit lists three
sources: GAIE's stable `v1-manifests.yaml` (`InferencePool`,
`inference.networking.k8s.io/v1` — pinned exactly at `GAIE_VERSION=v1.5.0`,
already sitting unused in `versions.lock.env`, per the task's own
observation that this path was planned and never finished) plus llm-d's own
`llm-d.ai/v1alpha2` `InferenceObjective` and `InferenceModelRewrite` CRDs
(not part of GAIE at all — a naming collision between two different
projects' "InferenceObjective" resources, in different API groups; only the
`llm-d.ai` one is what this EPP reads). Installed all three directly from
those sources, pinned to that commit/tag, cluster-wide (one-time bootstrap,
like `remote-wsl-device-plugin`, not part of the `llmd-qwen-test`
application release).

**Cheap check (Stage 1B.3), done, succeeded.** Set
`inferencePool.create: true` and defined three `InferenceObjective`s
(`warm`: 10, `normal`: 5, `demoted`: 1 — leaving negative priority
untouched for the future sheddable background class) in
`config/llmd/router-nvfp4-values.yaml`. `AIGatewayRoute` was **not**
touched — traffic still reaches EPP through the existing Backend. One
unexpected but harmless obstacle: flipping `inferencePool.create` changes
the EPP Deployment's pod-selector label
(`llm-d-router-standalone` → `llm-d-router-gateway`), which is immutable on
a `Deployment`; `helm upgrade` failed until the stale Deployment was deleted
first (no data loss — the EPP pod is stateless) and re-applied. After that,
the EPP pod came up in pool mode (`--pool-name llmd-qwen-test
--pool-namespace airgap-ai-stack`, replacing `--endpoint-selector`), logged
`"Provisioning priority band from control plane"` for priorities 1, 5, and
10, and its own `/metrics` showed `inference_extension_flow_control_*`
gauges for all four priorities (0, 1, 5, 10) immediately, before any test
traffic. Three direct chat-completion requests against the EPP's own proxy
port, one per objective (`x-llm-d-inference-objective: warm|normal|demoted`
+ a distinct `x-llm-d-inference-fairness-id` per request), each returned
`HTTP 200` and each showed up on `/metrics` labeled with exactly the
priority its objective declared:
`fairness_id="qos-test-warm",priority="10"`,
`...-normal",priority="5"`, `...-demoted",priority="1"`. Re-ran
`make llmd-nvfp4-smoke` (real edge → pat-service → AI Gateway → EPP → vLLM
path, not the direct EPP probe) afterward: token issuance, `/v1/models`,
and three separate `/v1/chat/completions` calls all returned `200` —
InferencePool mode does not break the existing route. (The smoke script's
own rate-limit assertion failed on an unrelated, pre-existing mismatch: the
per-user rate limit was raised from 3 to 60 req/min in the Stage 2/3-step-1
session per `CONTEXT.md`, but `scripts/pat-smoke-test`'s
`INFERENCE_SMOKE_VERIFY_RATE_LIMIT` scenario still assumes the old 3/min
ceiling trips in three quick requests; not caused by and not fixed as part
of this stage.)

**Per Stage 1B.4: the cheap check succeeded, so this stage stops here.**
`InferencePool` is now live in `config/llmd/router-nvfp4-values.yaml` as
the namespace anchor `InferenceObjective` resolves against — **it is not,
and must not become, the `AIGatewayRoute` backend**; the routing model from
ADR 0002 is unchanged, and `Makefile`'s comment about "no GAIE InferencePool
backend resource" was corrected to say so explicitly rather than read as
still-accurate.

**Revised Q1/Q2 answers** (supersedes the "Q1–Q4" section below for these
two; Q3/Q4 stand as originally answered):

- **Q1/Q2 revised**: this build **does** support a per-request priority
  band, contrary to Stage 1's original conclusion. The band is resolved via
  `x-llm-d-inference-objective` naming an `InferenceObjective` in the
  `InferencePool`'s namespace, with a static per-object `priority` (negative
  values allowed, confirmed by the CRD schema's own doc string: "requests
  with a Priority of -10 will always be served after requests with Priority
  of 0" — exactly the sheddable-background semantics
  `TASK-qos-fair-share.md` §4.1 wants reserved). This is a **config-time**
  set of bands (create/edit `InferenceObjective` objects), not a
  **request-time** arbitrary priority value — a request can only select
  among the bands that exist as objects, which is exactly the fixed
  `warm`/`normal`/`demoted` set the task asked for. Stage 1's "no,
  confirmed absent" verdict was correct for the *configuration surface as
  deployed* (`inferencePool.create: false`, no CRDs) and wrong as a
  statement about the *build's capability* — the distinction the task's own
  "не выдумывай" rule exists to catch, and which v4 caught by asking to
  re-verify against the stack config rather than the image alone.

## Stage 3, steps 1–3: header assignment and bands, validated live

`pat-service/internal/qos` now assigns a `Band` (`warm`/`normal`/`demoted`)
per request and `cmd/pat-service`'s `proxy` handler sends it as
`x-llm-d-inference-objective`, alongside a new `X-Session-Key` tracing
header (both added to `copyRequestHeaders`'s client-spoofing strip list,
same treatment as fairness id). Band logic (`Tracker.assignBand`,
`internal/qos/session.go`):

- **Demotion checked before warmth**, inverting the task doc's literal
  listing order. A session dispatched on every consecutive request (the
  ordinary shape of an agent mid-task) would always read as "recently
  dispatched" under a warm-first check, so demotion could never trigger and
  Stage 5 test 7 would fail by construction. Checking demotion first makes
  it win once triggered, regardless of warmth.
- **No `cached_tokens` correction for warm.** Dropped, not deferred: this
  vLLM build never populates `usage.prompt_tokens_details.cached_tokens`
  (already established above), so the chain match plus `WARM_TTL` is the
  only warmth signal that exists on this engine.
- New Valkey state: `qos:streak:session:<sub>` / `qos:streak:count:<sub>`
  (consecutive steps of the same session, resets when the user's session
  changes) and `qos:dispatch:<sub>:<session>` (last-dispatch timestamp, the
  `WARM_TTL` clock). Both best-effort read-then-write, not atomic
  check-and-increment — accepted for the same reason the rest of this
  package tolerates it: a race needs two truly concurrent requests from one
  user (bounded by `--max-num-seqs 2`), and band assignment is a heuristic
  fairness signal, not a correctness-critical admission decision.
- New metric `patsvc_rotations_total{user}`, incremented exactly once per
  demotion (when the streak first crosses `demoteAfterSteps`, not on every
  subsequent request while demoted).
- Config: `QOS_WARM_TTL_SECONDS` (default 120), `QOS_DEMOTE_AFTER_STEPS`
  (default 8), both read the same way every other QoS threshold is (env var
  with a default, never a literal).

**Validated live, 2026-09-07**, real PAT path (`***REMOVED***`,
throwaway Keycloak user, deployed via `make pat-deploy`): a 10-step growing
conversation produced `patsvc_session_match_total{result="new"}=1`,
`{result="matched"}=9`, `patsvc_session_steps_total{user}=10`,
`patsvc_rotations_total{user}=1` — session stitching and demotion both
exactly as designed (Stage 5 tests 5 and 7). Cross-checked against EPP's own
`/metrics` for the same `fairness_id`:
`inference_extension_flow_control_request_enqueue_duration_seconds_count`
showed `priority="5"` (normal, the first/new-session request) count 1,
`priority="10"` (warm) count 7, `priority="1"` (demoted) count 2 — summing
to exactly the 10 requests sent, with the demoted count matching the two
requests sent after the streak crossed 8. This is the doc's own required
check ("проверь, что EPP действительно раскладывает запросы по полосам —
по его метрикам, а не по факту «мы отправили заголовок»"), and it passed
end to end: pat-service's band decision and EPP's actual dispatch behavior
agree exactly.

`make llmd-nvfp4-smoke` re-run after the `pat-service` redeploy: token
issuance, `/v1/models`, and chat completions all `200`. (Its rate-limit
assertion still fails on the pre-existing, unrelated `3→60 req/min` drift
noted in Stage 1B — not caused by, and not fixed as part of, this stage.)

**Not done in this stage**: Stage 3 step 4 (cost accounting and
spend-based band modulation, §4.4) and step 5 (a hard slot ceiling, only if
measurement shows the round-robin isn't enough) remain open, along with all
of Stage 4 (Prometheus/ClickHouse/Grafana observability) and Stage 5's
formal `scripts/qos-smoke-test`.

## Stage 3, step 4: cost accounting and spend-based band modulation

Per explicit customer direction, this step is **modulation of the priority
queue only** -- no request ceiling, no rate limit, no admission blocking.
Limits, if this task ever adds them, stay Envoy AI Gateway's job (`QuotaPolicy`,
§2). `qos.Tracker.RecordCost` converts a completed request's measured
`usage.prompt_tokens`/`completion_tokens` into `cost = α·(prompt-cached) +
β·completion` (the calibrated coefficients from `tests/qos/baseline.md`
§0.7, now configurable via `QOS_COST_ALPHA_PER_TOKEN`/
`QOS_COST_BETA_PER_TOKEN` rather than hardcoded) and adds it to the user's
recent spend in Valkey (`qos:spend:<sub>`, a counter whose TTL is refreshed
on every add -- `QOS_SPEND_WINDOW_SECONDS`, default 600s -- so it decays to
zero only once a user goes fully idle, not on a fixed calendar window).
`assignBand` reads this alongside the existing streak/warmth check: a user
more than `QOS_SPEND_DEMOTE_THRESHOLD` (default 5.0 cost units, a first-cut
default -- roughly one heavy cold-prefill-and-decode request's worth of
lead, pending real spend-distribution data from Stage 4) ahead of the
lowest-spending *other currently active* user is demoted regardless of
session state. A lone active user is never demoted this way -- there is no
one to be ahead of, and it would cost work-conservingness for no fairness
benefit.

Cost can only be known after a response completes, so `RecordCost` updates
spend for **future** requests only; it never affects the request that
produced the usage figures. `cmd/pat-service`'s new `recordUsage` reads
`usage` from non-streaming chat-completion responses only (never buffers a
`text/event-stream` response -- consistent with this ADR's existing
`stream_options.include_usage` decision for the now-dropped warm/cached_tokens
correction, generalized here to cost too, since streamed responses are the
common case for coding agents and buffering one would add real latency for
a fairness signal that only needs to be roughly right). A response over
`QOS_USAGE_MAX_BODY_BYTES` (default 1MiB) is proxied unaffected, just
without a spend update -- the same degrade-don't-fail rule as the request
side.

**Validated live, 2026-09-07**, two throwaway users, real PAT path: user B
sent one minimal request (baseline ~0 spend); user A sent two
`max_tokens=250` requests (`cost ≈ 9.4e-5·56 + 0.0149·250 ≈ 3.73` each,
confirmed via the actual returned `usage` — `prompt_tokens_details` was
`null`, matching the standing finding), then a third request. EPP's own
`/metrics` for A's `fairness_id` showed the first two requests at
`priority="5"` (normal — spend hadn't been recorded yet when those headers
were set) and the third at `priority="1"` (demoted — A's ~7.46 accumulated
spend exceeded B's ~0 by more than the 5.0 threshold). B's own request
stayed at `priority="5"`, unaffected. Band assignment and cost recording
are correctly sequenced: a user's *own* expensive requests never demote
themselves in the moment, only their *next* request once the cost is
known — which is the only order physically possible, since usage isn't
available until the response completes.

Re-ran `make llmd-nvfp4-smoke` / `scripts/pat-smoke-test` afterward (rate-limit
assertion still skipped for the pre-existing, unrelated reason noted in
Stage 3 steps 1-3): token issuance, `/v1/models`, and chat completions all
`200`. `go test ./...` and `make verify` clean.

**Not done**: Stage 3 step 5 (hard per-user concurrency ceiling) remains
explicitly out of scope by customer direction for this round, and Stage 4's
full observability layer (the `patsvc_cost_units_total`,
`patsvc_virtual_time_seconds` etc. metrics this ADR's own spec lists) is
still unimplemented -- today the only visible trace of spend modulation is
the resulting `Band`/priority label, not a dedicated metric.

## Stage 4: user-activity dashboard (Prometheus/Grafana only)

Customer's framing for this round: prepare to onboard real users and collect
token consumption, TTFT and TPOT for setting future limits -- a metrics/
dashboard deliverable, not the full spec below. **Scoping decision**: this
round ships the Prometheus + Grafana half of Stage 4's spec exactly as
written; the ClickHouse `qos.requests`/`qos.sessions` event log and its
async-insert pipeline are deliberately deferred, not silently dropped --
they answer a different question (arbitrary historical drill-down by
`session_id`) than "which user needs a limit right now," which Prometheus
retention already answers well enough to act on. Revisit once real traffic
volume makes per-request analytics worth the extra moving part.

**Key finding that reshaped the plan**: `tests/qos/baseline.md` §0.3 had
already inventoried EPP's own `/metrics` and found a `fairness_id`-keyed
family this ADR hadn't yet used for anything: `llm_d_epp_request_ttft_seconds`
(split by a `streaming` label -- for a non-streaming response this equals
total duration), `llm_d_epp_request_streaming_tpot_seconds` (one sample per
streaming request, `(e2e-TTFT)/(output_tokens-1)`),
`llm_d_epp_request_streaming_itl_seconds` (one sample per inter-token gap --
the finest-grained TPOT signal available, streaming only),
`llm_d_epp_request_ntpot_seconds` (end-to-end/output_tokens, defined for
*every* request regardless of transport), and
`llm_d_epp_request_input_tokens`/`_output_tokens` (per-request token-count
histograms). All are already scraped (the `llmd-epp` job in
`helm/airgap-stack/templates/resources.yaml` predates this stage) and
`fairness_id` is confirmed to be the same Keycloak `sub` pat-service uses as
`user` everywhere else. Consequence: **token consumption, TTFT and TPOT
needed zero new pat-service instrumentation** -- building that a second time
in pat-service, especially the streaming-response body inspection and
`stream_options.include_usage` request mutation that would otherwise have
been required, would have duplicated EPP's own numbers while adding real
risk to the hot path right before onboarding real users. The dashboard reads
EPP directly for those three; pat-service's contribution is limited to what
only it can compute: the α/β-weighted GPU-cost axis (`patsvc_cost_units_total`)
and the token counters that feed it.

**pat-service changes** (`internal/qos/metrics.go`, `internal/qos/session.go`,
`cmd/pat-service/main.go`): `RecordCost` now also emits
`patsvc_prompt_tokens_total`, `patsvc_cached_prompt_tokens_total`,
`patsvc_completion_tokens_total` and `patsvc_cost_units_total` (all
`{user,model}`), and `recordUsage` emits `patsvc_requests_total{user,band,
outcome}` (`outcome`: `dispatched`/`rejected` on 429/`error` on 5xx or a
`Do` failure). `model` comes from `qos.Result.Model` (`chain.go`'s
`chatRequest.Model`, already parsed for the chain hash -- no extra body
read). These four counters are populated from non-streaming responses only,
the same limitation Stage 3 step 4 already had and documented: `recordUsage`
still never buffers a streaming body. This is a real, known gap for
streaming-heavy traffic (most coding agents), left deliberately unaddressed
this round for the reason above -- EPP's `_input_tokens`/`_output_tokens`
already cover streaming traffic for the token-consumption view; only the
GPU-cost view (`cost_units`) is incomplete for streamed requests until this
gap is closed (candidate fix: the `stream_options.include_usage` injection
this ADR's §5.2 already sketched and deferred once, now needed for cost too,
not just the dropped `cached_tokens` correction).

**Dashboard**: `config/grafana/dashboards/user-activity.json`, registered
in `Makefile`'s `monitoring-up` alongside `fair-share.json` (same
`grafana.dashboards.fair-share` folder key, so both QoS dashboards sit
together in Grafana). Panels: dispatched-request count and reject/error
rate (stat), cost units by user ranked (bar gauge, `patsvc_cost_units_total`),
input/output tokens by user ranked (bar gauge, EPP), token-consumption rate
by user (`patsvc_*_tokens_total`), TTFT p50/p95 by user split by `streaming`
(EPP), TPOT p50/p95 by user via inter-token latency (EPP,
`streaming_itl_seconds`), normalized time-per-output-token p50/p95 covering
every request regardless of transport (EPP, `ntpot_seconds`), requests by
user and outcome (pat-service), and EPP dispatch outcomes by priority band
(no per-user cardinality, a scheduler-health cross-check). Does not
duplicate `fair-share.json`'s session-stitching/warm/rotation panels.

**Validated live, 2026-09-07**: a throwaway user (`kc-user-add` /
provisioned PAT via the public-origin path, same workaround as Stage 3's
validation since `scripts/kc-pat-issue`'s Python `urllib` still fails
`TLSV1_ALERT_PROTOCOL_VERSION` against this host) sent one non-streaming and
one streaming chat completion. `patsvc_prompt_tokens_total`,
`_completion_tokens_total`, `_cost_units_total` and `_requests_total`
matched the non-streaming response's own `usage` block exactly
(58 prompt / 10 completion / cost 0.154452 / `outcome="dispatched",
band="normal"`). EPP's `/metrics` for the same `fairness_id` showed
`llm_d_epp_request_ttft_seconds{streaming="false"}` = 0.301s (equal to
`request_duration_seconds`, as documented), and after the streaming call,
`streaming="true"` TTFT = 0.303s plus 57 `streaming_itl_seconds` samples
(one per inter-token gap) -- both histograms populated exactly as expected
with no pat-service involvement. `helm upgrade` for `monitoring-up`
succeeded; the `grafana-dashboards-fair-share` ConfigMap contains both
`fair-share.json` and `user-activity.json`, and the Grafana pod's
provisioning log shows a clean `finished to provision dashboards` after
restart. `go vet`/`go test ./...` and `make verify` clean;
`scripts/pat-smoke-test` (rate-limit assertion skipped, same pre-existing
unrelated reason as Stage 3) passed end to end. Not re-run this round:
`scripts/llmd-nvfp4-smoke-test`'s own EPP-metrics port-forward step raced
and failed to connect before the underlying `pat-smoke-test` it wraps even
started -- reproduced twice, unrelated to this stage's changes (no EPP-side
code or config touched), most likely a pre-existing background-vs-curl race
in that script; not investigated further since `pat-smoke-test` run directly
already covers the same path cleanly.

## SGLang portability: routing fix for TASK-qos-fair-share.md §7.1

Customer's ask this round: switch the live engine from vLLM to SGLang,
keeping every fair-share guarantee this ADR already delivers (work-conserving
slots, warm/normal/demoted bands, per-user cost, all the Prometheus/Grafana
surface). Explicitly out of scope: llama.cpp and the external API stay on
their existing direct-bypass routes, not switched to.

**§7.1's snapshot of the problem didn't match the code, but the problem it
names was real.** The task doc describes three separate `AIGatewayRoute`
rules (`openai-sglang`, `openai-llamacpp`, `openai-external-api`), each
pointing at its own `AIServiceBackend` and bypassing llm-d. The actual
`helm/airgap-stack/templates/llmd.yaml` (as of commit `da4bab3`, "Model is
always qwen-3.8-27b") has no `openai-sglang` rule at all — SGLang shared the
single `openai-qwen38nvfp4` rule with vLLM, and that rule's `backendRefs`
branched on `.Values.inference.sglang.enabled`: `sglang-qwen38-openai`
(direct, bypassing EPP) when true, `llmd-qwen-test-openai` (through EPP)
otherwise. Different shape, identical consequence: flipping
`inference.sglang.enabled` to make SGLang live sent 100% of `qwen-3.8-27b`
traffic around llm-d's queue, priority bands and saturation detector — the
whole mechanism this ADR built. `docs/operations/inference-backends.md`
documented this bypass as the intended design (written for ADR 0006, before
this task existed), which is why it needed rewriting alongside the fix, not
just the routing objects.

**Fix**: the `openai-qwen38nvfp4` rule's `backendRefs` no longer branches —
it always targets `llmd-qwen-test-openai` (the EPP proxy), for either engine.
Which engine EPP actually dispatches to moves entirely into
`config/llmd/router-nvfp4-values.yaml`'s `router.modelServers` block
(`type`, `targetPorts`, `matchLabels`), which this round sets to SGLang:
`type: sglang`, `targetPorts: [30000]`,
`matchLabels: {app.kubernetes.io/name: sglang-qwen38}` (was
`vllm-qwen38-nvfp4:8000` / `app.kubernetes.io/name: vllm-qwen38-nvfp4`).
Two more fields move with it, both easy to miss because nothing fails loudly
if they don't:

- `core-metrics-extractor` needs an explicit
  `parameters.defaultEngine: sglang`. The upstream chart auto-derives this
  from `modelServers.type` in its own generated `default-plugins.yaml`
  (`llm-d-router-standalone/charts/router/templates/_config.yaml`), but this
  stack's `router.epp.pluginsConfigFile: fairshare-plugins.yaml` /
  `pluginsCustomConfig` replaces that generated file outright — confirmed by
  pulling and reading the chart (`LLMD_ROUTER_CHART_VERSION=v0`,
  `helm show values oci://ghcr.io/llm-d/charts/llm-d-router-standalone
  --version v0`) rather than assumed. Without it, `core-metrics-extractor`
  silently keeps parsing SGLang's `/metrics` as if it were vLLM's.
- `concurrency-detector.maxConcurrency` tracks the *active engine's*
  admission capacity, same principle as `docs/architecture/README.md`
  "Inference profile" already states for vLLM's `--max-num-seqs`. SGLang's
  equivalent is `--max-running-requests`
  (`helm/sglang-inference/values.yaml` `inference.maxRunningRequests: 3`),
  so this moves from `2` to `3`. Left at `2` under SGLang, the saturation
  detector would under-admit by one slot — not a correctness bug, but a
  silent throughput regression nobody would think to attribute to this
  change.

**Upstream auth is a new wrinkle this stack's SGLang deployment has that
vLLM's doesn't**: `.env`'s `SGLANG_API_KEY` is set (non-empty) on this
cluster, and `helm/sglang-inference`'s pod requires it
(`--api-key "$(SGLANG_API_KEY)"`). The old direct-bypass route carried this
via a `BackendSecurityPolicy` targeting the now-removed
`AIServiceBackend/sglang-qwen38-openai`. `BackendSecurityPolicy` injects its
`Authorization` header at the AI Gateway's hop into whichever
`AIServiceBackend` it targets; there is no equivalent hook further downstream
inside EPP's own dispatch to the endpoint it picks. The fix retargets that
same policy at `AIServiceBackend/llmd-qwen-test-openai` — the EPP proxy
backend — on the theory that EPP's sidecar Envoy proxy forwards the request
(headers included) to the picked pod unchanged, same as any transparent
proxy hop, so the header set at the Gateway survives through to SGLang. This
is a reasoned design choice, not a verified one: **it has not been checked
against a live request yet** (this ADR entry was written before cluster
access was authorized this round — `TASK-qos-fair-share.md`'s cluster rule:
"кластер недоступен — не выдумывай замеры, останавливайся и докладывай").
If verification finds the header does not survive the EPP hop, the fallback
is unsetting `SGLANG_API_KEY` (the pod is already unreachable from outside
the cluster, so upstream auth is defense-in-depth, not the only auth layer —
JWT at the gateway and PAT at pat-service still gate every request before it
gets this far) rather than reintroducing a bypass route. Gated on
`inference.sglang.enabled` (now repurposed to mean exactly this — "inject
the SGLang key on the shared path" — since it no longer selects a route) so
it is harmless against vLLM, which never checks `Authorization`.

**Left alone, deliberately**: the α/β cost-model coefficients
(`QOS_COST_ALPHA_PER_TOKEN`/`QOS_COST_BETA_PER_TOKEN`) were calibrated
2026-09-07 "against qwen-3.8-27b on vLLM 0.27.1"
(`pat-service/cmd/pat-service/main.go:106`). SGLang's radix-cache prefill and
decode timing differ from vLLM's block-cache numbers; §7.5 of the task doc
establishes the general principle (a cost model is tied to engine + flags,
not portable across an engine switch) for the more extreme `--cpu-moe` case,
and the same reasoning applies here in a milder form. The fairness axis keeps
working either way — cost accounting doesn't require correct calibration to
produce *a* ranking, only a well-calibrated one — but the spend-modulation
threshold (`QOS_SPEND_DEMOTE_THRESHOLD`) and the cost dashboard panels will
read numbers that no longer mean what the calibration date on them implies.
Re-running Stage 0.7's calibration procedure against SGLang is flagged as
follow-up, not done this round (out of the stated scope: "переключить
бекенд... сохранив всю функциональность" is about routing, not
recalibration, and doing it silently would change dashboard numbers a real
user might already be looking at without anyone deciding that on purpose).

Grafana's vendor SGLang dashboard (`llm-d-sglang-overview.json`) is already
wired into `monitoring-up` — this predates this stage, not new work here. The
vendor vLLM dashboard (`config/grafana/dashboards/vllm.json`) will read empty
while vLLM is scaled to zero; expected, not a regression, and out of scope to
fix per `TASK-qos-fair-share.md`'s "vendor dashboards... not to be touched."
`fair-share.json` and `user-activity.json` are unaffected either way — they
read `llm_d_epp_*` (fairness-id-keyed, engine-agnostic) and `patsvc_*`
metrics, never an engine-specific series.

**Applied and validated live, 2026-09-07.** Customer approved the cluster
change (`AskUserQuestion`: "Да, применяй сейчас"). Sequence actually run,
slightly reordered from the plan above after two real mistakes surfaced the
right order: `kubectl scale deployment/vllm-qwen38-nvfp4 --replicas=0` (the
customer did this manually, mid-session, before the agent got to it) →
`make sglang-up` (failed first: `CreateContainerConfigError`, `secret
"sglang-api-key" not found` — confirms the dependency this ADR's design
section states but the agent's own first attempt got the order wrong) →
`make helm-up` (creates the Secret + retargeted `BackendSecurityPolicy`; the
`sglang-qwen38` pod recovered on its own once the Secret existed, no restart
needed) → waited for the pod to pass its readiness probe (weight loading
~19s, then FlashInfer autotune and 42-step CUDA graph capture, ~9 more
minutes — `kubectl wait --for=condition=Ready`, not a fixed sleep) →
`make llmd-up` (EPP rolled out clean; its own logs immediately showed
`"Injected dynamic attribute into endpoint",
"endpoint":"airgap-ai-stack/sglang-qwen38-84bd47dd7-nvfjd-rank-0"` and
`"Cleaned up in-flight load for deleted endpoint"` for the old vLLM pod —
confirms the `InferencePool` selector switch took effect without anyone
touching the pool object by hand).

`scripts/pat-smoke-test` (`STACK_TUNNEL_PORT`, `STACK_BASE_URL` for the real
public origin, `INFERENCE_SMOKE_REQUIRE_LMSTUDIO=false`,
`INFERENCE_SMOKE_MODEL=qwen-3.8-27b`) run twice to completion end to end,
`sh -x`-traced both times: `POST /v1/chat/completions` → `200` (this is the
one that matters — the SGLang upstream auth question above is answered:
`SGLANG_API_KEY` injected by `BackendSecurityPolicy` on the *EPP-facing*
`AIServiceBackend` survives the EPP proxy hop and reaches SGLang correctly,
no 401, confirmed by a real response, not inferred), `GET /v1/models` →
`200`, PAT revocation → `204` then a post-revocation `GET /v1/models` →
`401` as required. Cleanup (test-user delete) ran cleanly both times.
`go vet ./...` and `go test ./...` (pat-service) and `make verify` all clean;
no pat-service code changed this round, so this is a regression check, not
new coverage.

One assertion in `scripts/pat-smoke-test` **failed on every run and is a
pre-existing bug, not something this stage introduced**: with
`INFERENCE_SMOKE_VERIFY_RATE_LIMIT=true` (the wrapper
`scripts/llmd-nvfp4-smoke-test`'s default), the fourth back-to-back chat
completion is asserted `= 429` and got `200` instead — `set -eu` then aborts
the whole script silently (a bare failed `test` inside an `if` body, no error
text). Root cause, confirmed by reading `helm/airgap-stack/values.yaml`:
`inference.perUserRateLimitPerMinute` is `60` today, raised from `3`
deliberately in an earlier stage ("Stage 3's observation-mode session
tracking needs real, unthrottled coding-agent traffic to validate against")
— the test script's own hardcoded assumption ("3 req/min... the fourth
inference call is rejected") was never updated to match. Same class of issue
CONTEXT.md has flagged repeatedly across earlier stages as a pre-existing,
unrelated drift ("rate-limit assertion still fails/skipped"); this is the
first time it was actually root-caused rather than just worked around. Fix
is out of scope here (the test script, not this stage's subject) — ran with
`INFERENCE_SMOKE_VERIFY_RATE_LIMIT=false` instead, consistent with prior
practice.

The other flakiness encountered during this validation — several
`scripts/pat-smoke-test` runs failing early with `curl: (52) Empty reply
from server` against the local `kubectl port-forward svc/keycloak` tunnel,
and one against a `--connect-to`-rewritten `*.localhost` request after
`STACK_BASE_URL` was accidentally left unset for a couple of attempts — was
entirely local tooling/tunnel instability (SSH port-forward through a
home-network link to the remote WSL2 host), reproduced and root-caused
in-session rather than assumed: confirmed the port-forward mechanism itself
was healthy in isolation (`curl` straight to a fresh `port-forward` returned
`200`) and Keycloak's own logs showed no restarts or errors across the whole
window. Same class already named in CONTEXT.md ("scripts/llmd-nvfp4-smoke-
test itself hit an unrelated port-forward race... reproduced twice").
`STACK_TUNNEL_PORT`/`STACK_BASE_URL` must both be set together for the
remote profile — dropping either one silently changes `lib-endpoints.sh`'s
routing mode instead of failing loudly, which is how two of these attempts
were actually self-inflicted rather than real flakiness; worth a
`scripts/lib-endpoints.sh` hardening follow-up (fail if `STACK_TUNNEL_PORT`
is set without `STACK_BASE_URL` or vice versa) but not done here, out of
scope for a routing-fix stage.

End state confirmed live: `vllm-qwen38-nvfp4` Deployment `0/0` (scaled down,
release still installed — not uninstalled, so `make vllm-up` alone reverts
it), `sglang-qwen38` Deployment `1/1`, `router.modelServers` in
`config/llmd/router-nvfp4-values.yaml` committed as `sglang`. This is now
the live engine, not a drill.

## Stage 1C: default priority band -- starvation found, root cause, fix applied

**Measured starvation, live Prometheus, last 24h.** One fairness_id
(`2d641ee3-9795-4e4c-ab89-38b2fef81d52`) sent 2197 of ~2683 dispatched
requests (82%). His own TTFT: p50 0.657s, p95 59.2s. Three other users whose
requests fell entirely inside his active window paid for it:
`67926262-a7e0...` p50 27.9s / p95 91.9s, `04877303-6e59...` p50 56.3s / p95
120s, `bdeffc66-023e...` p50 40.5s / p95 109s. **Aggregate TTFT p50 looked fine
(0.69s) only because the heavy user's own volume dominates the histogram** --
the single aggregate number `user-activity.json` (Stage 4) leads with is not
sufficient by itself to catch this; the per-user panels on the same
dashboard are what actually surfaced it. `llm_d_epp_flow_control_pool_saturation`
= 1 for both the "effective" and "decode" stages at the same time; SGLang KV
token usage 75%, GPU VRAM 33.5/34.2 GB -- the pool was genuinely saturated,
not idle-but-misrouted.

**Root cause, confirmed from the pinned build's source, not guessed.** Same
commit as everywhere else in this ADR, `b1bf63da5e9a52dc8815264809d00f45f5b5e966`.
`apix/config/v1alpha1/endpointpickerconfig_types.go` at that commit:
`PriorityBandConfig.FairnessPolicyRef`'s doc comment says "If omitted, the
system default (\"global-strict-fairness-policy\") is used";
`FlowControlConfig.DefaultPriorityBand *PriorityBandConfig` (json
`defaultPriorityBand`) is documented as "a template for handling traffic
with priority levels that are not explicitly configured in `PriorityBands`".
`config/llmd/router-nvfp4-values.yaml`'s `flowControl.priorityBands`
declared only `priority: 0` with `round-robin-fairness-policy` -- band 0
carries ~13 of ~2700 requests in 24h. The `warm`/`normal`/`demoted` bands
(priority 10/5/1, Stage 1B) are provisioned at runtime from
`router.inferenceObjectives`, not from `priorityBands`, so they inherit the
unset `DefaultPriorityBand` and get `global-strict-fairness-policy` instead
-- per `pkg/epp/framework/plugins/flowcontrol/fairness/README.md` at the same
commit, that policy "ignores flow boundaries and picks the absolute 'best'
request globally," the opposite of what `round-robin-fairness-policy`
("cycles through active flows one by one to guarantee no single flow can
starve others") was meant to provide.

Two independent confirmations, not one: (1) the EPP pod's own startup log
shows two distinct policy instances -- `PriorityBands:map[0:{...
FairnessPolicy:0x186e66b3f110 ...}]` vs `DefaultPriorityBand:{...
FairnessPolicy:0x186e66b3f340 ...}` -- different pointers, i.e. genuinely
different policy objects, not the same one reused; (2) live `/metrics` shows
per-flow queues exist inside the affected bands --
`inference_extension_flow_control_queue_size{fairness_id="2d641ee3-...",priority="10"}`
-- so the starvation is happening inside band 10, consistent with a fairness
policy that ignores flow boundaries there.

**When it broke, and why nobody noticed.** This is a regression with a
commit, not a config that was always wrong. `priorityBands` with
`round-robin-fairness-policy` on band 0 has been in
`config/llmd/router-nvfp4-values.yaml` since `c7b1193` (2026-09-03), and
`tests/qos/baseline.md` §0.4 *measured that policy working*: a second user's
lone request, fired one second into a four-request flood from another user,
came back in 5.58s rather than queueing behind the flood (10.4s/11.2s), and
§0.4's own reading credits "round-robin fairness across flows" for it. That
measurement predates `3f90a68` (2026-09-07 10:32, "migrated to llm-d queue,
sessions") -- confirmed by `git show 3f90a68^:tests/qos/baseline.md`, which
already contains the `userB-solo` result. `3f90a68` is the commit that added
`router.inferenceObjectives`, and with it the runtime-provisioned
warm/normal/demoted bands. So enabling per-request priority bands silently
moved every pat-service-tagged request out of the one band that had
round-robin configured and into bands that fall back to global-strict. The
fairness mechanism was verified working, then a later change moved the
traffic out from under it, and nothing failed loudly when that happened --
which is exactly why the aggregate p50 kept looking healthy for a day.

**Fix applied**: `config/llmd/router-nvfp4-values.yaml`'s embedded
`flowControl` now sets `defaultPriorityBand: {fairnessPolicyRef:
round-robin-fairness-policy, orderingPolicyRef: fcfs-ordering-policy}`
alongside the untouched `priorityBands` entry for `priority: 0`. Deployed as
`llmd-qwen-test` revision 7 (`make llmd-up`; rollback point is revision 6).
Expected effect: `warm`/`normal`/`demoted` bands dispatch
per-flow round-robin instead of globally-greedy, so a fairness_id with 10x
the request volume no longer gets proportionally more dispatch opportunities
inside its band -- it gets the same per-turn share as every other active
flow in that band, request-cost differences aside (round-robin is still
request-count-fair, not cost-fair; see §2(в) and "Round-robin itself is also
the wrong primitive," which this fix does not change).

**Correction to Q1.** The Q1 answer above tried four field names
(`flowControl.usageLimitPolicy`, `.usageLimit`, `.limits`,
`.usageLimitPolicyRef`), all four were rejected by the strict decoder, and
concluded a per-flow cap is "confirmed absent from this build's
configuration surface." Reading the same commit's
`endpointpickerconfig_types.go` this round surfaced the actual field:
`FlowControlConfig.UsageLimitPolicyPluginRef string`, json tag
`usageLimitPolicyPluginRef` -- none of the four guesses came close. **The
"confirmed absent" verdict about the configuration surface was wrong**; the
field exists and presumably would have been accepted. The verdict is
nonetheless **substantively correct**: `static-usage-limit-policy` (the
framework-injected default the Q1 probe already found unreachable,
`threshold` float, default 1.0) and `soft-reflective-ceiling-policy` gate
*priority bands* by pool saturation, not individual flows. This build still
offers no per-user concurrency cap through `flowControl`, for a different
reason than Q1 originally gave: not because the surface has no such field,
but because the field that exists configures band-level saturation gating,
not flow-level admission.

**`program-aware-fairness`, a bigger finding than the field-name miss.** The
same commit's `pkg/epp/framework/plugins/flowcontrol/fairness/program-aware/`
implements a fairness policy type `program-aware-fairness`, `strategy: las`
(least attained service). Per its README: it identifies "programs" by the
`x-llm-d-inference-fairness-id` header (the same header this stack already
sends), its unit of fairness is attained service as a weighted sum of input
and output tokens (output weighted 2x), it decays attained service in
wall-clock time by an explicit half-life so an idle program isn't penalized
indefinitely, and it reads token usage from `Response.Usage` on stream
completion. That is materially the same mechanism as the GPU-time virtual
clock with idle-clock pull-up this ADR's "Decision" section specifies
building inside `pat-service` -- and it already covers streaming responses,
which `pat-service`'s own cost accounting does not (`recordUsage`,
`pat-service/cmd/pat-service/main.go`, parses non-streaming JSON usage only
-- Stage 4 recorded this gap explicitly). If `program-aware-fairness` works
in this build, it removes most of the reason to build the pat-service
scheduler at all: EPP would already be doing token-weighted, idle-decaying,
per-flow fairness, for free, for both streaming and non-streaming traffic.

**Both claims then verified against the running binary, not left on source
reading.** The source above says what the type system allows; the two probes
below say what this deployed image actually accepts.

Probe method, refined from Stage 1's. Same idea -- a second `/app/epp`
inside the live pod, `--config-text`, never touching the live config -- with
one change: the probe runs on the *default* ports, which the live process
already holds, so it parses the config, instantiates every plugin, prints
both phase dumps, and then exits by itself on `address already in use`. No
kill step, and no way to leave an orphan behind in a pod capped at 1 CPU /
1Gi. The container is distroless (no shell, no `ls`), so the binary is
invoked directly as the exec argv.

- **(a) `defaultPriorityBand` is accepted.** No `unknown field`. The phase
  two dump reads `DefaultPriorityBand:{Priority:0
  OrderingPolicy:0x24aafb263700 FairnessPolicy:0x24aafb2634f0
  MaxBytes:1000000000 MaxRequests:5000}` against
  `PriorityBands:map[0:{Priority:0 OrderingPolicy:0x24aafb263700
  FairnessPolicy:0x24aafb2634f0 MaxBytes:1000000000 MaxRequests:32}]` --
  **the same pointer on both**, i.e. the template and band 0 now share one
  `round-robin-fairness-policy` instance. That is the exact inverse of the
  live config's dump, where the two pointers differ, and it confirms the
  root cause and the fix in a single reading rather than inferring either.
- **(b) `program-aware-fairness` is registered in this build.** Declared as
  `- type: program-aware-fairness` with `parameters: {strategy: las}` and
  referenced from `defaultPriorityBand.fairnessPolicyRef`, it instantiated
  cleanly: `Name: program-aware-fairness, Type: program-aware-fairness,
  Parameters: {"strategy":"las"}`. `--allow-experimental-plugins` was **not**
  passed, so it is not gated behind that flag. The run again ended on the
  port conflict, i.e. it got past config decode and plugin instantiation
  with no complaint. Q4's caveat -- that the startup log enumerates
  configured-plus-default plugins rather than every compiled-in type -- is
  what made the probe necessary, and the probe settles it: compiled in,
  registered, configurable.

The live EPP was unaffected by both probes: `restartCount` 0, `ready` true,
`up{job="llmd-epp"}` 1, and it kept serving (~0.098 req/s) throughout.

Still genuinely open, and not answered by either probe: whether
`program-aware-fairness` behaves well here in practice (its half-life and
its `Response.Usage` dependency are untested against SGLang's streaming
responses on this stack), and what it does to `pat-service`'s own band
assignment, which would then be modulating a policy that is already
cost-fair. Those belong to the step that adopts it, not to this one.

**Deploy and post-deploy verification.** `make llmd-up` took the release to
revision 7. The new pod's own phase two dump settles the question the whole
stage turns on: `PriorityBands:map[0:{... FairnessPolicy:0x2548a1b9a630 ...}]`
and `DefaultPriorityBand:{... FairnessPolicy:0x2548a1b9a630 ...}` -- one
pointer, where the pre-change pod had two. The runtime-provisioned bands do
materialize and dispatch under the new template
(`llm_d_epp_flow_control_requests_total` by priority: 6 in band 10, 3 in band
5, from the post-deploy smoke traffic). Pod `ready`, `restartCount` 0, and
the only error line since restart is the same benign "gRPC health check not
serving (leader election disabled)" the pre-change pod logged too.

The request path is intact end to end: `scripts/pat-smoke-test` with a real
chat call passes (PAT issued, accepted for inference, forwarded to the AI
Gateway, revocation immediate). `make llmd-nvfp4-smoke` still exits 1, on
exactly the pre-existing `INFERENCE_SMOKE_VERIFY_RATE_LIMIT` mismatch Stage
1B already recorded (script assumes the old 3/min ceiling, the limit is
60/min) -- unrelated to this change, and still not fixed here.

**Post-fix re-run of the §0.4 contention scenario**, recorded in
`tests/qos/baseline.md` ("0.4 re-run"): user B's lone request comes back in
5.755s, in the first wave rather than behind the flood, matching §0.4's
pre-regression 5.581s. Recorded with its own caveat, and the caveat matters
more than the number: the two throwaway users landed in *different* bands
(A 3 warm + 1 normal, B 1 normal), so user B was alone in its band and the
intra-band flow round-robin this stage actually changed was never exercised
on B's request; and five requests against three slots drain too fast to keep
a band-10 queue non-empty, so the scenario cannot force the cross-band
starvation that would discriminate between the old and new policy either.
Treat it as a live-contention sanity check on revision 7, not as evidence
the fix works.

**Still not re-measured**: the per-user TTFT numbers at the top of this
section, which remain the only measurement that would settle it. That needs
sustained load with two or more users sharing the warm band. Production
traffic did resume during the re-run (the heavy user was back to ~30
requests in the same window, still predominantly warm), so the comparison
should be available after the next busy period -- re-read the per-user
panels on `user-activity.json` then, and until then call this fix applied
and structurally verified, not effective. A controlled A/B (roll back to
revision 6, re-run, roll forward) was deliberately skipped: real users were
active again by that point, and two more EPP restarts to sharpen an
experiment that production data will answer on its own is not a good trade.

## Status (superseded text, kept for history)

Stage 0 (measurement), Stage 1 (EPP capability investigation) and Stage 2
(prefix caching) are complete; Stage 3 **step 1 only** (chain-hash session
matching, observation mode — computed, recorded in Valkey, exported as
metrics, but making no admission decision) is implemented and validated
against live traffic.

## Context

One GPU, one vLLM replica, `--max-num-seqs 2` — two execution slots shared by
4–6 developers, 3–4 concurrent at peak, running coding agents (Hermes Agent,
Pi Agent, OpenCode, KiloCode) through personal access tokens. Slots are
fewer than peak concurrent people, which reframes the problem: a hard
"one slot per user" ceiling is almost always self-enforcing once ≥2 people
are actually waiting; the real requirement is **fair time-ordering between
users when the ceiling binds, without wasting a slot when it doesn't**
(work-conserving). Full request path and the prior investigation are in
`TASK-qos-fair-share.md` §§1–2; this ADR records what changed after
verifying that investigation against the live cluster and the actual EPP
build, not against documentation or assumption.

### Session identity: why not a client header

v1 of this task assumed an optional `X-Session-Id` from the client. That
assumption did not survive contact with the actual client roster: **Hermes
Agent hardcodes every outbound HTTP header to its LLM provider** and cannot
send a custom one (NousResearch/hermes-agent#9398, open, unmerged); its own
`X-Hermes-Session-Id` is an *inbound* header to Hermes' own API server, never
forwarded outward. Session identity therefore has to be inferred entirely
server-side, from the one thing every agent conversation actually is: an
append-only chain of message prefixes (`TASK-qos-fair-share.md` §3.1). See
"Decision" for the chosen mechanism.

### Q1–Q4, answered against the running EPP build

Image: `ghcr.io/llm-d/llm-d-router-endpoint-picker:main`, digest in
`versions.lock.env` (`LLMD_EPP_IMAGE_MANIFEST_DIGEST`), build commit
`b1bf63da5e9a52dc8815264809d00f45f5b5e966` (from the pod's own startup log,
`"msg":"GIE build"`). All four answers are empirical: the running pod's own
`--help`/startup logs, live `/metrics`, direct probe requests against the
EPP's sidecar proxy port, and a second `/app/epp` process launched inside
the same pod (different ports, killed after each probe, never touching the
live config) whose `--config-text` was accepted or rejected by the binary's
own strict YAML decoder. No answer comes from upstream docs alone — every
one consulted was either silent or described a different project's header
convention, exactly the risk the task flagged. **Per v2 §1's explicit design
choice, the scheduler in "Decision" below does not depend on any of these
four answers** — it sits above EPP in the stack precisely so a floating
image tag can't invalidate it. They matter for a narrower question: does
EPP's own layer duplicate or waste effort the scheduler now owns, and can
EPP's role be reduced to a thin, harmless pass-through.

**Q1. Can `flowControl` cap concurrently-dispatched requests for a single
flow id, below a whole priority band?**

**No, not through this build's `EndpointPickerConfig` YAML schema.** The
running config's `flowControl` only exposes band-wide `maxRequests` (32 for
band 0) and a pool-wide `concurrency-detector` (`maxConcurrency: 2`,
matching `--max-num-seqs`) — both shared across every flow, not per-flow.
The runtime does have a distinct internal component for this shape of
problem: the startup log's "EPP config after phase two" dump shows
`FlowControlConfig{… UsageLimitPolicy: 0x21b0ff7f5dd0 …}`, a field separate
from the per-band `Registry`, and the plugin-instantiation log lists
`static-usage-limit-policy` as an auto-registered plugin type (alongside
`global-strict-fairness-policy`) even though neither appears in
`router-nvfp4-values.yaml`'s `plugins:` list — i.e. these exist in the
binary as system-default instances, the same way `max-score-picker` and
`single-profile-handler` do. I tried to reach it from config text and the
binary's own strict decoder rejected every field-name guess as
`unknown field`: `flowControl.usageLimitPolicy`, `flowControl.usageLimit`,
`flowControl.limits`, `flowControl.usageLimitPolicyRef` — all four rejected
verbatim. I did not find the right name and am not guessing further: per
the task's "не выдумывай" rule this counts as **confirmed absent** from this
build's configuration surface, not merely undocumented.

**Q2. How is a request's `priorityBand` assigned?**

**Not by any per-request header in this deployment, and not dynamically at
all.** `--help` has no priority/objective flag; `EndpointPickerConfig`
assigns bands only through the static `flowControl.priorityBands` list (one
band, `priority: 0`, live); `kubectl get crd | grep infer` returns
**nothing** — the `InferenceObjective`/`InferencePool` CRDs the wider Gateway
API Inference Extension project uses for objective-based priority aren't
installed in this cluster at all. I also tested the one header name this
codebase already defensively strips (`X-Llm-D-Inference-Objective`,
`pat-service/cmd/pat-service/main.go` `copyRequestHeaders` and its test) by
sending it directly to the EPP proxy port with three different values
(`premium`, `background`, an arbitrary string): the resulting
`llm_d_epp_request_duration_seconds{priority=…}` label stayed `"0"` every
time. So that header exists in the codebase purely as defensive stripping,
not as a live control this build honors. Every request resolves to band 0
because it's the only band that exists, confirmed both by absence of any
mechanism and by direct negative test of the one plausible header name.

**Q3. Is `X-Llm-D-Inference-Fairness-Id` really the fairness key this build
reads?**

**Yes — confirmed directly, not by re-reading `main.go`.** General upstream
docs for the wider Gateway API Inference Extension project describe
`x-gateway-inference-fairness-id` (different prefix) — the naming mismatch
the task warned about is real *between projects*. Two independent checks
agree: (1) probe requests straight to the EPP's proxy port with
`X-Llm-D-Inference-Fairness-Id: test-user-alpha` / `…-beta` produced exactly
those two strings as new `fairness_id` label values on
`llm_d_epp_request_duration_seconds` immediately after; (2) real traffic
through pat-service during the `tests/qos/baseline.md` §0.4 reproduction
showed `fairness_id` values matching the two test users' Keycloak `sub`s
exactly. What pat-service sends today is already read correctly.

Side-finding: a chunk of historical EPP traffic carries
`fairness_id="default-flow"` — the fallback when the header is absent. That
is Open WebUI's traffic, which reaches the same private AI Gateway route
without going through pat-service and never gets this header set (see
"Known limitations"). Out of this task's scope, but real: today all Open
WebUI users share one undifferentiated flow with each other.

**Q4. Is there a prefix-cache-aware ordering policy, or is ordering limited
to FCFS/round-robin?**

**Limited to FCFS.** The "Instantiated all plugins and applied system
defaults" startup log line lists every plugin type the binary knows how to
auto-register — the same enumeration that surfaced `static-usage-limit-policy`
and `global-strict-fairness-policy` as available-but-unconfigured
alternatives. No second `*-ordering-policy` type appears anywhere in that
list or in `--help`; `fcfs-ordering-policy` is the only one.
`prefix-cache-scorer` exists but scores *which endpoint* to route to among
several, moot with one vLLM replica, and has no bearing on *queue ordering
within a flow*. Confirms §2(б): scorer weights are not a lever here.

**Round-robin itself is also the wrong primitive for this workload**
(§2(в), independent of what EPP can or can't do): agent requests vary in
cost by an order of magnitude (a short tool-call reply vs. a 60k-token cold
prefill), so counting *requests* round-robin-fair, which is all EPP's
`round-robin-fairness-policy` does, systematically overweights whoever sends
expensive requests. This is the main reason the scheduler in "Decision"
uses GPU-time virtual-clock fairness instead of relying on EPP's fairness
policy at all, not just a consequence of Q1/Q2.

## Decision

### Where the logic lives, and why EPP's role shrinks

Q1 (no per-flow cap) and Q2 (no request-scoped priority) together mean
neither requirement 1 (work-conserving one-slot ceiling) nor requirements
2–3 (session-state-driven ordering) can be expressed in this EPP build's
configuration surface, and §2(в) means round-robin wouldn't be the right
fairness primitive even if they could. So **all of it — admission,
per-user fairness, and per-user session selection — moves into
`pat-service`**, above EPP in the stack, in a new `internal/qos` package.
This was in fact the design from the start (`TASK-qos-fair-share.md` §1:
"планировщик из §4 от ответов не зависит"); Q1/Q2/§2(в) confirm there was no
narrower option to give up in choosing it.

EPP is **not removed**. `X-Llm-D-Inference-Fairness-Id: sub` keeps being
sent (Q3: confirmed correct, unchanged, never becomes `session_id` — a
single fairness id per user is still correct once pat-service is the thing
deciding when that user's request is even allowed to be in flight).
`concurrency-detector.maxConcurrency` is brought in line with whatever
`--max-num-seqs` actually is at the time. EPP's role is now a second,
independent admission ceiling behind pat-service's own — redundant on
purpose: the day this deployment goes to multiple vLLM replicas, EPP's
scorers (`queue-scorer`, `kv-cache-utilization-scorer`, `prefix-cache-scorer`,
currently no-ops per §2(б) with one replica) start doing real endpoint
selection again, without pat-service having to know an endpoint exists.

### Session identity: hash chain, not a client header

Per-request `session_key`: walk the chain
`c_0 = H(model ‖ canonical(tools) ‖ canonical(messages[0]))`,
`c_i = H(c_{i-1} ‖ canonical(messages[i]))` and look up
`qos:chain:<sub>:<c_i>` in Valkey from the newest link backward; the first
hit is the deepest common prefix, i.e. the most KV actually shared, which is
exactly the ranking criterion warm/cold needs. A ring of the last ~64 links
per session (not just the latest) survives history compaction — an agent
dropping the middle of a long conversation still matches on `c_0`. Two
requests sharing a parent (branching, retries) legitimately match the same
session: they do share the KV prefix, so treating them as one session for
warmth purposes is correct, not a bug. Cost: one SHA-256 pass over a body
already bounded by the ext-proc 4Mi buffer — sub-millisecond next to
inference time.

**Implementation note (Stage 3 step 1):** `pat-service/internal/qos` gives
every `qos:chain:<sub>:<c_i>` key its own sliding TTL (`QOS_SESSION_TTL_SECONDS`,
default 1800s), refreshed on every request that touches it, rather than a
separate bounded ring structure. In practice this converges to the same
outcome as "keep the last ~64 links" for any session with normal step
cadence (links older than the TTL age out on their own), at the cost of not
bounding a single pathologically long-lived, constantly-active session's
key count. Revisit with an explicit ring only if that turns out to matter in
practice; it did not block validating session stitching (below).

**Validated against live traffic, 2026-09-07**: a real 3-step growing
conversation through the actual PAT path (not a direct EPP probe) produced
exactly one `new` outcome and two `matched` outcomes, confirmed independently
by reading the resulting Valkey keys directly — `patsvc_sessions_active`
does not grow linearly with request count, the task's own stated most likely
point of failure (`CONTEXT.md`, "QoS fair-share: Stage 2 + Stage 3 step 1").

**This is deliberately a heuristic, not a hard identifier**, and it degrades
on a client that reshapes its system prompt between steps — the ADR states
this because it changes how much to trust `patsvc_session_match_total` (see
Stage 4): a `matched` ratio that doesn't climb close to 1 on real traffic
means the heuristic itself needs revisiting before its output is used to
prioritize anything, not that the scheduler layer above it is broken.

**Verifying the prediction — superseded by a Stage 2 finding.** The plan was:
with prefix caching on, both vLLM and SGLang return
`usage.prompt_tokens_details.cached_tokens`, an exact measurement of how much
of the prompt came from cache, to correct the chain match's warmth
*prediction* against a post-hoc *measurement*. **Stage 2 found this vLLM
0.27.1 build never populates that field — it is `null` on every response,
streamed or not, despite `/metrics` showing real, substantial prefix-cache
hits** (`tests/qos/baseline.md`, "Stage 2 — prefix caching turned on"). There
is therefore no measured signal to correct the prediction against on this
engine today. The task §3.2 fork this ADR originally answered
(`stream_options.include_usage` for streamed requests vs. chain-only) is
moot: **the chain prediction stands alone for every request, streamed or
not**, not just as a starting point for streamed ones. `patsvc_session_match_total`
remains the only trust signal for the heuristic (see "Known limitations").
Revisit if a future engine/version populates `cached_tokens` correctly —
re-check on any vLLM/SGLang version bump before assuming this is still true.

### Two-level fairness

**Between users — GPU-time virtual clock, not round-robin-by-request**
(directly answers §2(в) and is why EPP's own fairness policy is not relied
upon): each user has a virtual clock that advances by their request's
measured `cost` (below) as it completes; the waiting user with the lowest
clock goes next when a slot frees. Idle users' clocks are pulled up to the
active minimum rather than the active minimum being pulled down, so idle
time cannot bank unbounded future priority — a standard deficit-round-robin
safeguard, needed here because a user who steps away for an hour must not
return with an hour of banked credit over everyone who kept working.

**Within a user — band by session state**: `warm` (dispatched within
`WARM_TTL`, default 120s, *and* last measured `cached_tokens` ratio above a
threshold), `demoted` (this session ran `N`, default 8, consecutive steps
without yielding the slot — demoted specifically to force a yield, not as a
punishment), `cold` (everything else, FIFO). On slot release, a `demoted`
session with another of the same user's sessions waiting yields the slot to
that other session and resets its own streak. This level is pure
`pat-service` bookkeeping; it has no analog in EPP config and doesn't need
one — it's a within-user decision, and EPP's fairness scope (Q1–Q3) is
between users, one flow id apiece.

**Work-conserving admission**: the effective per-user ceiling is computed
*at admission time*, not fixed. A lone user may hold both slots; the instant
any other user is waiting, that user stops receiving new slots and shrinks
to one as its own requests finish — no in-flight request is ever cancelled
to free a slot for someone else, so a waiting user's floor is "at most one
already-running generation's remaining time," bounded above by
`QUEUE_TIMEOUT`.

### Cost model: one number for both the scheduler and the dashboard

`cost = α · (prompt_tokens − cached_prompt_tokens) + β · completion_tokens`,
calibrated 2026-09-07, recorded with method and caveats in
`tests/qos/baseline.md` §0.7: **α ≈ 9.4×10⁻⁵ s/token** uncached-prefill,
**β ≈ 0.0149 s/token** decode (≈160× more expensive per token than
uncached prefill — matches the general shape of autoregressive decode being
the expensive phase). Both are properties of *this* model/engine/flag
combination and go stale the moment any of those change; §0.7 records the
exact conditions so a future recalibration knows what changed.

Rejected alternative, named explicitly because it's the obvious first
instinct: **wall-clock slot-occupancy time.** With 2 parallel slots, summing
per-user wall-clock occupancy double-counts real GPU time, and it charges a
cache-hit step and a 60k-token cold prefill the same because both can occupy
a slot for a similar duration despite wildly different actual cost — exactly
the opposite of what should drive fair sharing. `cost` is deliberately
token-based instead, and subtracts cached tokens almost to zero, so
**keeping a session's cache warm is systematically cheaper and therefore
buys a bigger share of future slots** — the incentive this design is meant
to create.

A second, independent measure — wall-clock time **fractionally attributed**
across whatever requests were actually running in each instant (two
requests sharing a second get 0.5s each) — is kept alongside `cost` for a
different purpose: it converges to true GPU-seconds-per-user over any
window, so it answers "how much of the card did this person use this week"
and cross-checks whether α/β have drifted. It does **not** feed the
scheduler; using two different cost definitions in the same decision would
make the dashboard lie about what's actually driving admission.

### State: Valkey, not in-process memory

`internal/qos` state (slot ownership, per-user queues, virtual clocks,
session chains and their step counters) lives in `envoy-ratelimit-valkey`
(already deployed), not in pat-service's process memory. Two reasons,
recorded because the second is the one that would otherwise get relitigated
later: pat-service survives a restart without losing scheduler state, and —
the real reason — **Open WebUI's traffic bypasses pat-service entirely**
(next section). The day that gap is closed, the fix is moving the same
`internal/qos` package into the private AI Gateway's ext-proc, and Valkey
means that move carries no data-model change; an in-process map would have
forced a rewrite, not a relocation.

### Prometheus vs. ClickHouse: two different jobs, not two copies

**Prometheus**: live state and rates — current slots, queue depth, p50/p95,
429 rate, future alerting. Low-cardinality labels (`user`, `band`, `class`,
`model`, `engine`, `outcome`); `session_id` must never become a Prometheus
label (unbounded cardinality, one series per agent conversation ever run).

**ClickHouse**: one row per completed request, in a **new `qos` database**
of its own — explicitly not Langfuse's tables, because Langfuse owns that
schema and changes it between versions without this task's involvement.
`session_id` lives only here. `qos.sessions` is an `AggregatingMergeTree`
materialized view over `qos.requests` keyed on `session_id`
(`argMin`/`argMax(ts)`, `count()`, token/cost sums, `max(prompt_tokens)`,
`avg(cached_ratio)`) rather than a second application-level write path, so
the aggregate can never drift out of sync with the raw rows that produced
it. Async, buffered inserts (`async_insert=1, wait_for_async_insert=0`);
ClickHouse being down degrades to `patsvc_event_log_dropped_total`
incrementing, never to a held or failed inference request — recorded here
because it is a hard invariant, not a tuning choice: no observability write
path may ever hold up a user's request.

Langfuse is unchanged and stays the place to look up *what* a specific
request contained; `qos.requests` never stores prompt/response bodies, so it
carries no retention constraint Langfuse's trace store has.

## Known limitations

- **Open WebUI traffic bypasses pat-service and this entire mechanism.**
  Open WebUI calls the private AI Gateway directly with the signed-in user's
  own Keycloak token (`docs/architecture/README.md` "Identity through the
  request"); it never reaches pat-service, so it gets no slot reservation,
  no queue entry, and no `internal/qos` accounting, and (Q3's side-finding)
  collapses into a single shared `default-flow` EPP fairness bucket with
  every other Open WebUI user. Browser chat traffic is invisible to this
  design's fair-share accounting. Named because it's real, not because it's
  in scope here: coding agents are this task's traffic; fixing Open WebUI's
  own gap is the reason state lives in Valkey rather than in-process (see
  above), so the fix, when it happens, is a relocation, not a rewrite.
- **Session identification is a heuristic and degrades on prompt-prefix
  churn.** A client that alters its system prompt between steps breaks the
  chain match at `c_0` and every session derived from it looks new. Watch
  `patsvc_session_match_total{result}` (Stage 4/5) as the trust signal for
  every other session-scoped number this design produces.
- **EPP has no native per-flow queue-depth or wait-time metric.** Everything
  flow-scoped it exposes is a post-hoc outcome histogram
  (`llm_d_epp_request_duration_seconds{fairness_id=…}`), never a live gauge
  (`tests/qos/baseline.md` §0.3's full inventory). Not a problem for this
  design since the scheduler now lives in pat-service and emits its own live
  gauges, but it does mean EPP-side dashboards can't show "queue depth by
  flow id" — only pat-service's `patsvc_queue_depth{user}` can.
- **Q1/Q2's "no" is scoped to this build** (`main` floating tag, commit
  `b1bf63da…`, chart `v0`), and Stage 1C corrected part of Q1's original
  basis: the field name it guessed for a per-flow cap does exist
  (`flowControl.usageLimitPolicyPluginRef`), the four names Q1 tried just
  missed it. The "no per-flow cap" verdict itself still stands, now for the
  reason Stage 1C gives -- `static-usage-limit-policy` and
  `soft-reflective-ceiling-policy` gate priority bands by pool saturation,
  not individual flows -- not because the config surface has no relevant
  field. Stage 1C also found `program-aware-fairness` (per-flow,
  token-weighted, idle-decaying) in this commit's source **and confirmed by
  in-pod probe that it is registered and configurable in the running binary,
  without `--allow-experimental-plugins`**. Re-run the Q1 `--config-text`
  probes whenever `LLMD_EPP_IMAGE_MANIFEST_DIGEST` moves, this time against
  the right field name -- and note that this build already exposes
  program-aware fairness, which per this bullet's own logic is a reason to
  *simplify* this design (push some of it back down to EPP config), never a
  reason the current one is wrong; nothing here depends on EPP staying
  limited.
- **Valkey is a new hard dependency for inference availability** that wasn't
  one before: today `envoy-ratelimit-valkey` backs only the rate limiter,
  whose failure mode is "no rate limiting," not "no inference." Once
  `internal/qos` state lives there too, Valkey being down has to have a
  defined, deliberately chosen failure mode (fail open to unmanaged
  best-effort dispatch, or fail closed to 503) — decide and record this
  explicitly. **Decided for Stage 3 step 1**: fails open — a Valkey error
  degrades that request's session match
  (`patsvc_session_match_total{result="degraded"}`) and nothing else, since
  step 1 makes no admission decision to fail closed on. This does not yet
  answer the question for steps 2-4, where a slot admission decision
  genuinely depends on Valkey state and fail-open/fail-closed stop being
  equivalent in effect — that decision is still open, for whichever step
  implements admission.
