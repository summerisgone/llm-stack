# Web search (OpenSERP + web-search-mcp)

Decision and verification stages:
[ADR 0017](../adr/0017-web-search-mcp-openserp-kagent.md). Search workload
details (OpenSERP config, engine-pinning sidecar):
[ADR 0016](../adr/0016-self-hosted-web-search-openserp.md).

Off by default. Queries leave the company to Google, Bing, DuckDuckGo and
Yandex from the site's egress address. Never enable on an air-gapped install.

## Path of a request

    Hermes agent (PAT) ------\
                              > pat-service /mcp/web-search/ -> web-search-mcp:8080 -> web-search:80 (nginx) -> OpenSERP 127.0.0.1:7000
    Open WebUI (user's JWT) -/                                  \-> fetch_url: public internet, 80/443 only

- pat-service authenticates (PAT, or Keycloak token with `ai-user`/`ai-admin`),
  sets `X-User-Id` / `X-User-Name` / `X-Pat-Token-Id`, limits tool calls to
  `MCP_CALLS_PER_MINUTE` per user and server (default 30, Valkey, fails open)
  and counts `patsvc_mcp_calls_total{user,server,tool,status}`.
- `web-search-mcp` accepts traffic from pat-service only (NetworkPolicy) and
  trusts its identity headers for that reason. It logs user, tool, status and
  latency, never page bodies.
- `fetch_url` refuses non-http(s), ports other than 80/443, credentials in the
  URL, and any host that resolves (any answer) to private, loopback,
  link-local, CGNAT, ULA, NAT64 or `EXTRA_DENY_CIDRS` addresses; it connects
  to the checked address and re-checks every redirect (max 5). The egress
  NetworkPolicy denies the same ranges.

## Switch on / off

`WEB_SEARCH_ENABLED` in `.env` is the only switch. It reaches four places:

| Where | How | Applied by |
| --- | --- | --- |
| `helm/web-search` release (OpenSERP, sidecar, web-search-mcp) | installed or not | `make websearch-up` / `make websearch-down` (`stack-up` runs `websearch-up` when true) |
| pat-service `/mcp/web-search/` | `airgap-runtime` Secret key; 404 unless `true` | `make helm-up`, then `kubectl -n airgap-ai-stack rollout restart deploy/pat-service` |
| Hermes profile `mcp_servers.web-search` | `hermes-images` ConfigMap -> broker -> agent sync container | `make hermes-up`; agents pick it up at their next start |
| Open WebUI tool server | entry in the `openwebui-tool-servers` ConfigMap rendered by `helm/airgap-stack` (docs/adr/0018 section 7), read through an optional `configMapKeyRef` | `make helm-up`; `websearch-up` / `websearch-down` restart Open WebUI |

On:

    # .env: WEB_SEARCH_ENABLED=true
    make helm-up && kubectl -n airgap-ai-stack rollout restart deploy/pat-service
    make websearch-up
    HERMES_OVERLAY=k8s/overlays/remote-wsl-hermes make hermes-up

Off: set `false`, then the same three steps with `make websearch-down`.

## Checks

    WEBSEARCH_SMOKE_PAT=$(scripts/kc-pat-issue <user> <password> ws-smoke 1 | awk '/^token:/{print $2}') \
      make websearch-smoke

Unit tests: `make web-search-mcp-test`, `cd pat-service && go test ./...`,
`make hermes-test`.

## Troubleshooting

- **Agent has no web tools.** `kubectl -n hermes-agents exec hermes-agent-<id> -c hermes -- grep -A4 mcp_servers /opt/data/home/config.yaml`.
  No entry: the broker's `WEB_SEARCH_ENABLED` is not `true` (`make hermes-up`,
  then stop the agent so it re-syncs). Entry present: check the agent's
  `INFERENCE_KEY` is a live PAT (`/platform` -> Hermes token).
- **401 on `/mcp/`.** Revoked or expired PAT, or an Open WebUI token whose
  issuer is not pat-service's `OIDC_ISSUER`.
- **403 on `/mcp/`.** Keycloak user without `ai-user` / `ai-admin`.
- **429.** Per-user limit; raise `MCP_CALLS_PER_MINUTE` on pat-service.
- **Empty results.** Engines serving captchas. `kubectl -n airgap-ai-stack logs deploy/web-search -c openserp`
  (503 "captcha detected" per engine) and the sidecar access log
  (`-c sidecar`, `upstream_time`). Circuit-breaker state:
  `kubectl -n airgap-ai-stack exec deploy/web-search -c openserp -- wget -qO- 127.0.0.1:7000/stats/cb`.
- **OpenSERP OOMKilled / Chromium crashes.** Raise `openserp.shmSize` or the
  memory limit, or lower `openserp.maxProcesses` in `helm/web-search/values.yaml`.
