# Cloud agent fleet

Per-user Hermes, pi, opencode and dsh agents behind `agent-broker`
([ADR 0009](../adr/0009-cloud-hermes-fleet-per-user-profiles.md),
[ADR 0014](../adr/0014-hermes-curated-catalog-and-worker-slots.md),
[ADR 0020](../adr/0020-dsh-runtime-and-per-user-web-ui.md)). This
page is the runbook for the `pods` backend as implemented; ADR 0014 records
the decisions and the V1 findings.

## What runs where

| Object | Owner | Where |
| --- | --- | --- |
| Namespace `agents`, broker Deployment/Service/ConfigMap, RBAC, ResourceQuota, NetworkPolicies | `k8s/agents` | `make agents-up` |
| ConfigMap `agent-images`, broker catalog image | `versions.lock.env` | `make agents-up` |
| Catalog content, MCP server list (`mcp-servers.yaml`) | `config/agents/base-profile/` | image `AGENT_CATALOG_IMAGE`, built by `make agent-catalog` |
| pi / opencode / dsh images (`PI_IMAGE`, `OPENCODE_IMAGE`, `DSH_IMAGE`) | `agent-adapter/` | `make agent-adapter-images` |
| dsh web route, OIDC SecurityPolicy, Secret `dsh-web-oidc` | `helm/airgap-stack/templates/dsh-web.yaml` | `make helm-up` (when `DSH_PUBLIC_ORIGIN` is set) |
| Pod `<runtime>-agent-<id>`, PVC `<runtime>-profile-<id>` (runtime `hermes`, `pi`, `opencode`, `dsh`), Secret `agent-cred-<id>` (one per user) | broker, at runtime | label `app.kubernetes.io/managed-by: agent-broker` |

`<id>` is the first 16 hex characters of `sha256(keycloak sub)`. The PVC
annotation `agents.llm-stack/username` names the user.

Agent pod layout:

- init `catalog-sync` (catalog image, uid 10001) runs `agent-sync` against
  the PVC: applies a pending `/skills` selection, replaces `catalog/`,
  rebuilds `catalog-enabled/`, merges `config.yaml` (locked keys win),
  appends `SOUL.user.md` to `SOUL.md`, archives personal skills shadowed by
  a catalog skill, and writes a JSON report to its termination message. The
  broker copies the report to the PVC annotation
  `agents.llm-stack/sync-report`.
- `hermes` (upstream image, uid 10000) runs `hermes gateway run` directly,
  not through the image's s6 `/init` (which needs root). API server on
  `:8642`, `HERMES_HOME=/opt/data/home` on the PVC, `/work` and `/tmp` are
  `emptyDir`, root filesystem read-only, no ServiceAccount token.
  `HERMES_BUNDLED_SKILLS=/nonexistent` stops Hermes seeding its ~100
  bundled skills: the catalog is the only skill source.
- The catalog layer (`catalog/`, `catalog-enabled/`, `config.yaml`,
  `SOUL.md`) is owned by uid 10001 in a sticky directory, so the agent can
  read it but not change, rename or delete it.

## pi and opencode

The broker lists one model per runtime whose image is set: `hermes-agent`,
`pi-agent` (`PI_IMAGE`), `opencode-agent` (`OPENCODE_IMAGE`); Open WebUI's
connection 2 declares all three. A user gets one agent and one profile PVC
per runtime; the K slots are shared, so one user with all three agents
running holds three slots.

- Neither agent has an OpenAI-compatible server, so the image runs
  `agent-adapter` on `:8642` (same `API_SERVER_KEY` bearer and `/health` as
  Hermes). opencode runs as `opencode serve` on loopback, driven over its
  HTTP API; pi runs in-process through its SDK. The adapter keeps one agent
  session per Open WebUI chat (`X-OpenWebUI-Chat-Id`, forwarded by the
  broker) and sends only the latest user message; sessions persist on the
  PVC.
