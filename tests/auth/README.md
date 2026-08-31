# Auth tests

`scripts/smoke-test` verifies OIDC discovery and the Open WebUI redirect to
Keycloak, and asserts that Open WebUI uses `system_oauth` for its Envoy model
connection. It also asserts the `ai-user`/`ai-admin` Keycloak-to-Open-WebUI
role mapping and the non-login pending bootstrap record that prevents the first
SSO user from being elevated. `scripts/inference-smoke-test` creates Keycloak
users through the Admin API, obtains OIDC tokens, verifies Envoy's JWT
enforcement and subject forwarding, then proves the per-user LLM API limit.

Open WebUI logout requires the client attribute `post.logout.redirect.uris`
to allow exactly `<PUBLIC_BASE_URL>/auth`. Realm JSON files cover fresh
imports; existing Keycloak databases need `make provision-pat-oidc` (also
called by `make helm-up`) during deployment. Updating only the realm ConfigMap
does not update an already imported client.

After deployment, log in to Open WebUI and log out. Keycloak must accept the
logout request and return to `<PUBLIC_BASE_URL>/auth` without an
`Invalid redirect uri` error. Login callback restrictions must remain unchanged.
