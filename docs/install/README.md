# Installing on a new site

How the installed stack works and is operated afterwards is in the
[handbook](../handbook/README.md).

This is the ordered runbook for standing the remote GPU profile up somewhere
it has never run. For native k3s with a CPU control host and a separate GPU
worker, see [k3s-split-gpu.md](k3s-split-gpu.md). Day-2 operations — tunnels, reboots, dashboards — are in
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
- The model weights on disk, and the vLLM image built. pat-service is pulled
  from `ghcr.io/summerisgone/pat-service` (built by
  `.github/workflows/pat-service-image.yml`); the site needs outbound access
  to ghcr.io, or the image mirrored in for an air-gapped install.

## Values that are site-specific

Every one of these has to be reviewed for a new installation. They are not yet
all in one file — collecting them into a single per-site values file is the
next piece of work, tracked in
[ADR 0002](../adr/0002-one-owner-per-object.md).

| Value | Where it is today |
| --- | --- |
| Public origin | `STACK_BASE_URL` in `.env`: the Makefile sets `oidc.publicBaseURL` / `oidc.externalIssuer` and substitutes the placeholder origin in rendered manifests (`helm-up`, `agents-up`, `monitoring-up`); also `k8s/realm-demo.json`, `scripts/provision-*-oidc` |
| Edge listener port | `helm/airgap-stack/values.yaml` (`routing.edge.externalPort`) |
| Operator UI hostnames and NodePorts | `helm/airgap-stack/values.yaml` (`routing.nodePorts`), `config/gateway-addons/values.yaml`, `k8s/overlays/remote-wsl-vllm-nvfp4/langfuse-public-url-patch.yaml`, `deploy/vllm-qwen38-nvfp4/systemd/*` |
| Realm name and client redirect URIs | `k8s/realm-demo.json` |
| GPU worker selection | No node names: GPU workers carry the label `node-role/inference=true` and the taint `nvidia.com/gpu=present:NoSchedule` (set on `agent-0` by `deploy/vllm-qwen38-nvfp4/k3d-create-nvidia`, [ADR 0019](../adr/0019-inference-plane-gpu-worker-nodes.md)); `nodeSelector`/`tolerations` in the engine chart values match them |
| k3d API port, model root | `deploy/vllm-qwen38-nvfp4/k3d-create-nvidia` (`K3D_API_PORT`, `K3D_MODEL_ROOT`) |
| Model weights | MinIO bucket `models`, uploaded by `scripts/models-upload` from a GPU node directory (`MODELS_SRC_DIR`, default `/var/lib/models`); `modelCache` in each engine chart; checksums in `models/checksums.txt` |
| Served model name, GPU sizing, launch flags | `helm/vllm-inference/values.yaml` (and `helm/sglang-inference/values.yaml` if SGLang is used) |
| Engine replicas | `VLLM_REPLICAS`, `SGLANG_REPLICAS`, `NINFER_REPLICAS` in `.env`, applied by `make engines-up` |
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

# 3. On the GPU host: create the cluster and load the vLLM image.
deploy/vllm-qwen38-nvfp4/k3d-create-nvidia          # destructive: recreates the named cluster
deploy/vllm-qwen38-nvfp4/build                      # builds the vLLM image
deploy/vllm-qwen38-nvfp4/k3d-load-image

# 4. GPU accounting: the device plugin must advertise nvidia.com/gpu before
#    the vLLM pod can be scheduled at all.
make device-plugin-load
make device-plugin-up
make device-plugin-status                           # must print 1

# 5. Optional one-shot probe that CUDA really works inside the runtime.
kubectl apply -f k8s/overlays/remote-wsl-vllm-nvfp4/gpu-runtime-smoke.yaml
kubectl -n airgap-ai-stack logs pod/cuda-runtime-smoke
kubectl -n airgap-ai-stack delete pod cuda-runtime-smoke

# 6. Model weights into MinIO (ADR 0019): the engines' model-cache
#    initContainers fetch them from there. Needs MinIO and the model-cache
#    ConfigMap first; files already in the bucket are skipped on a re-run.
make gateway-up operators-up helm-up
scripts/models-upload RadixArk-Qwen3.8-27B-NVFP4 bge-m3

# 7. Deploy. This is the whole stack: prerequisites, GPU objects, the Helm
#    release that owns routing, the engines (*_REPLICAS in .env, one vLLM by
#    default), llm-d, and Keycloak client reconciliation.
make stack-up

# 8. Verify.
make llmd-nvfp4-smoke                               # needs the GPU free
make smoke-nogpu                                    # everything except the model calls

# 9. Optional: another engine on the same card, e.g. SGLang.
#    .env: VLLM_REPLICAS=0 SGLANG_REPLICAS=1, then:
make engines-up
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
