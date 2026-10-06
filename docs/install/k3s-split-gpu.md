# Installing on plain k3s: a CPU control host plus a GPU worker

A variant of the [main runbook](README.md) for a site where the cluster is
native k3s (no k3d, no WSL2). The control-plane host carries the
whole stack except the engines; the GPU worker is a separate VM that joins
later. The model weights sit on a detachable volume that moves from the
control host (where they were downloaded) to the GPU worker.

This is a record of the first such install, done on 2026-10-06. Every
command below was run as written there, unless marked **not yet done**.
The GPU worker there was an A100 (Ampere, no FP4), so the engine serves the
W4A16 checkpoint, not NVFP4 (step 11). Site values are placeholders:

| Placeholder | Meaning |
| --- | --- |
| `<host>` | control-plane host, `ubuntu@<host>` over SSH |
| `<host-ip>` | its public IP |
| `<origin>` | `STACK_BASE_URL`, e.g. `https://ai.example.com` |
| `<gpu-ip>` | public IP of the GPU worker |

## What the control host needs

- Ubuntu with passwordless sudo, a root disk for the cluster, and an empty
  extra volume for the model.
- **16 GiB RAM, 8+ vCPU.** 4 vCPU / 8 GiB is not enough: during the first
  `helm-up` the load average reached 100+, SSH and the API server stopped
  responding (`TLS handshake timeout`) and the run had to be redone after a
  resize. With everything except the engines running the host uses about
  10.5 GiB.
- A DNS A record for `<origin>` pointing at `<host-ip>` (for Let's Encrypt).

## 1. Model volume

Format the volume and mount it at the engine charts' `modelCache.hostPath`,
so that the same path works on the GPU worker later:

```sh
sudo mkfs.ext4 -q -L model -m 0 /dev/vdb
sudo mkdir -p /var/lib/llm-stack/models
echo "UUID=$(sudo blkid -s UUID -o value /dev/vdb) /var/lib/llm-stack/models ext4 defaults,nofail,x-systemd.device-timeout=10s 0 2" | sudo tee -a /etc/fstab
sudo systemctl daemon-reload && sudo mount /var/lib/llm-stack/models
sudo chown ubuntu:ubuntu /var/lib/llm-stack/models
```

## 2. Model weights onto the volume

`RadixArk-Qwen3.8-27B-NVFP4` is the Hugging Face repo
`RadixArk/Qwen3.8-27B-NVFP4` (its `config.json` matches the hash in
`models/checksums.txt`). The revision used was
`319f741cce68d7914884900c138a1fbb70a42f30`, about 22 GB.

```sh
curl -LsSf https://astral.sh/uv/install.sh | sh
~/.local/bin/uv tool install "huggingface_hub[hf_xet]"
cd /var/lib/llm-stack/models
tmux new-session -d -s model \
  "~/.local/bin/hf download RadixArk/Qwen3.8-27B-NVFP4 \
     --revision 319f741cce68d7914884900c138a1fbb70a42f30 \
     --local-dir RadixArk-Qwen3.8-27B-NVFP4"
```

Then verify and write the cache marker the `model-cache` initContainer
checks (`helm/airgap-stack/files/model-cache-fill.sh`). The marker is the
`model_sums` output: the model's lines from `models/checksums.txt` with the
`models/` prefix dropped. With it in place the engine pod reuses the volume
and never reads MinIO.

```sh
# on the workstation, from the repo
awk '$2 ~ /^models\/RadixArk-Qwen3.8-27B-NVFP4\// {sub(/^models\//,"",$2); print $1"  "$2}' \
  models/checksums.txt > sums
scp sums ubuntu@<host>:/tmp/sums

# on <host>
cd /var/lib/llm-stack/models
sha256sum -c --quiet /tmp/sums && cp /tmp/sums .RadixArk-Qwen3.8-27B-NVFP4.sha256
```

The `.cache/huggingface` directory `hf` leaves inside the model directory is
not listed in the checksums and does no harm.

## 3. k3s server

Same k3s version as `deploy/vllm-qwen38-nvfp4/k3d-create-nvidia`. The nodes
talk over the public internet, so pod traffic goes through WireGuard
(`wireguard-native`) rather than plain VXLAN.

```sh
curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=v1.35.5+k3s1 \
  INSTALL_K3S_EXEC="server --node-external-ip=<host-ip> --tls-san=<host-ip> \
    --flannel-backend=wireguard-native --flannel-external-ip --write-kubeconfig-mode=600" sh -
mkdir -p ~/.kube && sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config && sudo chown ubuntu ~/.kube/config
echo 'export KUBECONFIG=$HOME/.kube/config' >> ~/.bashrc
```

Disable Traefik: it takes ports 80/443 (needed by the TLS proxy below) and
installs its own Gateway API CRDs, which make `make gateway-up` fail with
`conflict occurred while applying object /gatewayclasses.gateway.networking.k8s.io`.

```sh
printf 'disable:\n  - traefik\n' | sudo tee /etc/rancher/k3s/config.yaml
sudo systemctl restart k3s
kubectl -n kube-system delete helmchart traefik traefik-crd
# Only on a fresh cluster with no Gateway objects yet:
kubectl get crd -o name | grep -E 'gateway.networking|traefik' | xargs kubectl delete
```

k3s's ServiceLB publishes the `edge` Gateway's LoadBalancer listener on the
host's port 3091 (`routing.edge.externalPort`).

## 4. TLS proxy

The stack never terminates TLS ([ADR 0004](../adr/0004-tls-terminates-at-the-site-proxy.md)).
Caddy on the host gets a Let's Encrypt certificate once DNS resolves:

```sh
sudo apt-get install -y caddy
sudo tee /etc/caddy/Caddyfile <<'EOF'
<origin-host> {
	reverse_proxy 127.0.0.1:3091
}
EOF
sudo systemctl enable --now caddy && sudo systemctl reload caddy
```

## 5. Tools and the repo on the host

`make` runs on the host itself, against the local cluster, with the site's
own `.env`, so the workstation's `.env` (another site's) is never involved.

