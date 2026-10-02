# Operations

[Русский](operations.ru.md) | [Handbook index](README.md)

The day-2 index: how to reach the cluster, deploy, test, manage users, and
recognise the failures this stack has already had. Details stay in the
runbooks this page links to.

**Contents**

- [Reaching the cluster](#reaching-the-cluster)
- [Deploying changes](#deploying-changes)
- [Smoke tests](#smoke-tests)
- [Users and access](#users-and-access)
- [Known failure modes](#known-failure-modes)
- [Known gaps](#known-gaps)
- [Runbooks](#runbooks)
- [Related](#related)

## Reaching the cluster

The remote profile is a two-node k3d cluster on a Windows + WSL2 host:
`server-0` for the control plane and applications, `agent-0` for the GPU
([ADR 0019](../adr/0019-inference-plane-gpu-worker-nodes.md)).
From a workstation, `kubectl` goes through an SSH tunnel:

```sh
make k3s-tunnel            # foreground; host and ports from WSL_SSH_HOST / WSL_SSH_PORT in .env
kubectl --context wsl-llm-stack get nodes
```

The tunnel drops every 20 to 40 minutes: restart it, and run long in-pod
jobs detached (`nohup ... &` inside the container) rather than as a
foreground `kubectl exec`. Smoke tests against the edge need a second tunnel
to the edge port and `STACK_TUNNEL_PORT`, or `STACK_BASE_URL` pointing at the
public origin. Host details, the edge tunnel and the post-reboot recovery
(the k3d server container often has to be started by hand) are in
[operations/README.md](../operations/README.md).

## Deploying changes

| Change | Command |
| --- | --- |
| Anything in manifests or `helm/airgap-stack` | `make verify`, then `make helm-up` |
| Engine replicas | `.env` `VLLM_REPLICAS` / `SGLANG_REPLICAS` / `NINFER_REPLICAS`, then `make engines-up` ([engines](engines/README.md#engine-replicas)) |
| Engine flags | `make vllm-up` / `sglang-up` / `ninfer-up` |
| EPP | `make llmd-up` |
| Dashboards, Grafana | `make monitoring-up` |
| Agents | `make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents` ([managing agents](agents/managing.md)) |
| MCP servers | `make websearch-up`, `make repowise-up` |
| New pat-service code | push; CI builds `PAT_SERVICE_IMAGE`; `kubectl -n airgap-ai-stack rollout restart deploy/pat-service` |
| Whole remote stack | `make stack-up` |

`make helm-diff` shows what `helm-up` would change. `make down` scales the
application Deployments to zero and keeps the data. Commit messages and PRs
follow [AGENTS.md](../../AGENTS.md).

## Smoke tests

| Target | Proves | Needs |
| --- | --- | --- |
| `make verify` | render and validation, docs links | nothing |
| `make smoke`, `make services-smoke` | SSO, redirects, operator-managed stores, edge refusals | cluster |
| `make pat-smoke` | issue a PAT, use it, hit the rate limit, revoke | a model |
| `make llmd-nvfp4-smoke` | EPP metrics plus the PAT playbook against `qwen-3.8-27b` | a model |
| `make smoke-nogpu` | the above without calls to the model | cluster |
| `make embeddings-smoke` | `/v1/embeddings` | embeddings |
| `make websearch-smoke` | web-search boundaries | `WEBSEARCH_SMOKE_PAT` |
| `make agents-smoke` | the agent fleet end to end | Keycloak, a model for `AGENTS_SMOKE_INFERENCE=true` |
| `tests/telemetry/genai-smoke.sh` | traces reach Langfuse with content and user | a model |

The scripts derive endpoints from `scripts/lib-endpoints.sh`: set
`STACK_BASE_URL` for the remote origin. Scripts that sign in (`kc-pat-issue`)
need a Python with TLS 1.3, not macOS's `/usr/bin/python3`. Test layout:
[tests/](../../tests/auth/README.md) (`auth`, `inference`, `telemetry`,
`qos`, `airgap`).

## Users and access

Users live in Keycloak, realm `ai-stack`. Roles: `ai-user` (chat, PATs,
agents), `ai-admin` (also Open WebUI admin and Grafana Admin),
`repowise-admin` (repowise settings). Password sign-in in Open WebUI is off,
so everything goes through Keycloak; role changes apply at the next login.

```sh
scripts/kc-user-add <username> [role] [email] [first] [last]   # generated password, ai-user by default
scripts/kc-user-list [filter]
scripts/kc-user-delete <username>
scripts/kc-pat-issue <username> <password> [name] [days]       # a PAT without a browser
```

The admin console is not published (`/sso/admin` is 404 at the edge); use
`kubectl -n airgap-ai-stack port-forward svc/keycloak 8888:8080` for the REST
API, which is what the scripts do. Offboarding a user touches several stores:
[deleting a user's data](observability/data-and-retention.md#deleting-a-users-data).
Credential rotation for every component: [docs/security](../security/README.md).

## Known failure modes

| Symptom | Cause | Fix |
| --- | --- | --- |
| One Open WebUI user gets 401 from the model | Open WebUI lost that user's stored OAuth session (`No OAuth session found`, gateway `Jwt_is_missing`) | the user logs out and in; `offline_access` (applied by `helm-up`) makes it rare |
| Every PAT fails at once with 401 | pat-service's cached gateway token expired (`Jwt_is_expired` in the gateway Envoy log) | `kubectl -n airgap-ai-stack rollout restart deploy/pat-service`; check the `pat-gateway` client's token lifespan in Keycloak |
| Requests hang, `llm_d_epp_ready_endpoints` 0, stale-endpoint alert | no pool member is Ready, or EPP cannot read its metrics (missing `llm-d.ai/model` or `llm-d.ai/engine-type` label) | check the vLLM/SGLang replicas and pod labels; if everything matches, restart EPP (`make llmd-up` or delete the pod) |
| EPP KV and queue gauges stuck at 0 under traffic | stale EPP state after an engine switch (`llm_d_epp_datalayer_extract_errors_total` climbing) | restart EPP; set `router.epp.flags.v: 4` to capture `extract failed` if it recurs |
| False 504 on long requests | a missing edge or route timeout | timeouts are 10 min on the edge and route ([inference](inference/README.md#timeouts-and-limits-on-the-path)) |
| JSON-mode requests fail with 400 | the request went to `qwen-3.8-27b-ninfer`; ninfer refuses `response_format` | expected; use `qwen-3.8-27b` |
| Engine pod `Pending`, `Insufficient nvidia.com/gpu` | another engine holds the GPU | keep the replica total at the GPU count; `make engines-up` stops the zeroed engines first |
| LLM engine crash-loops at start after an embeddings restart | GPU embeddings took memory first | start order: engine, then embeddings (`engines-up` does it) |
| `kubectl` "connection refused" on the tunnel port | the SSH tunnel dropped | `make k3s-tunnel` again |

## Known gaps

Things that are knowingly incomplete. Fix them in the stack when you touch the
area, and update this list.

- No retention anywhere; Langfuse grows without bound ([data and retention](observability/data-and-retention.md)).
- The llm-d chart is installed by digest, but the EPP image it runs is the
  mutable `main` tag (`IfNotPresent`; the digest in `versions.lock.env` is
  recorded, not enforced), so a fresh node could pull a different build.
- With vLLM replicas and GPU embeddings, vLLM's
  `gpuMemoryUtilization: 0.94` leaves less GPU memory than the measured
  embeddings peak (ADR 0018 measured it under ninfer); lower it before heavy
  embedding load under vLLM.
- No per-server or per-tool MCP policy ([MCP](mcp/README.md#policies-what-exists-and-what-does-not)).
- No agent-broker metrics; the broker does not renew agent keys.
- `k8s/agents` is applied by `make agents-up`, not by the `airgap-stack`
  release.
- The vendored llm-d SGLang overview dashboard queries `sglang_*` names that
  SGLang 0.5.19 does not export.
- Some older docs (ADR texts, `CONTEXT.md` session notes) describe earlier
  states (`concurrency-detector`, `hermes-*` names). ADRs are history and
  are not edited; this handbook describes the current state.

## Runbooks

| Area | Runbook |
| --- | --- |
| Host access, tunnels, Keycloak admin, Langfuse traces, reboot recovery | [operations/README.md](../operations/README.md) |
| Engines and model routing | [operations/inference-backends.md](../operations/inference-backends.md) |
| vLLM settings and measurements | [operations/vllm-inference.md](../operations/vllm-inference.md) |
| Agent fleet, gVisor node setup | [operations/agents.md](../operations/agents.md) |
| Web search | [operations/web-search.md](../operations/web-search.md) |
| Repowise | [mcp/repowise.md](mcp/repowise.md) |
| New site install | [docs/install](../install/README.md) |
| Air-gap bundle | [docs/airgap](../airgap/README.md) |
| Security and credentials | [docs/security](../security/README.md) |

## Related

- Previous: [Adding an MCP server](mcp/adding-a-server.md). Next: [Contributing](contributing.md)
