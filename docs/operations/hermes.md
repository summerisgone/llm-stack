# Hermes agent fleet

Per-user Hermes agents behind `hermes-broker`
([ADR 0009](../adr/0009-cloud-hermes-fleet-per-user-profiles.md),
[ADR 0014](../adr/0014-hermes-curated-catalog-and-worker-slots.md)). This
page is the runbook for the `pods` backend as implemented; ADR 0014 records
the decisions and the V1 findings.

## What runs where

| Object | Owner | Where |
| --- | --- | --- |
| Namespace `hermes-agents`, broker Deployment/Service/ConfigMap, RBAC, ResourceQuota, NetworkPolicies | `k8s/hermes` | `make hermes-up` |
| ConfigMap `hermes-images`, broker catalog image | `versions.lock.env` | `make hermes-up` |
| Catalog content | `config/hermes/base-profile/` | image `HERMES_CATALOG_IMAGE`, built by `make hermes-catalog` |
| Pod `hermes-agent-<id>`, PVC `hermes-profile-<id>`, Secret `hermes-cred-<id>` | broker, at runtime | label `app.kubernetes.io/managed-by: hermes-broker` |

`<id>` is the first 16 hex characters of `sha256(keycloak sub)`. The PVC
annotation `hermes.llm-stack/username` names the user.

Agent pod layout:

- init `catalog-sync` (catalog image, uid 10001) runs `hermes-sync` against
  the PVC: applies a pending `/skills` selection, replaces `catalog/`,
  rebuilds `catalog-enabled/`, merges `config.yaml` (locked keys win),
  appends `SOUL.user.md` to `SOUL.md`, archives personal skills shadowed by
  a catalog skill, and writes a JSON report to its termination message. The
  broker copies the report to the PVC annotation
  `hermes.llm-stack/sync-report`.
- `hermes` (upstream image, uid 10000) runs `hermes gateway run` directly,
  not through the image's s6 `/init` (which needs root). API server on
  `:8642`, `HERMES_HOME=/opt/data/home` on the PVC, `/work` and `/tmp` are
  `emptyDir`, root filesystem read-only, no ServiceAccount token.
  `HERMES_BUNDLED_SKILLS=/nonexistent` stops Hermes seeding its ~100
  bundled skills: the catalog is the only skill source.
- The catalog layer (`catalog/`, `catalog-enabled/`, `config.yaml`,
  `SOUL.md`) is owned by uid 10001 in a sticky directory, so the agent can
  read it but not change, rename or delete it.

## Deploy and update

```sh
make hermes-test          # hermes-sync unit tests, broker go vet + go test
make hermes-catalog       # build catalog image, pin HERMES_CATALOG_IMAGE
make hermes-broker-image  # build llm-stack/hermes-broker:local
make hermes-up            # apply k8s/hermes + images, restart broker
make hermes-smoke         # needs Keycloak running
```

A catalog change is `make hermes-catalog` (review the new
`HERMES_CATALOG_IMAGE` line in `versions.lock.env`) and `make hermes-up`.
Running agents on the old catalog are stopped at their next idle moment and
pick up the new one at their next start; inactive users get it at their next
start. There is no forced rollout target yet: to cut over immediately,
delete the agent pods (`make hermes-down && make hermes-up`), which cuts
in-flight turns.

Remote profile (k3d on WSL2, through `make k3s-tunnel`); images are built
for amd64 and copied into the node's containerd over SSH:

```sh
make hermes-catalog HERMES_PLATFORM=linux/amd64
make hermes-broker-image HERMES_PLATFORM=linux/amd64
make hermes-k3d-load
make hermes-up KUBECTL="kubectl --context wsl-llm-stack" HERMES_OVERLAY=k8s/overlays/remote-wsl-hermes
KUBECONFIG=<wsl-only kubeconfig> STACK_BASE_URL=https://<public origin> \
  HERMES_SMOKE_INFERENCE=true ./scripts/hermes-smoke-test
```