```sh
sudo apt-get install -y make rsync jq
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash

# from the workstation
rsync -az --exclude .git --exclude .env --exclude runtime.secret.yaml \
  --exclude node_modules --exclude __pycache__ --exclude 'models/*/' \
  ./ ubuntu@<host>:llm-stack/
```

## 6. Site `.env`

Start from `.env.example` and generate every secret fresh. Settings for the
first pass, before a GPU exists:

- every `*_REPLICAS=0`, `STRATA_ON_HOST=0`;
- `STACK_BASE_URL=<origin>`;
- `WEB_SEARCH_ENABLED=false`, `REPOWISE_ENABLED=false`, `DSH_PUBLIC_ORIGIN=`;
- `KEYCLOAK_DEMO_USER_PASSWORD` / `KEYCLOAK_ADMIN_USER_PASSWORD`: must satisfy
  the realm policy (12+ characters with upper, lower, digit and special),
  otherwise the realm import fails and Keycloak exits with
  `invalidPasswordMinUpperCaseCharsMessage`.

```sh
python3 -c 'import secrets; print("Kc" + secrets.token_urlsafe(18) + "_9a")'
```

## 7. llm-d / GAIE CRDs

`make llmd-up` needs `InferenceObjective` (`llm-d.ai/v1alpha2`),
`InferenceModelRewrite` and `InferencePool`. No make target installs them.
On this install they were exported from the reference cluster and applied
here (with `last-applied-configuration` and `status` stripped):

```sh
kubectl get crd inferenceobjectives.llm-d.ai inferencemodelrewrites.llm-d.ai \
  inferencepools.inference.networking.k8s.io -o json > llmd-crds.json   # reference cluster
kubectl apply --server-side -f llmd-crds.json                            # new cluster
```

