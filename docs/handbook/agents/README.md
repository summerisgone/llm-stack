# Agents

[Русский](README.ru.md) | [Handbook index](../README.md)

Every user can have personal coding agents (Hermes, pi, opencode, dsh) that run in
their own sandboxed pod, keep a persistent profile, and use the model and MCP
tools with the user's own credentials. Users talk to them as models in Open
WebUI. `agent-broker` starts them on demand, keeps at most K running, and stops
idle ones. Operating and extending the fleet is on [managing agents](managing.md).

**Contents**

- [How it looks to a user](#how-it-looks-to-a-user)
- [Architecture](#architecture)
- [What runs in Kubernetes](#what-runs-in-kubernetes)
- [agent-broker](#agent-broker)
- [Runtimes](#runtimes)
- [dsh web UI](#dsh-web-ui)
- [Profiles and the catalog](#profiles-and-the-catalog)
- [Tokens](#tokens)
- [Isolation and gVisor](#isolation-and-gvisor)
- [Where it lives](#where-it-lives)
- [Related](#related)

## How it looks to a user

1. On `/platform`, press **Issue agent key** (once a week; the key lives 7
   days).
2. In Open WebUI pick `Hermes agent (personal)`, `Pi agent (personal)`,
   `OpenCode agent (personal)` or `DeepSeek agent (personal)`.
3. The first message creates the profile and is answered by the broker with
   an onboarding text. The next message starts the agent pod; replies stream
   back like any model.
4. `/skills` in the chat lists the skill catalog; `/skills on|off <name>`
   and `/skills reset` change the selection, applied at the agent's next
   start.
5. For pi, opencode and dsh each Open WebUI chat is its own agent session,
   kept on the profile across restarts; Hermes manages its sessions itself.
6. The dsh agent also has its own browser UI on the dsh host
   ([dsh web UI](#dsh-web-ui)): same login, same profile.

## Architecture

```
Open WebUI (connection 2, user's Keycloak token, X-OpenWebUI-Chat-Id)
  -> agent-broker (agents ns): verify token and role, pick runtime by model name,
     acquire a slot (start pod / reuse / evict LRU idle / queue), proxy the chat
  -> pod <runtime>-agent-<id> :8642 OpenAI chat-completions, bearer API_SERVER_KEY
       init catalog-sync: render the profile for this runtime
       agent: Hermes natively, or agent-adapter + pi / opencode / dsh
  -> pat-service /v1 (model) and /mcp/<name>/ (tools), with the user's agent PAT
```

`<id>` is the first 16 hex characters of `sha256(keycloak sub)`. One pod and
one profile per user and runtime.

## What runs in Kubernetes

| Object | Name | Created by |
| --- | --- | --- |
| Namespace | `agents` | `make agents-up` (`k8s/agents`) |
| Broker Deployment, Service, ServiceAccount, Role, ConfigMap | `agent-broker` | `make agents-up` |
| ConfigMap | `agent-images` (image refs, MCP flags) | `make agents-up` from `versions.lock.env` and `.env` |
| ResourceQuota | `agents` (pods 4 = K + broker, CPU and memory totals) | `k8s/agents/quota.yaml` |
| NetworkPolicies | default deny; broker from Open WebUI, to Keycloak and the API server; agents from the broker only, to pat-service only | `k8s/agents/networkpolicy.yaml` |
| RuntimeClass | `gvisor` (remote profile) | `k8s/overlays/remote-wsl-agents` |
| Pod | `<runtime>-agent-<id>` | broker, on demand |
| PVC | `<runtime>-profile-<id>` (2 Gi) | broker, first message |
| Secret | `agent-cred-<id>`: `API_SERVER_KEY`, `INFERENCE_KEY` | broker (API key), pat-service (inference key) |
| Role in `agents` for pat-service | `pat-service-agent-key` | `k8s/agents/rbac.yaml` |

Everything the broker creates carries `app.kubernetes.io/managed-by:
agent-broker` and `agents.llm-stack/user-id: <id>`; the PVC annotation
`agents.llm-stack/username` names the user.

```sh
kubectl -n agents get pods,pvc -l app.kubernetes.io/managed-by=agent-broker
```

## agent-broker

A small Go service (`agent-broker/cmd/agent-broker`) with no database: state
is rebuilt from the cluster at start.

- **API:** `GET /v1/models` (the runtimes whose image is set) and
  `POST /v1/chat/completions` (model `hermes-agent`, `pi-agent`,
  `opencode-agent` or `dsh-agent`), both requiring a Keycloak token with `ai-user` or
  `ai-admin`; `GET /healthz` returns `{"k", "running", "busy", "queued"}`.
- **Slots:** at most `AGENT_SLOTS` (3) agents run at once, shared by all users
  and runtimes. A new start takes a free slot, or evicts the least recently
  used idle agent (idle longer than `IDLE_TIMEOUT`, 15 min), or waits in a
  FIFO queue for up to `SLOT_WAIT_TIMEOUT` (60 s) with a "slots are busy"
  notice.
- **Restart-safe:** a restarted broker adopts running agents and counts them
  as active for one idle timeout. Pods deleted outside the broker are noticed
  within 30 s.
- **Chats:** the broker forwards the chat and `X-OpenWebUI-Chat-Id`; `/skills`
  commands and onboarding are answered by the broker itself.
- **Backend seam:** the `Backend` interface (`backend.go`) is where a move to
  Agent Substrate or kagent would plug in; today it is the `pods` backend
  ([ADR 0014](../../adr/0014-hermes-curated-catalog-and-worker-slots.md) section 10).

## Runtimes

| Runtime | Model id | Image | Inside the pod |
| --- | --- | --- | --- |
| Hermes | `hermes-agent` | `HERMES_IMAGE` (upstream) | `hermes gateway run`, OpenAI API server on `:8642` |
| pi | `pi-agent` | `PI_IMAGE` (`agent-adapter`, target `pi`) | agent-adapter on `:8642`, pi SDK in-process, `pi-mcp-adapter` extension for MCP |
| opencode | `opencode-agent` | `OPENCODE_IMAGE` (`agent-adapter`, target `opencode`) | agent-adapter on `:8642`, `opencode serve` on loopback driven over its HTTP API |
| dsh (DeepSeek Harness) | `dsh-agent` | `DSH_IMAGE` (`agent-adapter`, target `dsh`) | agent-adapter on `:8642` driving `dsh --profile acp` over stdio; `dsh web` on `:3080` for the [web UI](#dsh-web-ui) |

pi, opencode and dsh have no OpenAI-compatible server, so `agent-adapter`
(Node, `agent-adapter/`) provides the broker's contract: OpenAI chat
completions on `:8642` with SSE streaming (including reasoning), `/health`,
bearer `API_SERVER_KEY`. It sends only the latest user message to the agent;
the agent keeps the history in its own session (per chat id, stored on the
PVC).

All runtimes call the same model through pat-service, so they share the
per-user rate limit and appear in Langfuse and the QoS dashboards as that
user. No runtime has egress anywhere else: update checks, telemetry, model
catalogs, plugin installs and built-in web tools are switched off.

## dsh web UI

The dsh agent pod also runs `dsh web`, the DeepSeek Harness browser UI, for
that user only ([ADR 0020](../../adr/0020-dsh-runtime-and-per-user-web-ui.md)).
It is reached on its own host, `DSH_PUBLIC_ORIGIN` (dsh serves only from `/`,
so it cannot live under a path of the main origin).

```
browser -> site proxy (TLS) -> edge Envoy: HTTPRoute dsh-web (host match)
  SecurityPolicy dsh-web: OIDC with Keycloak client dsh-web, access token forwarded
  -> agent-broker :8081 web proxy: verify token and role, start the user's
     dsh-agent pod through the slots if needed, proxy HTTP + WebSocket
  -> pod dsh-agent-<id> :3080 dsh web (same DSH_HOME and workspace as the chats)
```

- **Login.** Keycloak SSO only. dsh's own launch-token cookie is obtained by
  the broker behind the scenes (it fetches the token from agent-adapter and
  redeems it); the user never sees a token.
- **Isolation.** The broker picks the pod from the token's subject, never
  from a cookie; each user sees only their own sessions and files.
- **Workspace.** `/opt/data/home/workspace` on the profile PVC: files survive
  idle eviction. Model and MCP settings are locked by the catalog.
- **Slots.** Page and API requests count as activity; an open tab alone does
  not keep the agent. After eviction the next click starts it again (cold
  start of up to a few minutes).
- **Setup.** `DSH_PUBLIC_ORIGIN` and `DSH_OIDC_CLIENT_SECRET` in `.env`, a
  site-proxy rule for that host to the edge listener (like repowise), then
  `make agents-up helm-up` (`helm-up` runs `provision-dsh-oidc`).

## Profiles and the catalog

Each profile PVC holds the agent's home. At every start the init container
`catalog-sync` (image `AGENT_CATALOG_IMAGE`, `agent-catalog/agent_sync.py`)
renders it from the catalog baked into that image
(`config/agents/base-profile/`):

| Source | Becomes |
| --- | --- |
| `skills/` + `catalog.yaml` (required, default on or off) + the user's `/skills` selection | `catalog/` and `catalog-enabled/` skill directories |
| `SOUL.md` + the user's `SOUL.user.md` | the agent's instructions (`SOUL.md`, `AGENTS.md` for pi, opencode and dsh) |
| `config.yaml` + `locked-keys.yaml` + the user's `config.user.yaml` | Hermes `config.yaml`; locked keys always win |
| `runtimes/pi/*`, `runtimes/opencode/opencode.json`, `runtimes/dsh/*` | pi `settings.json`, `models.json`; opencode `opencode.json`; dsh `dsh/cordis.patch.yml` + `acp.patch.yml` + `web.patch.yml` (provider = pat-service) |
| `mcp-servers.yaml` + the `*_ENABLED` flags | each runtime's MCP config ([MCP](../mcp/README.md#agents)) |

The catalog layer is owned by another uid and is read-only to the agent; the
user's own files and sessions are writable. A sync report is stored on the PVC
annotation `agents.llm-stack/sync-report`.

## Tokens

- **Inference key.** `/platform` -> "Issue agent key" calls pat-service
  `POST /api/agent-token`: it revokes the user's previous agent PAT, mints a
  new one (`issued_by = agents`, TTL `AGENT_PAT_TTL_DAYS`, 7 days), writes it
  as `INFERENCE_KEY` into `agents/agent-cred-<id>` and deletes the user's
  running agent pods so the next message starts them with the new key. Every
  runtime reads it as `AGENT_INFERENCE_KEY` (Hermes also as
  `HERMES_INFERENCE_KEY`) and sends it to pat-service `/v1` and `/mcp/`.
  The broker does not renew it; when it expires, the user issues a new one.
- **Broker to agent.** `API_SERVER_KEY` in the same Secret, generated by the
  broker; the agent accepts only requests bearing it.
- **User to broker.** The user's Keycloak token from Open WebUI, verified
  against `OIDC_ISSUER` (the browser-facing issuer).

## Isolation and gVisor

- Pods run as non-root (uid 10000), read-only root filesystem, no
  ServiceAccount token, writable only the profile PVC, `/work` and `/tmp`.
- The key is injected from the Secret, never in the pod spec.
- NetworkPolicy: agents accept traffic only from the broker and reach only
  DNS and pat-service; no agent-to-agent, no Keycloak, no internet.
- On the remote profile agents run under **gVisor** (`runtimeClassName:
  gvisor`, `AGENT_RUNTIME_CLASS` in the `remote-wsl-agents` overlay): each
  pod gets a user-space kernel (`uname` reports `*-gvisor`), so a compromised
  agent attacks gVisor, not the host kernel. NetworkPolicy still applies. The
  node setup (runsc and the containerd shim in the k3d node) is manual,
  described in [operations/agents.md](../../operations/agents.md#gvisor-on-k3dwsl2).
- "On demand under gVisor" means: a pod exists only while the user's agent is
  in use (or idle within the timeout), and every such pod is a gVisor sandbox.

## Where it lives

- Broker: `agent-broker/`; adapter: `agent-adapter/`; catalog and sync:
  `agent-catalog/`, `config/agents/base-profile/`
- Manifests: `k8s/agents/`, overlay `k8s/overlays/remote-wsl-agents/`
- Open WebUI connection: `k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml`
- Token issuance: `pat-service/cmd/pat-service/agents.go`
- Runbook: [operations/agents.md](../../operations/agents.md)

## Related

- Previous: [Tuning](../configuration/tuning.md). Next: [Managing agents](managing.md)
- [ADR 0009](../../adr/0009-cloud-hermes-fleet-per-user-profiles.md),
  [ADR 0014](../../adr/0014-hermes-curated-catalog-and-worker-slots.md),
  [ADR 0020](../../adr/0020-dsh-runtime-and-per-user-web-ui.md)
