HELM = helm
KUBECTL = kubectl
KUSTOMIZE_DIR = k8s
K8S_NAMESPACE = airgap-ai-stack
# Single-node k3d server. Site-specific: override on a differently named node.
K3D_NODE ?= k3d-llm-stack-server-0

# Pinned image and Helm chart versions. This file is the single source of
# truth for every --version below; do not inline a version in a recipe.
include versions.lock.env

# Local/site-specific overrides (gitignored). Not required to exist.
-include .env

.PHONY: up down logs ps smoke services-smoke inference-smoke pat-smoke preflight verify config gateway-up operators-up provision-grafana-oidc provision-pat-oidc provision-realm-security pat-image vllm-nvfp4-config vllm-nvfp4-smoke llmd-nvfp4-smoke smoke-nogpu stack-up nvfp4-up nvfp4-down gpu-objects-up gpu-objects-config vllm-up vllm-down sglang-up sglang-down embeddings-up embeddings-down embeddings-smoke llmd-up llmd-down render-check device-plugin-load device-plugin-up device-plugin-config device-plugin-status helm-render helm-env-values helm-up helm-down helm-diff monitoring-up

pat-image:
	docker buildx build --platform linux/amd64 --tag airgap-ai-stack/pat-service:local --load pat-service

# Both active profiles (local-mac and remote-wsl-vllm-nvfp4) route through
# plain Envoy Gateway Backends, not a GAIE InferencePool backendRef.
# remote-wsl-values.yaml keeps the AI Gateway translation hooks with an empty
# backendResources list. An InferencePool object *does* exist in the
# remote-wsl-vllm-nvfp4 profile (config/llmd/router-nvfp4-values.yaml,
# `inferencePool.create: true`) since TASK-qos-fair-share.md Stage 1B, but
# only as the namespace anchor EPP uses to resolve `InferenceObjective`
# priority bands -- it is never the route's backendRef.
GATEWAY_VALUES ?= config/gateway/remote-wsl-values.yaml
MONITORING_CHART ?= oci://docker.io/envoyproxy/gateway-addons-helm
# Grafana's public root URL (OIDC redirect target). Site-specific: the
# gpu-host.local default in config/gateway-addons/values.yaml is a generic
# placeholder; override with GRAFANA_BASE_URL in .env for a local dev host
# (e.g. GRAFANA_BASE_URL=http://grafana.***REMOVED***.local:32030) instead of
# committing that hostname.
GRAFANA_BASE_URL ?= http://grafana.gpu-host.local:32030

.PHONY: monitoring-up
monitoring-up:
	$(HELM) upgrade --install eg-addons $(MONITORING_CHART) --version $(ENVOY_GATEWAY_ADDONS_CHART_VERSION) --namespace monitoring --create-namespace --values config/gateway-addons/values.yaml --set grafana.env.GF_SERVER_ROOT_URL=$(GRAFANA_BASE_URL) --set-file grafana.dashboards.vllm.vllm.json=config/grafana/dashboards/vllm.json --set-file grafana.dashboards.llm-d.llm-d-diagnostic-drilldown-dashboard.json=config/grafana/dashboards/llm-d/llm-d-diagnostic-drilldown-dashboard.json --set-file grafana.dashboards.llm-d.llm-d-failure-saturation-dashboard.json=config/grafana/dashboards/llm-d/llm-d-failure-saturation-dashboard.json --set-file grafana.dashboards.llm-d.llm-d-inference-gateway.json=config/grafana/dashboards/llm-d/llm-d-inference-gateway.json --set-file grafana.dashboards.llm-d.llm-d-pd-coordinator-metrics.json=config/grafana/dashboards/llm-d/llm-d-pd-coordinator-metrics.json --set-file grafana.dashboards.llm-d.llm-d-performance-kv-cache.json=config/grafana/dashboards/llm-d/llm-d-performance-kv-cache.json --set-file grafana.dashboards.llm-d.llm-d-sglang-overview.json=config/grafana/dashboards/llm-d/llm-d-sglang-overview.json --set-file grafana.dashboards.llm-d.llm-d-vllm-overview.json=config/grafana/dashboards/llm-d/llm-d-vllm-overview.json --set-file grafana.dashboards.system-state.system-state.json=config/grafana/dashboards/system-state.json --set-file grafana.dashboards.fair-share.fair-share.json=config/grafana/dashboards/fair-share.json --set-file grafana.dashboards.fair-share.user-activity.json=config/grafana/dashboards/user-activity.json --set-file grafana.dashboards.fair-share.cluster-load.json=config/grafana/dashboards/cluster-load.json --set-file grafana.dashboards.cluster-monitor.cluster-monitor.json=config/grafana/dashboards/cluster-monitor.json --wait --timeout 5m
	$(KUBECTL) apply -f k8s/monitoring-tempo-headless.yaml

