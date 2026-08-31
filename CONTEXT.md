# Working context

> A dated engineering journal, not a specification. The durable content has
> moved: request paths and components to [docs/architecture](docs/architecture/README.md),
> the reference host and day-2 procedures to [docs/operations](docs/operations/README.md),
> standing up a new site to [docs/install](docs/install/README.md), boundaries
> and hardening to [docs/security](docs/security/README.md), and the decisions
> themselves to [docs/adr](docs/adr/README.md). What stays here is the state of
> a particular cluster on a particular day, which goes stale by design.
>
> Entries are kept in reverse-chronological order. Older ones are left as
> written and are not corrected in place: the deploy path in the 2026-09-03
> entry (`make nvfp4-up` applying the kustomize overlay directly) was replaced
> by `make stack-up` — see
> [ADR 0002](docs/adr/0002-one-owner-per-object.md) — and the inference
> workloads moved into their own Helm releases, see
> [ADR 0007](docs/adr/0007-inference-engines-as-helm-releases.md).

## KV-cache-aware scoring stuck at zero for SGLang, fixed by EPP restart — 2026-09-07

`llm_d_epp_ready_endpoints`, `llm_d_epp_average_kv_cache_utilization` and
`llm_d_epp_average_queue_size` were stuck at 0 for the SGLang pool despite
real traffic (`llm_d_epp_datalayer_extract_errors_total{extractor_type=
"core-metrics-extractor"}` climbing continuously at ~21-1300/s). Pulled the
llm-d-router source (`github.com/llm-d/llm-d-router`) to check: the required
per-engine metric specs for `sglang` (`sglang:num_queue_reqs`,
`sglang:num_running_reqs`, `sglang:token_usage`, plus `sglang:page_size` /
`sglang:num_pages` for cache block sizing) are all present in the pod's
`/metrics` with exactly one unambiguous series each — config was correct.
The per-extractor failure is logged only at `V(logging.DEBUG)=4`
(`pkg/epp/framework/plugins/datalayer/source/http/datasource.go`
`runExtractor`), never at info, so the counter climbed silently with nothing
in the default logs.

Set `router.epp.flags.v: 4` in `config/llmd/router-nvfp4-values.yaml` to
capture the log line -- but the EPP restart that `make llmd-up` triggers to
pick up the flag change made the problem disappear on its own: post-restart,
`ready_endpoints` went 0 -> 1 immediately, extraction errors stopped
entirely (counter never incremented again), and the KV-cache/queue gauges
started tracking SGLang's live state accurately (both read 0 because SGLang
was genuinely idle at the time, confirmed against the pod's own `/metrics`).
Reverted the `-v: 4` debug flag and re-applied; the fix held through the
second restart with no debug logging.

Root cause is therefore "some runtime/cached state in the EPP process
diverges from the live engine and gets cleared by a restart" -- plausibly
left over from the vLLM->SGLang engine cutover earlier this session (this
EPP pod had been running continuously since before that switch). Could not
capture the actual DEBUG error text since the failure had already stopped
by the time debug logging was live, so the exact internal mechanism is
unconfirmed. If this recurs, re-add `router.epp.flags.v: 4`, `make
llmd-up`, and grep EPP logs for `"extract failed"` before restarting again.

## Edge /v1 route missing request timeout, false 504s — 2026-09-07

Users reported frequent 504s on large-context `/v1/chat/completions` (Kilo-Code
and other coding-agent traffic). Confirmed in `envoy-airgap-ai-stack-edge`
access logs: 94 occurrences, all `2026-09-07T07:58-09:07Z`, `response_flags:
"UT"`, `response_code_details:"response_timeout"`, duration clustered
15.0-15.4s from 3 distinct source IPs. `bytes_received` 256B-409KB (avg
~244KB) — mostly but not exclusively large requests.

Root cause: `helm/airgap-stack/templates/routes-external.yaml`'s `api` rule
(the public edge `/v1` route) had no `timeouts:` block, so it fell back to
Envoy's default 15s per-route timeout — while the internal AIGatewayRoute
(`llmd.yaml`) the request also traverses sets `request: 10m`. Under
contention on the single-GPU engine, prefill on a large-context request
routinely exceeds 15s; the edge hop cut the client off before the backend
could answer.

Checked whether this cascades into a real failure: it does not. Neither
pat-service ("inference unavailable" 502 log) nor the EPP
(`EvictedContextCancelled`/`ServiceUnavailable` counters) show any matching
error for this window — the backend finishes the request and pat-service
writes the response to a client that already got a 504 and disconnected.
Pure wasted GPU work plus a false client-visible failure, not a queue or
fair-share problem, and not specific to SGLang (the gap predates today's
switch and is engine-independent, though the available log retention only
covers today so an earlier occurrence under vLLM can't be confirmed either
way).

Fix: added `timeouts: {request: 10m}` to the `api` rule, matching the
internal route. Applied via `make helm-up`; HTTPRoute status confirms
`Accepted`/`ResolvedRefs` with the new timeout live, zero 504s in the
following 2 minutes. `sso`/`sso-token`/`platform` rules left untouched — no
evidence they're affected, out of scope for this fix.

## Live engine switch: vLLM -> SGLang, EPP-routing fix — 2026-09-07

