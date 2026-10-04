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

## Admin console boundary (ADR 0022 stage 1)

`pat-service/cmd/pat-service/admin_test.go` covers the `/api/admin/*`
boundary against a fake Keycloak: anonymous and expired sessions get 401;
ordinary users, cookies that merely claim `ai-admin`, foreign-realm, disabled
and deleted accounts get 403; a PAT never authorizes; mutations without the
session-bound CSRF token or from another origin get 403; a role removal
blocks the next mutation at once and reads within 60 seconds; Keycloak
failure gives 503. With Postgres it also proves that rejected mutations are
audited without trusting client identity headers, that a change and its
audit event commit together (an audit failure undoes the change), that
`admin_audit_events` rejects UPDATE, DELETE and TRUNCATE, and the audit and
user list APIs.

```sh
cd pat-service && go test ./...   # Postgres tests skip without PATDB_TEST_DSN
docker run -d --rm --name patdb-test -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17.5-alpine
PATDB_TEST_DSN='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable' go test ./...
```

The `pat-directory` client holds realm-management `view-users` only. Checked
on Keycloak 26.1.4: it lists users (service accounts excluded), reads
effective realm roles with composites expanded and the enabled flag, and
gets 403 when it tries to grant a role, disable a user, reset a password,
create a user or read the master realm.

After deployment: sign in to `/platform` as an `ai-admin` user, open
**Administration**; as an `ai-user` user, opening `/platform/admin` directly
shows "Administrator role required" and `/platform/api/admin/users` answers
403.