`kc-pat-issue` needs a Python with TLS 1.3 (macOS `/usr/bin/python3` fails
with `TLSV1_ALERT_PROTOCOL_VERSION`; put Homebrew's first in `PATH`).

`OIDC_ISSUER` in `k8s/hermes/broker.yaml` must equal the `iss` of the tokens
Open WebUI forwards -- the issuer Keycloak advertises for the browser-facing
hostname (`KC_HOSTNAME`), not the in-cluster URL. The committed value is the
local-mac one (`http://sso.ai.localhost:8080/realms/ai-stack`).

## Operating

```sh
kubectl -n hermes-agents get pods,pvc -l app.kubernetes.io/managed-by=hermes-broker
kubectl -n hermes-agents port-forward svc/hermes-broker 18090:8080 &
curl -s localhost:18090/healthz        # {"k":3,"running":..,"busy":..,"queued":..}
kubectl -n hermes-agents logs deploy/hermes-broker | grep '"agent started"'   # includes cold-start seconds
```

- Offboarding: `scripts/hermes-profile-delete <keycloak-sub|id>` deletes the
  pod, Secret and PVC. It is the only path that deletes a profile; the
  broker's Role has no PVC delete.
- `make hermes-down` stops the broker and all agents; profiles stay.
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
  cluster, so `hermes-smoke-test` creates a temporary confidential client
  with the `open-webui` client's scopes (`basic`, `roles`, `profile`) and
  deletes it on exit.
- gVisor is not available; `AGENT_RUNTIME_CLASS` stays empty.

## gVisor on k3d/WSL2

Verified 2026-09-24 on Windows -> WSL2 (kernel 6.18) -> Docker 29 -> k3d
(k3s v1.35.5, containerd 2.2.3) with gVisor `release-20260921.0`, platform
`systrap` (`kvm` also boots: `/dev/kvm` is visible in the node). The
`remote-wsl-hermes` overlay sets `AGENT_RUNTIME_CLASS: gvisor` and adds the
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
  `INFERENCE_KEY` from `hermes-cred-<id>` (`model.api_key:
  ${HERMES_INFERENCE_KEY}`, a locked key). The user issues it on `/platform`
  ("Issue Hermes key", `POST /api/hermes-token`): pat-service mints a
  `hermes-agent` PAT (`issued_by = hermes`, `HERMES_PAT_TTL_DAYS`, default
  7), revokes the previous one, writes the Secret and deletes a running
  agent pod. The broker does not mint or renew it yet (ADR 0014 section 8,
  `POST /internal/hermes-tokens`); on expiry the user issues a new one.
  `scripts/hermes-inference-key` remains as an operator fallback. Without a
  key a turn ends with Hermes' `HTTP 401` error.
- Open WebUI second connection (`OPENAI_API_BASE_URLS` + `system_oauth` on
  both). Not added because the remote overlay's connection list is being
  changed in the working tree; the broker already accepts Open WebUI's
  forwarded Keycloak token.
- Remote profile: `k8s/hermes` is not folded into the `airgap-stack` chart
  yet; `make hermes-up` works against any cluster, but the remote issuer and
  a pushed broker/catalog image are needed.
- Broker metrics and Grafana panels; `hermes-catalog-rollout FORCE=1`.

## Moving to Agent Substrate or kagent AgentHarness

The broker's `Backend` interface
(`hermes-broker/cmd/hermes-broker/backend.go`) is the seam. Everything above
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
database, so switching `HERMES_BACKEND` needs no data migration beyond
moving each profile's files into its first snapshot. `AGENT_RUNTIME_CLASS`
already lets the `pods` backend run agents under a gVisor RuntimeClass where
the node has `runsc`, as an intermediate step before snapshots. The adoption
gate is ADR 0014 stage VS; none of it can run on OrbStack.