gateway-up:
	$(HELM) upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version $(ENVOY_GATEWAY_CHART_VERSION) --namespace envoy-gateway-system --create-namespace --values $(GATEWAY_VALUES) --set config.envoyGateway.extensionApis.enableBackend=true --set config.envoyGateway.rateLimit.backend.type=Redis --set config.envoyGateway.rateLimit.backend.redis.url=envoy-ratelimit-valkey.airgap-ai-stack.svc.cluster.local:6379 --wait
	$(KUBECTL) -n envoy-gateway-system rollout restart deployment/envoy-gateway
	$(KUBECTL) -n envoy-gateway-system rollout status deployment/envoy-gateway --timeout=120s
	$(MAKE) monitoring-up
	$(HELM) upgrade --install aieg-crd oci://docker.io/envoyproxy/ai-gateway-crds-helm --version $(AI_GATEWAY_CRDS_CHART_VERSION) --namespace envoy-ai-gateway-system --create-namespace --wait
	$(HELM) upgrade --install aieg oci://docker.io/envoyproxy/ai-gateway-helm --version $(AI_GATEWAY_CHART_VERSION) --namespace envoy-ai-gateway-system --create-namespace --values config/ai-gateway/values.yaml --wait

operators-up:
	$(HELM) repo add cnpg https://cloudnative-pg.github.io/charts
	$(HELM) repo add altinity https://helm.altinity.com
	$(HELM) repo add ot-helm https://ot-container-kit.github.io/helm-charts/
	$(HELM) repo update
	$(HELM) upgrade --install cnpg cnpg/cloudnative-pg --version $(CNPG_CHART_VERSION) --namespace cnpg-system --create-namespace --wait
	$(HELM) upgrade --install clickhouse-operator altinity/altinity-clickhouse-operator --version $(CLICKHOUSE_OPERATOR_CHART_VERSION) --namespace clickhouse-operator --create-namespace --set 'watchNamespaces[0]=$(K8S_NAMESPACE)' --wait
	$(HELM) upgrade --install redis-operator ot-helm/redis-operator --version $(REDIS_OPERATOR_CHART_VERSION) --namespace redis-operator --create-namespace --set featureGates.GenerateConfigInInitContainer=true --wait
	$(MAKE) minio-operator-up

# Chart 7.1.1 accepts repository@sha256 plus a separate digest. Keep the
# human-readable image tag as well; the digest determines the pulled content.
.PHONY: minio-operator-up
minio-operator-up:
	$(HELM) repo add minio-operator https://operator.min.io
	$(HELM) repo update minio-operator
	$(HELM) upgrade --install minio-operator minio-operator/operator --version $(MINIO_OPERATOR_CHART_VERSION) --namespace minio-operator --create-namespace --reuse-values --values config/minio-operator/values.yaml --set-string operator.image.repository=$(MINIO_OPERATOR_IMAGE_REPOSITORY):$(MINIO_OPERATOR_IMAGE_TAG)@sha256 --set-string operator.image.digest=$(MINIO_OPERATOR_IMAGE_DIGEST) --set-string operator.image.tag=$(MINIO_OPERATOR_IMAGE_TAG) --wait --timeout 5m

# --- Local Mac development profile (OrbStack + LM Studio, no GPU) ----------
# Deploys k8s/base plus the local-mac overlay, which owns the development
# `edge` Gateway on :8080, the host-based *.localhost routes and the whole LM
# Studio inference path. Unrelated to the remote profile's Helm release.
up: gateway-up operators-up pat-image
	$(KUBECTL) apply -f $(KUSTOMIZE_DIR)/base/namespace.yaml
	$(KUBECTL) -n $(K8S_NAMESPACE) create secret generic airgap-runtime --from-env-file=.env --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) apply -k $(KUSTOMIZE_DIR)
	$(KUBECTL) apply -k k8s/overlays/local-mac
	$(KUBECTL) apply -f $(KUSTOMIZE_DIR)/monitoring-referencegrant.yaml
	$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=condition=Ready cluster/pat-db --timeout=180s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/otel-collector
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/otel-collector --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/lmstudio-upstream
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/lmstudio-upstream --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/pat-service --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=condition=Programmed gateway/edge --timeout=120s
	$(MAKE) provision-grafana-oidc
	$(MAKE) provision-pat-oidc
	$(MAKE) provision-realm-security

