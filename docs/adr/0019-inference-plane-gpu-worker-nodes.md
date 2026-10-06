# 0019: Inference plane on dedicated GPU worker nodes; engines join a per-model pool

## Status

Proposed on 2026-10-02. Nothing below is implemented. Stage 1 (a two-node
k3d mock on the existing WSL2 host) is the first step; the work plan is at
the end.

Amends the single-GPU assumptions of
[ADR 0006](0006-pluggable-inference-backends.md) and
[ADR 0007](0007-inference-engines-as-helm-releases.md) -- one live engine,
`INFERENCE_ENGINE`, `make engine-up`. Their routing-by-model-name decision
and "each engine is its own Helm release" are unchanged.
[ADR 0005](0005-legacy-nvidia-runtime-hook.md) keeps applying to WSL2 nodes
only.

## Context

- One k3d node, `k3d-llm-stack-server-0`, carries the control plane, the
  application stack and the GPU engine
  (`deploy/vllm-qwen38-nvfp4/k3d-create-nvidia`: `--servers 1 --agents 0`,
  every GPU and model mount `@server:0`).
- Engine charts pin that node (`nodeName` in
  `helm/{vllm,sglang,ninfer,embeddings}-inference/values.yaml`, rendered as
  `nodeSelector: kubernetes.io/hostname`). The model PVs are hostPath with
  `nodeAffinity` on the same hostname.
- EPP's `router.modelServers` selects one engine by
  `app.kubernetes.io/name` and one port (`LLMD_ENGINE_SETS` in the
  Makefile); switching engines is a global stop/start.
- Prometheus scrapes engines, `gpu-exporter` and `node-exporter` by Service
  name (`static_configs` in
  `k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`). With
  more than one pod or node behind a Service, each scrape hits a random one
  and the series mix.
- Goal: add GPU capacity by buying machines and joining them, without
  touching the edge, Keycloak, Open WebUI, pat-service or the AI Gateway.

## Decision

### 1. GPU nodes are dedicated bare-metal Linux workers

Inference runs only on worker nodes labelled `node-role/inference=true` and
tainted `nvidia.com/gpu=present:NoSchedule`. A GPU node is a bare-metal
Linux host running `k3s agent`; it holds no state other than a model cache.

| Layer | Bare-metal node (target) | WSL2 node (mock, Stage 1) |
| --- | --- | --- |
| GPU runtime | NVIDIA GPU Operator, preinstalled driver, CDI | legacy hook, ADR 0005 |
| GPU metrics | DCGM exporter (GPU Operator) | `nvidia-smi` exporter (`gpu-exporter.yaml`); DCGM cannot enumerate the WSL2 GPU |
| Node labels | GPU Feature Discovery (`nvidia.com/gpu.product`, ...) | set by hand |

The control plane and the application stack run on CPU nodes. No
application workload tolerates the GPU taint.

### 2. A model is an InferencePool; engines are its members

- The pool selects pods by `llm-d.ai/model=<served-name>`, not by
  `app.kubernetes.io/name`. Engine charts set that label next to the
  existing `llm-d.ai/engine-type`.
- vLLM and SGLang pods may coexist in one pool: `core-metrics-extractor`
  already picks metric names per pod from `llm-d.ai/engine-type`.
- All pods of a pool listen on the same `targetPort` (8000); SGLang's
  `inference.port` moves from 30000.
- A mixed pool needs one upstream-auth story. The `SGLANG_API_KEY`
  injection on the shared path does not generalize; in-cluster engines run
  without a key, restricted by NetworkPolicy.
- Capacity is `replicas` per engine per pool, one replica per GPU (or per
  tensor-parallel group). `INFERENCE_ENGINE` and `make engine-up` are
  retired; `VLLM_REPLICAS`, `SGLANG_REPLICAS`, `NINFER_REPLICAS` in `.env`
  and `make engines-up` replace them.
- Engines that cannot join EPP (ninfer, ADR 0015) are separate model names
  on direct rules (`qwen-3.8-27b-ninfer`), not a switch of the pool's name:
  switching the shared name would turn fair share off for every client.
  On one GPU they run only while the pool has 0 replicas.
- A new model is a new pool, its EPP and one `AIGatewayRoute` rule.

### 3. Engines are scheduled by label, not by host

Engine charts, the device plugin, the GPU exporter and the GPU smoke pod use
`nodeSelector: {node-role/inference: "true"}` plus a toleration for
`nvidia.com/gpu`. No manifest names a node.

### 4. Weights come from MinIO into a node-local cache

MinIO is the source of truth for model weights. Each GPU node holds a cache
at `/var/lib/models` on local NVMe, filled and verified against
`models/checksums.txt` before the engine starts. No PV is bound to a host
name. Stage 1 keeps the hostPath PVs, with `nodeAffinity` on the label
instead of the hostname.

### 5. Metrics are pulled by two consumers, both from CPU nodes