- `agent-sync` (`AGENT_RUNTIME`) renders the runtime's config into the
  profile: `opencode/opencode.json` + `AGENTS.md`, or the pi agent dir
  `pi/` (`settings.json`, `models.json`, `mcp.json`, `AGENTS.md`). Both use
  pat-service `/v1` with `AGENT_INFERENCE_KEY` (the same `INFERENCE_KEY`
  from `agent-cred-<id>`), `catalog-enabled/` as a skills directory and
  `SOUL.md` (+ `SOUL.user.md`) as instructions.
- MCP servers come from `mcp-servers.yaml` for every runtime: Hermes
  `mcp_servers`, opencode `mcp` (remote), pi through `pi-mcp-adapter`
  (shipped in the image; pi has no MCP client). Each entry is pat-service
  `/mcp/<name>/` with the user's PAT, kept only when its flag
  (`WEB_SEARCH_ENABLED`, `REPOWISE_ENABLED`) is `true`. Adding a server:
  one entry there plus its pat-service upstream.
- Egress is pat-service only: opencode runs with `OPENCODE_PURE=1` and
  the update, models.dev, LSP-download and share switches off; pi with
  `PI_OFFLINE=1`. opencode's built-in `webfetch`/`websearch` are denied, as
  Hermes' `web` toolset is.
- Issuing a key on `/platform` restarts all of the user's agents.

```sh
make agent-adapter-images AGENTS_PLATFORM=linux/amd64   # pins PI_IMAGE / OPENCODE_IMAGE / DSH_IMAGE
make agents-k3d-load                            # also loads these images
```

An empty `PI_IMAGE`, `OPENCODE_IMAGE` or `DSH_IMAGE` (in `.env`) leaves that
agent out of the broker's model list; Open WebUI still shows the model and
gets a 404.

## dsh and its web UI

`dsh-agent` pods run agent-adapter with two DeepSeek Harness processes on
`DSH_HOME=/opt/data/home/dsh`: `dsh --profile acp` for the chats and
`dsh web` on `:3080` for the user's browser UI. The workspace is
`/opt/data/home/workspace` on the PVC. Memory limit `DSH_MEMORY_LIMIT`
(2Gi); the quota counts K x that.

The web UI needs, in this order:

1. `.env`: `DSH_PUBLIC_ORIGIN` (the dsh host, with port if the site proxy
   listens on one) and `DSH_OIDC_CLIENT_SECRET`.
2. A site-proxy rule for that host to the same edge listener as the main
   origin, keeping `Host` and setting `X-Forwarded-Proto` (as for repowise).
3. `make agents-up` (the broker reads `DSH_PUBLIC_ORIGIN` from
   `agent-images` and opens its web proxy on `:8081`), then `make helm-up`
   (route, SecurityPolicy, Secret, and `provision-dsh-oidc` for the
   Keycloak client `dsh-web`).

Checks:

```sh
kubectl -n airgap-ai-stack get httproute dsh-web -o jsonpath='{.status.parents[*].conditions[*].reason}'
kubectl -n airgap-ai-stack get securitypolicy dsh-web -o jsonpath='{.status.ancestors[*].conditions[*].reason}'
kubectl -n agents logs deploy/agent-broker | grep 'dsh web'
```

Symptoms: a 403 "untrusted host" page from dsh means the `Host` the pod got
is not the public authority (check `DSH_WEB_HOST` in the pod env); a loop
back to Keycloak means the `dsh-web` client's redirect URI does not match
`DSH_PUBLIC_ORIGIN/oauth2/callback`. `dsh web` never prints its launch
token to the logs unmasked; the broker fetches it from agent-adapter
`/web-token`.

## Deploy and update

```sh
make agents-test          # agent-sync, broker and agent-adapter tests
make agent-catalog       # build catalog image, pin AGENT_CATALOG_IMAGE
make agent-broker-image  # build llm-stack/agent-broker:local
make agents-up            # apply k8s/agents + images, restart broker
make agents-smoke         # needs Keycloak running
```

A catalog change is `make agent-catalog` (review the new
`AGENT_CATALOG_IMAGE` line in `versions.lock.env`) and `make agents-up`.
Running agents on the old catalog are stopped at their next idle moment and
pick up the new one at their next start; inactive users get it at their next
start. There is no forced rollout target yet: to cut over immediately,
delete the agent pods (`make agents-down && make agents-up`), which cuts
in-flight turns.

