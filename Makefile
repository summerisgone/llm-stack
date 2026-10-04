HELM = helm
KUBECTL = kubectl
KUSTOMIZE_DIR = k8s
K8S_NAMESPACE = airgap-ai-stack
# Pinned image and Helm chart versions. This file is the single source of
# truth for every --version below; do not inline a version in a recipe.
include versions.lock.env

# Local/site-specific overrides (gitignored). Not required to exist.
-include .env

# Engine capacity is replicas per engine (ADR 0019). vLLM and SGLang pods are
# members of the qwen-3.8-27b pool behind EPP; ninfer (qwen-3.8-27b-ninfer)
# and Strata (qwen-3.8-flash-next) are their own model names. Set the counts in .env; `make engines-up` applies
# them. One GPU here: keep the total at 1. docs/handbook/engines/README.md.
VLLM_REPLICAS ?= 1
SGLANG_REPLICAS ?= 0
NINFER_REPLICAS ?= 0
STRATA_REPLICAS ?= 0
# 1 while Strata runs on the GPU host (deploy/strata/run) instead of in the
# cluster: it holds the GPU outside Kubernetes, so GPU embeddings may start
# without an engine pod. Keep every *_REPLICAS at 0 then.
STRATA_ON_HOST ?= 0
ENGINE_DEPLOYMENT_vllm = vllm-qwen38-nvfp4
ENGINE_DEPLOYMENT_sglang = sglang-qwen38
ENGINE_DEPLOYMENT_ninfer = ninfer-qwen38
ENGINE_DEPLOYMENT_strata = strata-flash-next

.PHONY: up down logs ps smoke services-smoke inference-smoke pat-smoke preflight verify config gateway-up operators-up provision-grafana-oidc provision-pat-oidc provision-realm-security provision-openwebui-offline-access pat-image vllm-nvfp4-config vllm-nvfp4-smoke llmd-nvfp4-smoke smoke-nogpu stack-up nvfp4-up nvfp4-down gpu-objects-up gpu-objects-config vllm-up vllm-down sglang-up sglang-down ninfer-up ninfer-down strata-up strata-down embeddings-up embeddings-down embeddings-smoke llmd-up llmd-down engines-up render-check device-plugin-load device-plugin-up device-plugin-config device-plugin-status helm-render helm-env-values helm-up helm-down helm-diff monitoring-up agent-catalog agent-broker-image agent-adapter-images agents-k3d-load agents-up agents-down agents-smoke agents-test openwebui-agent-pipe websearch-up websearch-down websearch-smoke web-search-mcp-test repowise-up repowise-down provision-repowise-oidc provision-dsh-oidc

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
# (e.g. GRAFANA_BASE_URL=http://grafana.example.local:32030) instead of
# committing that hostname.
GRAFANA_BASE_URL ?= http://grafana.gpu-host.local:32030

# The browser-facing origin (.env STACK_BASE_URL, no trailing slash). Tracked
# manifests carry the placeholder origin below instead of the site's own;
# helm-render turns it into .Values.oidc.publicBaseURL, which helm-up sets
# from here, and agents-up/monitoring-up substitute it the same way.
STACK_ORIGIN = $(patsubst %/,%,$(STACK_BASE_URL))
PLACEHOLDER_ORIGIN = https://ai.example.com

