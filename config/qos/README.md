# QoS fair share

There is no separate scheduler service, and there should not be one --
see `docs/adr/0008-per-user-fair-share.md` and `TASK-qos-fair-share.md` §2
for why the queue belongs in llm-d, not here.

Roles, per `TASK-qos-fair-share.md` §2:

- **llm-d's EPP** owns the queue and per-user fairness. Fairness id is the
  Keycloak `sub` (`X-Llm-D-Inference-Fairness-Id`, round-robin between
  users); the priority band comes from an `InferenceObjective` object named
  by `x-llm-d-inference-objective`. `config/llmd/router-nvfp4-values.yaml`
  sets `inferencePool.create: true` and defines the fixed `warm`/`normal`/
  `demoted` objectives (`ADR 0008` "Stage 1B") -- `InferencePool` here is
  only the namespace anchor EPP resolves objectives against, never the
  `AIGatewayRoute` backend (ADR 0002's routing model is unchanged).
- **Envoy AI Gateway** owns limits: `QuotaPolicy` (token budget per window)
  and usage-based rate limiting (frequency) on the inference route.
- **`pat-service`** (`internal/qos`) is not a scheduler; it computes what
  llm-d and the Gateway cannot derive themselves -- session identity (a
  server-side prefix-hash chain, since clients can't be relied on to send
  a session header, ADR 0008 "Session identity") and, from that, which of
  the three bands a request belongs in. It never queues, blocks, or reorders
  a request; a Valkey failure degrades the band to `normal`, never the
  proxied call (ADR 0008 "Stage 3, steps 1-3").

What's implemented: chain-hash session matching, warm/demoted band
assignment, and cost-based band modulation (`pat-service/internal/qos`),
all validated live against real traffic. Cost modulation is deliberately
queue-only, per explicit customer direction -- it never blocks or limits a
request, only demotes a user's priority band once they run far enough
ahead of other active users on measured spend (`QOS_SPEND_DEMOTE_THRESHOLD`);
see ADR 0008 "Stage 3, step 4". A user-activity Grafana dashboard
(`config/grafana/dashboards/user-activity.json`) now covers token
consumption, TTFT and TPOT by reading llm-d EPP's own per-`fairness_id`
histograms directly, plus pat-service's own `patsvc_cost_units_total` and
token counters -- see ADR 0008 "Stage 4". What's open: a hard per-user
concurrency ceiling in `pat-service` (only if measurement ever shows
llm-d's round-robin insufficient, and out of scope for now by the same
customer direction -- see ADR 0008 "Known limitations"), the ClickHouse
event log (`TASK-qos-fair-share.md` Stage 4's `qos.requests`/`qos.sessions`,
deliberately deferred this round, see ADR 0008 "Stage 4"), and cost/token
accounting for streaming responses (pat-service's own counters are
non-streaming only today; EPP's token histograms already cover streaming
traffic, only the GPU-cost view does not).
