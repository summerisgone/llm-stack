# 0009: Cloud Hermes fleet with per-user profiles, managed by Kubernetes

## Status

Proposed (draft, discussion). No manifests or code written yet; this records
the architecture direction agreed in review before implementation.

Refined by [ADR 0014](0014-hermes-curated-catalog-and-worker-slots.md),
which settles open items 1 and 2 below, replaces "mount a profile PVC onto
a free worker" with per-user pods in `K` slots, and keeps the literal
worker-pool form of this ADR as a gated option on Agent Substrate
(0014 section 10).

## Context

Every user today runs a local Hermes Agent install. Profiles, skills, memory
and session history live on each person's laptop; the runtime is patched and
updated per machine. Non-technical users cannot or will not install the CLI,
and even for developers the agent's state is hostage to one disk.

The stack already has the identity and inference plumbing this needs:

- Keycloak SSO terminates at Open WebUI and forwards `X-User-Id` /
  `X-User-Name` (`k8s/base/applications.yaml`).
- Programmatic access goes through per-user PATs, and `pat-service` already
  derives a user identity from a token and enriches requests
  ([ADR 0003](0003-private-ai-gateway-and-pats.md),
  [ADR 0008](0008-per-user-fair-share.md)).
- Inference is one shared vLLM replica behind Envoy AI Gateway → pat-service →
  EPP, with per-user fair-share bands keyed on the user's `sub`.

The gap being closed is not inference or identity — both exist. It is the
*agent runtime*: how to run N cloud Hermes instances, one per user, inside
Kubernetes, so non-technical users reach a capable agent from a browser
without installing anything.

## Decision

### Centralise the agent; keep per-user profiles as the unit of state

A Hermes profile (`~/.hermes/profiles/<name>/` — skills, memories, session
history, `state.db`) is already the exact unit that should be handed to a
user. The cloud fleet runs one profile per user, keyed on the Keycloak `sub`
(validated into a safe directory name), lazily provisioned on first request
with the shared skill set mounted read-only plus an empty personal layer.

The identity→profile mapping reuses the existing token identity layer; no new
identity system is built. Open WebUI already forwards the user id, and
`pat-service` already proves the "token → user → policy" pattern.

### Separate state from compute: pool of workers + state-sidecar, not per-user pods

The only durable, valuable thing is the profile. The agent process itself is
near-stateless — it boots, attaches to a profile, works, exits. Therefore:

- **State** lives on a per-user PVC (profile directory). It survives any
  redeploy.
- **Compute** is a pool of `K` Hermes server pods. An adapter mounts a user's
  profile PVC onto a free worker, serves the session, and an idle-reclaimer
  releases the worker. "An instance" is an ephemeral container attached to a
  durable profile, not a lifelong pod per user.

This is chosen over a long-lived `StatefulSet` pod per user because that does
not scale past a handful of users and keeps idle pods alive for no reason.

### One hard constraint: SQLite on network storage

Hermes' canonical session store is SQLite (`state.db`). SQLite does not live
well on NFS/network PVCs. This forces either sticky routing (a user's requests
always land on the worker that owns their PVC) or splitting the profile into
object-storage-backed files rather than one SQLite file. This decision is the
main thing that shapes the replica scheme and must be settled before the
worker pool is designed.

### Sandbox: read-only repo scope by default

Non-technical users working against a corporate GitLab ask read/search
questions ("how is function A implemented in repo B"). The default agent
sandbox is therefore read-only over the repo: search and trace, no push. A
merge request is a separate, explicit, higher-privilege operation. Each agent
acts only within the scope of its user's credentials (minimal-scope
`read_repository` token). Network policy allows only GitLab and the inference
endpoint.

### Front-end: Open WebUI is one option, not the sole front

Open WebUI gives SSO and a familiar chat UI for free, and it can talk to
Hermes' OpenAI-compatible HTTP API. But it is a question-answer UI: it does
not show the agent's tool progress, so multi-step tasks read as a long spinner
— weak on trust for non-technical users. The web dashboard (which streams tool
progress) is the stronger option where that matters. The front-end choice is
left open and does not block the architecture, because every front-end shares
the same cloud Hermes API on a per-user profile.

### Corporate messenger is not the primary front

The messenger idea is rejected as the *primary* interface for two reasons:
it cannot stream tool progress (terminal/patches/lists), and the specific
corporate messenger has no connector, which would mean writing and maintaining
one. A messenger may later become a *second* channel (fire-and-forget tasks,
notifications) over the same cloud agent, but only with an existing connector
(Slack/Telegram/Matrix), never as the first interface.

## Consequences

- Local Hermes installs are replaced by a centrally managed fleet: one
  runtime to patch and update, profile state survives any client.
- Every agent inference call flows through the existing Envoy → pat-service →
  EPP path and gets a real per-user `fairness_id`, fixing the current gap where
  all Open WebUI traffic shares one undifferentiated flow (noted in
  [ADR 0008](0008-per-user-fair-share.md)).
- The worker-pool + state-sidecar shape means the front-end can be swapped
  (Open WebUI → dashboard → messenger) as one integration, not a rebuild.
- Open work, in order: (1) resolve the SQLite-on-network-storage question
  (sticky routing vs object-backed profile); (2) define the adapter contract
  between the chosen front-end and the Hermes HTTP API; (3) lay the objects
  down in `k8s/base` with one owner each, per
  [ADR 0002](0002-one-owner-per-object.md), keeping the inference path owned
  by the existing `airgap-stack` chart.