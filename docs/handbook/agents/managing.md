# Managing agents

[Русский](managing.ru.md) | [Handbook index](../README.md)

Day-2 work on the agent fleet: build and deploy, look at who is running,
change the catalog, handle keys, offboard a user, and add a new agent runtime.
Read [agents](README.md) first for the moving parts.

**Contents**

- [Build and deploy](#build-and-deploy)
- [Looking at the fleet](#looking-at-the-fleet)
- [Keys](#keys)
- [Changing the catalog](#changing-the-catalog)
- [Capacity](#capacity)
- [Offboarding](#offboarding)
- [Troubleshooting](#troubleshooting)
- [Adding a runtime](#adding-a-runtime)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Build and deploy

```sh
make agents-test                                      # agent-sync, broker and adapter unit tests
make agent-catalog AGENTS_PLATFORM=linux/amd64        # catalog image, pins AGENT_CATALOG_IMAGE
make agent-broker-image AGENTS_PLATFORM=linux/amd64   # llm-stack/agent-broker:local
make agent-adapter-images AGENTS_PLATFORM=linux/amd64 # pi and opencode images, pins PI_IMAGE / OPENCODE_IMAGE
make agents-k3d-load                                  # copy the images into the k3d node (over SSH)
make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
make agents-smoke                                     # end to end, see below
```

- `make agents-up` applies the overlay (with the public origin substituted),
  recreates the `agent-images` ConfigMap and restarts the broker. Running
  agents keep running; they pick up new images and catalog at their next
  start.
- `make agents-down` scales the broker to 0 and deletes all agent pods;
  profiles stay.
- `scripts/agents-smoke-test` creates two temporary users and checks:
  broker auth, onboarding, `/skills`, on-demand start, the answer from the
  model (`AGENTS_SMOKE_INFERENCE=true`), pi and opencode, sandbox properties
  (non-root, no token, read-only root, gVisor kernel), isolation between
  users, and broker restart. It needs `STACK_BASE_URL` and a Python with TLS
  1.3.

## Looking at the fleet

```sh
kubectl -n agents get pods,pvc -l app.kubernetes.io/managed-by=agent-broker -L agents.llm-stack/user-id
kubectl -n agents port-forward svc/agent-broker 18090:8080 &
curl -s localhost:18090/healthz                       # {"k":3,"running":..,"busy":..,"queued":..}
kubectl -n agents logs deploy/agent-broker | grep '"agent started"'   # includes cold-start seconds
kubectl -n agents get pvc -o custom-columns=PVC:.metadata.name,USER:.metadata.annotations.agents\.llm-stack/username
```

What an agent did is visible in Langfuse (traces under the user's name with
the agent key's `pat_token_id`) and in `patsvc_mcp_calls_total` for tools.

## Keys

- The user issues the key on `/platform`. Nothing else is needed; the pods
  restart with it.
- A key expires after `AGENT_PAT_TTL_DAYS` (7). An expired key shows up as
  `HTTP 401` in the agent's reply; the fix is a new key on `/platform`.
- Operator fallback, for tests or a broken dashboard:
  `scripts/agent-inference-key` writes a PAT into the user's Secret.
- To cut a user's agents off immediately, revoke the agent PAT on
  `/platform` or disable the Keycloak account.

## Changing the catalog

Skills, instructions, config defaults and MCP servers all live in
`config/agents/base-profile/`:

| Change | Edit |
| --- | --- |
| Add a skill | `skills/<name>/SKILL.md`; list it in `catalog.yaml` if it is required or off by default |
| Agent instructions | `SOUL.md` |
| Hermes settings, and which the user may override | `config.yaml`, `locked-keys.yaml` |
| pi or opencode settings | `runtimes/pi/*.json`, `runtimes/opencode/opencode.json` |
| MCP servers | `mcp-servers.yaml` ([MCP](../mcp/adding-a-server.md)) |

Then:

```sh
make agents-test
make agent-catalog AGENTS_PLATFORM=linux/amd64        # review the new AGENT_CATALOG_IMAGE in versions.lock.env
make agents-k3d-load && make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
```

Agents get the new catalog at their next start; users are told about new
skills in their next reply. To force it now, `make agents-down && make
agents-up` (cuts in-flight turns).

## Capacity

| Setting | Default | Where |
| --- | --- | --- |
| Running agents K | 3 | `AGENT_SLOTS` in `k8s/agents/broker.yaml`; keep `k8s/agents/quota.yaml` (pods = K + 1, CPU and memory totals) in step |
| Idle timeout | 15 min | `IDLE_TIMEOUT` |
| Queue wait | 60 s | `SLOT_WAIT_TIMEOUT` |
| Per-agent resources | CPU 100m / 1, memory 384Mi / 1Gi | `AGENT_CPU_*`, `AGENT_MEMORY_*` |
| Profile size | 2 Gi | `PROFILE_SIZE` |

Agents also share the model with everyone else: each agent step is a normal
PAT request with the user's rate limit and band.

## Offboarding

```sh
scripts/agent-profile-delete <keycloak-sub|id>
```

Deletes the user's pods, Secret and every runtime's profile PVC. It is the
only path that deletes a profile; the broker's Role cannot delete PVCs. The
rest of a user's data: [data and retention](../observability/data-and-retention.md#deleting-a-users-data).

## Troubleshooting

| Symptom | Check | Fix |
| --- | --- | --- |
| Agent replies `HTTP 401` | the key is missing or expired | new key on `/platform` |
| Model returns 404 in Open WebUI | the runtime's image is unset in `agent-images` | set `PI_IMAGE` / `OPENCODE_IMAGE`, `make agents-up` |
| "All agent slots are busy" | `curl .../healthz`: `running` = K, none idle | wait, or raise K with the quota |
| Pod stuck `ContainerCreating` on the remote profile | `kubectl -n agents describe pod`: RuntimeClass `gvisor` handler missing | node setup in [operations/agents.md](../../operations/agents.md#gvisor-on-k3dwsl2) |
| No MCP tools in the agent | the flag in `.env`, `agent-images` ConfigMap, the rendered config in the PVC | [MCP troubleshooting](../mcp/README.md#troubleshooting) |
| Broker 401 for every user | `OIDC_ISSUER` differs from the token `iss` | set the browser-facing issuer in the overlay |

## Adding a runtime

A new agent runtime (another coding agent) needs:

1. **An image that speaks the broker contract:** OpenAI chat completions with
   SSE on `:8642`, `GET /health`, bearer `API_SERVER_KEY`, all state under
   `AGENT_HOME` on the PVC, uid 10000, works with a read-only root
   filesystem, no egress except pat-service. If the agent has no such server,
   add a backend to `agent-adapter/` (see `pi.mjs`, `opencode.mjs`) and a
   Dockerfile target.
2. **Broker registration:** a `Runtime` entry in
   `agent-broker/cmd/agent-broker/main.go` (name, model id, title, image env
   `<NAME>_IMAGE`), plus its component name in the NetworkPolicy selectors
   (`k8s/agents/networkpolicy.yaml`) and in `pods.go` if it needs special
   env. Add a test like `TestAdapterRuntimePod`.
3. **Profile rendering:** a `render_<name>` in `agent-catalog/agent_sync.py`
   (provider = pat-service with `AGENT_INFERENCE_KEY`, skills directory,
   instructions, MCP servers from the registry) and defaults under
   `config/agents/base-profile/runtimes/<name>/`; tests in
   `test_agent_sync.py`.
4. **Deployment wiring:** the image pin in `versions.lock.env`, the build in
   `make agent-adapter-images`, the key in the `agent-images` ConfigMap
   (`agents-up`), loading in `scripts/agents-k3d-load`.
5. **Open WebUI:** add the model id to connection 2 `model_ids` in
   `openwebui-oidc-patch.yaml`, `make helm-up`.
6. **Smoke:** extend `scripts/agents-smoke-test`, run it with inference.
7. **Docs:** the runtime table in [agents](README.md) and this list, both
   languages.

## Where it lives

- Runbook with gVisor node setup and the Agent Substrate outlook:
  [operations/agents.md](../../operations/agents.md)
- Scripts: `scripts/agents-smoke-test`, `agent-profile-delete`,
  `agent-inference-key`, `agents-k3d-load`

## Related

- Previous: [Agents](README.md). Next: [MCP](../mcp/README.md)
