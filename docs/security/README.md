# Security

This stack is designed to be exposed: one HTTPS origin, reachable from the
public internet, in front of an identity provider and a model server. That
makes the boundary decisions load-bearing, so they are written down here
rather than inferred from manifests.

Status: **items 1, 3-7 from "Before public exposure" are applied.**
Item 2 (credential rotation) remains: the demo credentials listed in the
inventory below are still present and must be replaced before the stack handles
real users.

## Trust boundaries

```
[Internet]
  https://<public-origin>                     TLS terminates here (site proxy)
    -> Envoy Gateway `edge`, listener `external`
         /sso       Keycloak            <- the only credential-accepting surface
         /platform  PAT dashboard       <- Keycloak SSO only
         /v1        PAT service         <- `sk-…` token only
         /          Open WebUI          <- Keycloak SSO only

[Cluster only, no public listener]
  ai-gateway-private (ClusterIP)
    <- Open WebUI          (signed-in user's Keycloak token)
    <- PAT service         (its own service-account JWT, after PAT lookup)
    -> llm-d EPP -> vLLM

[Operator only, no public route]
  grafana-nodeport, langfuse-nodeport   <- reachable on the host network only
```

Four properties hold this together, and each is checked by a smoke test:

1. **The AI Gateway has no public listener.** Nothing outside the cluster can
   reach the model except through Open WebUI or the PAT service.
2. **`/v1` accepts only PATs.** Client-supplied `X-User-*` headers are
   stripped at the edge before the PAT service sees them, so a caller cannot
   assert an identity. The PAT service adds them back only after a successful
   lookup.
3. **PATs are stored as HMAC hashes**, never in clear, and revocation takes
   effect on the next request.
4. **Roles come from Keycloak.** Password sign-in is disabled in Open WebUI;
   an account without `ai-user` or `ai-admin` is rejected.

Traffic is plaintext HTTP from the TLS-terminating site proxy onward,
including inside the cluster. That is an accepted boundary for a single-host
deployment and an explicit decision, not an oversight — see
[ADR 0004](../adr/0004-tls-terminates-at-the-site-proxy.md). It stops being
acceptable the moment the proxy and the cluster are on different machines.

## Exposing Keycloak: what to close and what to leave open

The sign-in page must stay open. It is the entry point for Open WebUI, the PAT
dashboard and Grafana, and every one of those uses a redirect-based OIDC flow.
Putting HTTP basic auth (or any other shared-secret gate) in front of `/sso`
breaks all three — the browser is prompted in the middle of a redirect — hands
the same password to every user, and replaces a purpose-built, hardenable
login form with a weaker one. **Do not do it.**

What is closed is everything under `/sso` that is not the OIDC flow.
`routes-external.yaml` puts a `sso-reject-admin` rule ahead of the catch-all
`/sso` rule and answers `/sso/admin` and `/sso/realms/master` with a 404
direct response — longer path prefixes win in Gateway API, so this is
declarative rather than order-dependent.

Administration happens the way Grafana and Langfuse already do, over a
port-forward, never from the internet:

    kubectl -n airgap-ai-stack port-forward svc/keycloak 8888:8080
    # http://localhost:8888/sso/admin

Keycloak's `KC_HOSTNAME_ADMIN` is pinned to `http://localhost:8888/sso`
(`k8s/base/applications.yaml`). Unlike `KC_HOSTNAME` it is not derived from
the incoming request, so the admin console only works on exactly that local
port. `scripts/provision-*-oidc` and `scripts/pat-smoke-test` use the same
port-forward for Admin API access rather than the public origin.

## Before public exposure

Ordered by ratio of risk removed to effort.

1. **[DONE] Close the admin surface.** `/sso/admin` and `/sso/realms/master`
   return 404 from the edge (`routes-external.yaml`). Provision scripts and
   the PAT smoke test use a `kubectl port-forward` for admin API access.
   Verified by `make smoke-nogpu` (admin-surface check in `scripts/smoke-test`).
2. **[TODO] Rotate every credential** in the inventory below, and delete the
   `demo` realm user. Nothing with `-demo-only` in its name may survive.
3. **[DONE] Take Keycloak out of development mode.** Switched from `start-dev`
   to `start` with `KC_HOSTNAME_STRICT=true` in `k8s/base/applications.yaml`.
4. **[DONE] Turn on brute-force protection.** `bruteForceProtected: true`,
   `failureFactor: 5`, lockout increments, password policy (length 12,
   upper/lower/digit/special), and `sslRequired: all` added to
   `k8s/realm-demo.json`. Applied to running realm via
   `make provision-realm-security`.