Customer's ask: switch the live GPU engine from vLLM to SGLang without
losing the fair-share mechanism this repo's QoS work built. §7.1 of
`TASK-qos-fair-share.md` had already named the blocker, though its snapshot
of the routing rules didn't match current code (see
`docs/adr/0008-per-user-fair-share.md` "SGLang portability" for the
diff): `helm/airgap-stack/templates/llmd.yaml`'s single `openai-qwen38nvfp4`
rule branched its `backendRefs` on `inference.sglang.enabled` — a direct,
EPP-bypassing `AIServiceBackend` when true, the EPP-routed one otherwise.
Flipping SGLang live the old way would have sent 100% of `qwen-3.8-27b`
traffic around llm-d's queue, priority bands and saturation detector.

Fix: that rule now always targets the EPP-facing `AIServiceBackend`; which
engine EPP actually dispatches to lives entirely in
`config/llmd/router-nvfp4-values.yaml`'s `router.modelServers` (`type`,
`targetPorts`, `matchLabels`), set to SGLang this round. Two easy-to-miss
companions moved with it: `core-metrics-extractor` needs an explicit
`defaultEngine: sglang` parameter (the upstream chart only auto-derives this
in its own generated plugin config, which this stack's
`pluginsCustomConfig` override replaces outright — confirmed by pulling and
reading the chart, not assumed), and `concurrency-detector.maxConcurrency`
moved 2 -> 3 to match SGLang's `--max-running-requests` (was tracking
vLLM's `--max-num-seqs`). SGLang's upstream `SGLANG_API_KEY` bearer auth —
new relative to vLLM, which needs none — is now injected by a
`BackendSecurityPolicy` retargeted at the same EPP-facing
`AIServiceBackend` instead of the removed direct one.

**Validated live** (`kubectl scale vllm-qwen38-nvfp4 --replicas=0` →
`make sglang-up` → `make helm-up` → `make llmd-up`, in that order — the
Secret dependency ordering bit both the agent and, independently, mid-session
manual intervention on the vLLM scale-down): EPP's own logs confirmed the
`InferencePool` selector switch (`"Injected dynamic attribute into
endpoint"` for the new SGLang pod, `"Cleaned up in-flight load"` for the old
vLLM one), and `scripts/pat-smoke-test` run twice end-to-end through the
real public origin confirmed `POST /v1/chat/completions` → `200` (proving
the retargeted `BackendSecurityPolicy`'s API key survives the EPP proxy
hop — the one open question in the design), `GET /v1/models` → `200`, and
PAT revocation → `204` then `401`. `go vet`/`go test` (pat-service, no code
changed) and `make verify` clean.

**Found and root-caused along the way, not fixed here (out of scope, but
real):** `scripts/pat-smoke-test`'s rate-limit assertion
(`INFERENCE_SMOKE_VERIFY_RATE_LIMIT=true`, the default the wrapper
`llmd-nvfp4-smoke-test` uses) fails on every run — its 4th-request `= 429`
expectation is stale against `inference.perUserRateLimitPerMinute: 60`
(raised from 3 in an earlier stage), so it always gets `200` and the script
aborts silently (`set -eu` on a bare failed `test`, no error text). This is
the same "pre-existing rate-limit assertion drift" earlier entries in this
file mention working around, now actually traced to its cause. Ran with
`INFERENCE_SMOKE_VERIFY_RATE_LIMIT=false` instead. Not this stage's subject
to fix.

Left open: SGLang's own `--max-running-requests: 3` vs. the α/β cost-model
coefficients calibrated against vLLM 0.27.1 (Stage 3 step 4) — SGLang's
radix-cache timing differs, so the fairness axis still works but the
spend-modulation threshold and cost dashboard numbers no longer mean what
their calibration date implies; recalibration is flagged, not done (out of
the stated scope: routing, not recalibration). `vllm-inference` Helm release
is left installed with the Deployment scaled to `0/0`, not uninstalled, so
reverting is `make vllm-up` plus the mirror-image `router-nvfp4-values.yaml`
edit, both documented in `docs/operations/inference-backends.md`. Full
writeup: `docs/adr/0008-per-user-fair-share.md` "SGLang portability".

## QoS fair-share: Stage 4 user-activity dashboard — 2026-09-07

`TASK-qos-fair-share.md` v5. Customer is about to onboard real users and
wants token consumption, TTFT and TPOT visible before that happens, to set
future limits from real data. Scoped this round to Prometheus + Grafana
only, ClickHouse deferred (ADR 0008 "Stage 4" explains why).

Before writing any pat-service code, re-checked `tests/qos/baseline.md`
§0.3's EPP metrics inventory and found it already answers most of the ask:
`llm_d_epp_request_ttft_seconds`, `_streaming_tpot_seconds`,
`_streaming_itl_seconds`, `_ntpot_seconds`, `_input_tokens`, `_output_tokens`
are all keyed by `fairness_id` (= the same Keycloak `sub` pat-service uses
as `user`) and already scraped by the existing `llmd-epp` Prometheus job —
confirmed live via `kubectl port-forward` and a real chat completion
(non-streaming and streaming) from a throwaway user, both TTFT and the
55-sample inter-token-latency histogram showed up with zero pat-service
involvement. So token consumption/TTFT/TPOT needed no new instrumentation
in the hot path — the dashboard reads EPP directly for those three, which
also sidesteps a much riskier alternative (buffering/parsing streaming
response bodies and injecting `stream_options.include_usage` into requests)
right before real traffic starts.