provision-grafana-oidc:
	./scripts/provision-grafana-oidc

provision-pat-oidc:
	./scripts/provision-pat-oidc

provision-realm-security:
	./scripts/provision-realm-security

# Scales the application Deployments to zero without deleting PVCs. The LM
# Studio upstream exists only in the local-mac profile, so its absence on the
# remote profile is not an error.
down:
	$(KUBECTL) -n $(K8S_NAMESPACE) scale deployment/keycloak deployment/openwebui deployment/pat-service deployment/langfuse deployment/langfuse-worker deployment/grafana deployment/otel-collector deployment/prometheus --replicas=0
	-$(KUBECTL) -n $(K8S_NAMESPACE) scale deployment/lmstudio-upstream --replicas=0

logs:
	$(KUBECTL) -n $(K8S_NAMESPACE) get pods

ps:
	$(KUBECTL) -n $(K8S_NAMESPACE) get pods

config:
	$(KUBECTL) kustomize $(KUSTOMIZE_DIR)

preflight:
	./scripts/preflight

smoke:
	./scripts/smoke-test

services-smoke:
	./scripts/services-smoke-test

inference-smoke:
	./scripts/inference-smoke-test

# --- Remote single-GPU profile (Windows + WSL2 + k3d, RTX 5090) ------------
#
# ONE deploy path: `make stack-up`. It installs the prerequisites, applies the
# GPU objects that are deliberately kept out of the Helm release, runs the
# Helm release that owns the application stack and all routing, installs the
# llm-d router, and reconciles the Keycloak clients.
#
# `kubectl apply -k k8s/overlays/remote-wsl-vllm-nvfp4` is NOT a deploy path:
# the overlay holds workloads only and would leave the cluster without a
# Gateway. Use it for `--dry-run` validation (`make vllm-nvfp4-config`).

# Workstation access to the remote k3s API; reference-site defaults.
SSH ?= ssh
WSL_SSH_HOST ?= llmstack@gpu-host.local
WSL_SSH_PORT ?= 2222
K3S_LOCAL_PORT ?= 41755
K3S_REMOTE_PORT ?= 41755

.PHONY: k3s-tunnel
k3s-tunnel:
	$(SSH) -N -T -o ExitOnForwardFailure=yes \
		-o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
		-L "127.0.0.1:$(K3S_LOCAL_PORT):127.0.0.1:$(K3S_REMOTE_PORT)" \
		-p "$(WSL_SSH_PORT)" "$(WSL_SSH_HOST)"

vllm-nvfp4-config:
	$(KUBECTL) kustomize k8s/overlays/remote-wsl-vllm-nvfp4

vllm-nvfp4-smoke:
	./scripts/vllm-nvfp4-smoke-test

llmd-nvfp4-smoke:
	./scripts/llmd-nvfp4-smoke-test

# Everything the remote profile can prove while the GPU is busy elsewhere:
# routing, SSO, operator state, and the whole PAT lifecycle except the calls
# that reach the model. STACK_BASE_URL selects path routing.
smoke-nogpu:
	SMOKE_REQUIRE_LMSTUDIO=false ./scripts/services-smoke-test
	INFERENCE_SMOKE_REQUIRE_LMSTUDIO=false INFERENCE_SMOKE_VERIFY_CHAT=false ./scripts/llmd-nvfp4-smoke-test
	INFERENCE_SMOKE_VERIFY_CHAT=false ./scripts/pat-smoke-test
	./scripts/embeddings-smoke-test

embeddings-smoke:
	./scripts/embeddings-smoke-test

# GPU objects are excluded from the Helm release by scripts/helm-render
# (GPU_KEEP_OUT) so that a `helm upgrade` never restarts the model server.
# PV/PVC/RuntimeClass are applied directly; the vLLM Deployment is managed
# by the vllm-inference Helm release (make vllm-up / vllm-down).
GPU_OBJECTS = \
	k8s/overlays/remote-wsl-vllm-nvfp4/runtimeclass.yaml \
	k8s/overlays/remote-wsl-vllm-nvfp4/model-volume.yaml \
	k8s/overlays/remote-wsl-vllm-nvfp4/embeddings-model-volume.yaml

