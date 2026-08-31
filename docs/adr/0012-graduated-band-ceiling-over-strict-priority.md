# 0012: Priority bands stop being absolute -- a graduated dispatch ceiling, on a saturation signal that has resolution

## Status

Accepted on 2026-09-09 (`llmd-qwen-test` revision 8,
`config/llmd/router-nvfp4-values.yaml`). All three probes under "What must be
verified" are done and recorded there. Probe #3 -- the saturation baseline
under genuine multi-user load, left open by design at initial deployment --
was closed the same day: see "Post-deployment results, busy day" below for
the re-measured per-user TTFT split against this ADR's own Comparison target.
One correction to the Decision below stands: `soft-reflective-ceiling-policy`
is Alpha stability in this build and is rejected at startup without
`--allow-experimental-plugins`, now set via `router.epp.flags`.

Two follow-ups from the busy-day read: a Grafana panel and alert for
`llm_d_epp_flow_control_stale_endpoints` (`config/grafana/dashboards/user-activity.json`
panels 13/14, `config/gateway-addons/values.yaml` `grafana.alerting`, both
live via `make monitoring-up`) after the fail-closed condition in Consequences
was observed firing for real; and an explicit decision not to raise
`kvCacheUtilThreshold` off its 0.8 default, since the observed KV pressure
that reached it was genuine, not a false-positive gate. Both are detailed
below.

It follows directly from
ADR 0008 Stage 1C, which fixed the *intra*-band half of the same problem
(`flowControl.defaultPriorityBand` now pins `round-robin-fairness-policy` on
the bands provisioned from `router.inferenceObjectives`, deployed as
`llmd-qwen-test` revision 7) and left the *cross*-band half open. Every
capability claim below about the EPP build is either read from the pinned
source (`b1bf63da5e9a52dc8815264809d00f45f5b5e966`, digest
`LLMD_EPP_IMAGE_MANIFEST_DIGEST` in `versions.lock.env`) or measured on this
cluster; the two things that still need an in-pod probe are listed under
"What must be verified before this is accepted."

### Pre-deployment baseline, frozen (source: Prometheus, window 2026-09-08)

Revision 8 (this ADR's config change) deployed 2026-09-09 09:37:36 +05.
Everything below is from the full prior day, 2026-09-08T00:00Z-2026-09-09T00:00Z,
queried 2026-09-09 against `airgap-ai-stack/prometheus` -- confirmed still on
disk despite the Prometheus pod itself restarting at 2026-09-09T02:41:11Z
(head block only goes back to the restart; these numbers came from the
on-disk blocks that survived it). Recorded here as fixed text because that
survival is not guaranteed twice -- default retention, or another restart
that this time also loses the PVC, would take it with it. This is the same
window ADR 0008 Stage 1C already cited from live observation; the numbers
below are freshly re-queried, not retyped from that section, and read
slightly differently because they aggregate the full day rather than Stage
1C's narrower in-the-moment window.

`sum by (fairness_id) (increase(llm_d_epp_request_ttft_seconds_count[1d]))`
at `2026-09-09T00:00:00Z`, three most active users by request count:

| `fairness_id` | warm (10) | normal (5) | demoted (1) | TTFT p50 | TTFT p95 |
| --- | --- | --- | --- | --- | --- |
| `2d641ee3-...` (heavy) | 1879 | 314 | 772 | 0.598s | 56.25s |
| `67926262-...` (throttled) | 67 | 21 | 130 | 27.90s | 91.89s |
| `1b45eb90-...` (mid) | 88 | 21 | 123 | 0.83s | 62.40s |

(`histogram_quantile(0.5|0.95, sum by (le) (increase(llm_d_epp_request_ttft_seconds_bucket{fairness_id="..."}[1d])))`,
same `time`.) Total across all `fairness_id`s that day: 3443 requests.
`67926262-...`'s 130/67 demoted/warm split and its p50/p95 match ADR 0008
Stage 1C's own citation almost exactly (that section: "133 demoted against
67 warm", "p50 27.9s / p95 91.9s") -- confirms this is the same underlying
data, re-read, not a new or drifted number.

One more data point this pulls that Stage 1C's text didn't have: a
`llm_d_epp_average_kv_cache_utilization` sample **during this same busy
window** (2026-09-08T20:00Z) read **0.5**, not the 0.75 "one sample, not a
characterisation" this ADR's Decision section cites elsewhere (that 0.75 was
an idle-adjacent spot check, not from inside a busy period). 0.5 is
comfortably under the default `kvCacheUtilThreshold: 0.8`, which is mild
evidence against probe #2's flagged risk -- but it is still one sample from
one hour of one day, not the loaded characterisation probe #3 asks for.