.PHONY: monitoring-up
monitoring-up:
	@[ -n "$(STACK_ORIGIN)" ] || { echo "monitoring-up: STACK_BASE_URL must be set in .env" >&2; exit 1; }
	$(HELM) upgrade --install eg-addons $(MONITORING_CHART) --version $(ENVOY_GATEWAY_ADDONS_CHART_VERSION) --namespace monitoring --create-namespace --values config/gateway-addons/values.yaml --set grafana.env.GF_SERVER_ROOT_URL=$(GRAFANA_BASE_URL) --set grafana.env.GF_AUTH_GENERIC_OAUTH_AUTH_URL=$(STACK_ORIGIN)/sso/realms/ai-stack/protocol/openid-connect/auth --set-file grafana.dashboards.vllm.vllm.json=config/grafana/dashboards/vllm.json --set-file grafana.dashboards.llm-d.llm-d-diagnostic-drilldown-dashboard.json=config/grafana/dashboards/llm-d/llm-d-diagnostic-drilldown-dashboard.json --set-file grafana.dashboards.llm-d.llm-d-failure-saturation-dashboard.json=config/grafana/dashboards/llm-d/llm-d-failure-saturation-dashboard.json --set-file grafana.dashboards.llm-d.llm-d-inference-gateway.json=config/grafana/dashboards/llm-d/llm-d-inference-gateway.json --set-file grafana.dashboards.llm-d.llm-d-pd-coordinator-metrics.json=config/grafana/dashboards/llm-d/llm-d-pd-coordinator-metrics.json --set-file grafana.dashboards.llm-d.llm-d-performance-kv-cache.json=config/grafana/dashboards/llm-d/llm-d-performance-kv-cache.json --set-file grafana.dashboards.llm-d.llm-d-sglang-overview.json=config/grafana/dashboards/llm-d/llm-d-sglang-overview.json --set-file grafana.dashboards.llm-d.llm-d-vllm-overview.json=config/grafana/dashboards/llm-d/llm-d-vllm-overview.json --set-file grafana.dashboards.system-state.system-state.json=config/grafana/dashboards/system-state.json --set-file grafana.dashboards.fair-share.fair-share.json=config/grafana/dashboards/fair-share.json --set-file grafana.dashboards.fair-share.user-activity.json=config/grafana/dashboards/user-activity.json --set-file grafana.dashboards.fair-share.cluster-load.json=config/grafana/dashboards/cluster-load.json --set-file grafana.dashboards.cluster-monitor.cluster-monitor.json=config/grafana/dashboards/cluster-monitor.json --set-file grafana.dashboards.ninfer.ninfer.json=config/grafana/dashboards/ninfer.json --wait --timeout 5m
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
	$(MAKE) provision-openwebui-offline-access

provision-grafana-oidc:
	./scripts/provision-grafana-oidc

provision-pat-oidc:
	PUBLIC_BASE_URL=$(STACK_ORIGIN) ./scripts/provision-pat-oidc

provision-realm-security:
	./scripts/provision-realm-security

provision-openwebui-offline-access:
	./scripts/provision-openwebui-offline-access

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
# The RuntimeClass is applied directly; engines are their own Helm releases,
# and model weights come from MinIO into each GPU node's cache (ADR 0019,
# scripts/models-upload).
GPU_OBJECTS = \
	k8s/overlays/remote-wsl-vllm-nvfp4/runtimeclass.yaml

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
		--set replicas=$(VLLM_REPLICAS) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 15m

