# Queues and fair share

[Русский](queues-and-fair-share.ru.md) | [Handbook index](../README.md)

When the GPU is busy, llm-d EPP decides who goes next. It keeps one queue per
priority band and, inside a band, one queue per user, and it dispatches by a
saturation signal from the engine's metrics. pat-service only labels
requests; the decisions are EPP's ([ADR 0008](../../adr/0008-per-user-fair-share.md),
[ADR 0012](../../adr/0012-graduated-band-ceiling-over-strict-priority.md)).

**Contents**

- [The model in one picture](#the-model-in-one-picture)
- [Bands: there are four, not one](#bands-there-are-four-not-one)
- [Fairness inside a band](#fairness-inside-a-band)
- [Saturation: when EPP holds requests back](#saturation-when-epp-holds-requests-back)
- [Between bands: the graduated ceiling](#between-bands-the-graduated-ceiling)
- [Queue limits](#queue-limits)
- [Scoring](#scoring)
- [What to watch](#what-to-watch)
- [Where it lives](#where-it-lives)
- [Related](#related)

## The model in one picture

```
request --(x-llm-d-inference-objective, x-llm-d-inference-fairness-id)--> EPP
                                                                           |
   band warm (10)    [user A queue][user B queue]   round-robin between users, FCFS inside
   band normal (5)   [user A queue][user C queue]
   band demoted (1)  [user B queue]
   band 0 (no header)[Open WebUI ...]
                                                                           |
   dispatch loop: highest band first, gated by the ceiling policy while saturated
                                                                           v
                                                               engine pod (one)
```

Requests are always accepted into the queue (up to the limits below). The
questions are only *when* and *in what order* they leave it.

## Bands: there are four, not one

`config/llmd/router-nvfp4-values.yaml` lists one band under
`flowControl.priorityBands` (`priority: 0`), but the running EPP has four.
Bands 10, 5 and 1 are provisioned at runtime from the `InferenceObjective`
objects named in `router.inferenceObjectives`, resolved in the
`InferencePool`'s namespace. `router.inferencePool.create: true` exists only
as that anchor; the gateway route never points at the `InferencePool`.

| Band | Priority | Who lands here |
| --- | ---: | --- |
| `warm` | 10 | a PAT session dispatched within the last 120 s: its KV cache is probably still on the GPU, so serving it first is cheap |
| `normal` | 5 | a new or cooled-down PAT session; any request when pat-service cannot reach Valkey |
| `demoted` | 1 | a user running a long streak in one session, or spending far more than the other active users ([rules](../pat-service.md#priority-band-labelling)) |
| fallback | 0 | requests without the objective header: Open WebUI chats, anything that did not pass pat-service |

Higher priority is served first, so band 0 sits **below** `demoted`.
Negative priorities are left free on purpose, for a future background class.
EPP logs `"Provisioning priority band from control plane"` for 1, 5 and 10 at
start, and exports `llm_d_epp_flow_control_*` series for all four.

A client cannot choose its band: the header value must name an existing
`InferenceObjective`, and pat-service strips client copies.

## Fairness inside a band

Every band uses `round-robin-fairness-policy` between flows (a flow is one
`fairness_id`, i.e. one Keycloak user) and `fcfs-ordering-policy` inside a
flow. Band 0 sets them explicitly; bands 10, 5 and 1 inherit
`flowControl.defaultPriorityBand`. Without that default they fell back to a
global strict policy that let one user starve the others inside a band
(ADR 0008 Stage 1C).

Open WebUI requests carry no fairness id, so inside band 0 they share one flow.

## Saturation: when EPP holds requests back

EPP dispatches freely until the `utilization-detector` reports the pool
saturated. It reads the engine's `/metrics` (queue depth and KV-cache
utilisation, mapped per engine by `core-metrics-extractor` from the pod label
`llm-d.ai/engine-type`) with the plugin defaults:

- `queueDepthThreshold: 5` waiting requests in the engine
- `kvCacheUtilThreshold: 0.8` of the KV cache in use

Both can be set as `parameters` on the plugin. ADR 0012 decided not to raise
`kvCacheUtilThreshold`: the KV pressure that reached it on busy days was real.

If EPP cannot read an endpoint's metrics, the endpoint is **stale** and EPP
fails closed: nothing is dispatched to it. That is what the Grafana alert
`adr0012-stale-endpoints` (`llm_d_epp_flow_control_stale_endpoints > 0`)
catches. A wrong `modelServers` selector or engine label causes exactly this;
`make engine-up` keeps them consistent.

## Between bands: the graduated ceiling

Strict priority would let `warm` traffic starve `normal` and `demoted`
forever under load. `soft-reflective-ceiling-policy`
(`flowControl.usageLimitPolicyPluginRef`) gates bands by rank instead:

```
ceiling[i] = 1 - i * saturation / (N - 1)      i = 0 for the highest active band, N active bands
```

A band below its ceiling is open. A band at or past it is opened on some
dispatch cycles and closed on others (period `round(s / (1 - s))`), so lower
bands get a shrinking share instead of zero. At saturation 1.0 every band
waits. The policy only looks at band order, so the numeric priorities 10, 5,
1 need no tuning. It is an Alpha plugin in the pinned EPP build and needs
`router.epp.flags.allow-experimental-plugins: true`.

## Queue limits

| Setting | Value | Effect |
| --- | --- | --- |
| `flowControl.maxRequests` | 32 | requests held by EPP in total (queued); above it new requests are rejected |
| `flowControl.maxBytes` | 2 GiB | total body size held |
| `flowControl.defaultRequestTTL` | 10 min | a request waiting longer is dropped with an error |
| `priorityBands[0].maxRequests` | 32 | per-band cap for band 0 |
| `proxy.failOpen` | false | if EPP is down, requests fail instead of bypassing the queue |

## Scoring

After flow control releases a request, the scheduling profile scores the
candidate endpoints: `queue-scorer` (weight 2), `kv-cache-utilization-scorer`
(2) and `prefix-cache-scorer` (3, prefers the endpoint that already holds the
prompt's prefix). With one engine replica the choice is trivial; the weights
matter when more replicas exist.

## What to watch

| Question | Metric (EPP `:9090/metrics`, Prometheus job `llmd-epp`) | Dashboard |
| --- | --- | --- |
| Is anything queued, in which band | `llm_d_epp_flow_control_queue_size{priority}` | llm-d / Inference Gateway, QoS / Cluster load |
| How long do requests wait | `llm_d_epp_flow_control_request_queue_duration_seconds` | same |
| Is the pool saturated | `llm_d_epp_flow_control_pool_saturation` | QoS / User activity |
| Per-user latency | `llm_d_epp_request_ttft_seconds{fairness_id}` | QoS / User activity |
| Stale endpoints (fail closed) | `llm_d_epp_flow_control_stale_endpoints` | alert `adr0012-stale-endpoints` |
| Which bands pat-service assigns | `patsvc_requests_total{band}` | QoS / Fair share |

Some dashboards still use the deprecated `inference_extension_*` names; both
exist in this EPP build. Turning these readings into changes:
[tuning](../configuration/tuning.md#queues-and-bands).

## Where it lives

- EPP config: `config/llmd/router-nvfp4-values.yaml`, applied by `make llmd-up`
- Band assignment: `pat-service/internal/qos/session.go`
- Decision history: [ADR 0008](../../adr/0008-per-user-fair-share.md)
  (stages 1A-4), [ADR 0012](../../adr/0012-graduated-band-ceiling-over-strict-priority.md)
- Load tests: [tests/qos](../../../tests/qos/README.md)

## Related

- Previous: [Inference](README.md). Next: [KV cache](kv-cache.md)
- [PAT service: band labelling](../pat-service.md#priority-band-labelling)
- [Tuning](../configuration/tuning.md)
