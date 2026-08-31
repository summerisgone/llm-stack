# QoS baseline — 2026-09-06 / 2026-09-07

Superseded task revision: `TASK-qos-fair-share.md` v2 changed the fairness
mechanism (virtual-time GPU-cost fairness, not round-robin) and added §0.4
(real client header/body-key capture) and §0.7 (cost-model calibration)
below. §§0.1–0.3, 0.5–0.6 were measured against v1 and remain valid — v2 did
not change the request path, only the scheduler design built on top of it.

Measured against the live remote cluster (`wsl-llm-stack`, GPU switched from
SGLang to vLLM for this task — see `CONTEXT.md`). vLLM pod
`vllm-qwen38-nvfp4-6685bc95d4-g6gjq`, `--max-num-seqs 2`,
`--no-enable-prefix-caching` (`helm/vllm-inference/values.yaml`
`inference.maxNumSeqs: 2`, `inference.prefixCaching: false`). EPP
`llmd-qwen-test-epp` unchanged (`config/llmd/router-nvfp4-values.yaml`).

## 0.2 — vLLM prefix cache metrics

Before any traffic in this session:

```
vllm:prefix_cache_queries_total{engine="0",model_name="qwen38-nvfp4"} 0.0
vllm:prefix_cache_hits_total{engine="0",model_name="qwen38-nvfp4"} 0.0
```

After ~14 real chat completions across two users during the reproduction
below (see 0.4), still:

```
vllm:prefix_cache_queries_total{engine="0",model_name="qwen38-nvfp4"} 0.0
vllm:prefix_cache_hits_total{engine="0",model_name="qwen38-nvfp4"} 0.0
```

Confirms 2(а): the metric isn't merely low, it never increments — prefix
caching is fully inactive with `--no-enable-prefix-caching`, exactly as the
flag implies. There is no KV reuse to prioritize a session on, which is the
precondition Stage 2 has to fix before requirements 2–3 mean anything.

## 0.3 — EPP flow-control metrics inventory

Scraped from `llmd-qwen-test-epp:9090` (`kubectl -n airgap-ai-stack
port-forward svc/llmd-qwen-test-epp 19090:9090`). Full dump saved to
`/tmp/epp-metrics.txt` during the investigation (not committed — regenerate
with the command above). Flow-control-relevant families actually emitted by
this build (`ghcr.io/llm-d/llm-d-router-endpoint-picker:main`,
commit `b1bf63da5e9a52dc8815264809d00f45f5b5e966`):

| Metric | Labels | What it shows |
| --- | --- | --- |
| `llm_d_epp_flow_control_requests_total` | `inference_pool`, `outcome` (`Dispatched`/`EvictedContextCancelled`/…), `priority` | Outcome counts **per priority band**, not per flow. |
| `llm_d_epp_flow_control_capacity_utilization_requests` / `_bytes` | `inference_pool`, `priority` | Band occupancy (0–1), **per band**, not per flow. |
| `llm_d_epp_flow_control_global_capacity_utilization_requests` / `_bytes` | `inference_pool` | All-bands rollup, only emitted if a global capacity is configured (it isn't here). |
| `llm_d_epp_flow_control_pool_saturation` | `stage` (`prefill`/`decode`/`effective`) | The saturation-detector signal gating dispatch. |
| `llm_d_epp_flow_control_dispatch_cycle_duration_seconds` | none | Internal dispatch-loop timing histogram. |
| `llm_d_epp_request_duration_seconds`, `_ttft_seconds`, `_ntpot_seconds`, `_streaming_itl_seconds`, `_input_tokens`, `_output_tokens`, `_size_bytes` (+`inference_objective_*` aliases) | `fairness_id`, `model_name`, `priority`, `target_model_name` | **Per-flow** request/latency/token histograms — the only family that carries `fairness_id`. This is what a per-user dashboard panel (Stage 4) has to key on; `--fairness-id-metric-label-limit` (default 1000) bounds its cardinality. |
| `llm_d_epp_average_queue_size`, `_std_dev_queue_size`, `_per_endpoint_queue_size` (+`inference_pool_*` aliases) | `name` / `model_server_pod` | Model-server-side queue depth, per pod, not per flow. |

No metric exposes **queue depth or wait time per flow/fairness_id** — only
per-priority-band aggregates and per-flow *outcome histograms after the
fact*. Stage 4's Grafana panel for "queue depth by flow id" (as sketched in
the task) **does not exist as a native EPP metric** in this build; the
closest available signal is `patsvc_queue_depth{user}` from Stage 3's own
in-process queue, not anything EPP emits.