Without them `stack-up` stops at `llmd-up` with
`no matches for kind "InferenceObjective" in version "llm-d.ai/v1alpha2"`.

## 8. Deploy

```sh
cd ~/llm-stack
tmux new-session -d -s stack "bash -lc 'make stack-up 2>&1 | tee ~/stack-up.log'"
```

`stack-up` ends with an error at `embeddings-up`: `helm/embeddings-inference`
runs on the GPU (`accelerator: gpu`) and refuses to start before an engine
is Running. That is expected with no GPU. Run the remaining steps of
`stack-up` by hand:

```sh
kubectl apply -f k8s/monitoring-referencegrant.yaml
kubectl -n airgap-ai-stack wait --for=condition=Ready cluster/pat-db --timeout=180s
kubectl -n airgap-ai-stack rollout restart deployment/otel-collector deployment/prometheus
kubectl -n airgap-ai-stack rollout status deployment/pat-service --timeout=120s
kubectl -n airgap-ai-stack wait --for=condition=Programmed gateway/edge --timeout=120s
make provision-grafana-oidc provision-pat-oidc provision-realm-security provision-openwebui-offline-access
```

### Problems hit on the way

| Symptom | Cause | Fix |
| --- | --- | --- |
| `UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress` | the host rebooted mid-`helm-up`; release left `pending-install` with no deployed revision | delete only the release record, `kubectl -n airgap-ai-stack delete secret sh.helm.release.v1.airgap-stack.v1`, and rerun; `--take-ownership` adopts the existing objects |
| `minio-pool-0-0` `ImagePullBackOff` on `minio/minio:...` | upstream MinIO images need authorization | the tenant now uses `MINIO_IMAGE` (Pigsty build, `versions.lock.env`) |
| tenant image changed but the pod keeps the old one, operator logs `Waiting for Tenant to be healthy` | the operator only upgrades a healthy tenant | with no data in MinIO yet: delete `sts/minio-pool-0` and pod `minio-pool-0-0`; the operator recreates them with the new image |
| `services-smoke-test` dials `sso.ai.localhost:8080` | `STACK_BASE_URL` not in the environment, so it falls back to local-mac host mode | `set -a; . ./.env; set +a` first |
| Open WebUI login: "The email or password provided is incorrect"; its log has `OAuthError: not_allowed: Offline tokens not allowed for the user or client` | users imported from `realm-demo.json` get only their listed realm roles, not `default-roles-ai-stack` (which carries `offline_access`) | `realm-demo.json` now lists `default-roles-ai-stack` for `demo` and `admin`; on a realm imported before that, grant the role to each user through the admin API |

## 9. Verify

```sh
set -a; . ./.env; set +a
SMOKE_REQUIRE_LMSTUDIO=false ./scripts/services-smoke-test
curl -s <origin>/sso/realms/ai-stack/.well-known/openid-configuration   # issuer = <origin>/sso/realms/ai-stack
```

## 10. GPU worker

1. Detach the model volume from the control host: comment out its fstab
   line, `sudo umount /var/lib/llm-stack/models`, then move the volume to
   the GPU VM in the provider console. On the GPU VM add the same fstab line
   (same UUID: it is the same filesystem) and mount it.
2. The NVIDIA driver came with the VM image (`nvidia-driver-open` 615).
   Install the NVIDIA Container Toolkit:

   ```sh
   curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
     sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
   curl -sL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
     sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
     sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
   sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit
   ```

   k3s finds `/usr/bin/nvidia-container-runtime` at start and registers the
   `nvidia` containerd runtime itself; no containerd template is needed
   (unlike WSL2).
3. Join the cluster:

   ```sh
   # token: sudo cat /var/lib/rancher/k3s/server/node-token on <host>
   curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=v1.35.5+k3s1 \
     K3S_URL=https://<host-ip>:6443 K3S_TOKEN=<token> sh -s - agent \
     --node-external-ip=<gpu-ip> \
     --node-label node-role/inference=true \
     --node-taint nvidia.com/gpu=present:NoSchedule
   ```

