# 0002 — Every Kubernetes object has exactly one owner

Status: accepted

## Context

The stack was described twice. `k8s/` held kustomize sources; `helm/airgap-stack`
held a chart whose `templates/resources.yaml` is *generated* from those
sources by `scripts/helm-render`, with routing hand-written as real templates
so it could be driven by values.

Both descriptions were applied by documented commands — `make nvfp4-up` ran
`kubectl apply -k`, `make helm-up` ran the chart — and they had drifted:
different edge listener ports, host-based versus path-based routes, and a
Keycloak issuer in the kustomize copy that omitted the `/sso` prefix the
deployed realm actually uses. `helm-render` silently dropped routing objects
from its render, which is what let the second copy rot unnoticed. Following
the README produced a stack that did not work.

## Decision

One owner per object, and one deploy path.

- `k8s/base` holds application workloads only. No Gateway, no HTTPRoute, no
  AIGatewayRoute, no policy.
- The **local-mac** overlay owns the whole development routing surface and the
  LM Studio inference path.
- The **remote** overlay owns GPU workloads and site patches. Its routing is
  owned by `helm/airgap-stack/templates`.
- `scripts/helm-render` **fails** if a routing object appears in the remote
  overlay, instead of filtering it out.
- `make stack-up` is the single deploy path for the remote profile.
  `kubectl apply -k` on that overlay is a validation input, not a deployment.
- `make render-check` fails if the committed chart no longer matches what
  `helm-render` produces from the sources.

## Consequences

- `make nvfp4-up` becomes an alias for `make stack-up`; `make nvfp4-down`
  uninstalls llm-d only, since the route no longer comes from the base stack.
- The GPU objects stay outside the Helm release on purpose, so a `helm
  upgrade` never restarts the model server. `make stack-up` applies them
  directly from the overlay sources.
- Site-specific values are still spread across kustomize patches, the chart
  values, the realm JSON and several scripts. Collecting them into one
  per-site values file is the next step and is not done here; the current
  inventory is in [docs/install](../install/README.md).
- Templating the Keycloak realm JSON from those values remains open. Today its
  redirect URIs and web origins are literals.