`fairness_id` values observed during the reproduction below matched the two
test users' Keycloak `sub`s exactly (see ADR 0008 Q3) — direct confirmation
that `X-Llm-D-Inference-Fairness-Id` is the header this build's fairness
policy keys on, not a renamed upstream header.

## 0.4 — Reproduction: one user's flood vs. a second user's lone request

Command (two throwaway users provisioned first; see
`scripts/qos-baseline-repro` header comment for the exact `kc-user-add` /
`kc-pat-issue` invocations):

```sh
QOS_PAT_A=sk-... QOS_PAT_B=sk-... STACK_BASE_URL=***REMOVED*** \
  ./scripts/qos-baseline-repro /tmp/qos-baseline-run3
```

4 parallel long chat completions (`max_tokens=300`) fired from user A, one
short completion (`max_tokens=60`) fired from user B one second later, rate
limit temporarily raised to 100/min for the duration (see 0.5) and restored
to 3/min immediately after:

| Request | HTTP | TTFT | Total |
| --- | --- | --- | --- |
| userA-1 | 200 | 5.368s | 5.368s |
| userA-4 | 200 | 5.379s | 5.381s |
| userB-solo | 200 | **5.581s** | **5.582s** |
| userA-3 | 200 | 10.383s | 10.383s |
| userA-2 | 200 | 11.216s | 11.217s |

A prior identical run (rate limit at its default 3/min) got a `429` on
userA-4 in 0.29s instead of a 200 — see 0.5.

Reading: with `--max-num-seqs 2`, the two slots go to whichever two of
user A's four simultaneous requests the scheduler dispatches first
(userA-1, userA-4); the other two (userA-3, userA-2) queue and only run once
a slot frees up. User B's lone request, arriving ~1s later with a *different*
`X-Llm-D-Inference-Fairness-Id`, is **not** stuck behind user A's remaining
queue — round-robin fairness across flows gives it the very next freed slot
(5.58s, essentially tied with userA-1/userA-4 finishing), ahead of
userA-3/userA-2 (10.4s/11.2s). So round-robin fairness *does* already work
**once a second flow exists**. The actual bug is the instant before that:
at t=0, only user A's flow exists, and nothing in the current config stops
it from claiming both slots at once — `concurrency-detector.maxConcurrency:
2` gates the whole pool, not one flow. **This is requirement 1 exactly**:
today, one user with N≥2 concurrent agents always gets both slots the moment
they fire together, regardless of who else might show up a second later.
The number to compare against future work: an isolated single request from
a second user, arriving mid-flood, currently finishes in 5.58s — barely
worse than the fastest single request possible on this deployment.

## 0.5 — Rate limit interaction

Confirmed: at the default `llmd-per-user-limit` (3 req/min per `X-User-Id`),
firing 4 parallel requests from one user gets a `429` on the 4th in 0.29s,
before any queueing behaviour is observable. Measured with the limit
temporarily patched to 100/min:

