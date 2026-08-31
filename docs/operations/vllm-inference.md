# vLLM configuration and performance

Configuration is current. The "Measured speed" section holds two dated
benchmarks side by side: 2026-09-04 at `--max-model-len 65536` (before the
first context raise), and 2026-09-07 at the current `--max-model-len 131072`
with KV-cache CPU offloading (context-length tuning pass, `--max-num-seqs`
held at 2 throughout per the customer requirement).

The live vLLM Deployment is rendered from `helm/vllm-inference/values.yaml`
and applied by `make vllm-up` (also run by `make stack-up`) —
[ADR 0007](../adr/0007-inference-engines-as-helm-releases.md). Change a
setting there, not in a manifest; the copy still sitting in
`k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml` is not applied by anything.

## Serving path and hardware

The stack routes authenticated requests through PAT service / the private
AI Gateway and the llm-d endpoint picker to
`vllm-qwen38-nvfp4.airgap-ai-stack.svc.cluster.local:8000`.
One vLLM replica runs on `k3d-llm-stack-server-0`, using the NVIDIA runtime
and one RTX 5090. The model is mounted read-only at `/model`; its WSL path is
`/home/llmstack/models/RadixArk-Qwen3.8-27B-NVFP4`.

The image is `llm-stack/vllm-qwen38-nvfp4:0.27.1`; the installed Python package
also reports vLLM `0.27.1`. GPU memory reported during the 2026-09-04 check
(then at `--max-model-len 65536`) was 30,423 / 32,607 MiB (this includes
runtime allocations, not just weights). At the current `--max-model-len 131072`
/ `--gpu-memory-utilization 0.94`, idle GPU memory is 28,745–31,192 / 32,607
MiB free depending on whether a long-context burst has run since the last
restart — see "Measured speed" below; the PyTorch CUDA caching allocator does
not release memory back to the driver after a burst, so post-burst headroom
looks tighter than idle-after-restart headroom even though nothing leaked.

The checkpoint is named Qwen3.8 in the local inventory, but its own config uses
architecture `Qwen3_5ForConditionalGeneration`, model type `qwen3_5`, and BF16
compute dtype. Quantization is ModelOpt `MIXED_PRECISION`: attention projections
use FP8, while MLP projections and the language model head use NVFP4. It is not
a uniformly 4-bit checkpoint.

## Explicit launch settings

| Setting | Value |
| --- | --- |
| Served model name | `qwen-3.8-27b` |
| Input mode | `--language-model-only` |
| Compute dtype | `--dtype auto` (checkpoint BF16) |
| KV cache | `--kv-cache-dtype fp8` |
| Attention | `--attention-backend TRITON_ATTN` |
| Maximum context, prompt plus output | `--max-model-len 131072` (raised from 84,672 on 2026-09-07; see "Context-length tuning" below) |
| Concurrent sequences | `--max-num-seqs 2` (unchanged — the customer requirement was to raise context *without* raising slot count) |
| Tokens per scheduler batch | `--max-num-batched-tokens 8192` |
| GPU memory target | `--gpu-memory-utilization 0.94` (raised from 0.90 on 2026-09-07) |
| Reasoning parser | `--reasoning-parser qwen3` |
| Tool selection | `--enable-auto-tool-choice` |
| Tool parser | `--tool-call-parser qwen3_coder` |
| Prefix caching | `--enable-prefix-caching` (`inference.prefixCaching: true`, since 2026-09-07, `TASK-qos-fair-share.md` Stage 2) |
| Mamba cache | `inference.mambaCacheMode: none` is requested, but vLLM 0.27.1 **silently overrides it to `align`** whenever prefix caching is on for this checkpoint's architecture (`Qwen3_5ForConditionalGeneration`) — see its own startup log (`config.py:618`), which also calls `align`-mode prefix caching for Mamba layers "experimental". `--mamba-ssm-cache-dtype float32` is respected as requested. |
| KV-cache CPU offload | `--kv-offloading-size 8 --kv-offloading-backend native` (new 2026-09-07, `inference.kvOffloadingSizeGiB` / `kvOffloadingBackend`) |

