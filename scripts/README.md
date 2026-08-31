# Operational helpers

## Rendering and validation

- `helm-render` renders `k8s/overlays/remote-wsl-vllm-nvfp4` into
  `helm/airgap-stack/templates/resources.yaml` and writes a stable object
  manifest for review. GPU objects are kept out of the release on purpose;
  routing objects are an error, not a filter (see `k8s/README.md`).
- `helm-env-values` turns `.env` into the chart's runtime values overlay, so
  `.env` stays the source of truth for the workload Secret.
- `preflight` client-validates every kustomization.
- `make verify` runs `preflight`, `helm lint`, `helm template`, a dry run of
  the GPU prerequisite objects, and a check that the committed chart still
  matches what `helm-render` produces from the current sources. It covers
  `helm/airgap-stack` only — `helm/vllm-inference` and
  `helm/sglang-inference` are not linted or templated by it.

## Endpoint derivation

`lib-endpoints.sh` is sourced by every smoke test. It derives the SSO, Open
WebUI, dashboard and API endpoints for whichever routing profile is in use —
path-based when `STACK_BASE_URL` is set, host-based `*.localhost` otherwise —
so no test hardcodes a site's hostname. `STACK_TUNNEL_PORT` redirects the
development hostnames at a local SSH tunnel; `GRAFANA_BASE_URL` and
`LANGFUSE_BASE_URL` opt the operator UIs into the checks.

## Smoke tests

- `smoke-test` — Keycloak discovery, the Open WebUI and Grafana OIDC
  redirects, and the Keycloak-governed Open WebUI role configuration.
- `services-smoke-test` — the above plus operator-managed state (Postgres,
  ClickHouse, Valkey, MinIO, Prometheus) and the unauthenticated edge
  behaviour: the dashboard redirects to SSO, `/v1` answers 401.
- `pat-smoke-test` — the end-to-end PAT playbook: issue through the SSO
  dashboard, use against `/v1`, hit the per-owner rate limit, revoke, and
  prove the same key is rejected immediately. `inference-smoke-test` is a
  compatibility alias.
- `llmd-nvfp4-smoke-test` — the remote profile: llm-d EPP metrics plus the
  full PAT playbook against `qwen-3.8-27b` (the single canonical model name).
  Set `INFERENCE_SMOKE_MODEL` to
  aim it at another backend's model name (`llamacpp-local`, …).
- `vllm-nvfp4-smoke-test` — the GPU Deployment on its own.

Set `INFERENCE_SMOKE_VERIFY_CHAT=false` to skip the calls that reach the model
while keeping every other boundary under test; `make smoke-nogpu` is that mode
for the remote profile.

## User administration

These are Python (stdlib only, no extra dependency), unlike the rest of this
directory — the exception agreed for this one group of scripts, not a change
to the POSIX-sh convention in AGENTS.md for the others. `#!/usr/bin/env
python3` picks up whatever `python3` is first on `PATH`; on a Mac with only
the Xcode Command Line Tools' `/usr/bin/python3` (LibreSSL 2.8.3) ahead of a
modern interpreter, HTTPS calls to a TLS 1.2+/1.3-only origin fail with
`TLSV1_ALERT_PROTOCOL_VERSION` — point `PATH` (or invoke the script as
`python3.11 scripts/kc-pat-issue …`) at a Homebrew or pyenv Python instead.

- `lib_keycloak_admin.py` is imported by every script below. It opens the
  `kubectl port-forward svc/keycloak` and waits for it, since the admin
  console cannot be driven through a port-forward at all (`KC_HOSTNAME_ADMIN`
  expects the fixed public origin back for its third-party cookie check) and
  the public edge refuses `/sso/admin` outright — the admin REST API has
  neither restriction.
- `lib_endpoints.py` is imported by `kc-pat-issue`; a partial Python port of
  `lib-endpoints.sh` covering only the endpoints that script needs.
- `kc-user-add <username> [role] [email] [first_name] [last_name]` creates a
  user, assigns a realm role (default `ai-user`), sets a policy-conforming
  generated password, and clears the `CONFIGURE_TOTP` default required
  action so direct-grant and browser login both work immediately. `first_name`
  and `last_name` matter: the realm's user-profile config requires both for
  role `user`, and omitting them makes Keycloak silently attach an implicit
  `VERIFY_PROFILE` action that surfaces later as the same "Account is not
  fully set up" login error as a leftover `CONFIGURE_TOTP` action.
- `kc-user-list [username-filter]` lists users with their realm roles.
- `kc-user-delete <username>` removes a user.
- `kc-pat-issue <username> <password> [token-name] [expires-in-days]` issues
  a personal access token for an existing user without a browser, by
  replaying the dashboard's own OAuth-callback logic (sign in, build the
  signed session cookie, call the token API). Runs over the public origin,
  not the admin port-forward.

## OIDC reconciliation

`provision-grafana-oidc` and `provision-pat-oidc` reconcile the Keycloak
clients for both freshly imported and already-persistent realms;
`provision-realm-security` applies brute-force protection, the password
policy and the TOTP default action. All three run at the end of
`make stack-up` and `make helm-up`, and all three reach the Admin API through
a `kubectl port-forward`, never the public origin — `/sso/admin` is closed at
the edge.
