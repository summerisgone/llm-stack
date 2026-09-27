# MCP servers

[Русский](README.ru.md) | [Handbook index](../README.md)

MCP (Model Context Protocol) servers give the model tools: web search and
page fetching, and questions about indexed code repositories. In this stack
every MCP server sits behind **one door**, pat-service `/mcp/<name>/`,
which authenticates the caller, attaches the identity, rate-limits tool calls
and counts them. Open WebUI and every agent runtime reach the servers only
through that door. Adding one: [adding a server](adding-a-server.md);
repowise operations: [repowise](repowise.md).

**Contents**

- [Installed servers](#installed-servers)
- [The door: pat-service /mcp](#the-door-pat-service-mcp)
- [Open WebUI](#open-webui)
- [Agents](#agents)
- [Switching a server on and off](#switching-a-server-on-and-off)
- [Policies: what exists and what does not](#policies-what-exists-and-what-does-not)
- [Troubleshooting](#troubleshooting)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Installed servers

| Name | Tools | Upstream | Flag | Leaves the company |
| --- | --- | --- | --- | --- |
| `web-search` | `web_search`, `fetch_url` | `web-search-mcp` (Go, this repo) -> OpenSERP with an engine-pinning nginx sidecar | `WEB_SEARCH_ENABLED` | yes: queries go to public search engines, fetches to public sites |
| `repowise` | `get_overview`, `get_context`, `search_codebase`, `get_symbol`, `get_answer`, `get_risk`, `get_change_risk`, `get_why`, `get_dead_code`, `get_health`, `list_repos` | `repowise mcp` (upstream v0.53.0) over the indexed repositories | `REPOWISE_ENABLED` | fetches the listed public repositories |

Both flags are `false` in `.env.example` and must stay false on an air-gapped
install. Decisions: [ADR 0017](../../adr/0017-web-search-mcp-openserp-kagent.md)
(web search via MCP; [ADR 0016](../../adr/0016-self-hosted-web-search-openserp.md)
for the OpenSERP workload), [ADR 0018](../../adr/0018-repowise-codebase-intelligence.md)
(repowise).

## The door: pat-service /mcp

```
agent (PAT)            --\
                          > pat-service /mcp/<name>/  -- X-User-Id, X-User-Name, X-Pat-Token-Id -->  <name> MCP server
Open WebUI (user JWT)  --/     auth, rate limit, metric                                           (NetworkPolicy: only pat-service may connect)
```

| Step | Behaviour | Code |
| --- | --- | --- |
| Route | `GET|POST|DELETE /mcp/<name>/` for names in the server table whose flag is `"true"`; any other name is 404 | `pat-service/cmd/pat-service/main.go` (table), `mcp.go` |
| Authenticate | `Authorization: Bearer` with either a PAT (`sk-...`, not revoked or expired) or a Keycloak JWT with realm role `ai-user` or `ai-admin`; else 401 or 403 | `mcpAuthenticate` |
| Identity | strips client identity headers and cookies, sets `X-User-Id` (sub), `X-User-Name`, `X-Pat-Token-Id` (PAT callers) | `forwardMCP` |
| Rate limit | `tools/call` only, per user and server, fixed one-minute window in Valkey; `MCP_CALLS_PER_MINUTE` (30); 429 with `Retry-After: 60`; fails open if Valkey errs | `mcpLimited` |
| Metric | `patsvc_mcp_calls_total{user,server,tool,status}`; tool is the name for known tools, else `other` | `mcpToolName` |
| Transport | reverse proxy with streaming passthrough (SSE); upstream down is 502 | `forwardMCP` |

The upstream URL per server defaults to its in-cluster Service and can be
overridden with `WEB_SEARCH_MCP_URL` / `REPOWISE_MCP_URL`. `/mcp/` has no
public route: it is reachable only inside the cluster.

The MCP servers themselves have no authentication (repowise's transport has
none upstream). They trust the identity headers only because their
NetworkPolicies admit pat-service pods alone, so **the NetworkPolicy is part
of the security design**, not an optimisation.

## Open WebUI

- `helm/airgap-stack/values.yaml` `openwebuiToolServers` lists each server
  with its flag. `templates/openwebui-tool-servers.yaml` renders the enabled
  ones into ConfigMap `openwebui-tool-servers`, key
  `TOOL_SERVER_CONNECTIONS`, as MCP connections to
  `http://pat-service...:8080/mcp/<name>/` with `auth_type: system_oauth`
  (the chat user's own Keycloak token; no shared key) and read access for
  every user.
- Open WebUI reads it only at start: after a change, `make helm-up` and
  restart Open WebUI (`websearch-up`, `repowise-up` and their `-down` do it).
- Tools are off in a chat until the user enables them in the chat's tool menu;
  that is the consent point for sending data to the internet.
- Open WebUI's native web search is off (`ENABLE_WEB_SEARCH=false`), so search
  always goes through this door.

## Agents

One registry, `config/agents/base-profile/mcp-servers.yaml`, drives all
runtimes:

```yaml
pat_service: http://pat-service.airgap-ai-stack.svc.cluster.local:8080
servers:
  web-search: {flag: WEB_SEARCH_ENABLED, timeout: 60}
  repowise:   {flag: REPOWISE_ENABLED, timeout: 180}    # get_answer runs the LLM
```

At every agent start `catalog-sync` keeps the entries whose flag is `"true"`
(flags come from `.env` via the `agent-images` ConfigMap and the broker) and
renders them per runtime, each authenticated with the user's agent PAT:

| Runtime | File in the profile | Shape |
| --- | --- | --- |
| Hermes | `config.yaml` `mcp_servers.<name>` | `url`, `headers.Authorization: Bearer ${HERMES_INFERENCE_KEY}`, `timeout` (s); every registry name is a locked key, so users cannot add their own copy or keep a disabled one |
| pi | `pi/mcp.json` for `pi-mcp-adapter` | `url`, `headers`, `lifecycle: keep-alive`, `directTools: true` (tools appear as first-class pi tools), `requestTimeoutMs`; `allowInstall: false`, `hostConfigDiscovery: off` |
| opencode | `opencode/opencode.json` `mcp.<name>` | `type: remote`, `url`, `headers` with `{env:AGENT_INFERENCE_KEY}`, `timeout` (ms), `oauth: false` |

Built-in web tools of the agents are disabled (Hermes `web` toolset,
opencode `webfetch`/`websearch`), so the agent's web access is this server or
nothing. Agent pods can reach pat-service and nothing else, so they cannot
bypass the door.

## Switching a server on and off

The flag in `.env` is the only switch. It reaches four places, each applied
by its own target:

| Place | Applied by |
| --- | --- |
| the server's release (`helm/web-search`, `helm/repowise`) | `make websearch-up` / `websearch-down`, `make repowise-up` (`stack-up` runs `websearch-up` when enabled) |
| pat-service `/mcp/<name>/` | `make helm-up` (then restart pat-service if the pod did not roll) |
| Open WebUI tool server | `make helm-up` plus an Open WebUI restart |
| agent profiles | `make agents-up`; agents pick it up at their next start |

Runbooks: [operations/web-search.md](../../operations/web-search.md),
[repowise](repowise.md).

## Policies: what exists and what does not

| Control | Exists | Where |
| --- | --- | --- |
| Server on or off for the whole site | yes | `*_ENABLED` flags |
| Only `ai-user` / `ai-admin` may call | yes | `mcpAuthenticate` (JWT callers); PAT holders are users who had the role when they issued the PAT |
| Per-user rate limit on tool calls | yes, one value for all servers | `MCP_CALLS_PER_MINUTE` |
| Revoking a PAT cuts inference and tools at once | yes | PAT lookup |
| Users cannot add or keep MCP servers in Hermes | yes | locked keys |
| Egress limits of the server | yes | NetworkPolicies in `helm/web-search`, `helm/repowise`; `fetch_url` refuses private, cluster and metadata addresses |
| What a repowise user can see | all listed repositories for every user | `helm/repowise/values.yaml` `repos` (admission rule: only repositories every `ai-user` may read) |
| Per-server or per-tool allowlist per user or role | **no** | every authenticated user can call every enabled tool |
| Different rate limits per server or role | **no** | one `MCP_CALLS_PER_MINUTE` |
| Per-server enable for Open WebUI vs agents | **no** | one flag drives both |

A new policy of the missing kind belongs in pat-service's `/mcp` handler (the
one place all callers pass), and usually deserves an ADR.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| 404 on `/mcp/<name>/` | the flag is not `"true"` in the pat-service env: `make helm-up`, restart pat-service |
| 401 / 403 | PAT revoked or expired; JWT without `ai-user`/`ai-admin` |
| 429 | `MCP_CALLS_PER_MINUTE` reached; see `patsvc_mcp_calls_total{status="429"}` |
| 502 `<name> unavailable` | the server pod or its Service |
| Agent has no tools | the rendered config in the profile, e.g. `kubectl -n agents exec hermes-agent-<id> -c hermes -- grep -A4 mcp_servers /opt/data/home/config.yaml`; the flag in `agent-images` |
| Open WebUI has no tool | ConfigMap `openwebui-tool-servers` and an Open WebUI restart |

## Where it lives

- Proxy: `pat-service/cmd/pat-service/mcp.go`, server table in `main.go`
- Servers: `web-search-mcp/`, `helm/web-search/`, `helm/repowise/`
- Open WebUI: `helm/airgap-stack/values.yaml` `openwebuiToolServers`
- Agents: `config/agents/base-profile/mcp-servers.yaml`, `agent-catalog/agent_sync.py`
- Smoke: `make websearch-smoke`; unit tests: `make web-search-mcp-test`,
  `cd pat-service && go test ./...`, `make agents-test`

## Related

- Previous: [Managing agents](../agents/managing.md). Next: [Repowise](repowise.md)
- [PAT service](../pat-service.md), [Open WebUI](../openwebui.md), [Agents](../agents/README.md)