Every row above is a key under `inference` in
`helm/vllm-inference/values.yaml`. No speculative decoding/MTP configuration
is supplied (`inference.speculativeConfig: null`; setting it emits
`--speculative-config`). Resource requests are 4 CPU, 12 GiB RAM, one GPU;
limits are 8 CPU, 20 GiB RAM, one GPU (raised from 8/24 GiB on 2026-09-07 to
fit the KV offload buffer — see below). `/dev/shm` is a 12 GiB memory-backed
volume (`inference.dshmSizeGiB`, raised from 8 GiB — it must exceed
`kvOffloadingSizeGiB`, since the native offload backend mmaps its buffer at
`/dev/shm/vllm_offload_<engine_id>.mmap`). The Deployment uses `Recreate` to
avoid loading a second model replica into the same GPU. Hugging
Face/Transformers offline mode is enabled and vLLM usage reporting is
disabled.

## Context-length tuning (2026-09-07)

Goal: raise usable context length while holding `--max-num-seqs 2` fixed
(the customer requirement — see `TASK-qos-fair-share.md` §1). Two independent
levers, both validated live against the real cluster, not calculated blind:

**1. `--gpu-memory-utilization`.** Binary-searched with `--max-model-len`
fixed at 84,672: 0.90 → 186,278 GPU KV-cache tokens (1,886 MiB free per
`nvidia-smi`); 0.93 → 214,502 tokens (3,698 MiB free); 0.94 → 222,969 tokens
(3,443 MiB free); **0.95 crash-loops at startup** —
`ValueError: Free memory on device cuda:0 (30.2/31.84 GiB) on startup is less
than desired GPU memory utilization (0.95, 30.25 GiB)`. The real ceiling on
this card is ≈0.948; 0.94 is the value actually deployed, one step back from
that edge. Below 30.2 GiB is never available to vLLM at all — a fixed
driver/other-process reservation independent of any Helm setting.

**2. `--max-model-len` past the point where two full-length sessions fit.**
vLLM's own startup check only requires the GPU KV-cache pool to hold *one*
sequence at `max-model-len`, not `max-num-seqs` of them — confirmed by
deliberately deploying at `--max-model-len 200000` (pool was 235,820 tokens,
reported "Maximum concurrency for 200,000 tokens per request: 1.18x") and
watching it start and serve normally. Above that per-sequence ceiling, vLLM
degrades by **preempting** one of two simultaneous max-length sessions
instead of refusing to start or crashing. 200,000 was judged too aggressive
for a real two-concurrent-max-length guarantee against a ~230K-token pool, so
the deployed value is **131,072 (128K)**, at which the pool (230,104 tokens)
gives 1.76x concurrency — i.e. two full 131K sessions running at once would
together want ~12% more KV cache than the raw GPU pool provides.

**3. Native KV-cache CPU offloading closes that 12% gap.** vLLM 0.27.1 ships
`--kv-offloading-size <GiB> --kv-offloading-backend native`, which spills
evicted KV blocks to a `/dev/shm`-backed buffer instead of dropping them, so
a preempted session reloads from RAM instead of recomputing its prefill from
scratch. This is **not** a way to run a single generation's live context
larger than the GPU-resident pool — PagedAttention still requires every
token a running sequence attends to be GPU-resident at that step — it only
cushions the cost of losing the GPU-resident copy under `--max-num-seqs 2`
contention. Confirmed live: a two-concurrent-~115K-token-prompt stress test
(§"Measured speed") moved `vllm:kv_offload_load_bytes_total` from 0 to
~513 MB and `..._store_bytes_total` up by ~6.6 GB, and both requests still
finished cleanly (`finish_reason=length`, no errors, no pod restart).
Deployed at 8 GiB (`inference.kvOffloadingSizeGiB: 8`), comfortably inside
the 12 GiB `/dev/shm` volume and the node's spare RAM (node sits at 69% of
47.9 GiB total after a stress burst, still well short of pressuring the other
tenants on this single-node cluster). **First attempt at 16 GiB failed**: the
`dshm` `emptyDir` was still sized 8 GiB from before this change, and the
offload backend's `MADV_POPULATE_WRITE` call on its mmap failed with
`OSError: [Errno 14] Bad address` because the buffer didn't fit in the tmpfs
— `dshmSizeGiB` must always exceed `kvOffloadingSizeGiB`.