Remote profile (k3d on WSL2, through `make k3s-tunnel`); images are built
for amd64 and copied into the node's containerd over SSH:

```sh
make agent-catalog AGENTS_PLATFORM=linux/amd64
make agent-broker-image AGENTS_PLATFORM=linux/amd64
make agents-k3d-load
make agents-up KUBECTL="kubectl --context wsl-llm-stack" AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
KUBECONFIG=<wsl-only kubeconfig> STACK_BASE_URL=https://<public origin> \
  AGENTS_SMOKE_INFERENCE=true ./scripts/agents-smoke-test
```

`kc-pat-issue` needs a Python with TLS 1.3 (macOS `/usr/bin/python3` fails
with `TLSV1_ALERT_PROTOCOL_VERSION`; put Homebrew's first in `PATH`).

`OIDC_ISSUER` in `k8s/agents/broker.yaml` must equal the `iss` of the tokens
Open WebUI forwards -- the issuer Keycloak advertises for the browser-facing
hostname (`KC_HOSTNAME`), not the in-cluster URL. The committed value is the
local-mac one (`http://sso.ai.localhost:8080/realms/ai-stack`).

## Operating

```sh
kubectl -n agents get pods,pvc -l app.kubernetes.io/managed-by=agent-broker
kubectl -n agents port-forward svc/agent-broker 18090:8080 &
curl -s localhost:18090/healthz        # {"k":3,"running":..,"busy":..,"queued":..}
kubectl -n agents logs deploy/agent-broker | grep '"agent started"'   # includes cold-start seconds
```

- Offboarding: `scripts/agent-profile-delete <keycloak-sub|id>` deletes the
  user's pods, Secret and PVCs of every runtime. It is the only path that deletes a profile; the
  broker's Role has no PVC delete.
- `make agents-down` stops the broker and all agents; profiles stay.
- Broker restart: agents keep running; the new broker adopts them and treats
  them as active for one `IDLE_TIMEOUT`.
- Pods removed outside the broker (offboarding, eviction, OOM) are noticed
  within 30s, or at once when a request has to queue, and their slot is
  reused.

## Local-mac profile (OrbStack) notes

- OrbStack enforces NetworkPolicy. Its API server endpoint is `:26443`
  after Service DNAT, which is why the broker egress policy lists it.
- Keycloak needs the CNPG operator and `keycloak-db` running. After
  `make down` scale them back: `kubectl -n cnpg-system scale deploy --all
  --replicas=1`, then `kubectl -n airgap-ai-stack scale deploy/keycloak
  --replicas=1`.
- The realm's `llm-api` client issues tokens without `sub` and roles on this
  cluster, so `agents-smoke-test` creates a temporary confidential client
  with the `open-webui` client's scopes (`basic`, `roles`, `profile`) and
  deletes it on exit.
- gVisor is not available; `AGENT_RUNTIME_CLASS` stays empty.

## gVisor on k3d/WSL2

Verified 2026-09-24 on Windows -> WSL2 (kernel 6.18) -> Docker 29 -> k3d
(k3s v1.35.5, containerd 2.2.3) with gVisor `release-20260921.0`, platform
`systrap` (`kvm` also boots: `/dev/kvm` is visible in the node). The
`remote-wsl-agents` overlay sets `AGENT_RUNTIME_CLASS: gvisor` and adds the
`gvisor` RuntimeClass; the smoke test checks the agent's kernel is
`*-gvisor`. NetworkPolicy still applies (enforced on the host side of the
pod veth).

Node setup (manual, from the WSL host; the user needs docker, not sudo):

1. Download `releases/release/<version>/x86_64/gvisor.tar.bz2` from the
   `gvisor` GCS bucket and check its `.sha512`; `releases/release/latest/`
   has no files any more.
2. Copy `runsc`, `containerd-shim-runsc-v1` and `gvisor-bin/` into the node
   under `/var/lib/rancher/k3s/gvisor/` (a persistent volume; the node's own
   filesystem is lost when k3d recreates it). `runsc` finds the sidecars in
   `gvisor-bin/` next to itself and refuses to start without them.
3. `/var/lib/rancher/k3s/gvisor/runsc.toml`:
   `binary_name = "/var/lib/rancher/k3s/gvisor/runsc"` and
   `[runsc_config] platform = "systrap"`.
4. `/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/91-runsc.toml`:
   runtime `runsc`, `runtime_type = "io.containerd.runsc.v1"`, `runtime_path`
   to the shim, options `TypeUrl = "io.containerd.runsc.v1.options"`,
   `ConfigPath` to `runsc.toml`.
5. `docker restart k3d-llm-stack-server-0` (restarts the whole stack; the
   model server reloads). `crictl info` then lists `runsc`.

`ctr run --runtime io.containerd.runsc.v1` hangs in `created` on this node
(the shim waits for EOF on a pipe the sandbox inherited); it is not a valid
test. Test through a Pod with `runtimeClassName: gvisor`.

## Not done yet

- Inference credentials: agents call pat-service `/v1` with
  `INFERENCE_KEY` from `agent-cred-<id>` (`model.api_key:
  ${HERMES_INFERENCE_KEY}`, a locked key). The user issues it on `/platform`
  ("Issue agent key", `POST /api/agent-token`): pat-service mints an
  `agent` PAT (`issued_by = agents`, `AGENT_PAT_TTL_DAYS`, default
  7), revokes the previous one, writes the Secret and deletes the user's
  running agent pods. The broker does not mint or renew it yet (ADR 0014 section 8,
  `POST /internal/hermes-tokens`); on expiry the user issues a new one.
  `scripts/agent-inference-key` remains as an operator fallback. Without a
  key a turn ends with Hermes' `HTTP 401` error.
- Remote profile: `k8s/agents` is not folded into the `airgap-stack` chart
  yet; `make agents-up` works against any cluster, but the remote issuer and
  a pushed broker/catalog image are needed.
- Broker metrics and Grafana panels; `agent-catalog-rollout FORCE=1`.

## Moving to Agent Substrate or kagent AgentHarness

The broker's `Backend` interface
(`agent-broker/cmd/agent-broker/backend.go`) is the seam. Everything above
it -- token validation, `/skills`, onboarding and catalog notices, slot
count, LRU/idle rules, queueing -- stays as is.

| Concern | `pods` (now) | Agent Substrate | kagent `AgentHarness` |
| --- | --- | --- | --- |
| One agent per user | Pod `hermes-agent-<id>` | Actor from a Hermes `ActorTemplate` | One `AgentHarness` (`backend: hermes`, `runtime: substrate`) per user |
| `Start` | create PVC/Secret/Pod, wait Ready | `CreateActor` first time, else `ResumeActor`; wait for `/health` | create/resume the harness |
| `Stop` (idle, eviction) | delete pod | `SuspendActor` (RAM + disk snapshot) | suspend the harness actor |
| `K` | broker count + `ResourceQuota` | `WorkerPool.replicas` + broker count | `substrate.workerPoolRef` |
| Profile truth | RWO PVC | last snapshot (`snapshotsConfig.location`) | same as Substrate |
| Catalog update | init sync at every start | resume runs no init: rebase via profile bundle (ADR 0014 section 10) | same |
| Per-user credential | Secret env | pushed to an in-actor sidecar after every start | `gatewayTokenSecretRef` is per harness |
| Sandbox | runc, non-root, read-only root | gVisor | gVisor |

What already fits: the agent is one process with one HTTP port, a health
endpoint and all state under `HERMES_HOME`; the sync step is idempotent and
reads everything from the catalog image and one env var; the broker holds no
database, so switching `AGENT_BACKEND` needs no data migration beyond
moving each profile's files into its first snapshot. `AGENT_RUNTIME_CLASS`
already lets the `pods` backend run agents under a gVisor RuntimeClass where
the node has `runsc`, as an intermediate step before snapshots. The adoption
gate is ADR 0014 stage VS; none of it can run on OrbStack.
