# 0015: Metrics contract for third-party inference engines (llama.cpp, ninfer, ...)

## Status

Proposed.

## Context

[ADR 0006](0006-pluggable-inference-backends.md) defines how a new engine gets
reachability (`Backend` / `AIServiceBackend` / an exact-match rule on
`AIGatewayRoute/llmd`) but says nothing about observability. Today only vLLM
and SGLang are first-class from a metrics standpoint:

- `config/grafana/dashboards/vllm.json` and `fair-share.json` hardcode
  `vllm:*` / `sglang:*` Prometheus metric names (`time_to_first_token_seconds`,
  `inter_token_latency_seconds`, `generation_tokens_total`,
  `num_requests_running`, `num_requests_waiting`, `kv_cache_usage_perc`), as
  do the vendored `config/grafana/dashboards/llm-d/*.json` dashboards.
- `k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml` only
  scrapes `vllm-qwen38-nvfp4`, `sglang-qwen38`, `embeddings-bge-m3`,
  `llmd-qwen-test-epp`, `pat-service`, `node`, and two `gpu-exporter` targets.
  There is no scrape job for llama.cpp at all, and `deploy/llamacpp/run` does
  not pass any metrics-enabling flag to the container -- confirmed by reading
  both files. llama.cpp today has zero Prometheus surface, by omission, not
  by a documented decision.
- llm-d's `core-metrics-extractor` plugin (`config/llmd/router-nvfp4-values.yaml`)
  takes a `defaultEngine` parameter this repo has only ever set to `sglang`
  or left to auto-derive to `vllm`. Whether the installed llm-d chart
  recognizes any other value is unverified here; `metrics-data-source` and
  the KV/queue scorers that key off it assume the `vllm:`/`sglang:`
  Prometheus shape. This is *why* llama.cpp and the external API stay on
  the direct-`AIServiceBackend`, bypass-EPP pattern documented in
  [docs/operations/inference-backends.md](../operations/inference-backends.md)
  instead of joining the shared `qwen-3.8-27b` route -- not a limitation
  this ADR can lift.
- pat-service's own accounting (`pat-service/cmd/pat-service/main.go`
  `recordUsage`, `pat-service/internal/qos/metrics.go`) is already
  engine-agnostic: it reads the OpenAI-schema `usage` object out of a
  non-streaming chat-completion (or embeddings) response body and feeds
  `patsvc_prompt_tokens_total`, `patsvc_completion_tokens_total`,
  `patsvc_cached_prompt_tokens_total`, `patsvc_embedding_tokens_total`, and
  `qos.RecordCost`'s spend accounting from it. Nothing here is vLLM/SGLang
  specific; it works for any OpenAI-compatible engine today, including
  llama.cpp, subject to the same limits already documented for vLLM/SGLang
  (streaming bodies are never parsed; a body over `QOS_USAGE_MAX_BODY_BYTES`,
  default 1 MiB, is proxied unread).

Adding llama.cpp, ninfer, or any other engine via ADR 0006's pattern needs a
clear line between "required for the stack to work at all", "required for
Grafana to show anything", and "nice to have, degrades one panel at a time"
-- and an explicit acknowledgment that this repo has not read llama.cpp's or
ninfer's source, so it cannot hardcode their native metric names the way it
hardcodes `vllm:`/`sglang:` today.

## Decision

Split "metrics the stack accepts" into three tiers. Tiers 1 and 2 are
mandatory for any engine added through ADR 0006's direct-backend pattern;
Tier 3 is optional and fails one Grafana panel at a time, never the request
path.

### Tier 1 -- OpenAI-schema `usage` (mandatory, protocol-level)

The engine's non-streaming `/v1/chat/completions` (and `/v1/embeddings`,
where offered) response body must include:

```json
{"usage": {"prompt_tokens": 0, "completion_tokens": 0}}
```