An isolation test (same 131,072/0.94 config, offloading toggled off) found
the offload connector's own per-step overhead is small: ~5–6% off
single-request decode speed, noise-level (<1%) on the 2-concurrent case —
see the table below. Most of the throughput cost of this tuning pass comes
from the larger `--max-model-len`/`--gpu-memory-utilization` themselves, not
from offloading.

Net result: **200,000 has an upper bound above this session's actual target,
so the >100K bar was cleared without falling back to SGLang** — 131,072 is
the number actually deployed, chosen for concurrency margin, not a hard
ceiling. `--max-num-seqs` was never touched.

## Measured speed

### 2026-09-04 baseline — `--max-model-len 65536`, no offloading

Measured 05:16 UTC (10:16 Asia/Yekaterinburg) through a Kubernetes
port-forward directly to vLLM, with the prefix-cache and mamba flags absent.
The server was idle before the test. Three sequential requests were followed
by two concurrent requests. Each used a 47-token synthetic prompt, 512
generated tokens, temperature zero, `ignore_eos=true`, streaming with usage,
and `enable_thinking=false`.

| Workload | Decode speed | End-to-end throughput | First token |
| --- | --- | --- | --- |
| One request, three trials | 71.25–71.59 tokens/s | 68.57–70.04 tokens/s | 158–294 ms |
| Two requests concurrently | 66.26–66.55 tokens/s per request | 126.91 tokens/s aggregate | 333–348 ms |

Decode speed is `(output_tokens - 1) / (last_content_time - first_content_time)`.
End-to-end throughput includes prompt processing and transport; aggregate
throughput divides the total 1,024 output tokens by the concurrent batch's
8.069-second wall time. Single requests finished in 7.31–7.47 seconds.

These are short-prompt measurements, not a 64K-context benchmark or a test of
the entire PAT/AI Gateway path. Historical active log intervals showed about
67–71 generated tokens/s, consistent with this test. Dashboard token throughput
uses rates over wall-clock windows, including idle time, so it will be lower
than active decode speed with sparse traffic.

Raw timings: [vllm-benchmark-2026-09-04.json](evidence/vllm-benchmark-2026-09-04.json).

### 2026-09-07 — `--max-model-len 131072`, `gpu-memory-utilization 0.94`

Same short-prompt method as above (this time a 25-token synthetic prompt;
every request hit `finish_reason=length`, i.e. genuinely decoded the full
512 tokens), run via `kubectl exec` straight into the vLLM pod
(`localhost:8000`, bypassing PAT/AI Gateway/EPP same as the 09-04 run). Three
configurations, same idle server, same method, to separate the cost of the
larger context/utilization from the cost of KV offloading:

| Config | Single, 3 trials | 2 concurrent, per-request | Aggregate |
| --- | --- | --- | --- |
| 09-04 baseline (84,672, util 0.90, no offload) | 71.25–71.59 tok/s | 66.26–66.55 tok/s | 126.91 tok/s |
| 131,072, util 0.94, **no** offload (isolation test) | 66.62–66.68 tok/s | 61.49–61.56 tok/s | 113.93 tok/s |
| 131,072, util 0.94, **with** 8 GiB offload (deployed) | 62.46–62.71 tok/s | 61.88–61.97 tok/s | 119.30 tok/s |