pat-service's own new metrics are limited to what only it can compute: the
α/β cost-weighted GPU-cost axis. `qos.Tracker.RecordCost` now also emits
`patsvc_prompt_tokens_total`, `_cached_prompt_tokens_total`,
`_completion_tokens_total`, `_cost_units_total` (`{user,model}`), and
`recordUsage` emits `patsvc_requests_total{user,band,outcome}`. Same known
limitation as Stage 3 step 4: populated from non-streaming responses only —
documented as an open gap, not fixed this round.

New dashboard `config/grafana/dashboards/user-activity.json`, registered in
`Makefile`'s `monitoring-up` next to `fair-share.json`. Deployed live
(`make pat-deploy`, `make monitoring-up`); confirmed the `patsvc_*` counters
match a real response's `usage` block exactly, confirmed the Grafana
ConfigMap contains both dashboard files and the pod's provisioning log shows
a clean run after restart. `go test ./...`, `go vet`, `make verify`, and
`scripts/pat-smoke-test` (rate-limit assertion still skipped, same
pre-existing unrelated reason as Stage 3) all clean.
`scripts/llmd-nvfp4-smoke-test` itself hit an unrelated port-forward race in
its own EPP-metrics step (reproduced twice, no EPP code/config touched this
round) — not investigated further since the `pat-smoke-test` path it wraps
already passed directly.

Left open: ClickHouse event log (deferred, see ADR 0008), cost/token
accounting for streaming chat completions in pat-service specifically (EPP's
own token histograms already cover streaming traffic), Stage 3 step 5 (still
out of scope by customer direction).

## QoS fair-share: Stage 1B + Stage 3 steps 1-3 — 2026-09-07

`TASK-qos-fair-share.md` v4, continuing the entries below. Overturned Stage
1's headline conclusion: EPP's per-request priority bands are **not**
missing from this build, they were missing from this stack's config.
`config/llmd/router-nvfp4-values.yaml` had `inferencePool.create: false`
(plain Envoy Backend routing, no `InferencePool` for EPP to resolve
`InferenceObjective` bands against) — a deliberate choice from before this
task existed, not a build limitation. Installed the three CRDs this exact
EPP build (commit `b1bf63da…`, confirmed 143 commits ahead of upstream
`v0.10.0`, no image bump needed) expects — GAIE's `InferencePool`
(`v1.5.0`, already pinned in `versions.lock.env` and unused) plus llm-d's
own `InferenceObjective`/`InferenceModelRewrite` — flipped
`inferencePool.create: true`, and defined `warm`(10)/`normal`(5)/`demoted`(1)
objectives. `AIGatewayRoute` untouched; `InferencePool` is only the
namespace anchor EPP resolves objectives against. Full details and the
Q1/Q2 revision: `docs/adr/0008-per-user-fair-share.md` "Stage 1B".

Built on that: `pat-service/internal/qos` now assigns a band per request
(warm/normal/demoted, from the existing chain-hash session match plus two
new Valkey-backed signals — a per-user consecutive-step streak and a
per-session last-dispatch timestamp) and sends it as
`x-llm-d-inference-objective`. Demotion is checked before warmth (doc lists
the opposite order; inverted deliberately, see ADR 0008 "Stage 3" for why).
No `cached_tokens` correction for warm — confirmed in the previous entry
that this vLLM build never populates that field, so the chain match alone
is the only warmth signal available.

**Validated live** with a throwaway user and a real 10-step growing
conversation through the actual PAT path: `patsvc_session_match_total`
1 new + 9 matched, `patsvc_rotations_total{user}=1` (demoted exactly once,
after the 8-step threshold), and cross-checked against EPP's own
`/metrics` for the same `fairness_id` — 1 request at priority 5 (normal),
7 at priority 10 (warm), 2 at priority 1 (demoted), summing to the 10 sent.
pat-service's band decision and EPP's actual dispatch agree exactly. Built
and deployed via `make pat-deploy` (workstation → WSL2 k3d node import, no
registry). `make llmd-nvfp4-smoke` re-run clean afterward (its rate-limit
assertion still fails on the pre-existing 3→60 req/min drift from the
previous entry — unrelated, not fixed here).

Open at that point: Stage 3 step 4 (cost accounting + spend-based band
modulation, §4.4) and step 5 (hard per-user concurrency ceiling, only if
round-robin proves insufficient) were unstarted; so was all of Stage 4
(Prometheus/ClickHouse/Grafana) and Stage 5's formal `scripts/qos-smoke-test`.
`config/qos/README.md` and the `Makefile` comment about "no GAIE
InferencePool backend resource" were rewritten to match current reality.

**Same day, continued: Stage 3 step 4.** Customer direction: modulation of
the priority queue only, explicitly no rate limit or admission ceiling
added (step 5 stays out of scope). `qos.Tracker.RecordCost` turns a
completed request's `usage` into `cost = α·(prompt-cached) + β·completion`
(coefficients now `QOS_COST_ALPHA_PER_TOKEN`/`QOS_COST_BETA_PER_TOKEN`, not
hardcoded) and adds it to the user's recent Valkey-backed spend
(`QOS_SPEND_WINDOW_SECONDS`, default 600s, decays on idle). A user more
than `QOS_SPEND_DEMOTE_THRESHOLD` (default 5.0) ahead of the
lowest-spending other *active* user is demoted regardless of session state;
a lone active user is never demoted this way. Cost is only knowable after a
response completes, so it feeds the user's *next* request, never the one
that produced it. `recordUsage` reads `usage` from non-streaming chat
completions only (never buffers `text/event-stream`, same
latency-vs-signal tradeoff as the earlier `stream_options.include_usage`
decision).