```sh
kubectl -n airgap-ai-stack patch backendtrafficpolicy llmd-per-user-limit \
  --type merge -p '{"spec":{"rateLimit":{"global":{"rules":[{"clientSelectors":[{"headers":[{"invert":false,"name":"X-User-Id","type":"Distinct"}]}],"limit":{"requests":100,"unit":"Minute"},"xRateLimitHeaders":"DraftVersion03"}]}}}}'
# ... run scripts/qos-baseline-repro ...
kubectl -n airgap-ai-stack patch backendtrafficpolicy llmd-per-user-limit \
  --type merge -p '{"spec":{"rateLimit":{"global":{"rules":[{"clientSelectors":[{"headers":[{"invert":false,"name":"X-User-Id","type":"Distinct"}]}],"limit":{"requests":3,"unit":"Minute"},"xRateLimitHeaders":"DraftVersion03"}]}}}}'
```

Restored to the committed value (3/min) immediately after each measurement
run; the chart still has 3/min today. Confirms 2(г): the 3 req/min limit is
what actually blocks a coding agent doing tens of steps per minute, not the
2-slot concurrency ceiling — the two problems need two different fixes
(Stage 3 for the ceiling, §4.5 for the rate, see ADR 0008).

**Update, 2026-09-07 (Stage 3 step 1):** raised permanently to 60/min
(`helm/airgap-stack/values.yaml` `inference.perUserRateLimitPerMinute`, no
longer hardcoded in the template) per §4.5's own guidance, since real coding-
agent traffic doing tens of steps per minute needs to run unthrottled while
Stage 3's chain-hash session tracking is validated against it. 60/min is not
independently re-derived from a new measurement here — it is §4.5's own
suggested order of magnitude, chosen because it is far above any legitimate
single-user step rate (Hermes-style agents observed well under 10 req/min in
practice) while still bounding an accidental retry storm.

## 0.4 — Real client traffic shape (v2)

Deployed, not yet collected for a full work day — see ADR 0008's "Known
limitations" for why this is reported as pending, not fabricated.

`pat-service` was rebuilt (`make pat-image` locally — the WSL2 host's
`k3d-load-pat-service` step needs local Docker access to the k3d node's
containerd, which this machine doesn't have, so the image was built here,
`docker save`d, `scp`'d to `llmstack@gpu-host.local`, and imported there with the
same `ctr -n k8s.io images import` step `k3d-load-pat-service` uses) with a
new opt-in diagnostic, `QOS_LOG_CLIENT_SHAPE` (`pat-service/cmd/pat-service/main.go`,
`logClientShape`): for every proxied `/v1/*` call, after PAT validation, it
logs the request's header **names** (sorted, comma-joined), the `User-Agent`
**value** (a client-product identifier, not customer code, and the same
dimension `qos.requests.client_user_agent` will use in Stage 4), and the
proxied JSON body's top-level key **names** — never header values beyond
`User-Agent`, never body values, per the task's "тело — код заказчика"
constraint. It reads at most 1MiB of the body to get those key names and
reconstructs the exact original byte stream for the proxied request
afterwards (`io.MultiReader`), so it cannot affect what's forwarded.

Deployed live 2026-09-07 via `kubectl -n airgap-ai-stack set env
deployment/pat-service QOS_LOG_CLIENT_SHAPE=true` (an imperative override,
not yet the committed default — `k8s/base/applications.yaml` ships the
variable defaulting to `"false"`; a future `make helm-up` will reset the
live value to that committed default unless the override is reapplied,
which is intentional: this is meant to be temporary). Verified working
end-to-end with a real PAT call through the public origin:

```
qos client-shape: owner=fb5cebb4-7c71-4c34-b651-7210de9abcf6 user_agent="qos-verification-probe/1.0" headers=Accept,Authorization,Content-Length,Content-Type,User-Agent,X-Envoy-External-Address,X-Forwarded-For,X-Forwarded-Proto,X-Real-Ip,X-Request-Id body_keys=max_tokens,messages,model
```

**To pull results after a real work day of traffic:**
`kubectl -n airgap-ai-stack logs deployment/pat-service --since=24h | grep 'qos client-shape'`.
**To turn it off afterwards** (does not require a rebuild):
`kubectl -n airgap-ai-stack set env deployment/pat-service QOS_LOG_CLIENT_SHAPE-`
(unsets the override, falling back to the committed `"false"`) or roll out
`k8s/base/applications.yaml` normally. Do not leave it enabled past the
collection window — it is a standing (if narrow) diagnostic log of every
request's shape.