- `usage.prompt_tokens_details.cached_tokens` is optional. If absent,
  `patsvc_cached_prompt_tokens_total` simply stays at zero for that engine's
  traffic -- the same gap already documented for the current vLLM build
  (`pat-service/internal/qos/session.go`, ADR 0008 "Verifying the
  prediction"), not a new one.
- Streaming responses (`Content-Type: text/event-stream`) are never parsed
  by `recordUsage`, for any engine. Adding a new engine does not change this.
- The full body must arrive within `QOS_USAGE_MAX_BODY_BYTES` (default
  1 MiB) or it is proxied through unread, silently skipping the spend update
  for that one response.

**If Tier 1 is missing or malformed:** the request still proxies
successfully (recordUsage fails open), but `patsvc_*_tokens_total`,
`patsvc_cost_units_total`, and `qos.Tracker`'s spend-based band demotion
never see that engine's traffic. Since llama.cpp/ninfer reach the stack via
the direct-backend pattern (already outside llm-d's fair-share queue per
`inference-backends.md`), this only blinds the QoS/cost Grafana panels and
any future quota policy -- it does not change request admission for these
engines, because they are not admitted through `qos.Tracker` at all today
for the direct-backend path used by non-EPP engines. Do not skip Tier 1: it
is the one tier with zero effort cost (every serious OpenAI-compatible
server implements `usage`) and the only tier that feeds accounting.

### Tier 2 -- Prometheus scrape target (mandatory for any visibility)

Add a job to `k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`
mirroring the existing `sglang-qwen38` entry:

```yaml
- job_name: llamacpp
  metrics_path: /metrics
  static_configs:
    - targets: [host.k3d.internal:8090]
      labels:
        namespace: airgap-ai-stack
```

The minimum bar is that `/metrics` returns 200 with at least the process
being scrapeable -- that alone populates `up{job="llamacpp"}` and lights up
`cluster-monitor.json`'s "Component health" / "Scrape target up state" /
"Target availability" panels for that engine.

**If Tier 2 is missing:** the engine is invisible to Prometheus entirely.
No alert can fire on it being down; no dashboard, including ones unrelated
to inference (cluster-wide "Target availability"), lists it.

### Tier 3 -- engine-native inference metrics (optional, per-panel)

Everything the vLLM/SGLang dashboards actually chart beyond raw liveness:

| Signal | Feeds | vLLM/SGLang metric name used today |
| --- | --- | --- |
| TTFT histogram | "TTFT p50/p95", "System TTFT (avg)" | `*_time_to_first_token_seconds_bucket/_sum/_count` |
| Inter-token latency histogram | "TPOT p50/p95", "System TPOT (avg)" | `*_inter_token_latency_seconds_bucket/_sum/_count` |
| Generated-token counter | "System-wide output tokens/sec" | `*_generation_tokens_total` |
| In-flight / queued request gauges | saturation and drilldown dashboards | `*_num_requests_running`, `*_num_requests_waiting` |
| KV-cache occupancy gauge | saturation and drilldown dashboards, and (only on the shared EPP route) the `kv-cache-utilization-scorer` plugin | `*_kv_cache_usage_perc` |

This repo has not read llama.cpp's or ninfer's source or a live `/metrics`
dump from either, so it does not assert that either one exposes any of
these under a `llamacpp:` or `ninfer:` prefix, or at all. Do not assume the
table's names transfer -- verify against the running container:

```sh
curl -s http://127.0.0.1:8090/metrics | less
```

Some servers only expose Tier 3 metrics behind an explicit flag (a
`--metrics`-style opt-in); `deploy/llamacpp/run` does not currently pass one,
which is why llama.cpp has no Tier 3 (or Tier 2) data today even once a
model is wired up.

**If Tier 3 is partially or fully missing:** each missing metric fails only
the one or two panels that read it, independently:

- No histograms -> latency percentile panels stay "No data"; token/sec and
  saturation panels are unaffected if their own metrics exist.
- No queue/KV gauges -> saturation and drilldown panels stay "No data"; on
  the direct-backend pattern this has no scoring effect, since EPP never
  sees these engines regardless.
- Nothing at all -> the engine gets no inference-specific panel, only the
  Tier 2 up/down signal. Chat completions still work end-to-end; only
  observability is reduced.

No Tier 3 metric feeds pat-service's accounting or the AI Gateway's routing.
It is pure observability.

## Instructions: connecting a new provider

1. **Verify Tier 1.** `curl` a real non-streaming chat completion against
   the engine directly (bypassing the gateway) and confirm `usage.prompt_tokens`
   / `usage.completion_tokens` are present and non-zero.
2. **Wire reachability per ADR 0006**: add the engine's `Backend`,
   `AIServiceBackend`, and exact-match rule to
   `helm/airgap-stack/templates/inference-backends.yaml`, gated behind a new
   `inference.<name>.enabled` value, following the llama.cpp/external-API
   shape already there. This repo cannot put a third-party engine on the
   shared `qwen-3.8-27b` EPP route (see Context) -- it always gets its own
   model name and its own rule.
3. **Add the Tier 2 scrape job** to `prometheus-config-patch.yaml` as shown
   above, using whatever host/port the engine actually listens on
   (`host.k3d.internal:<port>` for a host-run Docker container, per the
   existing llama.cpp/SGLang pattern).
4. **Inventory Tier 3** by hitting the engine's own `/metrics` (enabling
   whatever flag exposes it, if any) and comparing what it emits against the
   table above. Do not force a mismatch: if the engine emits counters but no
   histograms, only replicate the counter-fed panels.
