# Official vLLM dashboard

`vllm.json` is vendored from the [official Prometheus/Grafana example](https://docs.vllm.ai/en/stable/examples/observability/prometheus_grafana/).

- Repository: `vllm-project/vllm` (Apache-2.0).
- License: [LICENSE.vllm](LICENSE.vllm).
- Commit: `8a728663c1c3eeace834a95f5654fa653cc1998c`.
- Source: https://raw.githubusercontent.com/vllm-project/vllm/8a728663c1c3eeace834a95f5654fa653cc1998c/examples/observability/prometheus_grafana/grafana.json
- Original SHA-256: `651f1cf353024072197214df101a6f641839099736939b22b2f46b11c9faf527`.

All 12 upstream panels and their queries are unchanged. Local adaptations:

- Clear the database-specific `id` and use stable dashboard UID `vllm`.
- Select the provisioned datasource UID `vllm-prometheus` by default and limit
  the datasource selector to `vLLM Prometheus`.
- Default the model selector to `qwen-3.8-27b`; discover model names from
  `vllm:num_requests_running`.
- Show the last hour and refresh every 15 seconds.

Install or update with `make monitoring-up`. This passes the local JSON through
`--set-file grafana.dashboards.vllm.vllm.json=...` to the existing, pinned
`eg-addons` Helm release. The Grafana subchart creates
`monitoring/grafana-dashboards-vllm`, mounts it, and provisions the `vLLM` folder.
Its checksum annotation rolls Grafana when the JSON changes. No dashboard
download is required from inside the cluster.

Provider and datasource configuration live in
`config/gateway-addons/values.yaml`. The separate datasource connects to
`http://prometheus.airgap-ai-stack.svc.cluster.local:9090`, where the existing
remote overlay scrapes vLLM every 15 seconds. The add-on's original Prometheus,
Tempo, Loki, and Envoy dashboards remain available.

Open http://grafana.gpu-host.local:32030/d/vllm/vllm after signing in through SSO.
The dashboard is managed in Git; edit this JSON and run `make monitoring-up`
to persist changes. `make gateway-up` also invokes `monitoring-up`.

For an air-gapped Helm client, supply an unpacked copy of chart v1.8.1 with its
dependencies: `make monitoring-up MONITORING_CHART=/path/to/gateway-addons-helm`.

Validation and measured inference performance are recorded in
`docs/operations/vllm-inference.md`.

## cluster-monitor

`cluster-monitor.json` (uid `cluster-monitor`) is maintained in this repo. It is
a cluster/overview monitor for the single-node k3d profile, built only from
series the airgap Prometheus actually scrapes:

- **Node** (node-exporter, job `node`): CPU utilization, memory utilization,
  load average versus CPU cores, memory used/available, network traffic, and
  filesystem used.
- **Component health** (all scrape jobs): a `Target availability` stat (share
  of the profile's targets currently up) and a time series of `up{job=...}`
  per job so historical collector outages are visible across
  prometheus / node / vllm / sglang / llmd-epp / pat-service / the two GPU
  exporters.

`make monitoring-up` drops it into the `cluster-monitor` folder with the same
provider + `--set-file` wiring as the other self-maintained dashboards.

**Why this rather than the classic Grafana #315?** The bundled Prometheus only
scrapes static targets (see
`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`). It does
**not** scrape kube-state-metrics or the kubelet/cAdvisor, so #315's kube_*
and container_* panels (the bulk of it) would render empty. This dashboard
covers the Kubernetes-relevant node view plus what this profile can actually
observe. If workload/namespace pod panels are wanted later, that requires
adding a kube-state-metrics scrape and (optionally) cAdvisor — out of scope
here.

## llm-d

`llm-d/` vendors the seven Grafana dashboards from the [official llm-d
repository](https://github.com/llm-d/llm-d/tree/main/guides/recipes/observability/grafana/dashboards),
at commit `080c14d957755da4cc363745638619fc748558f1` (Apache-2.0; see
`llm-d/LICENSE`). `make monitoring-up` adds their ConfigMap to the `llm-d`
folder of the same Grafana instance.

The dashboard queries are unchanged. The provisioned copies remove only the
database IDs, bind the datasource selector to `vLLM Prometheus`, and default
the model and namespace selectors to `qwen-3.8-27b` and `airgap-ai-stack`.
The P/D dashboard required an equivalent provisioned datasource variable: its
upstream JSON contains an import-time `__inputs` datasource placeholder instead
of a dashboard variable.

The remote Prometheus static targets add the `namespace=airgap-ai-stack` label
to vLLM and EPP series so the official namespace filters work. The vLLM and EPP
dashboards receive data from this profile. SGLang and prefill/decode dashboards
are installed for feature parity, but show no workload data until those serving
profiles are deployed. This EPP build has no `llm_d_epp_request_error_total`
series, so the error-rate panels show no data until the upstream metric is
emitted.

Future usage dashboards still depend on implementing the canonical `ai_usage`
ClickHouse schema and gateway metric names.

## system-state

`system-state.json` is maintained in this repo (uid `system-state`). It renders
the current state of the node and the GPU:

- **Host** (node-exporter, job `node`): CPU utilization, load average, memory
  used/available, host temperature (empty on WSL2 — no real hwmon sensors are
  exposed), uptime and CPU-core count.
- **GPU** (gpu-exporter, job `gpu-exporter` / `gpu-exporter-sglang`):
  utilization, VRAM used/total, VRAM %, GPU and memory temperature, power draw,
  SM/memory clocks, and an up/count stat. A `$gpu_job` template variable lets
  you pick which engine's exporter feeds the panels.

`make monitoring-up` drops it into the `system-state` folder
(`config/gateway-addons/values.yaml` provider + the `--set-file` in the
Makefile target), exactly like the vLLM and llm-d dashboards.

## cluster-load

`cluster-load.json` (uid `cluster-load`) is maintained in this repo, in the
`QoS` folder alongside `fair-share.json` and `user-activity.json` (reuses the
`fair-share` provider key -- no new folder). It answers two operational
questions: which per-user limit to set (req/min, tokens/min, spend threshold),
and which queue parameter to turn (saturation detector thresholds, band
ceilings, `maxRequests`, `defaultRequestTTL`, pat-service warm/demote/spend
knobs). It does not duplicate `fair-share.json` (session stitching, per-user
session counters) or `user-activity.json` (per-user TTFT/TPOT/token/cost
panels) -- see those dashboards for that view.

**Step 0 (metric-name verification) ran**, against the live cluster
(`llmd-qwen-test-epp`, `pat-service`, `gpu-exporter`, `prometheus`, all in
`airgap-ai-stack`) on 2026-09-10. Findings:

- Prefix choice: `llm_d_epp_*` for every EPP family. `inference_extension_*`,
  `inference_pool_*` and `inference_objective_*` are live but their own HELP
  text marks them `[Deprecated: Use llm_d_epp_*]` -- confirmed on the running
  pod, not just from source.
- **Correction to the brief's draft** (panels 13 and 24, "enqueued"/"offered"
  series): the brief's draft PromQL used
  `inference_extension_flow_control_request_enqueue_duration_seconds_count`.
  Switched to the non-deprecated
  `llm_d_epp_flow_control_request_enqueue_duration_seconds_count`, confirmed
  live with a `priority` label. Semantics confirmed: the only `outcome` value
  observed is `NotYetFinalized` -- this counts enqueue attempts, not only
  successful ones, so no `outcome` filter is applied and the series is an
  upper bound on offered load per band.
- All `patsvc_*` families (`patsvc_requests_total{band,outcome,user}`,
  `patsvc_cost_units_total{model,user}`, `patsvc_session_steps_total{user}`,
  `patsvc_rotations_total{user}`, `patsvc_session_match_total{result}`)
  confirmed present with exactly the label sets the brief assumes.
- `gpu_utilization_percent`: the `job` label is not in the raw exporter
  exposition (added by Prometheus scrape relabeling), same as
  `system-state.json`'s existing `$gpu_job` variable -- no change needed.
- `default-flow` as a literal `fairness_id` value confirmed live (233 series
  at time of check), matching ADR 0008 Q3.
- `kvCacheUtilThreshold` (0.8) and `queueDepthThreshold` (5) confirmed left
  undeclared/default in `config/llmd/router-nvfp4-values.yaml` -- the brief's
  hardcoded threshold markers on panels 4, 5 and 11 are correct as written.
- `llm_d_epp_request_error_total` confirmed absent from this EPP build (as
  already noted elsewhere in this file) -- no panel depends on it.
- `llm_d_epp_flow_control_pool_saturation` currently emits only
  `stage="decode"` and `stage="effective"` -- no `stage="prefill"`, because
  this deployment runs no prefill/decode disaggregation. Panel 9's query has
  no stage filter, so it renders whatever stages exist; it will show 2 series
  here, not the 3 the brief's description assumed for a disaggregated
  deployment.
- The `bands`-templated scalar arithmetic in panels 2, 3 and 12 (e.g.
  `(${bands}-1)/(${bands}-1+1)`) was verified directly against the live
  Prometheus (`kubectl -n airgap-ai-stack port-forward svc/prometheus`) with
  `bands=4` substituted in: all three expressions returned valid vector
  results, not errors. No fallback to hardcoded 0.75/0.6 was needed; `bands`
  stays a live, non-hidden template variable.
- No panel was dropped for a missing series -- every family the brief names
  was found live under one of the two prefixes.

**The `bands` (N) open question, per Step 1, is unresolved**: whether the
policy counts a band as active when merely provisioned or only when it has
pending work is not established by ADR 0012 or by any reading taken in this
session. Panel 8 ("Bands with traffic (N)") shows what N reads as by traffic;
compare it against the `$bands` variable (default 4) by hand. Grafana panel
thresholds (panel 9's dashed ceiling lines) cannot read a template variable,
so those lines are hardcoded to the N=4 defaults (0.6, 0.75) and will not
move if `$bands` is switched to 3 -- recompute from ADR 0012's table by hand
in that case.

Caveats fixed into panel 31 on the dashboard itself: `patsvc_cost_units_total`
covers non-streaming responses only (streaming/coding-agent traffic is
missing from every cost panel; EPP token panels do cover it); saturation
readings before/after `llmd-qwen-test` revision 8 (2026-09-09 09:37 +05) are
different quantities and must not be compared across that boundary; Open
WebUI traffic bypasses pat-service entirely; ceiling thresholds depend on
`$bands`.

`make monitoring-up` drops it into the same `fair-share.json`/`user-activity.json`
ConfigMap (`grafana-dashboards-fair-share`) via the added `--set-file` in the
`monitoring-up` Makefile target. **Deployed in this session**
(`helm upgrade eg-addons`, revision 15, 2026-09-10): the ConfigMap carries all
three keys (`cluster-load.json`, `fair-share.json`, `user-activity.json`),
Grafana rolled and logged `finished to provision dashboards`. The dashboard
itself was **not opened in a browser** -- Grafana in this cluster is
SSO-only (`grafana.gpu-host.local`, see top of this file) and no session was
available to authenticate a port-forwarded API check from here, so panel
rendering was not visually confirmed. Every panel's underlying PromQL was
however run directly against the live Prometheus during Step 0 and returned
data (not "no data" or errors) for: `pool_saturation`, `flow_control_requests_total`,
`capacity_utilization_requests`, `stale_endpoints`, `request_ttft_seconds`,
`request_total`, `enqueue_duration_seconds_count`, `patsvc_requests_total`,
`patsvc_cost_units_total`, `patsvc_session_steps_total`, `patsvc_rotations_total`,
`patsvc_session_match_total`, `gpu_utilization_percent`, and the `$bands`
scalar-arithmetic expressions themselves. Panel 15 (non-dispatch outcomes) is
expected to render empty on this idle cluster -- only `outcome="Dispatched"`
has been observed. Whoever next opens the dashboard through SSO should record
here which panels are empty due to idle traffic vs. an actual problem, per
this file's usual practice (see `system-state`/`cluster-monitor` sections
above) -- not done in this session for lack of an SSO session.

**Why not DCGM?** The remote profile runs on WSL2's paravirtual GPU (RTX 5090).
`nvidia/dcgm-exporter` starts but its DCGM hostengine init fails silently
(`exit 1`) even when the WSL driver (`libcuda.so.1`, `libnvidia-ml.so.1`) is
mounted and the `nvidia` RuntimeClass is used — DCGM cannot enumerate the
virtualized card. Instead, `k8s/overlays/remote-wsl-vllm-nvfp4/gpu-exporter.yaml`
runs a tiny `nvidia-smi`-based exporter in the same image + `nvidia` RuntimeClass
the inference engines use (one carrier for vLLM, one for SGLang), with **no**
`nvidia.com/gpu` resource request so it never competes for the only GPU. See
the manifest header for the full rationale.