No results are reported here yet — only the instrumentation and its own
verification line above. Whoever collects the real sample should append the
per-client header/body-key patterns actually observed (Hermes, Pi Agent,
OpenCode, KiloCode) directly below this note, confirming or refuting §3's
claim that none of them can carry a session-identifying header.

## 0.7 — GPU cost-model calibration (v2 §4.6)

`cost = α · (prompt_tokens − cached_prompt_tokens) + β · completion_tokens`.
Calibrated 2026-09-07 against the live deployment (`qwen38-nvfp4`,
`--max-num-seqs 2`, prefix caching **off** — meaning every request below is a
genuine cold prefill, exactly what α needs; `cached_tokens` is `null` in
every response, consistent with 0.2). Fired sequentially, one request at a
time, directly at the EPP proxy port (bypasses pat-service/rate-limit —
synthetic calibration traffic, not real client traffic, so it correctly
does not appear in the §0.4 log).

**α (uncached prefill), `max_tokens=1` fixed, prompt length varied:**

| prompt_tokens | wall time |
| --- | --- |
| 112 | 0.267s |
| 562 | 0.278s |
| 2062 | 0.339s |
| 8062 | 1.017s |

Slope over the widest interval (112→8062 tokens): `(1.017−0.267)/(8062−112)
≈ 9.4×10⁻⁵ s/token` → **α ≈ 9.4×10⁻⁵ s/token** (≈10,600 tok/s prefill at
batch size 1). The narrower 562→2062 interval gives a noisier `4.1×10⁻⁵`,
consistent with a roughly constant ~0.25s fixed per-request floor (network +
scheduling + admission) dominating at small prompt sizes — see caveat below.

**β (decode), prompt fixed at 82 tokens, `max_tokens` varied:**

| completion_tokens (actual) | wall time |
| --- | --- |
| 10 | 0.375s |
| 50 | 0.985s |
| 150 | 2.472s |
| 198 (asked for 400; model stopped itself) | 3.193s |

Slope, two independent intervals agree closely: `(2.472−0.985)/(150−50) =
0.01487` and `(3.193−0.375)/(198−10) = 0.01499` s/token → **β ≈
0.0149 s/token** (≈67 tok/s decode at batch size 1). Cross-checked: `cost`
for a request is dominated by decode length far more than by prompt length
per token (β is ≈160× α) — matches the general shape of autoregressive
decode being the expensive phase per token, prefill being cheap per token
but paid once for the whole (uncached) prompt.

**Caveat, record before using these numbers**: extrapolating the β line back
to `completion_tokens=0` gives a ≈0.22s residual — bigger than the entire
prefill cost of an 82-token prompt (`82 × 9.4×10⁻⁵ ≈ 0.008s`). That residual
is network/queueing/admission overhead, constant per request regardless of
size. `cost = α·prompt + β·completion` deliberately does **not** include it,
because it's identical for every user and every request and therefore
irrelevant to *fairness between users* — but it means `cost` is not a
predictor of wall-clock latency by itself, only of relative GPU-second
consumption, which is what §4.1 needs it for.

## Stage 2 — prefix caching turned on (2026-09-07)

`helm/vllm-inference/values.yaml` `inference.prefixCaching: true`, deployed
with `make vllm-up`. Pod came up `1/1 Running` without crashing — this
hybrid Mamba/SSM checkpoint (`Qwen3_5ForConditionalGeneration`) **does**
support prefix caching on vLLM 0.27.1, contrary to the real risk the task
flagged. Two things vLLM did that our values did not ask for, both visible
only in the pod's own startup log, confirming the task's "не предполагай"
instruction was warranted:

- `WARNING config.py:618] Mamba cache mode is set to 'align' for
  Qwen3_5ForConditionalGeneration by default when prefix caching is enabled`
  — our `mambaCacheMode: none` is **silently overridden** to `align` the
  moment prefix caching turns on; vLLM itself calls Mamba-layer prefix
  caching support in `align` mode "experimental" in the very next log line.
  This is not a config we control today; noting it because it's exactly the
  kind of silent override that would otherwise go unnoticed.
- The model-name rename to `qwen-3.8-27b` (this session, unrelated to QoS)
  is confirmed live: `served_model_name=qwen-3.8-27b` in the engine's own
  startup line.

**Acceptance criteria, checked against `/metrics` and real requests, not
assumed:**

1. **Hit rate**: a 3-step repeated-long-prefix sequence plus one more call
   produced `vllm:prefix_cache_queries_total` 7707 →
   `vllm:prefix_cache_hits_total` 4704 (≈61%) from a cold cache. **Pass.**
2. **TTFT drops within a session**: same sequence, `time_starttransfer`
   1.307s (step 1, cold) → 0.672s (step 2) → 0.527s (step 3). **Pass.**
3. **`usage.prompt_tokens_details.cached_tokens` appears in the response**:
   checked directly on both `/v1/chat/completions` and `/v1/completions` —
   the field is present but its value is **`null` on every response**,
   including ones the metrics above prove hit the cache. **Fails, and this
   is an engine-version gap, not a config mistake**: the task's §3.2
   assumption ("vLLM и SGLang возвращают... cached_tokens") does not hold
   for vLLM 0.27.1's OpenAI-compatible endpoints on this build. Consequence
   for Stage 3.2: the "measure `cached_tokens`, correct the chain
   prediction" feedback loop **has no data source to read from** on vLLM
   today. The chain-hash prediction has to stand on its own (already the
   plan for streamed requests, per ADR 0008 — this makes it the plan for
   *all* requests, not a per-transport special case). Re-check this the
   moment `versions.lock.env`'s vLLM version changes; do not assume a vLLM
   upgrade fixes it without checking, and do not assume it stays broken
   either.

**Metric name correction**: the task and this repo's own
`docs/operations/vllm-inference.md` assumed `vllm:gpu_cache_usage_perc`.
The metric actually exported by this vLLM build is
**`vllm:kv_cache_usage_perc`** — checked directly against `/metrics`, not
assumed from either document.

**KV headroom for a `--max-num-seqs` decision** (task §2.5/Stage 2.5):
2 concurrent ~14,450-token requests (realistic long coding-agent turn, not
worst case) held `vllm:kv_cache_usage_perc` at a steady **0.13–0.15**
throughout. `--max-num-seqs 2` is a hard admission limit inside vLLM's own
scheduler — confirmed by firing 4 concurrent requests straight at vLLM
(bypassing EPP) and watching `num_requests_waiting` show 2 queued instantly
while `num_requests_running` stayed at 2 — so a true concurrent-4 KV reading
isn't obtainable without actually changing the flag and redeploying, which
this measurement step does not do.

Extrapolating current usage linearly to worst case (2 concurrent sessions
both at the full 84,672-token `max-model-len`) gives ≈0.88 — close to the
ceiling implied by `gpuMemoryUtilization: 0.90`, meaning **2 concurrent
max-length sessions is close to what this GPU/quantization/context budget
was actually sized for**, even though typical (~14k-token) sessions leave
substantial headroom.

**Recommendation: do not raise `--max-num-seqs` yet.** Two independent
reasons, not one: (a) the worst-case KV extrapolation above leaves little
margin once real max-length sessions are considered, and (b) more
fundamentally, raising slot count *before* Stage 3's pat-service admission
layer exists would hand a single flooding user more slots to grab
simultaneously, making today's actual complaint worse, not better — the
concurrency-detector's `maxConcurrency` would have to move in lockstep
(`config/llmd/router-nvfp4-values.yaml`) and round-robin fairness protects
even less well across more slots. Revisit this with fresh KV numbers once
Stage 3's own admission control is what's actually bounding concurrency per
user, not `--max-num-seqs` alone.

