# Adding an MCP server

[Русский](adding-a-server.ru.md) | [Handbook index](../README.md)

The steps to put a new MCP server behind pat-service and make it available to
Open WebUI and all agent runtimes. The pattern is the one web-search and
repowise follow; read [MCP servers](README.md) first.

**Contents**

- [Before you start](#before-you-start)
- [Steps](#steps)
- [Rollout order](#rollout-order)
- [Checklist](#checklist)
- [Related](#related)

## Before you start

- **Transport:** the server must speak MCP over streamable HTTP (or SSE). A
  stdio-only server needs a small HTTP wrapper in its own image.
- **Trust:** it will receive `X-User-Id`, `X-User-Name` and `X-Pat-Token-Id`
  from pat-service and may trust them only if nothing else can reach it.
- **Data:** decide whether it sends anything out of the company. If so, it
  gets its own flag that stays `false` on air-gapped installs, and an ADR.
- **Cost:** tools that call the model themselves (like repowise
  `get_answer`) need a longer client timeout and a service PAT.

## Steps

1. **Workload.** A Helm chart `helm/<name>/` with Deployment and Service in
   `airgap-ai-stack`, image pinned in `versions.lock.env` (by digest), and a
   NetworkPolicy that admits ingress on the MCP port **only** from pods
   labelled `app.kubernetes.io/name: pat-service`, with egress limited to
   what the server needs. Copy `helm/web-search/templates/networkpolicy.yaml`.
   Makefile targets `<name>-up` / `<name>-down` that restart Open WebUI like
   `websearch-up`.
2. **Flag.** `<NAME>_ENABLED=false` in `.env.example` with a comment on what
   leaves the company.
3. **pat-service route.** Add a row to the server table in
   `pat-service/cmd/pat-service/main.go`
   (`{"<name>", "<NAME>_ENABLED", "<NAME>_MCP_URL", "<default in-cluster URL>"}`).
   The list of names is code, not config: this needs a pat-service build
   (CI publishes the image). Add the server's tool names to `mcpToolName` in
   `mcp.go` so `patsvc_mcp_calls_total` labels them (unknown tools count as
   `other`), and a test in `mcp_test.go`. Pass the flag to pat-service: the
   env block in `k8s/base/applications.yaml` reads it from `airgap-runtime`
   like `WEB_SEARCH_ENABLED`.
4. **Open WebUI.** An entry in `openwebuiToolServers` in
   `helm/airgap-stack/values.yaml` (`flag`, `id`, `name`, `description`,
   `url: http://pat-service.airgap-ai-stack.svc.cluster.local:8080/mcp/<name>/`).
5. **Agents.** An entry in `config/agents/base-profile/mcp-servers.yaml`
   (`<name>: {flag: <NAME>_ENABLED, timeout: <seconds>}`). The flag must reach
   the broker: add it to the `agent-images` ConfigMap in the Makefile
   `agents-up` target and to the env the broker passes to the sync init
   container (`agent-broker/cmd/agent-broker/main.go`, `pods.go`), next to
   `WEB_SEARCH_ENABLED`. `make agents-test` covers the renderers.
6. **Smoke.** At least: `/mcp/<name>/` answers `tools/list` with a PAT, 401
   without, and the server refuses a direct connection from another pod.
   `scripts/websearch-smoke-test` is the model.
7. **Docs.** The server table and policies in [MCP servers](README.md), a
   runbook if it needs operations, both languages.

## Rollout order

```sh
# .env: <NAME>_ENABLED=true
make <name>-up                                   # the server; restarts Open WebUI
make helm-up                                     # airgap-runtime flag, Open WebUI tool entry
kubectl -n airgap-ai-stack rollout restart deploy/pat-service   # if its pod did not roll
make agent-catalog AGENTS_PLATFORM=linux/amd64 && make agents-k3d-load
make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
```

pat-service must run an image that contains the new table row before
`helm-up`, or `/mcp/<name>/` stays 404.

## Checklist

- [ ] Chart, NetworkPolicy (ingress from pat-service only), pinned image
- [ ] `.env.example` flag, off by default
- [ ] pat-service table row, tool names, test, released image
- [ ] `openwebuiToolServers` entry
- [ ] `mcp-servers.yaml` entry and the flag passed through `agents-up` and the broker
- [ ] Smoke test, `make verify`, `make agents-test`, `cd pat-service && go test ./...`
- [ ] ADR if data leaves the company or a new policy is introduced
- [ ] Handbook pages updated in English and Russian

## Related

- Previous: [Repowise](repowise.md). Next: [Operations](../operations.md)
- [ADR 0017](../../adr/0017-web-search-mcp-openserp-kagent.md),
  [ADR 0018](../../adr/0018-repowise-codebase-intelligence.md) section 5