```
CPU node                                GPU node
 EPP        -- podIP:8000/metrics  -->  engine pod     (endpoint choice, saturation, fair-share)
 Prometheus -- podIP:8000/metrics  -->  engine pod
            -- podIP:9400/metrics  -->  GPU exporter (DaemonSet)
            -- nodeIP:9100/metrics -->  node-exporter (hostNetwork)
      '--> Grafana
```

- EPP discovers endpoints from the InferencePool through the Kubernetes API
  and polls each pod directly; it does not read Prometheus.
- Prometheus discovers targets with `kubernetes_sd` (pods and nodes) and
  carries `node`, `pod`, `model`, `engine` and `gpu_product` labels.
- The GPU exporter becomes a DaemonSet on GPU nodes, one per node.
- Nothing on a GPU node pushes. Pod-to-pod and node traffic between CPU and
  GPU nodes must be routable (flannel; `wireguard-native` across networks).

### 6. Unchanged

EPP owns queueing and fairness
([architecture](../architecture/README.md#queueing-and-fairness-who-owns-what)).
The edge, Keycloak, Open WebUI, pat-service and the AI Gateway do not know
how many GPU nodes exist. Traces still go AI Gateway -> OTEL Collector ->
Langfuse and do not involve GPU nodes.

## Consequences

- Fair-share becomes global across every GPU in a pool.
- Adding capacity: install OS and driver, join with `k3s agent`, label and
  taint (GFD on bare metal), warm the cache, raise `replicas`.
- EPP `flowControl.maxRequests` (32) and `utilization-detector` thresholds
  were sized for one card; they are per-pool values and must be re-measured
  once a pool has more than one endpoint. EPP needs `replicas > 1`.
- Values profiles are per GPU class (`gpuMemoryUtilization`, `maxNumSeqs`,
  tensor parallelism).
- Embeddings in `gpu` mode lose the shared-card "paper budget"
  ([ADR 0013](0013-embeddings-api-bge-m3.md)): they run on CPU nodes or get
  a GPU resource of their own.
- Grafana dashboards gain `node` / `pod` variables; GPU panels move to DCGM
  metric names (`DCGM_FI_DEV_*`) and keep the `nvidia-smi` exporter's names
  only while a WSL2 node exists.
- Images come from an in-cluster registry once there is more than one host;
  `k3d image import` only covers the mock.
- EPP datalayer errors are logged only at verbosity 4 and leave an endpoint
  silently stale (CONTEXT.md, 2026-09-07). With several endpoints, alerts on
  `llm_d_epp_datalayer_extract_errors_total` and
  `llm_d_epp_ready_endpoints` are required.

## Open questions

1. Resolved 2026-10-02: `k3d node create` (v5.9.0) has no `--volume` or
   `--gpus`, so the live cluster got its GPU worker from
   `deploy/vllm-qwen38-nvfp4/k3d-add-gpu-agent`, a plain `k3s agent`
   container on the k3d network (`K3S_URL` + cluster token) with the same
   mounts, label and taint. No recreate, no PVC restore. The host needed
   `fs.inotify.max_user_instances` raised from 128 to 1024, or the agent's
   containerd CRI fails with "too many open files".
2. Can a remote `k3s agent` join a k3d server (node IP on the docker bridge,
   flannel reachability), or does the server move to native k3s or a
   separate Linux host before the first bare-metal node? Prefer a separate
   small Linux control-plane host.
3. WSL2 inbound for remote agents if the control plane stays there:
   `networkingMode=mirrored` vs. a WireGuard/Tailscale overlay.
4. One EPP per pool or one shared; EPP HA.
5. A second endpoint without a second GPU: is `llm-d-inference-sim` (vLLM
   metric shape) compatible with the pinned EPP build?

## Work plan

Each stage ends in a state that can be left running.

### Stage 1 -- two-node mock on the WSL2 host

| # | Task | Files |
| --- | --- | --- |
| 1.1 | Back up PVC data (Postgres via CNPG, ClickHouse, MinIO buckets), unless open question 1 allows adding the agent in place | runbook |
| 1.2 | `--agents 1`; GPU, toolkit and model mounts `@agent:0`; `--k3s-node-label node-role/inference=true@agent:0`; `--k3s-arg --node-taint=nvidia.com/gpu=present:NoSchedule@agent:0`; containerd template patch and restart on `agent-0` | `deploy/vllm-qwen38-nvfp4/k3d-create-nvidia` |
| 1.3 | `nodeName` -> `nodeSelector` + `tolerations` values | `helm/{vllm,sglang,ninfer,embeddings}-inference` |
| 1.4 | Model PV `nodeAffinity` by label | `k8s/overlays/remote-wsl-vllm-nvfp4/*model-volume.yaml` |
| 1.5 | Device plugin, `gpu-runtime-smoke`: selector + toleration; `gpu-exporter`: one DaemonSet; `node-exporter`: toleration | `k8s/overlays/remote-wsl-device-plugin/`, `k8s/overlays/remote-wsl-vllm-nvfp4/` |
| 1.6 | Prometheus `kubernetes_sd` for engines, GPU exporter and node-exporter; RBAC | `prometheus-config-patch.yaml`, `k8s/base/observability.yaml` |
| 1.7 | `K3D_NODE` -> `GPU_NODE` (or a label lookup) | `Makefile` |
| 1.8 | Check the operator-UI proxies still resolve the right container | `deploy/vllm-qwen38-nvfp4/systemd/` |

Acceptance:

- `nvidia.com/gpu: 1` allocatable on `agent-0` only.
- Engine, device plugin and GPU exporter pods run on `agent-0`; every
  application pod runs on `server-0`.
- `llm_d_epp_ready_endpoints == 1`; `up` is 1 for engine, GPU and both
  node-exporter targets, each with a `node` label.
- `make llmd-nvfp4-smoke` passes.
- `docker stop k3d-llm-stack-agent-0`: edge, Keycloak and Open WebUI keep
  serving; inference fails cleanly; after `docker start` the engine
  recovers without manual steps.

### Stage 2 -- per-model pool

| # | Task | Files |
| --- | --- | --- |
| 2.1 | `llm-d.ai/model` label on engine pods | engine charts |
| 2.2 | Pool selector by model label; single `targetPort` 8000; SGLang on 8000 | `config/llmd/router-nvfp4-values.yaml`, `helm/sglang-inference`, `Makefile` (`LLMD_ENGINE_SETS`) |
| 2.3 | Drop upstream key on the shared path; NetworkPolicy EPP -> engines | `helm/airgap-stack` (`inference.sglang`, `BackendSecurityPolicy`) |
| 2.4 | Retire `INFERENCE_ENGINE` / `engine-up` / `HELM_ENGINE_SETS`; replace with replicas per engine (`engines-up`); ninfer becomes its own model name | `Makefile`, `.env.example`, `helm/airgap-stack` (`inference.ninfer`) |
| 2.5 | EPP alerts on extract errors and ready endpoints | Prometheus rules, Grafana |
| 2.6 | Optional: second agent with an engine simulator to exercise two endpoints | k3d, a test chart |

Acceptance: switching or mixing engines is a `replicas` change; EPP
dispatches across every pod of the pool (with 2.6, both endpoints receive
traffic and are scored).

### Stage 3 -- weights from MinIO

| # | Task | Files |
| --- | --- | --- |
| 3.1 | Upload checkpoints to a MinIO bucket from `models/manifest.yaml` | `scripts/`, `models/` |
| 3.2 | Cache fill and checksum verification on GPU nodes (initContainer or DaemonSet) | engine charts or a new chart |
| 3.3 | Remove hostPath model PVs and `gpu-objects-up`'s PV part | `k8s/overlays/remote-wsl-vllm-nvfp4/`, `Makefile` |

Acceptance: an empty GPU node serves after a cold start with no manual copy.

Implementation (2026-10-02):

- Bucket `models` in the existing MinIO tenant, one prefix per
  `models/manifest.yaml` name, anonymous read (public checkpoints), writes
  with the root user. `scripts/models-upload` runs a Job on a GPU node that
  verifies the source files against `models/checksums.txt` and uploads them.
- Each engine pod has a `model-cache` initContainer that fetches the listed
  files into `/var/lib/llm-stack/models` on its node, verifies them, and
  publishes the model atomically; a cache whose recorded checksums match is
  reused. Scripts and checksums come from the `model-cache` ConfigMap of
  `helm/airgap-stack`.
- Image: `rclone/rclone`, pinned in `versions.lock.env` (`MODEL_CACHE_IMAGE`).
  The upstream MinIO server image is no longer pullable from docker.io or
  quay.io without authorization, so it could not carry `mc`; the tenant now
  runs the Pigsty-maintained build (`MINIO_IMAGE`).
- The tenant's PVC is declared 10Gi but holds the weights (45 GiB); local-path
  does not enforce the size. Size it properly before Stage 4.

### Stage 4 -- first bare-metal GPU node

| # | Task |
| --- | --- |
| 4.1 | Resolve open questions 2-3; stand up the control plane where remote agents can reach it |
| 4.2 | In-cluster registry; engine and helper images pulled from it |
| 4.3 | Node bring-up runbook: OS, driver, `k3s agent`, GPU Operator (driver preinstalled, CDI, DCGM, GFD) |
| 4.4 | Dashboards on DCGM names; `node` / `pod` / `gpu_product` variables |
| 4.5 | Per-GPU-class values profile for the new card |

Acceptance: the bare-metal node joins, passes `gpu-runtime-smoke`, and its
engine replica serves through the same pool and route.

### Stage 5 -- scale-out

| # | Task |
| --- | --- |
| 5.1 | Two or more real endpoints: benchmark, re-tune `flowControl.maxRequests` and `utilization-detector` per pool |
| 5.2 | EPP `replicas > 1` |
| 5.3 | Decide the WSL2 node's fate: CPU-only or retired |
| 5.4 | Update `docs/architecture` ("Inference profile"), `docs/install`, handbook engines pages (EN + RU) |