gpu-objects-config:
	$(KUBECTL) apply --dry-run=client $(addprefix -f ,$(GPU_OBJECTS)) >/dev/null

gpu-objects-up:
	$(KUBECTL) apply $(addprefix -f ,$(GPU_OBJECTS))

# vLLM inference deployment. Edit helm/vllm-inference/values.yaml, then
# run `make vllm-up` to apply. Settings: maxModelLen, maxNumSeqs,
# maxNumBatchedTokens, gpuMemoryUtilization, kvCacheDtype, prefixCaching.
VLLM_CHART = helm/vllm-inference
VLLM_RELEASE = vllm-inference

vllm-up:
	$(HELM) upgrade --install $(VLLM_RELEASE) $(VLLM_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(VLLM_CHART)/values.yaml \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 15m

vllm-down:
	$(HELM) uninstall $(VLLM_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

# SGLang inference deployment. Edit helm/sglang-inference/values.yaml, then
# run `make sglang-up` to apply. Requires the model PVC from gpu-objects-up
# and the sglang-api-key Secret from helm-up (SGLANG_API_KEY in .env).
SGLANG_CHART = helm/sglang-inference
SGLANG_RELEASE = sglang-inference

sglang-up:
	$(HELM) upgrade --install $(SGLANG_RELEASE) $(SGLANG_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(SGLANG_CHART)/values.yaml \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 15m

sglang-down:
	$(HELM) uninstall $(SGLANG_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

# bge-m3 embeddings deployment (docs/adr/0013-embeddings-api-bge-m3.md). Edit
# helm/embeddings-inference/values.yaml, then run `make embeddings-up`.
# `accelerator: cpu` (the default) has no ordering constraint. `accelerator:
# gpu` is refused unless the live LLM engine (vLLM or SGLang) already has a
# Running pod -- the ADR's fixed start order, checked here since Helm itself
# has no notion of "another release's Deployment is Ready".
EMBEDDINGS_CHART = helm/embeddings-inference
EMBEDDINGS_RELEASE = embeddings-inference

embeddings-up:
	@accel=$$(awk '/^accelerator:/{print $$2; exit}' $(EMBEDDINGS_CHART)/values.yaml); \
	if [ "$$accel" = "gpu" ]; then \
		running=$$($(KUBECTL) -n $(K8S_NAMESPACE) get pods -l 'app.kubernetes.io/name in (vllm-qwen38-nvfp4,sglang-qwen38)' --field-selector=status.phase=Running -o name 2>/dev/null); \
		if [ -z "$$running" ]; then \
			echo "embeddings-up: accelerator: gpu requires the live LLM engine (vLLM or SGLang) to be Ready first -- see docs/adr/0013-embeddings-api-bge-m3.md 'Start order is fixed'." >&2; \
			exit 1; \
		fi; \
	fi
	$(HELM) upgrade --install $(EMBEDDINGS_RELEASE) $(EMBEDDINGS_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(EMBEDDINGS_CHART)/values.yaml \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 15m

embeddings-down:
	$(HELM) uninstall $(EMBEDDINGS_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

llmd-up:
	$(HELM) upgrade --install llmd-qwen-test oci://ghcr.io/llm-d/charts/llm-d-router-standalone --version $(LLMD_ROUTER_CHART_VERSION) --namespace $(K8S_NAMESPACE) --create-namespace --values config/llmd/router-nvfp4-values.yaml --wait
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/llmd-qwen-test-epp --timeout=5m

llmd-down:
	$(HELM) uninstall llmd-qwen-test --namespace $(K8S_NAMESPACE) --ignore-not-found

# Full remote deploy. Run from the WSL2 host directly, or through the SSH
# tunnel from a workstation. pat-service is pulled from ghcr.io (see
# k8s/base/applications.yaml), no local build or image import needed.
stack-up: gateway-up operators-up
	$(KUBECTL) apply -f $(KUSTOMIZE_DIR)/base/namespace.yaml
	$(MAKE) gpu-objects-up
	$(MAKE) vllm-up
	$(MAKE) embeddings-up
	$(MAKE) helm-up
	$(KUBECTL) apply -f $(KUSTOMIZE_DIR)/monitoring-referencegrant.yaml
	$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=condition=Ready cluster/pat-db --timeout=180s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/otel-collector deployment/prometheus
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/otel-collector --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/prometheus --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/pat-service --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=condition=Programmed gateway/edge --timeout=120s
	$(MAKE) llmd-up
	$(MAKE) provision-grafana-oidc
	$(MAKE) provision-pat-oidc
	$(MAKE) provision-realm-security

# Compatibility aliases for the previous target names.
nvfp4-up: stack-up
nvfp4-down: llmd-down

# NVIDIA device plugin registers the RTX 5090 with the kubelet so that
# nvidia.com/gpu is advertised, scheduled, and accounted. Run on the WSL2
# host; the image must be imported before the DaemonSet is applied.
device-plugin-load:
	@deploy/vllm-qwen38-nvfp4/k3d-load-device-plugin

device-plugin-up:
	$(KUBECTL) apply -k k8s/overlays/remote-wsl-device-plugin

device-plugin-config:
	$(KUBECTL) kustomize k8s/overlays/remote-wsl-device-plugin
	$(KUBECTL) apply --dry-run=client -k k8s/overlays/remote-wsl-device-plugin >/dev/null

device-plugin-status:
	$(KUBECTL) -n kube-system rollout status daemonset/nvidia-device-plugin --timeout=2m
	$(KUBECTL) get node $(K3D_NODE) -o go-template='{{index .status.allocatable "nvidia.com/gpu"}}{{"\n"}}'

# Helm wrapper over the kustomize profile (see helm/airgap-stack).
# `helm-up` re-renders the chart templates from the kustomize sources, so
# iterative changes in the repo converge on a single `helm upgrade`. GPU
# inference objects (vLLM Deployment/Service, RuntimeClass, model PV/PVC)
# are filtered out by scripts/helm-render and stay managed by hand; the
# local pat-service image import also remains outside Helm (k3d-load-...).
HELM_CHART = helm/airgap-stack
HELM_RELEASE = airgap-stack
HELM_VALUES ?= $(HELM_CHART)/values.yaml
HELM_RUNTIME_VALUES = $(HELM_CHART)/runtime.secret.yaml
# --force-conflicts was added in Helm 4; omit it on Helm 3. It requires --server-side=true.
HELM_FORCE_CONFLICTS := $(shell $(HELM) upgrade --help 2>&1 | grep -q force-conflicts && echo --server-side=true --force-conflicts)

helm-render:
	./scripts/helm-render

helm-env-values:
	./scripts/helm-env-values

helm-up: helm-render helm-env-values
	$(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(K8S_NAMESPACE) --create-namespace \
		--values $(HELM_VALUES) \
		--values $(HELM_RUNTIME_VALUES) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 10m --debug
	$(MAKE) provision-grafana-oidc
	$(MAKE) provision-pat-oidc
	$(MAKE) provision-realm-security

helm-diff: helm-render helm-env-values
	$(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(K8S_NAMESPACE) --create-namespace \
		--values $(HELM_VALUES) \
		--values $(HELM_RUNTIME_VALUES) \
		--dry-run

helm-down:
	$(HELM) uninstall $(HELM_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

pat-smoke:
	./scripts/pat-smoke-test

# Everything that can be checked without a cluster and without the GPU:
# rendering, chart validity, and a guard against the generated chart
# resources drifting away from the kustomize sources.
verify: preflight render-check gpu-objects-config
	$(HELM) lint $(HELM_CHART) --values $(HELM_VALUES)
	$(HELM) template $(HELM_RELEASE) $(HELM_CHART) --namespace $(K8S_NAMESPACE) --values $(HELM_VALUES) >/dev/null
	printf '%s\n' 'Repository configuration is valid.'

# Fails if templates/resources.yaml or manifest.yaml is not what
# scripts/helm-render produces from the current kustomize sources.
render-check:
	./scripts/helm-render >/dev/null
	git diff --quiet -- $(HELM_CHART)/templates/resources.yaml $(HELM_CHART)/manifest.yaml \
		|| { printf '%s\n' 'helm-render output differs from the committed chart; commit the regenerated files.' >&2; \
		     git --no-pager diff --stat -- $(HELM_CHART)/templates/resources.yaml $(HELM_CHART)/manifest.yaml >&2; exit 1; }