5. **Build the engine its own dashboard file** under
   `config/grafana/dashboards/` (e.g. `llamacpp.json`), wired into
   `Makefile`'s `--set-file grafana.dashboards....` chain the same way
   `vllm.json` and `fair-share.json` are. Do not add panels for a new engine
   into `vllm.json`/`fair-share.json` themselves -- those stay the
   vLLM/SGLang-specific contract this ADR does not touch.
6. **Never set `core-metrics-extractor.defaultEngine`** (or otherwise try to
   route the new engine through llm-d EPP) to an unverified value. Probe the
   installed chart the way [ADR 0012](0012-graduated-band-ceiling-over-strict-priority.md)
   probed `allow-experimental-plugins` -- by checking the running pod's
   accepted config, not by assuming. An unrecognized `defaultEngine` value
   should be expected to fail EPP startup outright; if the value is not
   recognized, leave the engine on the direct-backend pattern from step 2.
7. **Document what was skipped.** If Tier 3 (or part of it) is not
   available, say so next to the engine's own README, the way
   `deploy/llamacpp/README.md` already documents "not deployed today" --
   not as a silent gap future operators have to rediscover.

## Consequences

- Tier 1 is the only tier with any effect on accounting, cost, or QoS.
  Tiers 2 and 3 are pure-observability and never affect request admission
  or routing for an engine reached via ADR 0006's direct-backend pattern.
- `vllm.json` and `fair-share.json` stay a stable, vLLM/SGLang-only
  contract; a third engine always gets its own dashboard file rather than
  edited shared panels.
- No third-party engine can join the shared EPP `qwen-3.8-27b` route's
  fair-share/queueing unless the installed llm-d chart's
  `core-metrics-extractor` recognizes its engine type -- an upstream llm-d
  constraint, not something this repo's Helm values can work around. Until
  then, llama.cpp, ninfer, and any future third engine stay on the
  direct-backend, bypass-EPP path, with the fair-share/priority-band
  guarantees of [ADR 0008](0008-per-user-fair-share.md) and
  [ADR 0012](0012-graduated-band-ceiling-over-strict-priority.md) not
  applying to their traffic.
- This ADR does not hardcode `llamacpp:`/`ninfer:` metric names, unlike the
  `vllm:`/`sglang:` names baked into today's dashboards -- those must be
  confirmed against each engine's live `/metrics` output before a query is
  written. Treat the Tier 3 table above as this repo's contract for what a
  panel *would* consume, not as a claim about what any specific third-party
  engine emits.
