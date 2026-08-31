# llm-stack — self-hosted, SSO-gated LLM platform

A Kubernetes stack that puts one identity boundary in front of local model
inference: Keycloak for sign-in, Envoy Gateway and Envoy AI Gateway for
routing and policy, Open WebUI for humans, personal access tokens for
programmatic clients, and Langfuse plus Grafana for what happened. Model
serving is vLLM behind the llm-d endpoint picker, with SGLang, llama.cpp and
external OpenAI-compatible endpoints selectable by model name. Every stateful
component is run by an operator (CloudNativePG, Altinity ClickHouse, Redis, MinIO).

Nothing in the request path leaves the cluster: the model, the token store and
the trace store are all local. The stack is built to be air-gappable, and to
be stood up more than once — see [docs/install](docs/install/README.md).

## Deployment profiles

| | `remote-wsl-vllm-nvfp4` | `local-mac` |
| --- | --- | --- |
| Purpose | the deployable product | development without a GPU |
| Inference | vLLM on an RTX 5090 via llm-d, optionally SGLang | LM Studio on the LAN |
| Cluster | k3d on Windows + WSL2 | OrbStack on macOS |
| Public routing | one origin, split by path | `*.localhost` hosts on `:8080` |
| Routing owned by | `helm/airgap-stack/templates` | `k8s/overlays/local-mac` |
| Deploy command | `make stack-up` | `make up` |

Application workloads live in `k8s/base` and are shared; each profile owns its
own routing and its own inference path, and neither can be applied on top of
the other. The model servers are separate Helm releases of their own
(`helm/vllm-inference`, `helm/sglang-inference`), so tuning a launch flag
never restarts the application stack — see
[ADR 0007](docs/adr/0007-inference-engines-as-helm-releases.md).
`make stack-up` is the only supported way to deploy the remote profile — see
[k8s/README.md](k8s/README.md) for why `kubectl apply -k` on the remote overlay
is not a deploy path.

## Deploy the remote profile

Prerequisites: a k3d cluster created by
`deploy/vllm-qwen38-nvfp4/k3d-create-nvidia`, the NVIDIA device plugin
(`make device-plugin-load device-plugin-up`), the vLLM image built and
imported (`deploy/vllm-qwen38-nvfp4/build`, `k3d-load-image`), and the
pat-service image imported (`make nvfp4-pat-load`). The full ordered runbook,
including the values that must be changed for a new site, is in
[docs/install](docs/install/README.md).

```sh
cp .env.example .env         # then replace every value; see docs/security
make verify                  # renders and validates everything, no cluster needed
make stack-up                # prerequisites, GPU objects, vLLM, Helm release, llm-d, OIDC
make llmd-nvfp4-smoke        # end-to-end: PAT issue, inference, rate limit, revoke
```

vLLM's launch settings are values in `helm/vllm-inference/values.yaml`;
`make vllm-up` applies a change on its own, without touching the rest of the
stack.

While the GPU is occupied by something else, `make smoke-nogpu` proves
everything except the calls that reach the model.

## Deploy the local development profile

```sh
cp .env.example .env
make preflight
make up
make services-smoke
make pat-smoke               # needs a model loaded in LM Studio
```

## Public surface

The remote profile publishes one HTTPS origin through the site's
TLS-terminating reverse proxy, and splits it by path:

| Path | Service | Authentication |
| --- | --- | --- |
| `/` | Open WebUI | Keycloak SSO, roles `ai-user` / `ai-admin` |
| `/sso` | Keycloak | the sign-in surface itself |
| `/platform` | PAT self-service dashboard | Keycloak SSO only |
| `/v1` | OpenAI-compatible API | personal access token only |

`/sso/admin` and `/sso/realms/master` are refused at the edge with a 404;
Keycloak administration goes through `kubectl port-forward svc/keycloak
8888:8080` only.

Grafana and Langfuse have **no public route**. They are reached through the
operator NodePorts described in
[docs/operations](docs/operations/README.md).

Open WebUI and the PAT service both reach inference through the
cluster-internal AI Gateway, which has no public listener. Open WebUI sends
the signed-in user's Keycloak token; the PAT service exchanges a validated
`sk-…` token for its own service-account JWT. Envoy validates the token,
derives the identity, and applies the per-user rate limit. A PAT is stored
only as an HMAC hash and can be revoked by its owner, taking effect
immediately.

Roles are decided in Keycloak: `ai-user` maps to the regular Open WebUI role,
only `ai-admin` maps to admin, and any other account is rejected. Password
sign-in is disabled, so a role change takes effect at the user's next login.

## Credentials

`.env.example` and the checked-in manifests carry `*-demo-only` placeholders
so a disposable local stack comes up without editing. **They are not safe on a
public network.** Every one of them, including the values that still live in
manifests rather than in `.env`, is listed with its rotation procedure in
[docs/security](docs/security/README.md). Read that before exposing anything.

## Layout

| Path | Contents |
| --- | --- |
| `k8s/base` | profile-neutral application workloads, no routing |
| `k8s/overlays/local-mac` | development Gateway, `*.localhost` routes, LM Studio |
| `k8s/overlays/remote-wsl-vllm-nvfp4` | GPU prerequisites and site patches for the remote profile |
| `k8s/overlays/remote-wsl-device-plugin` | one-time NVIDIA device plugin bootstrap |
| `helm/airgap-stack` | the release: routing templates plus rendered workloads |
| `helm/vllm-inference` | the deployed vLLM server, values-driven (`make vllm-up`) |
| `helm/sglang-inference` | the optional SGLang server, same shape (`make sglang-up`) |
| `config/<service>` | per-service configuration and Helm values |
| `deploy/vllm-qwen38-nvfp4` | host and k3d bootstrap for the GPU node |
| `deploy/sglang-qwen38`, `deploy/llamacpp` | host-Docker bring-up for the optional backends — see [docs/operations/inference-backends.md](docs/operations/inference-backends.md) |
| `scripts` | render, validation and smoke helpers |
| `docs` | install, operations, architecture, security, ADRs |
| `models`, `versions.lock.env` | model inventory and pinned image/chart versions |