4. NVIDIA device plugin. `nvcr.io` answers 403 from this site, so the
   upstream image cannot be pulled; the overlay's image
   (`llm-stack/k8s-device-plugin:0.20.0`) was streamed from the reference
   cluster's GPU node into the new node's containerd:

   ```sh
   ssh <reference-host> "docker exec k3d-llm-stack-agent-0 ctr -n k8s.io images export \
       --platform linux/amd64 - docker.io/llm-stack/k8s-device-plugin:0.20.0" |
     ssh ubuntu@<gpu-ip> 'sudo k3s ctr -n k8s.io images import --platform linux/amd64 -'
   ```

   Then on `<host>`: `make gpu-objects-up device-plugin-up`;
   `make device-plugin-status` must print `gpu inference=true gpu=1`.
5. `gpu-exporter` (Prometheus GPU metrics) now lands on the GPU node too.
   Its image used to be the locally built vLLM image, which does not exist
   here (`ImagePullBackOff`, and `helm-up` fails waiting for the
   DaemonSet). It now runs `python:3.12-slim`, pinned by digest; the
   `nvidia` RuntimeClass injects `nvidia-smi`.

## 11. Model and engine for A100

NVFP4 needs Blackwell; the A100 has no FP4 (or FP8) tensor cores. The
engine serves `RedHatAI/Qwen3.8-27B-INT4` (W4A16, compressed-tensors,
Marlin kernels), revision `91bd022d5b49442a868bc35008f6c21e1860edfa`,
about 19.5 GB, downloaded straight onto the volume on the GPU worker:

```sh
cd /var/lib/llm-stack/models
hf download RedHatAI/Qwen3.8-27B-INT4 \
  --revision 91bd022d5b49442a868bc35008f6c21e1860edfa \
  --local-dir RedHatAI-Qwen3.8-27B-INT4
```

It is in `models/manifest.yaml` and `models/checksums.txt`; write its cache
marker as in step 2 (`.RedHatAI-Qwen3.8-27B-INT4.sha256`). `make helm-up`
must run after `checksums.txt` changes, since the `model-cache` ConfigMap
carries it; otherwise the initContainer fails with
`no entries for <model> in models/checksums.txt`.

Engine settings are an overlay, `helm/sglang-inference/values-a100.yaml`
(target: 2x A100 40GB, `tensorParallelSize: 2`), selected in the host's
`.env`:

```sh
SGLANG_REPLICAS=1
SGLANG_EXTRA_ARGS=--values helm/sglang-inference/values-a100.yaml
```

then `make sglang-up`. The overlay sets `contextLength: 180224`,
`maxRunningRequests: 32` and `maxMambaCacheSize: 128`: SGLang keeps
4 mamba state slots per running request and caps `maxRunningRequests` at
`maxMambaCacheSize / 4` (with 96 it logs `max_running_requests is capped
to 24 by the mamba state cache`). With `kvCacheDtype: auto` SGLang picks
`fp8_e4m3` for this checkpoint's KV cache.