Reading: the larger `--max-model-len`/`--gpu-memory-utilization` cost ~6-12%
of single-request decode speed on their own (71→66.6 tok/s) — larger
block-table/CUDA-graph padding at a bigger `max-model-len`. KV offloading
adds a further ~5-6% on top for single requests (66.6→62.5 tok/s, the
connector's per-step bookkeeping) but is noise-level under 2-concurrent load
(61.5 vs 61.9 tok/s) — GPU compute is already the bottleneck there. Net: about
9-12% slower short-prompt decode than the pre-tuning baseline, in exchange for
55% more usable context at the same 2-slot concurrency.

### 2026-09-07 — long-context concurrent stress test

Two concurrent chat completions with distinct ~110-117K-token synthetic
prompts (different filler text, so no shared prefix-cache hit), 64 output
tokens each, `ignore_eos=true`, temperature zero, against the deployed
131,072/0.94/8 GiB-offload config. Combined prompt tokens (~228K) sit right
at the 230,104-token GPU KV pool — the scenario the offload buffer exists
for.

| Request | Prompt tokens | TTFT | Decode speed |
| --- | --- | --- | --- |
| 1 | 111,132 | 3.79 s | 45.21 tok/s |
| 2 | 117,042 | 48.43 s | 26.92 tok/s |

Both finished cleanly (`finish_reason=length`, no errors, no pod restart).
`vllm:kv_offload_load_bytes_total` moved from 0 to ~513 MB and
`..._store_bytes_total` grew by ~6.6 GB during the run — real GPU↔CPU KV
traffic, not just an idle feature flag. Decode speed at this real long
context (27-45 tok/s) is well below the short-prompt figures above by
design: attention cost during decode scales with context length. Request 2's
48.4 s TTFT reflects two ~115K-token prefills sharing
`--max-num-batched-tokens 8192` chunked-prefill slots on one GPU, not KV
eviction cost.

GPU memory after this burst: 996 MiB free (vs. 3,443 MiB measured at idle
right after a restart) — PyTorch's CUDA caching allocator keeps pages
reserved after a burst rather than returning them to the driver; this is
expected allocator behavior, not a leak, and `/health` stayed green
throughout.

Raw timings and the `gpu-memory-utilization` ceiling probe:
[vllm-benchmark-2026-09-07.json](evidence/vllm-benchmark-2026-09-07.json).

## Dashboard installation and validation

The [official vLLM dashboard](https://docs.vllm.ai/en/stable/examples/observability/prometheus_grafana/)
is now provisioned by Helm in the operator-facing Grafana in namespace
`monitoring`, using the existing `eg-addons` v1.8.1 release. The separate Grafana
in `airgap-ai-stack` is not the NodePort operator UI.

Open http://grafana.gpu-host.local:32030/d/vllm/vllm (folder `vLLM`). Reapply with
`make monitoring-up`. The datasource is `vLLM Prometheus`, UID
`vllm-prometheus`, pointing to the stack Prometheus in `airgap-ai-stack`.
The existing scrape target is healthy and collects `/metrics` every 15 seconds.

The same Grafana now provisions the seven dashboards from the [official llm-d
dashboard directory](https://github.com/llm-d/llm-d/tree/main/guides/recipes/observability/grafana/dashboards)
in its `llm-d` folder: vLLM overview, SGLang overview, failure/saturation,
diagnostic drill-down, performance/KV cache, P/D coordinator, and inference
gateway. They are vendored at llm-d commit
`080c14d957755da4cc363745638619fc748558f1`, configured for the same datasource,
and deployed by `make monitoring-up`.

Prometheus adds `namespace=airgap-ai-stack` to the static vLLM, SGLang and EPP
scrape targets, matching the official dashboard namespace filters. The active
profile supplies EPP and vLLM data; the SGLang panels fill in only while
`make sglang-up` is the running engine. Prefill/decode panels have no
corresponding workloads. This EPP build does not currently export
`llm_d_epp_request_error_total`, so error-rate panels remain empty until that
metric is emitted.

Validation completed:

- `make preflight`: all Kubernetes configurations valid.
- Helm template, lint, and Kubernetes client dry-run succeeded. Lint reports
  the upstream chart's existing `v1.8.1` SemVer formatting warning.
- `make monitoring-up MONITORING_CHART=/tmp/vllm-dashboard-chart/gateway-addons-helm`:
  `eg-addons` revision 3, `STATUS: deployed`, `Upgrade complete`.
- The deployed ConfigMap JSON equals the tracked JSON and is owned by
  `eg-addons`; all 12 upstream panels are preserved.
- All 21 referenced vLLM metric names exist. All 27 panel PromQL queries
  returned nonempty, finite data for `qwen38-nvfp4`, using a 10-minute rate
  window covering the benchmark.
- Grafana successfully mounted the provider and datasource; Prometheus was
  reachable from its container.

Grafana provisioning log excerpts:

```text
2026-09-04T05:17:30.118554513Z inserting datasource from configuration name="vLLM Prometheus" uid=vllm-prometheus
2026-09-04T05:17:30.131760996Z starting to provision dashboards
2026-09-04T05:17:30.27777834Z finished to provision dashboards
```

Visual browser verification was blocked by automatic approval review of the
redirect to the existing external SSO origin, `***REMOVED***`.
No browser login or authentication-setting changes were performed.