## Cost-model calibration cross-check

§0.7's independently-derived β (≈0.0149 s/token, ≈67 tok/s decode) matches
`docs/operations/vllm-inference.md`'s separately-measured decode speed
(66.26–71.59 tokens/s, measured 2026-09-04 at a shorter context and without
prefix caching) to within the same ballpark — two unrelated measurement
methods agreeing is a good sign neither is a fluke, though the Stage 2
change (prefix caching on) means α in particular should be re-verified
under concurrent realistic load before it's trusted for the scheduler,
per §0.7's own caveat.

**These numbers are tied to**: model `qwen38-nvfp4`, engine `vLLM`,
`--max-num-seqs 2`, prefix caching off, measured 2026-09-07 at low
concurrency (sequential single-flight calibration, not under the 3-4
concurrent users the real deployment sees). Re-calibrate whenever the model,
engine, quantization, or launch flags change, and ideally once more under
realistic concurrent load once Stage 2's prefix caching changes the
uncached-prefill baseline itself (an uncached-prompt token should cost
roughly the same either way, since α is defined on *uncached* tokens only —
but re-verify rather than assume).

## 0.4 re-run -- after the defaultPriorityBand fix (2026-09-09)

Same `scripts/qos-baseline-repro` scenario as §0.4, run against
`llmd-qwen-test` revision 7 (ADR 0008 Stage 1C: `flowControl.defaultPriorityBand`
now pins `round-robin-fairness-policy` on the bands provisioned from
`router.inferenceObjectives`). No rate-limit patch needed this time -- the
per-user limit is 60/min since Stage 3 step 1, and the scenario fires 5
requests.

| Request | HTTP | TTFT | Total |
| --- | --- | --- | --- |
| userA-4 | 200 | 5.733s | 5.735s |
| userA-2 | 200 | 5.733s | 5.735s |
| userA-3 | 200 | 5.735s | 5.736s |
| userB-solo | 200 | **5.755s** | **5.756s** |
| userA-1 | 200 | 9.982s | 9.984s |

User B's lone request is again served in the first wave rather than behind
the flood, matching §0.4's pre-regression reading (5.581s there). Four
requests rather than two clear the first wave because the engine changed:
SGLang's `--max-running-requests` is 3, where §0.4 ran vLLM at
`--max-num-seqs 2`.

**This run does not, on its own, demonstrate the fix works.** Two reasons,
both from the per-user metrics taken straight afterwards:

- The two throwaway users landed in *different* bands -- user A
  (`6b5fe4db-...`) 3 warm + 1 normal, user B (`47ef59dc-...`) 1 normal. User
  B was therefore alone in its band, so the intra-band flow round-robin that
  Stage 1C actually changed was never exercised on B's request. What B's
  5.755s shows is that it was not starved across bands, not that
  within-band fairness is restored.
- Five requests against three slots drain too fast to hold a band-10 queue
  non-empty, so the scenario cannot produce the cross-band starvation it
  would need to discriminate between the old and new fairness policy either.

The run is still worth recording: it went through the full real path
(pat-service assigned bands, EPP saw two distinct `fairness_id`s, 4 and 1
requests respectively) and it happened while the heavy production user
(`2d641ee3-...`, ~30 requests in the same window, predominantly warm) was
active again, so it is at least a live-contention sanity check that the
revision 7 config serves traffic correctly.

**What would actually discriminate**: the production per-user TTFT split
recorded in ADR 0008 Stage 1C (heavy user p50 0.66s against 27-56s for
others, measured 2026-09-08 under sustained multi-user load). Re-read the
per-user panels of `user-activity.json` after the next busy period with two
or more users sharing the warm band, and compare against those numbers. A
controlled A/B (roll back to revision 6, re-run, roll forward) was
deliberately not done: real users were active again by then, and two extra
EPP restarts to sharpen an experiment the production data will answer
anyway is not a trade worth making.
