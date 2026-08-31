# Kubernetes sources

## The split, and why it exists

Routing is the only part of this stack that differs between deployment sites,
and it used to be defined twice: once in the kustomize sources and once in the
Helm chart templates. The two copies drifted — different listener ports,
different route matching, and a Keycloak issuer in the kustomize copy that no
longer matched the deployed realm — so which one you got depended on which
command you happened to run. That is fixed by giving every object exactly one
owner:

- **`base/`** — application workloads only. Keycloak, Open WebUI, the PAT
  service, Langfuse, Grafana, Prometheus, the OTEL Collector, the database and
  object-storage operator CRs, the rate-limit Valkey, the `GatewayClass`.
  No Gateway, no HTTPRoute, no AIGatewayRoute, no policy. Every profile
  deploys all of it.
- **`overlays/local-mac/`** — the development profile owns its whole routing
  surface: the `edge` Gateway on `:8080`, the host-based `*.localhost`
  HTTPRoutes, and the LM Studio inference path with its Backend,
  AIGatewayRoute, JWT SecurityPolicy and per-user BackendTrafficPolicy.
- **`overlays/remote-wsl-vllm-nvfp4/`** — the remote profile's GPU
  prerequisites (the `nvidia` RuntimeClass, the model PV/PVC) and the patches
  that point Open WebUI, Langfuse and Prometheus at this site. Its routing is
  owned by `helm/airgap-stack/templates`, rendered from values; the model
  servers are their own Helm releases (`helm/vllm-inference`,
  `helm/sglang-inference`, [ADR 0007](../docs/adr/0007-inference-engines-as-helm-releases.md)).
  `vllm.yaml` here is a leftover copy of the vLLM Deployment: `helm-render`
  filters it out (`GPU_KEEP_OUT`), nothing applies it, and it is not what
  runs.
- **`overlays/remote-wsl-device-plugin/`** — the NVIDIA device plugin
  DaemonSet, applied once per cluster bootstrap, separately from the app
  overlay.

`scripts/helm-render` enforces the split: if a routing object reappears in the
remote overlay it aborts instead of silently dropping it, so a second copy
cannot start drifting again.

## The remote overlay is not a deploy path

`kubectl apply -k k8s/overlays/remote-wsl-vllm-nvfp4` deploys workloads with
no Gateway and no routes. It exists for `kubectl kustomize` and
`--dry-run=client` validation (`make vllm-nvfp4-config`, `make preflight`) and
as the input `scripts/helm-render` reads. The deploy path is `make stack-up`,
which applies the GPU prerequisites directly, the model server through
`helm/vllm-inference`, and everything else through the `airgap-stack`
release.

## Realm ConfigMap

The top-level `kustomization.yaml` generates the `keycloak-realm` ConfigMap
from `realm-demo.json`. `scripts/helm-render` folds that one object into the
chart, because the overlay render does not include it.

The realm JSON still contains site-specific redirect URIs and web origins.
Templating it from values is open work — see
[docs/adr/0002-one-owner-per-object.md](../docs/adr/0002-one-owner-per-object.md).

## Inspecting a deployed cluster

```sh
kubectl -n airgap-ai-stack get gateway,httproute,backend,aiservicebackend,aigatewayroute
kubectl -n airgap-ai-stack get securitypolicy,backendtrafficpolicy
```
