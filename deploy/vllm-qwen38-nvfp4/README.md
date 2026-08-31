# Qwen3.8 27B NVFP4 vLLM experiment

This directory is intentionally independent from the AI stack manifests. It
builds the image the cluster runs and validates a single-GPU vLLM container
directly on the remote Windows + WSL2 host, outside Kubernetes. The deployed
Deployment itself comes from `helm/vllm-inference`.

The profile targets the local ModelOpt checkpoint at
`/home/llmstack/models/RadixArk-Qwen3.8-27B-NVFP4` and an RTX 5090 (SM 120,
32 GB). The model mount is read-only and the image is forced offline at
runtime. Model weights are not copied into the image. The vLLM 0.27.1 base is
pinned by manifest digest in `Dockerfile` and `build`.

## Build and run on the WSL2 host

```sh
./build
./run
./smoke
```

Environment overrides are available for automation:

```sh
VLLM_IMAGE=registry.local/vllm-qwen38-nvfp4:0.27.1 \
VLLM_MODEL_DIR=/srv/models/RadixArk-Qwen3.8-27B-NVFP4 \
VLLM_PORT=18000 \
./run
```

Additional `vllm serve` arguments can be appended to `./run`. They are useful
for controlled tuning only; vLLM uses the last value when an option is repeated.

The initial correctness profile deliberately uses text-only mode, no MTP,
FP8 KV cache, and `TRITON_ATTN`. This keeps the 27B checkpoint inside the
32 GB VRAM budget and avoids the current FlashInfer attention instability on
SM 120 with ModelOpt NVFP4 plus FP8 KV. Enable multimodal input, MTP, larger
contexts, or a different attention backend only as separate validation steps.

## Kubernetes profile: WSL2 + k3d

`k3d-create-nvidia` recreates the single-node lab cluster, passes the WSL2 GPU
through to the node, mounts the model store read-only, and configures K3s's
embedded containerd with an NVIDIA legacy runtime wrapper. The wrapper is
needed because CDI does not resolve the WSL2 driver mount reliably in this
node image.

```sh
deploy/vllm-qwen38-nvfp4/k3d-create-nvidia
make nvfp4-pat-load                  # pat-service image -> k3d node (WSL2 host)
make pat-deploy                      # same import, from a workstation over SSH (no registry)
make stack-up                        # gateway + operators + vLLM + full stack + llm-d EPP
./scripts/vllm-nvfp4-smoke-test      # direct GPU vLLM smoke
./scripts/llmd-nvfp4-smoke-test      # EPP metrics + PAT end-to-end
```

`stack-up` applies the GPU prerequisites from the `remote-wsl-vllm-nvfp4`
overlay, installs the vLLM server from `helm/vllm-inference`, then the
application stack and routing from `helm/airgap-stack`, then the llm-d
standalone router. The overlay creates a static, `Retain` PV and read-only
PVC rooted at `/var/lib/models/RadixArk-Qwen3.8-27B-NVFP4` in
`k3d-llm-stack-server-0`; the node receives that path from
`/home/llmstack/models` during cluster creation. The Deployment uses
`RuntimeClass/nvidia`, requests `nvidia.com/gpu: 1`, and `Recreate` so a
rollout never loads two copies of the 27B model into the single RTX 5090.
Its launch flags are values in `helm/vllm-inference/values.yaml`; `make
vllm-up` re-applies them alone.

## GPU accounting: NVIDIA device plugin

The node does not advertise `nvidia.com/gpu` by itself. The NVIDIA device
plugin registers the GPU with the kubelet, which lets scheduling and resource
accounting treat the RTX 5090 as a real resource:

```sh
deploy/vllm-qwen38-nvfp4/k3d-load-device-plugin          # on the WSL2 host
kubectl apply -k k8s/overlays/remote-wsl-device-plugin
kubectl get node -o jsonpath='{.items[0].status.allocatable["nvidia.com/gpu"]}'
```

The plugin DaemonSet runs as a regular `RuntimeClass/nvidia` pod because the
WSL2 driver reaches the container through the containerd hook's overlay plus
the Windows driver store (`libcuda_loader.so`); a plain hostPath mount of the
three shim libraries is not enough and NVML fails with `ERROR_DRIVER_NOT_LOADED`.

Caveats: `ghcr.io/nvidia/k8s-device-plugin` publishes images only under CI
SHA tags, not semver tags; `v0.20.0` is built as `1b826acc-amd64` (the commit
behind the `v0.20.0` release, 2026-08-19) and imported locally as
`llm-stack/k8s-device-plugin:0.20.0`. `nvcr.io` returns 403 from this host and
Docker Hub only hosts stale mirrors, so the ghcr build is the freshest source.
`k3d image import` drops this image inside the tools node, so
`k3d-load-device-plugin` saves the tar and imports it directly with `ctr`.
The plugin pod itself consumes no GPU resource; consumer pods must request
`nvidia.com/gpu` in their limits to be scheduled and accounted.

The node currently advertises `nvidia.com/gpu: 1` through the plugin, and the
vLLM Deployment requests it formally now that accounting exists. Air-gapped
bundles must re-pin the plugin to the official `nvcr.io` image when anonymous
pulls become reachable.