The first worker had a single A100 80GB, so the overlay was tested at TP=1
through a second values file on the host (a `--set` with `nvidia\.com/gpu`
does not survive make's shell quoting):

```yaml
# ~/sglang-1gpu.yaml
inference:
  tensorParallelSize: 1
  memFractionStatic: "0.90"
resources:
  requests: { nvidia.com/gpu: "1" }
  limits: { nvidia.com/gpu: "1" }
```

```sh
make sglang-up SGLANG_EXTRA_ARGS="--values helm/sglang-inference/values-a100.yaml --values $HOME/sglang-1gpu.yaml"
```

What SGLang reported there:

| | 1x A100 80GB, TP=1, 0.90 |
| --- | --- |
| weights | 17.65 GB |
| mamba state, 128 slots | 9.4 GB |
| KV cache | fp8, 1,431,737 tokens (43.7 GB) |
| `max_running_requests` / `context_len` | 32 / 180224 |
| start to ready | ~7 min, ~6 of them CUDA graph capture |

If a `helm upgrade` of the engine is interrupted (its tmux session killed
during `--wait`), the release is left `pending-upgrade`; delete that
revision's `sh.helm.release.v1.sglang-inference.v<N>` secret and rerun.

## 12. Benchmark

1x A100 80GB, TP=1, `values-a100.yaml` (`contextLength` 180224,
`maxRunningRequests` 32, `chunkedPrefillSize` 2048), straight at the engine:
`python3 -m sglang.bench_serving` run inside the engine pod against
`127.0.0.1:8000` (`kubectl exec deploy/sglang-qwen38 -c sglang -- ...`).

Decode, ShareGPT prompts, 512 output tokens, `--seed 1`. The pod runs with
`HF_HUB_OFFLINE=1`, so the dataset is downloaded on the host and copied in
(`kubectl cp sharegpt.json <pod>:/tmp/sharegpt.json -c sglang`, then
`--dataset-path /tmp/sharegpt.json`). Random-token prompts (`random-ids`)
are useless for MTP: drafts of random text are never accepted.

| Concurrency | no MTP, tok/s | no MTP, TPOT ms | MTP, tok/s | MTP, TPOT ms | MTP gain |
| --- | --- | --- | --- | --- | --- |
| 1 | 73 | 13.3 | 123 | 7.8 | +68% |
| 4 | 268 | 14.2 | 392 | 9.1 | +46% |
| 8 | 503 | 15.0 | 634 | 11.6 | +26% |
| 16 | 856 | 17.4 | 874 | 16.8 | +2% |
| 32 | 981 | 24.3 | 936 | 30.3 | -5% |

MTP is the checkpoint's own layer (`model_mtp.safetensors`, bf16):
`inference.speculative` = `{algorithm: NEXTN, numSteps: 3, eagleTopk: 1,
numDraftTokens: 4}`; mean accept length 2.96. It costs KV: SGLang adds a
9.3 GB intermediate mamba-state cache, and the KV pool drops from 1,431,737
to 934,545 tokens (-35%).

Prefill, one uncached request (`random-ids`, `--warmup-requests 0`, radix
cache flushed before each run with `curl -X POST 127.0.0.1:8000/flush_cache`
inside the pod), MTP on:

| Input | TTFT | Prefill |
| --- | --- | --- |
| 32768 | 13 s | 2.5k tok/s |
| 100000 | 59 s | 1.7k tok/s |
| 150000 | 106 s | 1.4k tok/s |

Long-context prefill is the weak point of this model on A100: a 150k prompt
waits ~1.75 min for its first token.

Measurement traps met on the way:

- bench_serving's warmup request sends the first prompt once before the
  measured run; with the radix cache on, the measured request then hits the
  cache (`#cached-token: 165568` in the engine log, TTFT ~1 s). The first
  long-context numbers taken here averaged one cached and one cold request
  (mean 54 s, P99 105 s) and were wrong. `--seed` does not change
  `random-ids` prompts.
- `random-ids` prompts grow ~10% when SGLang retokenizes them: a
  170000-token request arrives as ~186.5k and is rejected against the 180224
  context (`The input (186562 tokens) is longer than the model's context
  length`), which bench_serving still counts as successful with TTFT 0.

`chunkedPrefillSize` 8192 instead of 2048: no change in ShareGPT decode
(within 3% at every concurrency), the same long-context TTFT, more
activation memory, and under 4 concurrent 150k prompts `/health` stopped
answering within the probe's 5 s and the liveness probe restarted the engine
mid-benchmark. The overlay keeps 2048.

### vLLM on A100

`helm/vllm-inference/values-a100.yaml`, selected with
`VLLM_EXTRA_ARGS=--values helm/vllm-inference/values-a100.yaml` (and
`VLLM_REPLICAS=1`, `SGLANG_REPLICAS=0`). The RTX 5090 image is a local
build; the overlay uses its upstream base `vllm/vllm-openai@sha256:0a51...`
(vLLM 0.27.1), whose kernels cover sm80. Same model, `maxModelLen` 180224,
`maxNumSeqs` 32, MTP `{method: mtp, num_speculative_tokens: 3}`. The chart
gained `inference.tensorParallelSize` (`--tensor-parallel-size`).

`kvCacheDtype: auto` resolves to FP8 for this checkpoint, and the Triton
backend rejects FP8 KV on sm80 (`native FP8 (fp8e4nv) requires SM89+`); the
pod crash-loops until the KV dtype or the backend changes. Measured with
`vllm bench serve` in the pod (ShareGPT, 512 output tokens; its ShareGPT
sampling differs from SGLang's, so input totals differ):

| | FlashInfer, fp8 KV (overlay) | Triton, bf16 KV | SGLang, MTP |
| --- | --- | --- | --- |
| KV pool | 1,353,088 tokens | 717,156 | 934,545 |
| decode, 1 request | 54 tok/s | 104 | 121 |
| decode, 8 requests | 354 | 344 | 620 |
| decode, 32 requests | 764 | 860 | 951 |
| MTP accept length | 2.55 | 2.55 | 2.96 |
| cold prefill 100k / 150k | 51 s / 91 s | not measured | 59 s / 106 s |

With FlashInfer and spec decode vLLM drops CUDA graphs to PIECEWISE, which
is where its decode speed goes. vLLM prefills ~15% faster; SGLang decodes
faster at every concurrency. SGLang stays the engine of record for this
site; the vLLM overlay keeps FlashInfer for the larger KV pool.

### Embeddings on A100

`make embeddings-up` with `EMBEDDINGS_EXTRA_ARGS=--values
helm/embeddings-inference/values-a100.yaml` in the host's `.env`. The
overlay sets TEI's sm80 image (`EMBEDDINGS_GPU_IMAGE_SM80`; the default is
the sm120 build) and `maxConcurrentRequests: 64`. `bge-m3` goes onto the
volume the same way as the LLM (dense files only, the ones listed in
`models/checksums.txt`, plus the `.bge-m3.sha256` marker). TEI shares the
GPU with the engine outside scheduler accounting (~1.6 GB next to SGLang's
75 GB at `memFractionStatic` 0.90).

`EMBEDDINGS_SMOKE_ENABLED=true STACK_BASE_URL=<origin> ./scripts/embeddings-smoke-test`
passes. With the base `maxConcurrentRequests: 16` it failed on the
32-item batch: TEI counts every input against that limit, so batches above
16 items get `429 Model is overloaded`.

## Open items

- `OPENWEBUI_AUTOMATIONS_PAT` is still the placeholder: issue it with
  `scripts/kc-pat-issue` for a dedicated user.
- The Keycloak bootstrap admin is still `admin-demo-only`
  (`k8s/base/applications.yaml`); it is reachable only via port-forward,
  but rotate it.
- The k3s API (6443) and the plain-HTTP edge listener (3091) are reachable
  from the internet; no host firewall is configured.
- The llm-d / GAIE CRDs have no pinned source in the repo (step 7).
- The base `helm/embeddings-inference` values pair `maxClientBatchSize: 32`
  with `maxConcurrentRequests: 16`, which rejects batches above 16 items
  (step 12); only the A100 overlay fixes it.
- The engines' liveness probe (5 s timeout, 3 x 30 s) can restart SGLang
  during several concurrent 150k prefills (step 12).
- The device plugin image has no registry this site can pull from (step 10).
- TP=2 on 2x A100 40GB is not yet run; the numbers in step 11 are TP=1.
