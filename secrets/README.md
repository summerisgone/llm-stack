# Secrets

Nothing generated belongs in this directory under version control:
`.gitignore` excludes `*.txt`, `*.pem` and `*.key` here.

Runtime credentials come from `.env`, which is gitignored and rendered into
the `airgap-runtime` Kubernetes Secret — by `make up` / `make stack-up`
directly, and into the Helm release through `scripts/helm-env-values`.

A number of credentials still live in clear text in tracked manifests rather
than in `.env`. The full inventory, what each one breaks when rotated, and the
hardening work needed before a public deployment are in
[docs/security](../docs/security/README.md). Read it before exposing anything;
the checked-in `*-demo-only` values are for a disposable local stack only.
