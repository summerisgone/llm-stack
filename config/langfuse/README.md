# Langfuse

Langfuse v4 web and worker run from `k8s/base/applications.yaml`; ClickHouse,
PostgreSQL, Valkey and MinIO are operator-managed. This directory is for
future OIDC, content-capture and retention policy configuration.

Never put production secrets here. Langfuse's `NEXTAUTH_SECRET`, `SALT` and
`ENCRYPTION_KEY` are currently literals in the manifest — see
[docs/security](../../docs/security/README.md).