5. **[DONE] Rate-limit `/sso` by client IP.** `BackendTrafficPolicy`
   `edge-sso-token-rate-limit` (10 req/min) on the token endpoint and
   `edge-sso-rate-limit` (100 req/min) on the general SSO prefix, both in
   `routes-external.yaml`.
6. **[DONE] Rate-limit `/v1` by client IP before the PAT lookup.**
   `BackendTrafficPolicy` `edge-api-ip-rate-limit` (60 req/min per source IP)
   targets the `api` rule in `routes-external.yaml`.
7. **[DONE] Require a second factor.** `CONFIGURE_TOTP` added as a default
   required realm action in `k8s/realm-demo.json` and applied to the running
   realm via `make provision-realm-security`. Direct-grant flows (smoke test)
   are unaffected; browser logins will prompt for TOTP enrollment.

Optional, depending on how the user population is shaped: an IP/CIDR allowlist
on the site proxy is worthwhile if the set of users is known and stable, and
pointless if the whole reason for a public origin is access from anywhere.

## Credential inventory

`.env` is gitignored and holds only part of the set. Everything else is in
tracked files in clear text. Rotating a credential means changing it in
**every** location listed, then restarting the affected workload.

| Credential | Locations |
| --- | --- |
| Keycloak bootstrap admin | `k8s/base/applications.yaml` (`KC_BOOTSTRAP_ADMIN_*`); used by `scripts/provision-*-oidc` and `pat-smoke-test` via `KEYCLOAK_ADMIN_USER` / `KEYCLOAK_ADMIN_PASSWORD` |
| Realm user `demo` | `k8s/realm-demo.json` — delete rather than rotate |
| `open-webui` client secret | `k8s/base/applications.yaml` (`OAUTH_CLIENT_SECRET`) and `k8s/realm-demo.json` |
| `pat-gateway` client secret | `.env` (`PAT_GATEWAY_CLIENT_SECRET`) and `k8s/realm-demo.json` — **known to be out of sync today**, see below |
| `grafana` client secret | `k8s/realm-demo.json`, `config/gateway-addons/values.yaml`, `scripts/provision-grafana-oidc` (`GRAFANA_OIDC_CLIENT_SECRET`) |
| Grafana admin | `k8s/base/applications.yaml` and `config/gateway-addons/values.yaml` |
| Langfuse `NEXTAUTH_SECRET`, `SALT`, `ENCRYPTION_KEY` | `k8s/base/applications.yaml`, both the web and worker Deployments — the encryption key is a fixed `0123456789abcdef…` pattern |
| ClickHouse `langfuse` password | `k8s/base/applications.yaml` (twice, one inside a connection URL) and `k8s/base/databases.yaml` |
| MinIO root credentials | `k8s/base/object-storage.yaml` and both Langfuse Deployments |
| `PAT_HASH_KEY` | `.env` — rotating it invalidates every stored token hash; it needs a revocation plan, not a quiet change |
| `PAT_COOKIE_KEY` | `.env` — rotating it invalidates dashboard sessions |
| `WEBUI_SECRET_KEY` | `.env` — rotating it invalidates stored OAuth tokens, so `system_oauth` chat stops working until users sign in again |
| Langfuse API keys and init password | `.env` |
| `SGLANG_API_KEY` | `.env` — the upstream bearer token the gateway injects toward SGLang; also the value the SGLang container is started with |
| `EXTERNAL_API_KEY` | `.env` — same, for the `externalApi` backend |

Moving the manifest-resident values into the `airgap-runtime` Secret (which
already exists and is fed from `.env`) is the natural next step and removes
most of this table.

### Known inconsistency

The deployed `pat-gateway` client secret differs from the value in `.env`: a
client-credentials JWT minted from the `.env` secret fails JWKS validation
while the running service works. Reconcile the two before rotating anything
else, or the rotation will look like it worked and quietly break the API path.

## What a smoke test proves

`make smoke-nogpu` exercises the boundaries without touching the GPU: SSO
discovery and redirects, the closed admin surface, the Open WebUI role
configuration, the dashboard's SSO-only behaviour, a 401 on anonymous `/v1`,
PAT issue against the catalogue, and immediate revocation. Its throwaway user
is created through the port-forwarded Admin API with a password that
satisfies the realm policy from item 4, and with the realm's default
`CONFIGURE_TOTP` action cleared — the realm re-adds it to every new account,
which would otherwise block the test's direct-grant login with "Account is
not fully set up". Add the checks for anything in this document that
becomes a rule — a boundary with no test decays.