**Comparison to run once revision 8 has seen a comparable busy day**: re-run
the same two queries with the fairness_ids that turn out active, and check
whether `67926262-...`-like users (heavy demotion, high TTFT) move toward
`2d641ee3-...`-like TTFT under contention, per this ADR's Consequences
section, rather than whether any single number hits a target -- the specific
`fairness_id`s will differ, since `pat-service` assigns them per session.

### Post-deployment results, busy day (source: Prometheus, window 2026-09-09T04:37:36Z-18:33:33Z)

Revision 8 has now seen a comparable busy day (~2085 requests in ~14h post-
deploy vs. the baseline's 3443 in 24h -- ~149 req/h vs. ~143 req/h, comparable
load, not a quieter window). Same three `fairness_id`s happened to still be
the most active. Same queries as the frozen baseline above, re-run against
the live window instead of retyped:

| `fairness_id` | warm (10) | normal (5) | demoted (1) | TTFT p50 | TTFT p95 |
| --- | --- | --- | --- | --- | --- |
| `2d641ee3-...` (heavy) | 400 | 91 | 546 | 2.73s | 97.09s |
| `67926262-...` (throttled) | 54 | 27 | 159 | 3.01s | 105.00s |
| `1b45eb90-...` (mid) | 31 | 13 | 76 | 2.00s | 96.03s |

This is the comparison the Decision was made to produce, and it reads the
way this ADR's Consequences section predicted: the heavy/throttled p50 gap
was 46.7x before (0.598s vs. 27.90s) and is 1.1x now (2.73s vs. 3.01s) --
`67926262-...` moved to `2d641ee3-...`-like TTFT under contention rather than
waiting behind it, which is the specific claim probe #3 was deferred to
check, not a generic "did it get faster." Note the heavy user's own demoted
share also grew past its warm share (546 vs. 400, was 772 vs. 1879) --
`pat-service`'s spend-based demotion is now visibly biting the heavy user
harder under this same load, and the graduated ceiling is what makes that
demotion a throttle instead of a wait-behind-everything sentence, per this
ADR's third Consequences bullet.

The cost side of the same trade: p95 rose for every one of the three users
(56.25s/91.89s/62.40s before, to 97.09s/105.00s/96.03s after) rather than
holding steady for the two lighter users while only the heavy user's own p95
moved. `soft-reflective-ceiling-policy`'s alternation and longer queues under
real saturation (this ADR's fourth Consequences bullet) are the likely
mechanism; distinguishing that from "the day was just busier in the tail"
would need a longer run than one day, so this is read as expected cost, not
re-litigated here.

Saturation signal, over the same window:

| Metric | avg | max |
| --- | --- | --- |
| `llm_d_epp_average_kv_cache_utilization` | 0.33 | 0.99 |
| `inference_extension_flow_control_pool_saturation{stage="effective"}` | 0.42 | 1.24 |

Max saturation above 1.0 (not pinned at exactly 1.0) is, by this ADR's own
rule for telling the two apart, genuine oversubscription rather than a stale
scrape -- and KV utilization spiking to 0.99 against the unchanged
`kvCacheUtilThreshold: 0.8` default is consistent with that being real
contention, not an artifact of an under-tuned threshold. **Decision: leave
`kvCacheUtilThreshold` at its default.** Probe #2's flagged risk (default
threshold gating continuously if resident KV sits near it in steady state)
did not materialize -- KV utilization spent the day averaging well below
threshold (0.33) and only touched the ceiling during genuine spikes, and
`EvictedContextCancelled` stayed at 0 for the whole window, so the ceiling
gated without ever overflowing a queue.

The Consequences section's fail-closed risk did materialize once, for one
Prometheus scrape interval: `llm_d_epp_flow_control_stale_endpoints` read 1
at 2026-09-09T10:12:36Z, coinciding exactly with `pool_saturation` reading
1.0 (not the >1.0 seen at genuine peaks) -- the stale-artifact signature this
ADR's own rule predicts. It cleared on the next scrape by itself (`detector`
is a live per-cycle gauge against `metricsStalenessThreshold`, not a latch:
see the Consequences bullet below on recovery). No eviction and no visible
TTFT damage traced to that minute. This had no dashboard or alert at the
time; both now exist, see Status.

