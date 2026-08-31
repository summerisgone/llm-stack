# 0010: Keep SGLang KV cache on GPU for the hybrid Mamba model

## Status

Accepted on 2026-09-08.

## Context

The SGLang release serves the ModelOpt NVFP4 hybrid checkpoint on one RTX
5090. The model has 16 full-attention layers and 48 linear-attention layers,
so a reusable prefix contains both attention KV and Mamba state. The original
proposal was to add an 8–16 GB host tier with SGLang HiCache, increasing the
number of retained long conversations without relying on shared prefixes.

The existing GPU-only profile used SGLang commit
`5f55db35e926d50676f75b812640ea2410b0fe0e`, `contextLength: 155648`,
`memFractionStatic: 0.88`, three request slots and 12 Mamba slots. A first
experiment showed that HiCache allocated its host pools but crashed on the
first inference request. CPU weight offload also failed while loading the
mixed ModelOpt NVFP4/FP8 checkpoint.

The experiment was repeated with the official SGLang 0.5.19 image, commit
`0bcd822377da7b5718e674eaf9c870d349424dd1`, PyTorch 2.13.0 and CUDA 13.0.
This build contains the Mamba host-index placement fix that was absent from
older builds, but the hybrid HiCache transfer path still corrupts CUDA memory.

With `CUDA_LAUNCH_BLOCKING=1`, the kernel configuration failed while backing
up Mamba state from device to host:

```text
MambaPoolHost.backup_from_device_all_layer
  -> transfer_kv_mamba_lf_pf
  -> transfer_mamba.cuh:184
RuntimeError: CUDA error: an illegal memory access was encountered
```

The failure was reproduced with `kernel/page_first` and
`direct/page_first_direct`. Replacing `extra_buffer_lazy` with
`extra_buffer` and disabling the overlap scheduler did not prevent it. Host
RAM was available and both host pools were allocated successfully, so this
was not a host OOM. It is the same model and hardware class as upstream issue
[#24121](https://github.com/sgl-project/sglang/issues/24121), but the index
placement change alone is insufficient on this RTX 5090 SM120 path.

`--cpu-offload-gb 2` also remains incompatible with this checkpoint and fails
before KV allocation:

```text
RuntimeError: Expected all tensors to be on the same device,
but found at least two devices, cuda:0 and cpu!
```

MTP was intended only to compensate for measured CPU-offload slowdown. Since
CPU offload cannot start, and MTP consumes VRAM otherwise available for the
active KV pool, it does not serve this decision's long-context goal.

Full measurements are recorded in
[the initial run](../../tests/inference/sglang-hicache-results-2026-09-07.md)
and [the SGLang 0.5.19 run](../../tests/inference/sglang-hicache-results-2026-09-08.md).

## Decision

Do not enable HiCache, CPU weight offload or MTP for this SGLang checkpoint.
Keep active attention KV and Mamba state on the GPU.

Upgrade the SGLang inference release to the digest-pinned 0.5.19 image and use
the validated GPU-only profile:

```yaml
inference:
  contextLength: 180224
  memFractionStatic: "0.90"
  maxRunningRequests: 3
  cudaGraphMaxBsDecode: 3
  maxMambaCacheSize: 12
```

SGLang publishes `contextLength` as `max_model_len` in `/v1/models`, making
180224 the advertised API limit as well as the server-side admission limit.

## Evidence

The production-shaped 0.5.19 baseline at static fraction 0.88 allocated
172380 GPU KV tokens and left 1.79 GB after CUDA graph capture. Raising the
fraction to 0.90 allocated 191972 tokens and left 1.22 GB. The candidate
successfully processed 170094 input tokens and returned the complete unique
marker.

On three matched 384-token decode runs, median throughput changed from 69.10
to 68.34 chunks/s (-1.1%), while median TPOT changed from 14.43 to 14.55 ms
(+0.8%). This is within the experiment's 10% performance-loss gate.

Three independent requests of about 60k tokens each returned HTTP 200, but
the scheduler kept at most two active while one remained queued. The evidence
therefore supports the larger single-request context window. It does not show
that three 60k contexts decode simultaneously or that host RAM extends the
active GPU working set.

The profile was deployed as Helm release revision 3. The resulting pod became
Ready without restarts, ran the pinned 0.5.19 image digest, and returned
`max_model_len: 180224` from `/v1/models`; a request to the chat-completions
endpoint also completed successfully.

## Consequences

- `/v1/models` advertises `max_model_len: 180224`.
- The single-request context cap increases by 24576 tokens without PCIe
  transfers in the decode path.
- The GPU KV pool increases by 19592 tokens while preserving the configured
  three-request admission ceiling.
- Runtime GPU headroom after graph capture decreases by about 0.57 GB. A
  longer mixed-load soak remains required; any runtime OOM must revert the
  static fraction rather than enabling the broken offload paths.
- HiCache may be reconsidered only after an upstream hybrid-Mamba SM120 fix
  passes first-request, eviction/load-back, marker-correctness and speed tests
  on this exact checkpoint.
