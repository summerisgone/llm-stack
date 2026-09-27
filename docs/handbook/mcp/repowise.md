# Repowise

[Русский](repowise.ru.md) | [Handbook index](../README.md)

Repowise indexes a fixed list of code repositories (wiki pages, symbols,
full-text and vector search, change risk) and answers questions about them.
People use its web UI on its own host; the model and agents use it as the
`repowise` MCP server through pat-service. This page is the operations view
that [ADR 0018](../../adr/0018-repowise-codebase-intelligence.md) section 8
asked for.

**Contents**

- [What runs](#what-runs)
- [Preconditions](#preconditions)
- [Deploy and update](#deploy-and-update)
- [Repositories](#repositories)
- [Web UI access](#web-ui-access)
- [Credentials](#credentials)
- [Checks](#checks)
- [Where it lives](#where-it-lives)
- [Related](#related)

## What runs

One Deployment `repowise` (release `helm/repowise`) with a 20 Gi PVC:

| Container | Does |
| --- | --- |
| `workspace` (init) | clones the repositories and builds the initial index (`repowise reindex`), which can take tens of minutes |
| `app` | web UI and API on port 3000 |
| `mcp` | `repowise mcp` over streamable HTTP on port 7338 (Service `repowise-mcp`) |
| `sync` | fetches every `syncIntervalSeconds` (900) so the index follows the branches |

Its LLM and embeddings calls go through pat-service `/v1` with a service
PAT (user `svc-repowise`), so they are rate-limited, queued and traced like
any user: `qwen-3.8-27b` for prose and `get_answer`, `bge-m3` for vectors.
Thinking is switched off (`REPOWISE_REASONING=off`), otherwise answers came
back empty.

## Preconditions

- `REPOWISE_ENABLED=true`, `REPOWISE_PUBLIC_ORIGIN`, `STACK_BASE_URL`,
  `REPOWISE_API_KEY`, `REPOWISE_OIDC_CLIENT_SECRET`, `REPOWISE_PAT` in `.env`.
- Embeddings in `accelerator: gpu` mode with one Ready replica
  (`make repowise-up` refuses otherwise): indexing on the CPU embeddings
  starves the node. The GPU budget was measured with ninfer as the live
  engine (peak about 2.2 GiB); vLLM at `gpuMemoryUtilization: 0.94` leaves
  less than that free, so lower it before indexing under vLLM
  ([KV cache](../inference/kv-cache.md#how-the-stack-uses-the-cache)).
- A site-proxy rule that forwards the repowise host to the same edge
  listener as the main origin.
- The Keycloak client `repowise`: `make provision-repowise-oidc`.
- The service user and its PAT: `scripts/kc-user-add` then
  `scripts/kc-pat-issue` ([scripts/README.md](../../../scripts/README.md)).

## Deploy and update

```sh
make provision-repowise-oidc        # once, after REPOWISE_OIDC_CLIENT_SECRET is set
make helm-up                        # pat-service /mcp/repowise/, Open WebUI tool entry
make repowise-up                    # Secrets from .env, the release, Open WebUI restart
make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents   # agent profiles
```

`make repowise-down` uninstalls the release, deletes its Secrets and restarts
Open WebUI; set the flag to `false` and run `make helm-up` and `make
agents-up` to remove the tool everywhere. An upgrade is a new
`REPOWISE_IMAGE` in `versions.lock.env` (built by
`.github/workflows/repowise-image.yml`) plus `make repowise-up`.

## Repositories

`repos` in `helm/repowise/values.yaml` (alias, URL, branch). **Admission
rule:** list only repositories every `ai-user` may read. Everyone signed in
sees all of them, in the UI and through MCP; repowise has no per-user
access. Only public HTTPS repositories are supported in this stage (no
credentials in the release). Adding one is an edit plus `make repowise-up`;
the init container indexes it.

## Web UI access

The UI is on its own host (`REPOWISE_PUBLIC_ORIGIN`), behind Envoy Gateway
OIDC with realm-role authorization (`helm/repowise/templates/route.yaml`):

| Role | Can |
| --- | --- |
| `ai-user`, `ai-admin` | read everything, use chat and blast-radius analysis |
| `repowise-admin` | everything: settings, add or delete repositories, sync, re-index |

The shared `REPOWISE_API_KEY` is injected at the edge; browsers never see
it. Chats in the UI are shared between users (repowise has no users).

## Credentials

| Secret | From | Rotation |
| --- | --- | --- |
| `repowise` (`REPOWISE_API_KEY`, `OPENAI_API_KEY` = service PAT) | `.env` via `make repowise-up` | new values in `.env`, `make repowise-up` |
| `repowise-credential` (`Bearer <api key>` for the edge) | same | same |
| `repowise-oidc` (client secret) | same | also update the Keycloak client: `make provision-repowise-oidc` |

The service PAT expires like any PAT (up to 365 days); an expired one breaks
indexing and `get_answer` with 401s in the `app` and `mcp` logs.

## Checks

```sh
kubectl -n airgap-ai-stack get pods -l app.kubernetes.io/name=repowise
kubectl -n airgap-ai-stack logs deploy/repowise -c sync --tail=20
```

Through the door, with a PAT: an MCP `tools/list` on
`pat-service /mcp/repowise/` lists 11 tools, and
`patsvc_mcp_calls_total{server="repowise"}` grows with calls
([ADR 0018 verification log](../../adr/0018-repowise-codebase-intelligence.md#verification-log)).

## Where it lives

- Chart: `helm/repowise/` (`values.yaml`, `templates/workload.yaml`,
  `networkpolicy.yaml`, `route.yaml`); Makefile `repowise-up`
- MCP registry entries: `config/agents/base-profile/mcp-servers.yaml`,
  `helm/airgap-stack/values.yaml` `openwebuiToolServers`

## Related

- Previous: [MCP](README.md). Next: [Adding an MCP server](adding-a-server.md)