Read together: probe #3 is closed. The graduated ceiling produces the
cross-band convergence this ADR exists to get, at the previously-flagged
cost of longer tails under load, and the one operational risk this ADR
called out ahead of time (fail-closed on a stale scrape) has now been
observed for real, exactly matching its own predicted signature, and exactly
once in 14 hours of genuine contention.

## Context

### The warm band is not negotiable

`warm` exists because active agents thrash the KV cache: a session that
loses its hot prefix has to re-prefill it, and the re-prefill traffic eats
the pipe that the rest of the users are queueing for. Prioritising a session
that is already warm (dispatched within `WARM_TTL`, `pat-service`
`internal/qos/session.go:219`) is a *throughput* decision, and ADR 0010's
GPU-only KV profile is the reason it matters here specifically: KV is the
constrained resource, not raw compute. Removing warmth to get fairness is
therefore rejected as a design direction, not merely deprioritised.

### Fairness policies cannot reach across bands

The EPP dispatches on a strict three-tier hierarchy
([fairness README](https://github.com/llm-d/llm-d-router/blob/b1bf63da5e9a52dc8815264809d00f45f5b5e966/pkg/epp/framework/plugins/flowcontrol/fairness/README.md)):

1. **Priority** -- select the highest-priority band that has pending work.
2. **Fairness** -- select which flow (`fairness_id`) within that band gets
   the dispatch. `round-robin-fairness-policy` lives here.
3. **Ordering** -- select which request within that flow's queue.

Tier 1 is resolved before Tier 2 is consulted. So *any* Tier 2 policy only
equalises flows that already share a band. Two users in different bands
never meet at Tier 2 at all.

This is why ADR 0008 Stage 1C's fix, though necessary, is not sufficient,
and it is worth being explicit that the same limit applies to
`program-aware-fairness` (`strategy: las`), which Stage 1C confirmed is
registered and configurable in this build: LAS is also a Tier 2 policy.
Adopting it would replace request-count fairness with token-weighted
fairness *inside* a band, and would do nothing about a user starving in a
band below.

The measured shape of the problem, from ADR 0008 Stage 1C: the heavy user
carried 1725 `warm` requests while `67926262-...` accumulated 133 `demoted`
against 67 `warm` and paid p50 27.9s / p95 91.9s TTFT, against the heavy
user's own p50 0.657s. Stage 1C's post-fix re-run of the §0.4 contention
scenario could not even exercise the intra-band fix, because the two
throwaway users landed in different bands -- an accident of band assignment
that neatly illustrates the general point.

### The lever that keeps warmth and still yields

The same build ships usage limit policies, wired through
`FlowControlConfig.UsageLimitPolicyPluginRef` (json
`usageLimitPolicyPluginRef`, `apix/config/v1alpha1/endpointpickerconfig_types.go`).
They gate *bands*, not flows, which is exactly the axis Tier 1 owns:

- [`static-usage-limit-policy`](https://github.com/llm-d/llm-d-router/blob/b1bf63da5e9a52dc8815264809d00f45f5b5e966/pkg/epp/framework/plugins/flowcontrol/usagelimits/README.md)
  -- one uniform ceiling across all priorities, `threshold` float, default
  `1.0` (no gating). Framework-injected by default, which is what this stack
  runs today without having chosen it. Not priority-aware, so it cannot help
  a lower band.
- [`soft-reflective-ceiling-policy`](https://github.com/llm-d/llm-d-router/blob/b1bf63da5e9a52dc8815264809d00f45f5b5e966/pkg/epp/framework/plugins/flowcontrol/usagelimits/softreflectiveceiling/README.md)
  -- graduated and priority-aware. Takes no parameters at all ("Any
  non-empty parameters block is rejected at load time") and is **not**
  framework-injected, so it has to be declared explicitly.
- [`priority-holdback-policy`](https://github.com/llm-d/llm-d-router/blob/b1bf63da5e9a52dc8815264809d00f45f5b5e966/pkg/epp/framework/plugins/flowcontrol/usagelimits/priorityholdback/README.md)
  -- also priority-aware, but with fixed, explicitly configured ceilings
  (`shape`, `domain`, `minCeiling`, `maxCeiling`; `domain: rank` spaces them
  evenly by ordinal, `domain: value` scales by the numeric priority).

The README's own upstream reference for the whole layer is the
[Gateway API Inference Extension flow-control guide](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/v1.5.0/site-src/guides/flow-control.md).

What `soft-reflective-ceiling-policy` does, in its own terms: it gates
**dispatch**, not admission -- requests keep being enqueued, a gated band is
simply not drawn from on that call. Per band it computes

    ceiling[i] = 1 - i * saturation / (N - 1)

with `priorities[0]` the highest and `N` the number of active bands. A band
whose saturation is below its ceiling is fully open. At `saturation >= 1.0`
every non-critical band is fully gated. In between, a band at or past its
ceiling *alternates* open and closed across calls with

    period = round(saturation / (1 - saturation))

so its effective dispatch rate degrades continuously, approximating
`(1 - saturation) / saturation`, rather than snapping shut the way a
fixed-threshold policy does. All gated bands share one policy-wide tick,
because the dispatch loop aborts at the first band whose ceiling gates.

The important property for us: it is **rank-only**. Only the order of the
bands matters, not the numeric spacing, so the existing 10/5/1 values need
no re-tuning.

### The finding that changes the recommendation

A graduated ceiling is only as good as the saturation number it is fed, and
this deployment currently feeds it a signal with almost no resolution. This
follows from the configuration by arithmetic, so it needs no traffic
measurement to establish. `config/llmd/router-nvfp4-values.yaml` sets
`saturationDetector.pluginRef: concurrency-detector` with
`concurrencyMode: requests` and `maxConcurrency: 3`, so pool saturation is
`inflight / 3` and takes four values: 0, 1/3, 2/3, 1. Substituting each into
`ceiling[i] = 1 - i * saturation / (N - 1)` with the three bands this stack
uses (`warm`, `normal`, `demoted`, so `N = 3`):

| saturation | ceilings (warm / normal / demoted) | effect |
| --- | --- | --- |
| 0 | 1.0 / 1.0 / 1.0 | nothing gated |
| 1/3 | 1.0 / 0.833 / 0.667 | every ceiling is above the saturation, so still nothing gated |
| 2/3 | 1.0 / 0.667 / 0.333 | `normal` and `demoted` are at or past their ceiling, `period = round(0.667/0.333) = 2`, so they are drawn from on one call in two |
| 1 | -- | every band gated, but there are no free slots to dispatch into anyway |

So on today's configuration the policy has exactly **one** operating point,
`saturation = 2/3`. That is a property of the divisor being 3, not of how
busy the cluster happens to be. A policy whose whole value is that it
degrades continuously is being handed a signal that can express one step.

Both alternatives produce a signal with real resolution:

- [`utilization-detector`](https://github.com/llm-d/llm-d-router/blob/b1bf63da5e9a52dc8815264809d00f45f5b5e966/pkg/epp/framework/plugins/flowcontrol/saturationdetector/utilization/README.md)
  scores each endpoint on a roofline model,
  `max(QueueDepth/QueueThreshold, KVCacheUsage/KVCacheThreshold)`, and
  averages across endpoints. It is **enabled by default when flow control is
  on** -- this stack replaced it by naming `concurrency-detector`
  explicitly. It is also the detector that matches our actual bottleneck:
  KV, per ADR 0010. A spot reading on this cluster had
  `llm_d_epp_average_kv_cache_utilization` at 0.75 while the request-count
  saturation read 0 -- one sample, not a characterisation, but it is the
  right order of magnitude to expect from a resident KV pool and it is the
  quantity the request-count detector cannot see at all.
- [`concurrency-detector`](https://github.com/llm-d/llm-d-router/blob/b1bf63da5e9a52dc8815264809d00f45f5b5e966/pkg/epp/framework/plugins/flowcontrol/saturationdetector/concurrency/README.md)
  in `tokens` mode (inflight tokens over `MaxTokenConcurrency`) or `hybrid`
  mode (per-endpoint max of the request and token ratios, averaged) instead
  of `requests` mode.

## Decision

Keep warmth. Keep the bands. Keep `round-robin-fairness-policy` at Tier 2
(and revisit `program-aware-fairness` separately, on its own merits, as a
Tier 2 question). Change two coupled things at Tier 1:

1. Give the saturation signal resolution, by moving
   `flowControl.saturationDetector` off `concurrency-detector` in `requests`
   mode -- preferring `utilization-detector`, because KV pressure is the
   constraint this deployment actually hits and it is the flow-control
   default this stack silently overrode.
2. Declare `soft-reflective-ceiling-policy` and point
   `flowControl.usageLimitPolicyPluginRef` at it, so a lower band under
   contention receives a degrading share of dispatch opportunities instead
   of zero.

`soft-reflective-ceiling-policy` is preferred over `priority-holdback-policy`
because it needs no thresholds invented for it: its ceilings are derived
from the observed saturation and the number of active bands. If field
evidence shows its alternation is too coarse at our band count,
`priority-holdback-policy` with explicit `minCeiling`/`maxCeiling` and
`domain: rank` is the fallback, and the swap is one config field.

Neither change is a substitute for the other. The ceiling policy without a
resolved saturation signal is close to a no-op on today's numbers; the
resolved signal without the ceiling policy just makes the existing
all-or-nothing gate trigger on a different quantity.

## Consequences

- A user in `normal` or `demoted` behind a warm flood gets a bounded, slowly
  degrading share rather than waiting for the warm queue to drain. That is
  the cross-band half of the fairness story the fair-share work has been
  missing since bands were introduced.
- Warm sessions keep their KV advantage. Nothing about `assignBand` changes,
  and the ordering between bands is unchanged when the pool is not
  saturated -- which, per the table above, is most of the time.
- `pat-service`'s spend-based demotion becomes meaningful rather than
  punitive: demoting a heavy user to `demoted` currently sends its requests
  behind everything, whereas under a graduated ceiling demotion throttles
  them. That is what ADR 0008 §4.4 wanted demotion to mean.
- Queues get longer under sustained saturation. The policy's own README is
  explicit that gated items stay queued and that bounding queue size is the
  eviction plugins' job, not this policy's. `defaultRequestTTL` (10m today)
  and the flow control `maxRequests` become the real backstop, and
  `EvictedContextCancelled` counts should be watched after the change.
- `utilization-detector` is fail-closed on stale endpoint metrics: a scrape
  outage pins pool saturation at 1.0 and halts dispatch entirely. This is a
  new operational coupling between the model server's `/metrics` health and
  inference availability that `concurrency-detector` does not have, since it
  counts in-flight requests internally. The `flow_control_stale_endpoints`
  gauge distinguishes it from genuine overload (stale reads exactly 1.0,
  real oversubscription typically reads above 1.0) and should be alerted on
  before this change goes anywhere near a busy period. Not a latch: staleness
  is re-derived every scrape cycle against `metricsStalenessThreshold`
  (default 200ms), so a transient scrape miss clears itself on the endpoint's
  next successful scrape with no restart or manual reset -- confirmed by the
  live single-sample event recorded in "Post-deployment results" below.
  Recovery is conditional on the endpoint actually being reachable again;
  a genuinely down model-server pod keeps reading stale, correctly, until it
  comes back. Alerting is now live: `llm_d_epp_flow_control_stale_endpoints`
  is panel 13/14 on `user-activity.json` (`QoS` folder) and a Grafana alert
  rule `adr0012-stale-endpoints` (`config/gateway-addons/values.yaml`
  `grafana.alerting`) fires on any nonzero reading. No contact point/receiver
  is wired beyond Grafana's built-in default (no SMTP or chat webhook
  configured on this cluster) -- the alert is visible in the Grafana Alerting
  UI and on the dashboard, not yet delivered anywhere; wiring a receiver is a
  deployment-specific decision for whoever owns this cluster's paging.
- Saturation stops being comparable across the change. Every historical
  saturation number in `tests/qos/baseline.md` and ADR 0008 is a request
  ratio; afterwards it is a KV/queue roofline score. Do not plot them on the
  same axis.

## What must be verified before this is accepted

Both by the in-pod `--config-text` probe method recorded in ADR 0008 Stage
1C (a second `/app/epp` on the default ports, which parses the config and
then exits by itself on `address already in use`, leaving no orphan):

1. **Done, with a correction.** Probed in-pod against the live
   `llmd-qwen-test-epp` pod (build `b1bf63da5e9a52dc8815264809d00f45f5b5e966`)
   by the same `--config-text` method as ADR 0008 Stage 1C. `utilization-detector`
   as `saturationDetector.pluginRef` and `usageLimitPolicyPluginRef` pointing
   at an explicitly declared `soft-reflective-ceiling-policy` (no parameters
   block) both decode and instantiate cleanly -- **but** the runner's plugin
   stability validation then rejects `soft-reflective-ceiling-policy` outright:
   `"has Alpha stability level, but command line flag
   --allow-experimental-plugins is not set"`. Re-run with that flag added gets
   past validation, phase two, and into controller startup, self-terminating
   on the expected `:9090: address already in use` with no other errors and
   no effect on the live pod (`restartCount` 0 throughout both runs). Note
   this is the opposite of `program-aware-fairness` in ADR 0008, which was
   *not* Alpha-gated -- stability level is per plugin, not inferrable from one
   example. `--allow-experimental-plugins` is now set via
   `router.epp.flags.allow-experimental-plugins: true` (the chart's generic
   `--flag=value` passthrough), confirmed present on the deployed pod's args.
2. **Done.** `utilization-detector` instantiates with defaults
   `queueDepthThreshold: 5`, `kvCacheUtilThreshold: 0.8`,
   `metricsStalenessThreshold: 200ms` when declared with no parameters, and
   the probe confirmed both thresholds are overridable via `parameters` if
   needed. What it *reports right now* is unremarkable only because the
   cluster is idle: every relevant gauge
   (`llm_d_epp_average_kv_cache_utilization`,
   `llm_d_epp_average_queue_size`, `inference_extension_flow_control_pool_saturation`)
   reads 0 at the time of this probe -- consistent with no in-flight requests,
   not evidence about loaded behavior. Deployed with the plugin's own
   defaults, undeclared, rather than pre-tuning `kvCacheUtilThreshold` against
   a single stale 0.75 sample. The risk this item originally flagged --
   default `kvCacheUtilThreshold: 0.8` gating continuously if resident KV
   usage sits near 0.75 in steady state -- closed the same way as #3, with a
   loaded reading rather than an idle one: see "Post-deployment results"
   above. KV utilization averaged 0.33 and only touched 0.99 during genuine
   spikes, with zero `EvictedContextCancelled` for the day, so the default
   threshold is not gating outside of real contention. **Decision:
   `kvCacheUtilThreshold` stays at its 0.8 default**, not raised.
3. **Done.** A saturation baseline taken under genuine multi-user load. No
   such measurement existed at initial deployment: the cluster was restarted
   for ADR 0008 Stage 1C's revision 7, and everything observable since --
   including at deployment time for this ADR's revision 8 -- had been either
   idle or the five synthetic requests of the §0.4 re-run, so the argument for
   deploying rested on arithmetic on the configured divisor, not on observed
   traffic. Revision 8 has since seen a comparable busy day; see
   "Post-deployment results, busy day" above for the re-measured per-user TTFT
   split against the frozen pre-deployment baseline. Deliberately not
   substituted with a synthetic multi-session load test at the time -- the
   real busy period was the intended answer, and it has now arrived.

All three probes are done. Probe #3's re-measured per-user TTFT split against
ADR 0008 Stage 1C's outstanding evidence is recorded in "Post-deployment
results, busy day" above, read off the per-user panels on
`user-activity.json`. The comparison that stage's own numbers set as the bar
-- heavy user p50 0.657s against 27.9-56.3s for everyone else -- is now
heavy 2.73s against 2.00-3.01s for everyone else: no longer a 46x-86x spread,
a 1.4x one.

## Alternatives rejected

- **Remove the warm band, put all traffic in one band, let a Tier 2 policy
  do all the work.** Cleanest fairness story available and it is rejected on
  the merits: warmth exists to stop KV thrashing, and the cost of losing it
  is paid by every user, not just the heavy one.
- **`program-aware-fairness` (LAS) instead of this.** Wrong tier for this
  problem. It is a genuine improvement to make separately -- token-weighted
  fairness with idle decay, covering streaming, which `pat-service`'s own
  cost accounting does not -- but it cannot reach across bands, so it does
  not close this gap.
- **`static-usage-limit-policy` with a threshold below 1.0.** Uniform across
  priorities by construction; reserves headroom without giving any of it to
  the lower bands specifically.
- **The `pat-service` admission ceiling from ADR 0008's Decision section.**
  Still the fallback if the EPP-side levers prove insufficient, but it is
  materially more code, it is blind to streaming today, and it duplicates
  scheduling that the EPP is now demonstrably able to express in config.
  Reach for it after the two probes above fail, not before.
