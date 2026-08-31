# Installing on a new site

This is the ordered runbook for standing the remote GPU profile up somewhere
it has never run. Day-2 operations — tunnels, reboots, dashboards — are in
[docs/operations](../operations/README.md).

## What a site needs

- A Linux host with an NVIDIA GPU. The reference site is Windows + WSL2 with
  an RTX 5090 (32 GiB); the model profile below assumes roughly that much
  VRAM.
- Docker and k3d on that host, and `kubectl` plus `helm` wherever you run
  `make` from.
- A TLS-terminating reverse proxy in front, publishing one origin and
  forwarding to the Envoy edge listener. The stack never terminates TLS
  itself — see [ADR 0004](../adr/0004-tls-terminates-at-the-site-proxy.md).
- The model weights on disk, and the vLLM and pat-service images built.

## Values that are site-specific

Every one of these has to be reviewed for a new installation. They are not yet
all in one file — collecting them into a single per-site values file is the
next piece of work, tracked in
[ADR 0002](../adr/0002-one-owner-per-object.md).

| Value | Where it is today |
| --- | --- |
| Public origin | `helm/airgap-stack/values.yaml` (`oidc.publicBaseURL`, `oidc.externalIssuer`), `k8s/base/applications.yaml`, `k8s/realm-demo.json`, `config/gateway-addons/values.yaml`, `scripts/provision-*-oidc` |
| Edge listener port | `helm/airgap-stack/values.yaml` (`routing.edge.externalPort`) |
| Operator UI hostnames and NodePorts | `helm/airgap-stack/values.yaml` (`routing.nodePorts`), `config/gateway-addons/values.yaml`, `k8s/overlays/remote-wsl-vllm-nvfp4/langfuse-public-url-patch.yaml`, `deploy/vllm-qwen38-nvfp4/systemd/*` |
| Realm name and client redirect URIs | `k8s/realm-demo.json` |
| k3d node name | `Makefile` (`K3D_NODE`), `helm/vllm-inference/values.yaml`, `helm/sglang-inference/values.yaml`, `k8s/overlays/remote-wsl-vllm-nvfp4/model-volume.yaml` |
| k3d API port, model root | `deploy/vllm-qwen38-nvfp4/k3d-create-nvidia` (`K3D_API_PORT`, `K3D_MODEL_ROOT`) |
| Model directory | `k8s/overlays/remote-wsl-vllm-nvfp4/model-volume.yaml` |
| Served model name, GPU sizing, launch flags | `helm/vllm-inference/values.yaml` (and `helm/sglang-inference/values.yaml` if SGLang is used) |
| Which backends the gateway advertises | `helm/airgap-stack/values.yaml` (`inference.*.enabled`), `k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml` (`OPENAI_API_CONFIGS`) |
| SSH host for remote `kubectl` | `docs/operations/README.md` |
| Every credential | see [docs/security](../security/README.md) |

## Order of operations

```sh
# 1. Credentials. Replace every value; the defaults are not safe on a network.
cp .env.example .env
$EDITOR .env

# 2. Validate everything that needs no cluster.
make verify

# 3. On the GPU host: create the cluster and load the images.
deploy/vllm-qwen38-nvfp4/k3d-create-nvidia          # destructive: recreates the named cluster
deploy/vllm-qwen38-nvfp4/build                      # builds the vLLM image
deploy/vllm-qwen38-nvfp4/k3d-load-image
make nvfp4-pat-load                                 # builds and imports pat-service

# 4. GPU accounting: the device plugin must advertise nvidia.com/gpu before
#    the vLLM pod can be scheduled at all.
make device-plugin-load
make device-plugin-up
make device-plugin-status                           # must print 1

# 5. Optional one-shot probe that CUDA really works inside the runtime.
kubectl apply -f k8s/overlays/remote-wsl-vllm-nvfp4/gpu-runtime-smoke.yaml
kubectl -n airgap-ai-stack logs pod/cuda-runtime-smoke
kubectl -n airgap-ai-stack delete pod cuda-runtime-smoke

# 6. Deploy. This is the whole stack: prerequisites, GPU objects, the vLLM
#    release, the Helm release that owns routing, llm-d, and Keycloak client
#    reconciliation.
make stack-up

# 7. Verify.
make llmd-nvfp4-smoke                               # needs the GPU free
make smoke-nogpu                                    # everything except the model calls

# 8. Optional: SGLang instead of vLLM on the same card. Not part of step 6.
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=0
make sglang-up                                      # needs SGLANG_API_KEY in .env
```

Step 6 is the only deploy command for the stack. `kubectl apply -k` on the remote overlay
deploys workloads without a Gateway; see [k8s/README.md](../../k8s/README.md).

## Before the origin is public

Work through "Before public exposure" in
[docs/security](../security/README.md) first. The default configuration
publishes the Keycloak admin console with a password that is in this
repository, and the realm has no brute-force protection.

## Air-gapped installation

`versions.lock.env` pins every image and Helm chart used by the deploy path,
and `models/` holds the model inventory and checksums. Mirroring them into a
private registry, and a `bundle` target that produces the archive, is not yet
written — see [docs/airgap](../airgap/README.md).
