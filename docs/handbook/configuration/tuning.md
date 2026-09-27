# Tuning limits and queues

[Русский](tuning.ru.md) | [Handbook index](../README.md)

How to decide, from metrics, whether to change a rate limit, a queue setting,
a band coefficient or an engine's capacity, and how to apply the change. The
concepts are on [queues and fair share](../inference/queues-and-fair-share.md)
and [KV cache](../inference/kv-cache.md); this page is the operator's loop.

**Contents**

- [The knobs](#the-knobs)
- [Rate limits](#rate-limits)
- [Queues and bands](#queues-and-bands)
- [pat-service QoS coefficients](#pat-service-qos-coefficients)
- [Engine capacity](#engine-capacity)
- [Signal to knob](#signal-to-knob)
- [Change workflow](#change-workflow)
- [Where it lives](#where-it-lives)
- [Related](#related)

## The knobs

Start every tuning session on **QoS / Cluster Load & Queue Tuning** in
Grafana: its rows follow this page (verdict, saturation, bands, fairness,
limit sizing, pat-service knobs, blind spots).

| Layer | Knob | Current | File | Apply |
| --- | --- | --- | --- | --- |
| Gateway | per-user requests/min, model route | 60 | `helm/airgap-stack/values.yaml` `inference.perUserRateLimitPerMinute` | `make helm-up` |
| Gateway | per-user requests/min, embeddings | 120 | `inference.embeddings.perUserRateLimitPerMinute` | `make helm-up` |
| Edge | per-IP requests/min on `/v1` | 60 | `helm/airgap-stack/templates/routes-external.yaml` `edge-api-ip-rate-limit` | `make helm-up` |
| Edge | per-IP on `/sso`, token endpoint | 100, 10 | same file | `make helm-up` |
| pat-service | MCP tool calls/min per user and server | 30 | `k8s/base/applications.yaml` `MCP_CALLS_PER_MINUTE` | `make helm-up` |
| EPP | queue size, bytes, wait TTL | 32, 2 GiB, 10 min | `config/llmd/router-nvfp4-values.yaml` `flowControl` | `make llmd-up` |
| EPP | saturation thresholds | queue 5, KV 0.8 (plugin defaults) | same, `utilization-detector` `parameters` | `make llmd-up` |
| EPP | scorer weights | queue 2, KV 2, prefix 3 | same, `schedulingProfiles` | `make llmd-up` |
| pat-service | warm window, demotion streak, spend | 120 s, 8, 5.0 / 600 s | `QOS_*` env (defaults in `main.go`) | `make helm-up` |
| Engine | concurrent sequences, context, memory | vLLM 2 / 131072 / 0.94; SGLang 3 / 180224 / 0.90 | `helm/<engine>-inference/values.yaml` | `make <engine>-up` |

## Rate limits

All request limits are Envoy `BackendTrafficPolicy` global rate limits backed
by Valkey, counted per `X-User-Id` (per user, not per token) or per source
IP. Above the limit Envoy answers 429 with `X-RateLimit-*` headers. A change
applies through xDS without restarting pods.

- The per-user limit is a **storm guard**, not a concurrency control. It was
  3/min only during the early fair-share measurements; 60/min is the value
  for real agent traffic. Concurrency is decided by the engine and EPP.
- The per-IP edge limit counts every user behind one NAT address together.
  If users share an egress IP, it binds before the per-user limit does.
- There is no token-based quota. `QOS_MONTHLY_LIMIT` is a dashboard figure,
  not enforced.
- Different limits per role are not implemented; the policy matches every
  user alike.

## Queues and bands

- **Queue fills and requests expire:** the offered load is above what the GPU
  serves. Raising `defaultRequestTTL` only makes users wait longer; lowering
  the per-user limit or adding capacity addresses it.
- **Saturation sits at 1 while the GPU is not busy:** the thresholds are too
  tight for the traffic shape. Raise `queueDepthThreshold` or
  `kvCacheUtilThreshold` as plugin `parameters`; ADR 0012 records why 0.8
  stayed (the KV pressure seen was real), so verify with `KV utilization p95
  vs 0.8` first.
- **A lower band gets nothing:** check `Time above normal/demoted ceiling`.
  The graduated ceiling should give it a shrinking share, not zero; a flat
  zero points at stale endpoints or a broken band provisioning.
- **Band 0 is large:** that is Open WebUI and anything bypassing pat-service
  (`Fallback-band (default-flow) share`). It is the lowest band by design.
- The band priorities (10, 5, 1) need no tuning: the ceiling policy uses rank
  only.

## pat-service QoS coefficients

Set as env on the pat-service Deployment in `k8s/base/applications.yaml`;
unset ones use the defaults in `loadConfig`
(`pat-service/cmd/pat-service/main.go`).

| Env | Default | Raise when | Lower when |
| --- | --- | --- | --- |
| `QOS_WARM_TTL_SECONDS` | 120 | agents pause longer between steps but their prefix is still cached (prefix hit ratio stays high after 120 s) | warm band takes most dispatches and starves new sessions |
| `QOS_DEMOTE_AFTER_STEPS` | 8 | `Session steps per rotation vs 8` shows normal agent runs demoted mid-task | one long session blocks others in `warm` |
| `QOS_SPEND_DEMOTE_THRESHOLD` | 5.0 | light users get demoted by small spend gaps | one heavy user keeps `normal` while others wait |
| `QOS_SPEND_WINDOW_SECONDS` | 600 | spend spikes are short and noisy | demotion should react faster |
| `QOS_COST_ALPHA_PER_TOKEN`, `QOS_COST_BETA_PER_TOKEN` | 9.4e-5, 0.0149 | `Cost units/s vs GPU busy fraction` drifts: re-fit so cost tracks GPU time | same |
| `QOS_SESSION_TTL_SECONDS` | 1800 | long pauses split one task into several sessions | sessions merge unrelated work |

These change labels only; the queue itself is EPP's. A change rolls the
pat-service pod.

## Engine capacity

`maxNumSeqs` (vLLM) and `maxRunningRequests` (SGLang) are the real
concurrency. Raising them helps throughput for short prompts and hurts when
long prompts preempt each other; the pool is fixed by the card
([KV cache](../inference/kv-cache.md#trading-context-for-concurrency)). Rules:

- Change one engine value at a time; `make <engine>-up` restarts only that
  pod (minutes of downtime while the model loads).
- With GPU embeddings on, any increase of `gpuMemoryUtilization` or
  `memFractionStatic` must leave `gpu.gpuMemoryBudgetGiB` free; restart the
  embeddings pod after the engine.
- Re-measure under the same load (see `tests/qos`, `tests/inference`) and
  record the numbers in the change.

## Signal to knob

| Signal (dashboard / metric) | Meaning | Knob |
| --- | --- | --- |
| Many 429s: `patsvc_requests_total{outcome="rejected"}`, envoy-gateway rate-limit panels | users hit the per-user or per-IP limit | raise `perUserRateLimitPerMinute` if the GPU has headroom; check NAT for the IP limit |
| `Peak requests/min per user vs quota` near the limit, GPU idle | limit too low | raise the per-user limit |
| `llm_d_epp_flow_control_queue_size` > 0 for long, `Non-dispatch outcomes` > 0 | overload, requests expire | lower per-user limit, or add capacity; not TTL |
| `Saturation (effective)` = 1, `GPU busy` low | thresholds too tight | `utilization-detector` parameters |
| `vllm:num_requests_running` below `maxNumSeqs`, `vllm:kv_cache_usage_perc` < 0.4 in busy hours | GPU underused | raise the per-user limit, or engine slots |
| `vllm:num_preemptions_total` rising | long contexts fight for KV | fewer slots or shorter context |
| `TTFT p95 spread (worst / best user)` wide, `Top-1 user share` high | one user dominates | spend and streak coefficients |
| `Warm-band share per user` near 1 for everyone | warm window too long | lower `QOS_WARM_TTL_SECONDS` |
| `Session match ratio` low | clients do not resend history, or bodies exceed the fingerprint cap | nothing to tune; sessions are best-effort |
| `llm_d_epp_flow_control_stale_endpoints` > 0 | EPP cannot read engine metrics, dispatch stopped | not a tuning issue: engine label or selector, see [engines](../engines/README.md) |
| MCP 429s: `patsvc_mcp_calls_total{status="429"}` | tool-call storm | `MCP_CALLS_PER_MINUTE` |

## Change workflow

1. Record the baseline: at least 30 minutes of working-hours traffic on the
   Cluster Load dashboard (export or screenshot the panels you will compare).
2. Change one value, in the repository, never with `kubectl edit`.
3. `make verify`, then the target from the table above.
4. Watch at least 15 minutes. Keep the change if TTFT did not grow and
   nothing expires; otherwise revert or take a smaller step.
5. Commit with the old value, the new value and the reason (the signal you
   saw). A change that sets a new operating policy deserves an ADR.

Prometheus keeps about 15 days ([retention](../observability/data-and-retention.md)),
so write the numbers down in the commit or ADR.

## Where it lives

- Rate limits: `helm/airgap-stack/templates/llmd.yaml`, `embeddings.yaml`,
  `routes-external.yaml`
- EPP: `config/llmd/router-nvfp4-values.yaml`
- pat-service: `k8s/base/applications.yaml`, `pat-service/internal/qos/`
- Dashboards: `config/grafana/dashboards/cluster-load.json`,
  `user-activity.json`, `fair-share.json`
- Measurements and load tests: [tests/qos](../../../tests/qos/README.md)

## Related

- Previous: [Configuration](README.md). Next: [Agents](../agents/README.md)
- [ADR 0008](../../adr/0008-per-user-fair-share.md), [ADR 0012](../../adr/0012-graduated-band-ceiling-over-strict-priority.md)