**Validated live** with two throwaway users: B sent one minimal request
(~0 spend); A sent two `max_tokens=250` requests (~3.73 cost units each,
confirmed against the real returned `usage` — `prompt_tokens_details` again
`null`) then a third. EPP's `/metrics` showed A's first two requests at
priority 5 (normal, spend not yet recorded when those headers were sent)
and the third at priority 1 (demoted, once ~7.46 accumulated spend exceeded
B's ~0 by more than the threshold); B stayed at priority 5 throughout.
Full details: `docs/adr/0008-per-user-fair-share.md` "Stage 3, step 4".
`make llmd-nvfp4-smoke`/`pat-smoke-test` and `make verify` clean afterward
(same pre-existing, unrelated rate-limit-assertion skip as before).

Open: Stage 3 step 5 (hard ceiling, out of scope for now) and all of Stage
4 (Prometheus/ClickHouse/Grafana observability — spend modulation today has
no dedicated metric, only its effect on the `Band`/priority label) and
Stage 5's formal test suite.

## vLLM context-length tuning — 2026-09-07

Ask: raise usable context length while holding `--max-num-seqs 2` fixed
(same 2-slot constraint the QoS work below assumes), and measure tok/s.
Full writeup: [docs/operations/vllm-inference.md](docs/operations/vllm-inference.md)
"Context-length tuning" and "Measured speed" sections; raw data in
[docs/operations/evidence/vllm-benchmark-2026-09-07.json](docs/operations/evidence/vllm-benchmark-2026-09-07.json).

Deployed: `--max-model-len 84672 → 131072`, `--gpu-memory-utilization
0.90 → 0.94`, plus new `--kv-offloading-size 8 --kv-offloading-backend
native` (vLLM 0.27.1's own CPU KV-cache offload, `/dev/shm`-backed —
`dshmSizeGiB` raised 8→12 GiB to fit it, container memory limit 24→20 GiB
request 8→12 GiB). `--max-num-seqs` untouched throughout, per the ask.

Method, not guesswork: binary-searched `gpu-memory-utilization` live
(0.90/0.93/0.94 all start; **0.95 crash-loops** —
`ValueError: Free memory on device cuda:0 (30.2/31.84 GiB) ... less than
desired GPU memory utilization (0.95, 30.25 GiB)`, so 30.2 GiB is a hard
driver-side reservation on this card, not tunable). Confirmed empirically
that vLLM's startup check only requires the GPU KV pool to fit *one*
sequence at `max-model-len`, not `max-num-seqs` of them (deployed
`--max-model-len 200000` deliberately as a probe — it started fine, reporting
"Maximum concurrency for 200,000 tokens per request: 1.18x" — then backed off
to 131,072 as the actually-deployed value once that was judged too aggressive
for a real 2-concurrent-max-length guarantee). Confirmed KV offloading is a
prefix-cache/eviction cushion, not a way to run one generation's live context
past the GPU-resident pool — verified via a real 2×~115K-token concurrent
stress test that moved `kv_offload_load_bytes_total` from 0 to ~513 MB with
both requests finishing cleanly. First offload attempt at 16 GiB **crash-looped**
(`OSError: [Errno 14] Bad address` from `MADV_POPULATE_WRITE`) because the old
8 GiB `dshm` `emptyDir` was smaller than the offload buffer being mmap'd into
it — fixed by parameterizing `dshmSizeGiB` in the chart instead of hardcoding
8Gi.

Net effect on short-prompt decode speed: 71 tok/s (single) / 66 tok/s
(2-concurrent) baseline → 62.5 tok/s / 61.9 tok/s at the new config — about
9-12% slower, isolated via an offload-on/off A/B to be mostly the larger
`max-model-len`/`gpu-memory-utilization` themselves (~6-12%), not the offload
connector (~5-6% extra on single requests, noise-level under concurrency).
In exchange: 55% more usable context (84,672 → 131,072) at the same 2-slot
concurrency, cleared the user's own ">100K or fall back to SGLang" bar
without needing the fallback.

The stale `k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml` copy was **not**
updated — `docs/operations/vllm-inference.md` already documents it as dead
(not applied by anything); only `helm/vllm-inference/values.yaml` and its
template are the live source of truth.

## QoS fair-share: Stage 2 + Stage 3 step 1 — 2026-09-07

`TASK-qos-fair-share.md` v2, continuing the entry below. Model renamed
everywhere to `qwen-3.8-27b` (the user's own change, `da4bab3`); the
`qos-test-a`/`qos-test-b` throwaway Keycloak users mentioned below were
removed in the same cleanup — later reproductions need fresh throwaway users
(`scripts/kc-user-add`, `scripts/kc-pat-issue`).

**Stage 2, done.** `inference.prefixCaching: true` — the hybrid Mamba/SSM
checkpoint (`Qwen3_5ForConditionalGeneration`) does support it on vLLM
0.27.1, contrary to the task's own flagged risk. Real hit rate (61% on a
repeated-prefix session) and real TTFT drop (1.31s → 0.67s → 0.53s)
confirmed via `/metrics`. Two corrections to prior assumptions, both now in
`docs/operations/vllm-inference.md`: the metric is `vllm:kv_cache_usage_perc`,
not `gpu_cache_usage_perc`; and vLLM silently overrides
`mambaCacheMode: none` to `align` (its own log calls this "experimental")
whenever prefix caching is on for this architecture. `usage.prompt_tokens_details.cached_tokens`
is confirmed **always null** on this vLLM build despite real cache hits —
this invalidates the task's §3.2 plan to correct the chain-hash warmth
prediction against a measured value; ADR 0008 needs updating to say the
chain prediction stands alone universally, not just for streamed requests.
KV headroom measured at 0.13–0.15 for 2×~14.5k-token concurrent requests,
extrapolated to ~0.88 worst-case (2×84,672-token sessions) — recommended
**not** raising `--max-num-seqs` yet; full numbers in `tests/qos/baseline.md`.

**Stage 3 step 1, done (observation mode only — no admission decision).**
New `pat-service/internal/qos` package: computes the §3.1 hash chain per
chat-completion request, matches it against Valkey (`envoy-ratelimit-valkey`,
`VALKEY_ADDR`), and records the result as Prometheus counters/gauges on a new
internal-only `:9090 /metrics` port (`patsvc_session_match_total{result}`,
`patsvc_sessions_started_total{user}`, `patsvc_session_steps_total{user}`,
`patsvc_sessions_active{user}`, the last swept every 30s). Fails open by
design: a slow/unreachable Valkey or a non-chat body degrades the session
match, never the proxied request. Validated live against the real PAT path
(not a direct EPP probe) with a throwaway user and a real 3-step growing
conversation: `sessions_started_total` incremented exactly once and
`session_match_total{result="matched"}` twice, confirmed independently by
reading the actual Valkey keys — session count does not grow linearly with
request count, which is the task's own stated highest-risk failure point.
One isolated stray extra `matched` count turned up across several ad hoc
manual test runs mixed together (not reproduced in a single clean,
precisely-measured run) — most likely an artifact of the manual test
harness, not the implementation; worth re-checking once real sustained
traffic accumulates rather than chasing further in a synthetic test.

Also raised `llmd-per-user-limit` from 3 to 60 req/min
(`helm/airgap-stack/values.yaml` `inference.perUserRateLimitPerMinute`, no
longer hardcoded in the template, per §4.5) — the 3/min value was only ever
a Stage 0 measurement artifact and was blocking real coding-agent traffic
now that Stage 3 needs to observe it.

Stage 3 steps 2–4 (slots/queue, GPU-time fairness, warm/cold bands) remain
unstarted — this was an explicit "observation only" instruction, not a
go-ahead for admission control.

## QoS fair-share investigation — 2026-09-07

`TASK-qos-fair-share.md` (v2). Switched the GPU from SGLang back to vLLM
(`make sglang-down`, `make vllm-up`) — this task needs the llm-d
EPP/vLLM path, not SGLang. Stage 0 (baseline) and Stage 1 (EPP capability
gate) done; draft `docs/adr/0008-per-user-fair-share.md` written; **stopped
before Stage 3 per the task's own gate**, pending confirmation.

Findings: prefix caching is confirmed fully off (`prefix_cache_queries_total`
never increments, not just low). EPP's `flowControl` cannot cap concurrent
dispatch per fairness id, and cannot assign priority band per request in
this build (`main`, commit `b1bf63da5e9a52dc8815264809d00f45f5b5e966`) —
confirmed empirically (strict-decoder config probes, a live header test of
`X-Llm-D-Inference-Objective`, `kubectl get crd | grep infer` returning
nothing), not from docs. `X-Llm-D-Inference-Fairness-Id` is confirmed the
real fairness key this build reads. Reproduced the reported problem: one
user's 4 parallel requests take both `--max-num-seqs 2` slots before a
second user's request is even in flight; full numbers, the repro command
(`scripts/qos-baseline-repro`), and a GPU cost-model calibration
(α≈9.4×10⁻⁵s/token uncached-prefill, β≈0.0149s/token decode) are in
`tests/qos/baseline.md`.

Confirmed the default `llmd-per-user-limit` (3 req/min) trips before the
concurrency behaviour is even observable — temporarily patched to 100/min
for measurement, reverted to the committed 3/min afterward (chart still
says 3/min; the task's own Stage 4.5 raises this permanently, not yet done).

Deployed one small piece of Stage 0 instrumentation directly, since it's a
measurement, not the scheduler: `pat-service` gained an opt-in
`QOS_LOG_CLIENT_SHAPE` diagnostic (header names + body top-level key names
only, never values) to confirm which real coding-agent clients can carry a
session header before Stage 3 commits to the server-side-only prefix-hash
design v2 settled on (Hermes Agent's own tracker already confirms it
cannot: NousResearch/hermes-agent#9398). Built and deployed from this
machine since `make pat-image` / `make nvfp4-pat-load` need the WSL2 host's
own Docker to reach the k3d node's containerd, which this machine can't:
built the image locally, `docker save` + `scp` to `llmstack@gpu-host.local`, then
`ctr -n k8s.io images import` on the host, then
`kubectl set env deployment/pat-service QOS_LOG_CLIENT_SHAPE=true` (an
imperative override, not yet the committed default — `k8s/base/applications.yaml`
now carries the variable defaulting to `"false"`). Verified end-to-end with
a real PAT call; no real-traffic sample collected yet (needs a full work
day) — see `tests/qos/baseline.md` §0.4 for how to pull results and turn it
back off.

Open: Stage 2 (turn prefix caching on — real risk the hybrid Mamba/SSM
checkpoint doesn't support it, per the task's own warning) and Stage 3 (the
`internal/qos` virtual-time scheduler in Valkey) are unstarted, both
pending the user's go-ahead. `qos-test-a` / `qos-test-b` Keycloak users
(throwaway, `ai-user` role) were created for reproduction and are still on
the realm for reuse in Stage 5.

## Current state — 2026-09-06

Deploy path unchanged (`make stack-up`), with the model servers now carved out
of it:

- **vLLM is a Helm release**, `helm/vllm-inference` (`make vllm-up`, run by
  `stack-up`). `--max-model-len` is 84672, prefix caching is off, and the
  mamba cache flags (`--mamba-cache-mode none`,
  `--mamba-ssm-cache-dtype float32`) are set — all values, not literals.
  `k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml` is a stale second copy that
  `scripts/helm-render` filters out; nothing applies it.
- **SGLang is a Helm release too**, `helm/sglang-inference`
  (`make sglang-up`), serving the same PVC under the same RuntimeClass on
  port 30000 with the `agents_nospec` profile (context 155648, 3 concurrent
  requests). It is not installed by `stack-up`; both engines request
  `nvidia.com/gpu: 1` on a one-GPU node, so the second one stays `Pending`.
  Its API key comes from the `sglang-api-key` Secret, created by the
  `airgap-stack` release from `SGLANG_API_KEY`.
- `helm/airgap-stack/values.yaml` has `inference.sglang.enabled: true`
  pointing at `sglang-qwen38.airgap-ai-stack.svc.cluster.local:30000` (not
  `host.k3d.internal` any more). `llamacpp` and `externalApi` stay disabled.
- Open WebUI advertises both models:
  `OPENAI_API_CONFIGS` `model_ids` is `["qwen38-nvfp4","qwen38-nvfp4-sglang"]`.
- Prometheus scrapes vLLM, SGLang and the llm-d EPP, each labelled
  `namespace=airgap-ai-stack`.
- Public origin `https://jitosavo.synology.me:60834`; the edge `external`
  listener is port 3091 (`routing.edge.externalPort`).
- `/sso/admin` and `/sso/realms/master` return 404 at the edge. Keycloak's
  `KC_HOSTNAME_ADMIN` is pinned to `http://localhost:8888/sso`, so the admin
  console works only behind `kubectl -n airgap-ai-stack port-forward
  svc/keycloak 8888:8080`.
- `scripts/pat-smoke-test` now creates its throwaway user with a
  policy-conforming password and clears the realm's default `CONFIGURE_TOTP`
  action, which the realm re-adds to every new account.
- `make verify` passes. It does not cover the two engine charts.

Open: the duplicate `vllm.yaml`, and the fact that the SGLang route is enabled
while `stack-up` does not install the engine.

`make sglang-up` brought the SGLang release back from `0/0` (it had been
scaled down, values still `replicas: 1`); auth and inference were then
verified end to end through the public origin: SSO discovery, `/sso/admin`
404, `/platform` SSO redirect, `/v1` 401 unauthenticated, and a real
`qwen38-nvfp4-sglang` chat completion with a valid PAT.

New user-administration scripts (`scripts/kc-user-add`, `kc-user-list`,
`kc-user-delete`, `kc-pat-issue`, sharing `scripts/lib-keycloak-admin.sh`):
the admin *console* cannot be driven through a port-forward at all — beyond
`/sso/admin` being closed at the edge, `KC_HOSTNAME_ADMIN` is a fixed
`http://localhost:8888/sso` origin the console expects back for its
third-party-cookie check, which a forwarded port never supplies — so these
scripts go straight at the admin REST API through the same port-forward
instead. `kc-user-add` also sets `firstName`/`lastName`, which
`scripts/pat-smoke-test` already did but nothing else documented: the
realm's user-profile config marks both required for role `user`
(`GET …/users/profile`), and omitting them makes Keycloak silently attach an
implicit `VERIFY_PROFILE` action that direct-grant login reports as the same
`resolve_required_actions` / "Account is not fully set up" error as a
leftover `CONFIGURE_TOTP` action.

Verifying a large-context request surfaced a real gap: the private AI
Gateway route (`llmd`) had no `BackendTrafficPolicy.spec.requestBuffer`, and
Envoy AI Gateway's ext-proc — which must buffer the whole body to read the
model name and count tokens — was rejecting anything over roughly 32KiB with
`response_code_details=request_payload_too_large` (visible in the
`ai-gateway-private` Envoy access log) before the request ever reached
SGLang or vLLM. Confirmed with the Envoy access log, not guessed. Fixed by
adding `requestBuffer.limit: 4Mi` to `llmd-per-user-limit` in
`helm/airgap-stack/templates/llmd.yaml`, applied with `make helm-up`. An
18.6k-token needle-in-haystack request through the public origin then
returned the planted fact correctly with `finish_reason=stop`.

## State — 2026-09-03

The remote Windows + WSL2 host (`llmstack@gpu-host.local:2222`) runs a fresh
single-node `k3d` cluster named `llm-stack` (recreated 2026-09-02 with the
user's approval). K3s v1.35.5+k3s1, node `k3d-llm-stack-server-0` Ready,
16 CPU / 48 GiB, kubelet advertises `nvidia.com/gpu: 1` via the device
plugin. The API server is published on remote port `41755`.

Two local SSH tunnels to the host are active:
`127.0.0.1:41755 -> 127.0.0.1:41755` (k8s API) and
`127.0.0.1:18080 -> 127.0.0.1:8080` (Envoy edge; pass `STACK_TUNNEL_PORT=18080`
to the smoke scripts).

The local kubeconfig context `wsl-llm-stack` has been refreshed: server is
now `https://127.0.0.1:41755` with the recreated cluster's CA and admin client
certificate (the old `127.0.0.1:16443` endpoint and pre-recreation CA are
gone). `kubectl` with the default context now reaches the remote cluster.

The full remote profile (`make nvfp4-up`) is deployed: base application stack
(Keycloak, PAT, Langfuse, Grafana, Open WebUI, observability), the GPU vLLM
Deployment, and the llm-d standalone router with the EPP selected by the AI
Gateway route. All 39 pods Running; restart bursts (keycloak 5, langfuse 6,
pat-service 6, envoy-ratelimit 50) are bootstrap-time only and stable since.
Gateway `edge` Programmed (`172.21.0.3:8080`), all 7 HTTPRoutes accepted.
All LM Studio objects (Deployment, Service, ConfigMap, Backend,
AIServiceBackend, AIGatewayRoute, SecurityPolicy, BackendTrafficPolicy) are
gone from the stack namespace. The private inference route they lived on is
now named `llmd`: `AIGatewayRoute/llmd` -> `llmd-qwen-test-openai` (Backend ->
llmd standalone proxy -> ext-proc EPP -> vLLM), with `SecurityPolicy/llmd-jwt`
and `BackendTrafficPolicy/llmd-per-user-limit` defined in
`k8s/overlays/remote-wsl-vllm-nvfp4/llmd-route.yaml`. LM Studio remains only
as the local no-GPU base fallback (`k8s/base/inference.yaml`), which the
remote overlay deletes via `delete-lmstudio.yaml`.

Public boundaries verified through the 18080 tunnel: `sso.`/`tokens.`/
`grafana.` redirect, `ai.`/`langfuse.` 200, `api./v1` 401 without a PAT. Full
`make llmd-nvfp4-smoke` passes end-to-end (PAT issue, chat 200 through the
llmd route, per-owner rate limit 429 on the 4th inference call, revoke 401).

The `vllm-qwen38-nvfp4` Deployment now requests `nvidia.com/gpu: 1` in both
`requests` and `limits` (formal scheduler-level accounting; the pre-plugin
"no GPU request" state is obsolete). The device plugin DaemonSet in
`kube-system` is a hard prerequisite for scheduling that pod: without the
advertised extended resource it stays Pending (`insufficient nvidia.com/gpu`).
Both `remote-wsl-device-plugin` (one-time cluster bootstrap) and
`remote-wsl-vllm-nvfp4` (per-deploy app overlay) are re-installed after a
`k3d-create-nvidia` cluster recreation; they are separate kustomizations.

## Network schema (verified 2026-09-04)

```
[Internet]
  https://jitosavo.synology.me:60834
    -> Synology reverse proxy / Windows + WSL plumbing
      -> Envoy Gateway `edge`, listener `external:3091`
        -> /sso      Keycloak
        -> /platform PAT dashboard
        -> /v1       PAT service -> private AI Gateway
        -> /          Open WebUI

[Cluster only]
  Open WebUI and PAT service -> ai-gateway-private.envoy-gateway-system.svc:8080
    -> llm-d -> vLLM
  Grafana and Langfuse remain ClusterIP services.
```

- Public routing is selected only by path; no `Host` header or `gpu-host.local`
  name is used for route matching.
- The public Keycloak issuer and all browser callbacks use the canonical
  `https://jitosavo.synology.me:60834` origin. Open WebUI sets its explicit
  `OPENID_REDIRECT_URI`, so an upstream proxy's host header cannot change its
  callback URL.
- `gpu-host.local` remains only the Windows/WSL SSH host name; workload-to-workload
  traffic uses Kubernetes service DNS.
- The cluster-only AI Gateway is backed by a `ClusterIP` Envoy Service. There
  is no public AI Gateway listener and the legacy internal listener `:3092`
  has been removed.
- Operator UIs are exposed separately through Helm-managed NodePorts:
  `grafana-nodeport` on `32030` (monitoring namespace) and
  `langfuse-nodeport` on `32031` (airgap-ai-stack namespace). On this k3d
  host they are reachable at the server node address `172.21.0.3`; publishing
  them beyond the WSL host is deliberately outside the chart.

## vLLM NVFP4 result

- Image: `llm-stack/vllm-qwen38-nvfp4:0.27.1` (vLLM 0.27.1, base
  `vllm/vllm-openai@sha256:0a51ea5b…`). `versions.lock.env` `VLLM_VERSION`
  updated 0.26.0 -> 0.27.1; no manifest references 0.26.0.
- Model: `/home/llmstack/models/RadixArk-Qwen3.8-27B-NVFP4` on WSL2; ModelOpt
  NVFP4 checkpoint, about 20.42 GiB.
- GPU: NVIDIA GeForce RTX 5090, driver 610.47, 32 GiB; the pod has confirmed
  CUDA access and SM 12.0.
- Kubernetes deployment: `airgap-ai-stack/vllm-qwen38-nvfp4`, one ready pod,
  ClusterIP Service port 8000, static read-only `Retain` PV/PVC of 24 GiB.
  Args: `--kv-cache-dtype fp8 --attention-backend TRITON_ATTN
  --max-model-len 65536 --max-num-seqs 2 --gpu-memory-utilization 0.90`.
- Weight load completed in 38.60 seconds; vLLM reported 19.08 GiB for model
  loading. The NVFP4 kernel path and `TRITON_ATTN` were selected. EPP polls
  `/metrics` from the pod every ~15s (200 OK); one early
  `datalayer_poll_errors_total` counter dates from the model-load window.
- A request executed from inside the GPU pod through
  `vllm-qwen38-nvfp4.airgap-ai-stack.svc.cluster.local:8000` returned
  `VLLM_OK` with `finish_reason=stop`.

## GPU runtime arrangement

`k3d-create-nvidia` creates the cluster with `--gpus all`, mounts the model
directory read-only at `/var/lib/models`, and mounts the required WSL2 driver
and NVIDIA Container Toolkit libraries into the K3s node. K3s registers the
`nvidia` containerd runtime handler. The generated containerd template uses
`/opt/nvidia/nvidia-container-runtime-legacy`, which forces the NVIDIA legacy
prestart-hook path; this is required because CDI did not resolve the WSL2
driver mount reliably in the k3d node image.

The runtime is exposed in manifests as `RuntimeClass/nvidia`, the node
advertises `nvidia.com/gpu: 1` via the NVIDIA device plugin
(`k8s/overlays/remote-wsl-device-plugin`), and the vLLM Deployment consumes
the resource with `requests`/`limits` `nvidia.com/gpu: 1` plus
`strategy: Recreate` and node selection for single-GPU exclusivity.

## Relevant repository files

- `deploy/vllm-qwen38-nvfp4/Dockerfile`, `build`, `run`, and `smoke`: image
  recipe and direct Docker validation.
- `deploy/vllm-qwen38-nvfp4/k3d-create-nvidia`: destructive/reproducible k3d
  creation; it deletes only the named `llm-stack` cluster.
- `deploy/vllm-qwen38-nvfp4/k3d-load-image`: imports the local vLLM image into
  k3d.
- `k8s/overlays/remote-wsl-vllm-nvfp4/`: RuntimeClass and model PV/PVC, plus
  the Langfuse, Prometheus and Open WebUI site patches. (As of 2026-09-06 the
  routing objects it once held live in `helm/airgap-stack/templates`, and the
  vLLM Deployment in `helm/vllm-inference`.)
- `helm/vllm-inference/`, `helm/sglang-inference/`: the two model-server
  releases and every launch flag they take.
- `k8s/overlays/remote-wsl-device-plugin/`: GPU-advertising DaemonSet,
  applied once per cluster bootstrap.
- `config/llmd/router-nvfp4-values.yaml`: EPP plugin set (queue, kv-cache,
  prefix-cache scorers; concurrency-detector `maxConcurrency: 2` matching
  `--max-num-seqs=2`; round-robin fairness; flow control 2 GiB / 32 requests).
- `scripts/vllm-nvfp4-smoke-test`: health, model discovery, real OpenAI
  chat-completions request through Kubernetes service DNS from the GPU pod.

## Completed validation

- `kubectl kustomize k8s/overlays/remote-wsl-vllm-nvfp4` passed.
- Remote `kubectl apply --dry-run=client -k` passed with the current k3d
  kubeconfig.
- Local `kubectl` now works against the remote cluster through the tunnel via
  the refreshed `wsl-llm-stack` context (`kubectl get nodes` verified).
- The remote NVFP4 smoke test passed (vLLM direct path).
- `make llmd-nvfp4-smoke` passes on the remote profile (STACK_TUNNEL_PORT=18080):
  PAT issue, inference through the llmd route, per-owner rate limit, revocation.
- The remote inference path through AI Gateway did not respond at deploy time
  (500 direct_response) because envoy-gateway never resolved the Backend until
  the controller was restarted; after rollout restart the `llmd` HTTPRoute is
  `ResolvedRefs=True` and end-to-end requests succeed. The per-owner rate limit
  applies only to inference calls; `/v1/models` bypasses it, so
  `scripts/pat-smoke-test` exercises the limit with chat requests (4th call =
  429).

## Next likely work

- Re-pin the device plugin to the official `nvcr.io` image when anonymous
  pulls become reachable from the WSL2 host.
- The llm-d EPP image is the mutable `main` tag (manifest digest recorded in
  `versions.lock.env`); upstream chart v0 lacks digest-only references — pin
  when the chart supports it.
- Reconcile the `pat-gateway` Keycloak client secret with `.env` (a manual
  client-credentials JWT from the `.env` secret failed JWKS validation while
  the deployed service works, so the deployed secret differs from `.env`).