vllm-down:
	$(HELM) uninstall $(VLLM_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

# SGLang inference deployment. Edit helm/sglang-inference/values.yaml, then
# run `make sglang-up` to apply. Requires the model in MinIO (scripts/models-upload).
SGLANG_CHART = helm/sglang-inference
SGLANG_RELEASE = sglang-inference

sglang-up:
	$(HELM) upgrade --install $(SGLANG_RELEASE) $(SGLANG_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(SGLANG_CHART)/values.yaml \
		--set replicas=$(SGLANG_REPLICAS) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 15m

sglang-down:
	$(HELM) uninstall $(SGLANG_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

# ninfer pilot inference deployment (docs/adr/0015, ~/agent/ninfer-pilot-task.md).
# Edit helm/ninfer-inference/values.yaml, then run `make ninfer-up`. Requires
# the model in MinIO (scripts/models-upload) and the ninfer-api-key Secret from helm-up (NINFER_API_KEY in .env). One GPU on
# this node: do not run alongside vllm-up/sglang-up at replicas=1 -- k8s's
# device plugin refuses to co-schedule both (Risk 1, deploy/ninfer/README.md).
NINFER_CHART = helm/ninfer-inference
NINFER_RELEASE = ninfer-inference

ninfer-up:
	$(HELM) upgrade --install $(NINFER_RELEASE) $(NINFER_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(NINFER_CHART)/values.yaml \
		--set replicas=$(NINFER_REPLICAS) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 15m

ninfer-down:
	$(HELM) uninstall $(NINFER_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

# Strata, Qwen3.8-Flash-Next IQ3_S under its own model name (deploy/strata/README.md).
# Requires the image imported on the GPU node, the model in MinIO
# (scripts/models-upload) and the strata-api-key Secret from helm-up
# (STRATA_API_KEY in .env). One GPU: the other engines must be at 0.
STRATA_CHART = helm/strata-inference
STRATA_RELEASE = strata-inference

strata-up:
	$(HELM) upgrade --install $(STRATA_RELEASE) $(STRATA_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(STRATA_CHART)/values.yaml \
		--set replicas=$(STRATA_REPLICAS) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 30m

strata-down:
	$(HELM) uninstall $(STRATA_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

# bge-m3 embeddings deployment (docs/adr/0013-embeddings-api-bge-m3.md). Edit
# helm/embeddings-inference/values.yaml, then run `make embeddings-up`.
# `accelerator: cpu` (the default) has no ordering constraint. `accelerator:
# gpu` is refused unless the live LLM engine (vLLM, SGLang, ninfer or Strata) already has a
# Running pod -- the ADR's fixed start order, checked here since Helm itself
# has no notion of "another release's Deployment is Ready".
EMBEDDINGS_CHART = helm/embeddings-inference
EMBEDDINGS_RELEASE = embeddings-inference

embeddings-up:
	@accel=$$(awk '/^accelerator:/{print $$2; exit}' $(EMBEDDINGS_CHART)/values.yaml); \
	if [ "$$accel" = "gpu" ] && [ "$(STRATA_ON_HOST)" != 1 ]; then \
		running=$$($(KUBECTL) -n $(K8S_NAMESPACE) get pods -l 'app.kubernetes.io/name in (vllm-qwen38-nvfp4,sglang-qwen38,ninfer-qwen38,strata-flash-next)' --field-selector=status.phase=Running -o name 2>/dev/null); \
		if [ -z "$$running" ]; then \
			echo "embeddings-up: accelerator: gpu requires the live LLM engine (vLLM, SGLang, ninfer or Strata) to be Ready first -- see docs/adr/0013-embeddings-api-bge-m3.md 'Start order is fixed'." >&2; \
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

# Web search (docs/adr/0017-web-search-mcp-openserp-kagent.md): OpenSERP,
# its engine-pinning sidecar and web-search-mcp. Open WebUI's MCP tool
# connection (ConfigMap openwebui-tool-servers, helm/airgap-stack since
# docs/adr/0018 section 7), pat-service /mcp/ and the agent profile entry
# follow WEB_SEARCH_ENABLED in .env (helm-up, agents-up), not this release.
# Open WebUI reads the tool connections only at start, hence the restart.
WEBSEARCH_CHART = helm/web-search
WEBSEARCH_RELEASE = web-search

websearch-up:
	$(HELM) upgrade --install $(WEBSEARCH_RELEASE) $(WEBSEARCH_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(WEBSEARCH_CHART)/values.yaml \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 10m
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/openwebui

websearch-down:
	$(HELM) uninstall $(WEBSEARCH_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/openwebui

websearch-smoke:
	./scripts/websearch-smoke-test

# Repowise (docs/adr/0018-repowise-codebase-intelligence.md). Refuses unless
# REPOWISE_ENABLED=true and the embeddings run on the GPU with one Ready
# replica (section 6a). Secrets come from .env. Open WebUI reads its tool
# connections only at start, hence the restart; pat-service /mcp/repowise/
# and the agent entry follow REPOWISE_ENABLED (helm-up, agents-up).
REPOWISE_CHART = helm/repowise
REPOWISE_RELEASE = repowise

repowise-up:
	@[ "$(REPOWISE_ENABLED)" = true ] || { echo "repowise-up: REPOWISE_ENABLED is not true in .env" >&2; exit 1; }
	@[ -n "$(REPOWISE_PUBLIC_ORIGIN)" ] && [ -n "$(STACK_BASE_URL)" ] || { echo "repowise-up: REPOWISE_PUBLIC_ORIGIN and STACK_BASE_URL must be set in .env" >&2; exit 1; }
	@[ -n "$(REPOWISE_PAT)" ] || { echo "repowise-up: REPOWISE_PAT is empty in .env (svc-repowise PAT, ADR 0018 section 6)" >&2; exit 1; }
	@[ -n "$(REPOWISE_OIDC_CLIENT_SECRET)" ] && [ -n "$(REPOWISE_API_KEY)" ] || { echo "repowise-up: REPOWISE_OIDC_CLIENT_SECRET and REPOWISE_API_KEY must be set in .env (run make provision-repowise-oidc after setting the secret)" >&2; exit 1; }
	@rc=$$($(KUBECTL) -n $(K8S_NAMESPACE) get deploy embeddings-bge-m3 -o jsonpath='{.spec.template.spec.runtimeClassName}/{.status.readyReplicas}' 2>/dev/null); \
	if [ "$$rc" != "nvidia/1" ]; then \
		echo "repowise-up: embeddings-bge-m3 must run in accelerator: gpu mode with 1 Ready replica (got '$$rc') -- docs/adr/0018 section 6a." >&2; \
		exit 1; \
	fi
	@$(KUBECTL) -n $(K8S_NAMESPACE) create secret generic repowise \
		--from-literal=REPOWISE_API_KEY='$(REPOWISE_API_KEY)' \
		--from-literal=OPENAI_API_KEY='$(REPOWISE_PAT)' \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	@$(KUBECTL) -n $(K8S_NAMESPACE) create secret generic repowise-credential \
		--from-literal=credential='Bearer $(REPOWISE_API_KEY)' \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	@$(KUBECTL) -n $(K8S_NAMESPACE) create secret generic repowise-oidc \
		--from-literal=client-secret='$(REPOWISE_OIDC_CLIENT_SECRET)' \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(HELM) upgrade --install $(REPOWISE_RELEASE) $(REPOWISE_CHART) \
		--namespace $(K8S_NAMESPACE) \
		--values $(REPOWISE_CHART)/values.yaml \
		--set image=$(REPOWISE_IMAGE) \
		--set publicOrigin=$(REPOWISE_PUBLIC_ORIGIN) \
		--set stackOrigin=$(STACK_BASE_URL) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 60m
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/openwebui

repowise-down:
	$(HELM) uninstall $(REPOWISE_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found
	$(KUBECTL) -n $(K8S_NAMESPACE) delete secret repowise repowise-credential repowise-oidc --ignore-not-found
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/openwebui

provision-repowise-oidc:
	./scripts/provision-repowise-oidc

# docs/adr/0020: Keycloak client dsh-web for the dsh web UI host; a no-op
# while DSH_PUBLIC_ORIGIN is empty.
provision-dsh-oidc:
	./scripts/provision-dsh-oidc

web-search-mcp-test:
	cd web-search-mcp && go vet ./... && go test ./...

llmd-up:
	$(HELM) upgrade --install llmd-qwen-test oci://ghcr.io/llm-d/charts/llm-d-router-standalone@$(LLMD_ROUTER_STANDALONE_CHART_DIGEST) --namespace $(K8S_NAMESPACE) --create-namespace --values config/llmd/router-nvfp4-values.yaml --wait
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/llmd-qwen-test-epp --timeout=5m

llmd-down:
	$(HELM) uninstall llmd-qwen-test --namespace $(K8S_NAMESPACE) --ignore-not-found

# Full remote deploy. Run from the WSL2 host directly, or through the SSH
# tunnel from a workstation. pat-service is pulled from ghcr.io (see
# k8s/base/applications.yaml), no local build or image import needed.
stack-up: gateway-up operators-up
	$(KUBECTL) apply -f $(KUSTOMIZE_DIR)/base/namespace.yaml
	$(MAKE) gpu-objects-up
	$(MAKE) helm-up
	$(MAKE) engines-up
ifeq ($(WEB_SEARCH_ENABLED),true)
	$(MAKE) websearch-up
endif
	$(KUBECTL) apply -f $(KUSTOMIZE_DIR)/monitoring-referencegrant.yaml
	$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=condition=Ready cluster/pat-db --timeout=180s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout restart deployment/otel-collector deployment/prometheus
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/otel-collector --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/prometheus --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/pat-service --timeout=120s
	$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=condition=Programmed gateway/edge --timeout=120s
	$(MAKE) provision-grafana-oidc
	$(MAKE) provision-pat-oidc
	$(MAKE) provision-realm-security
	$(MAKE) provision-openwebui-offline-access

# Apply VLLM_REPLICAS, SGLANG_REPLICAS, NINFER_REPLICAS and STRATA_REPLICAS in the order a
# shared GPU allows: GPU embeddings and the engines going to 0 stop first,
# then the others start, then EPP, then GPU embeddings (ADR 0013 start
# order). Changing which engine serves is a replica change (ADR 0019).
engine_replicas = $(if $(filter vllm,$(1)),$(VLLM_REPLICAS),$(if $(filter sglang,$(1)),$(SGLANG_REPLICAS),$(if $(filter strata,$(1)),$(STRATA_REPLICAS),$(NINFER_REPLICAS))))
engines-up:
	@accel=$$(awk '/^accelerator:/{print $$2; exit}' $(EMBEDDINGS_CHART)/values.yaml); \
	if [ "$$accel" = gpu ] && $(KUBECTL) -n $(K8S_NAMESPACE) get deployment/embeddings-bge-m3 >/dev/null 2>&1; then \
		$(KUBECTL) -n $(K8S_NAMESPACE) scale deployment/embeddings-bge-m3 --replicas=0; \
	fi
	@if [ "$(call engine_replicas,vllm)" = 0 ]; then \
		$(MAKE) vllm-up && \
		$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=delete pod -l app.kubernetes.io/name=$(ENGINE_DEPLOYMENT_vllm) --timeout=5m; \
	fi
	@if [ "$(call engine_replicas,sglang)" = 0 ]; then \
		$(MAKE) sglang-up && \
		$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=delete pod -l app.kubernetes.io/name=$(ENGINE_DEPLOYMENT_sglang) --timeout=5m; \
	fi
	@if [ "$(call engine_replicas,ninfer)" = 0 ]; then \
		$(MAKE) ninfer-up && \
		$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=delete pod -l app.kubernetes.io/name=$(ENGINE_DEPLOYMENT_ninfer) --timeout=5m; \
	fi
	@if [ "$(call engine_replicas,strata)" = 0 ]; then \
		$(MAKE) strata-up && \
		$(KUBECTL) -n $(K8S_NAMESPACE) wait --for=delete pod -l app.kubernetes.io/name=$(ENGINE_DEPLOYMENT_strata) --timeout=5m; \
	fi
	@if [ "$(call engine_replicas,vllm)" != 0 ]; then $(MAKE) vllm-up; fi
	@if [ "$(call engine_replicas,sglang)" != 0 ]; then $(MAKE) sglang-up; fi
	@if [ "$(call engine_replicas,ninfer)" != 0 ]; then $(MAKE) ninfer-up; fi
	@if [ "$(call engine_replicas,strata)" != 0 ]; then $(MAKE) strata-up; fi
	$(MAKE) llmd-up
	$(MAKE) embeddings-up
	$(KUBECTL) -n $(K8S_NAMESPACE) scale deployment/embeddings-bge-m3 --replicas=1
	$(KUBECTL) -n $(K8S_NAMESPACE) rollout status deployment/embeddings-bge-m3 --timeout=10m
	@printf 'Engine replicas: vllm=%s sglang=%s ninfer=%s strata=%s\n' '$(VLLM_REPLICAS)' '$(SGLANG_REPLICAS)' '$(NINFER_REPLICAS)' '$(STRATA_REPLICAS)'

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
	$(KUBECTL) get nodes -o go-template='{{range .items}}{{.metadata.name}} inference={{index .metadata.labels "node-role/inference"}} gpu={{index .status.allocatable "nvidia.com/gpu"}}{{"\n"}}{{end}}'

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

HELM_ORIGIN_SETS = --set oidc.publicBaseURL=$(STACK_ORIGIN) \
	--set oidc.externalIssuer=$(STACK_ORIGIN)/sso/realms/ai-stack \
	--set dshWeb.publicOrigin=$(DSH_PUBLIC_ORIGIN) \
	--set-file modelCache.checksums=models/checksums.txt

helm-up: helm-render helm-env-values
	@[ -n "$(STACK_ORIGIN)" ] || { echo "helm-up: STACK_BASE_URL must be set in .env" >&2; exit 1; }
	$(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(K8S_NAMESPACE) --create-namespace \
		--values $(HELM_VALUES) \
		--values $(HELM_RUNTIME_VALUES) \
		$(HELM_ORIGIN_SETS) \
		--take-ownership \
		$(HELM_FORCE_CONFLICTS) \
		--wait --timeout 10m --debug
	$(MAKE) provision-grafana-oidc
	$(MAKE) provision-pat-oidc
	$(MAKE) provision-realm-security
	$(MAKE) provision-openwebui-offline-access
	$(MAKE) provision-dsh-oidc

helm-diff: helm-render helm-env-values
	$(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(K8S_NAMESPACE) --create-namespace \
		--values $(HELM_VALUES) \
		--values $(HELM_RUNTIME_VALUES) \
		$(HELM_ORIGIN_SETS) \
		--dry-run

helm-down:
	$(HELM) uninstall $(HELM_RELEASE) --namespace $(K8S_NAMESPACE) --ignore-not-found

pat-smoke:
	./scripts/pat-smoke-test

# Everything that can be checked without a cluster and without the GPU:
# rendering, chart validity, and a guard against the generated chart
# resources drifting away from the kustomize sources.
verify: preflight render-check gpu-objects-config
	./scripts/docs-check
	$(HELM) lint $(HELM_CHART) --values $(HELM_VALUES)
	$(HELM) template $(HELM_RELEASE) $(HELM_CHART) --namespace $(K8S_NAMESPACE) --values $(HELM_VALUES) >/dev/null
	$(HELM) lint $(WEBSEARCH_CHART)
	$(HELM) template $(WEBSEARCH_RELEASE) $(WEBSEARCH_CHART) --namespace $(K8S_NAMESPACE) >/dev/null
	printf '%s\n' 'Repository configuration is valid.'

# Fails if templates/resources.yaml or manifest.yaml is not what
# scripts/helm-render produces from the current kustomize sources.
render-check:
	./scripts/helm-render >/dev/null
	git diff --quiet -- $(HELM_CHART)/templates/resources.yaml $(HELM_CHART)/manifest.yaml \
		|| { printf '%s\n' 'helm-render output differs from the committed chart; commit the regenerated files.' >&2; \
		     git --no-pager diff --stat -- $(HELM_CHART)/templates/resources.yaml $(HELM_CHART)/manifest.yaml >&2; exit 1; }

# --- Cloud agent fleet: Hermes, pi, opencode (docs/adr/0009, docs/adr/0014) --
# Catalog image tag = content hash of the base profile and the sync code, so
# the same catalog always has the same tag. Records it in versions.lock.env.
AGENT_CATALOG_SRC = config/agents/base-profile agent-catalog/Dockerfile agent-catalog/agent_sync.py
# Remote profile: AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents and
# AGENTS_PLATFORM=linux/amd64, then `make agents-k3d-load` before agents-up.
AGENTS_OVERLAY ?= k8s/agents
AGENTS_PLATFORM ?=
AGENTS_BUILD_FLAGS = $(if $(AGENTS_PLATFORM),--platform $(AGENTS_PLATFORM))
agent-catalog:
	python3 -m unittest discover -s agent-catalog -p 'test_*.py'
	tag=$$(find $(AGENT_CATALOG_SRC) -type f | LC_ALL=C sort | xargs shasum -a 256 | shasum -a 256 | cut -c1-12); \
	image=llm-stack/agent-catalog:$$tag; \
	docker buildx build --load $(AGENTS_BUILD_FLAGS) --build-context profile=config/agents/base-profile --tag $$image agent-catalog && \
	perl -pi -e "s|^AGENT_CATALOG_IMAGE=.*|AGENT_CATALOG_IMAGE=$$image|" versions.lock.env && \
	printf 'AGENT_CATALOG_IMAGE=%s\n' "$$image"

agent-broker-image:
	docker buildx build --load $(AGENTS_BUILD_FLAGS) --tag $(AGENT_BROKER_IMAGE) agent-broker

# pi, opencode and dsh agents behind agent-broker (agent-adapter/Dockerfile,
# one target each). Tag = content hash of the adapter sources, recorded as
# PI_IMAGE / OPENCODE_IMAGE / DSH_IMAGE in versions.lock.env. An empty one
# (e.g. in .env) leaves that agent out of Open WebUI.
AGENT_ADAPTER_SRC = agent-adapter/Dockerfile agent-adapter/package.json agent-adapter/server.mjs
agent-adapter-images:
	cd agent-adapter && node --test
	for rt in pi opencode dsh; do \
		tag=$$(shasum -a 256 $(AGENT_ADAPTER_SRC) agent-adapter/$$rt.mjs | shasum -a 256 | cut -c1-12); \
		image=llm-stack/agent-$$rt:$$tag; \
		var=$$(printf %s $$rt | tr a-z A-Z)_IMAGE; \
		docker buildx build --load $(AGENTS_BUILD_FLAGS) --target $$rt --tag $$image agent-adapter && \
		perl -pi -e "s|^$$var=.*|$$var=$$image|" versions.lock.env && \
		printf '%s=%s\n' "$$var" "$$image" || exit 1; \
	done

agents-k3d-load:
	WSL_SSH_HOST=$(WSL_SSH_HOST) WSL_SSH_PORT=$(WSL_SSH_PORT) ./scripts/agents-k3d-load $(AGENT_CATALOG_IMAGE) $(AGENT_BROKER_IMAGE) $(PI_IMAGE) $(OPENCODE_IMAGE) $(DSH_IMAGE)

agents-test:
	python3 -m unittest discover -s agent-catalog -p 'test_*.py'
	cd agent-broker && go vet ./... && go test ./...
	cd agent-adapter && node --test
	python3 -m unittest discover -s config/openwebui -p 'test_*.py'

# Installs or updates the Open WebUI agent Pipe (config/openwebui/
# agent_pipe.py, docs/adr/0021) through the admin API, with one model per
# runtime in AGENT_PIPE_RUNTIMES. Needs OPENWEBUI_API_KEY (an admin's key).
openwebui-agent-pipe:
	STACK_BASE_URL=$(STACK_BASE_URL) AGENT_PIPE_RUNTIMES=$(AGENT_PIPE_RUNTIMES) ./scripts/openwebui-agent-pipe

# Applies k8s/agents, then the values it cannot hold itself because they
# come from versions.lock.env and .env (WEB_SEARCH_ENABLED, docs/adr/0017;
# REPOWISE_ENABLED, docs/adr/0018; DSH_PUBLIC_ORIGIN, docs/adr/0020).
# Restarts the broker so it picks up a new catalog; running agents move to it
# at their next idle point.
agents-up:
	@[ -n "$(STACK_ORIGIN)" ] || { echo "agents-up: STACK_BASE_URL must be set in .env" >&2; exit 1; }
	$(KUBECTL) kustomize $(AGENTS_OVERLAY) | sed 's#$(PLACEHOLDER_ORIGIN)#$(STACK_ORIGIN)#g' | $(KUBECTL) apply -f -
	$(KUBECTL) -n agents create configmap agent-images \
		--from-literal=HERMES_IMAGE=$(HERMES_IMAGE) \
		--from-literal=AGENT_CATALOG_IMAGE=$(AGENT_CATALOG_IMAGE) \
		--from-literal=PI_IMAGE=$(PI_IMAGE) \
		--from-literal=OPENCODE_IMAGE=$(OPENCODE_IMAGE) \
		--from-literal=DSH_IMAGE=$(DSH_IMAGE) \
		--from-literal=DSH_PUBLIC_ORIGIN=$(DSH_PUBLIC_ORIGIN) \
		--from-literal=WEB_SEARCH_ENABLED=$(or $(WEB_SEARCH_ENABLED),false) \
		--from-literal=REPOWISE_ENABLED=$(or $(REPOWISE_ENABLED),false) \
		--from-literal=AGENT_PIPE_RUNTIMES=$(AGENT_PIPE_RUNTIMES) \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n agents set image deployment/agent-broker catalog-index=$(AGENT_CATALOG_IMAGE) broker=$(AGENT_BROKER_IMAGE)
	$(KUBECTL) -n agents rollout restart deployment/agent-broker
	$(KUBECTL) -n agents rollout status deployment/agent-broker --timeout=120s

# Stops the broker and every running agent (Hermes, pi, opencode). Profiles
# (PVCs) and credentials stay; agents-up brings everything back.
agents-down:
	-$(KUBECTL) -n agents scale deployment/agent-broker --replicas=0
	-$(KUBECTL) -n agents delete pod -l app.kubernetes.io/managed-by=agent-broker --wait=true

agents-smoke:
	./scripts/agents-smoke-test

