# 0005 — The GPU runtime uses the NVIDIA legacy prestart hook

Status: accepted

## Context

On Windows + WSL2 the GPU driver is projected into the Linux guest rather than
installed in it. CDI, the modern NVIDIA Container Toolkit path, did not
resolve the WSL2 driver mount reliably inside the k3d node image.

## Decision

`deploy/vllm-qwen38-nvfp4/k3d-create-nvidia` creates the cluster with
`--gpus all`, mounts the WSL2 driver and NVIDIA Container Toolkit libraries
into the K3s node, and generates a containerd template pointing at
`/opt/nvidia/nvidia-container-runtime-legacy`, which forces the legacy
prestart-hook path. K3s registers the handler as the `nvidia` containerd
runtime; manifests select it through `RuntimeClass/nvidia`.

The NVIDIA device plugin DaemonSet advertises `nvidia.com/gpu`, and the vLLM
Deployment requests it in both `requests` and `limits`. Without the plugin the
pod stays Pending on insufficient `nvidia.com/gpu` — the RuntimeClass alone is
not enough.

The container also sets `LD_LIBRARY_PATH` explicitly, because Triton's
registry subprocess consults it before `ld.so.cache` and the legacy hook
injects the WSL2 `libcuda` only at container start.

## Consequences

- The device plugin overlay is a separate, one-time cluster bootstrap; it must
  be re-applied after every `k3d-create-nvidia`.
- The plugin image is currently a mirrored tag rather than the official
  `nvcr.io` reference, because anonymous pulls are not reachable from the
  reference host.
- On a Linux host without WSL2 this decision should be revisited: CDI is the
  better path where it works.
