# 0001 — Public routing is by path on one canonical origin

Status: accepted

## Context

The stack originally published each service on its own hostname
(`ai.localhost`, `sso.ai.localhost`, `tokens.ai.localhost`, …). At the
reference site the only available public entry point is a single origin on a
TLS-terminating reverse proxy, and adding a DNS name and certificate per
service was not on offer.

Host-based routing also made the OIDC redirect chain fragile: an upstream
proxy that rewrote the `Host` header changed the callback URL a relying party
computed for itself.

## Decision

Public traffic is matched by path only, on one canonical origin:

    /sso       Keycloak
    /platform  PAT dashboard
    /v1        OpenAI-compatible API
    /          Open WebUI

No route matches on `Host`. Every relying party is given an explicit absolute
callback URL (`OPENID_REDIRECT_URI`, `OIDC_REDIRECT_URL`) so an upstream proxy
cannot change it. Keycloak runs under the `/sso` relative path
(`KC_HTTP_RELATIVE_PATH`) in every profile, so a token's `iss` claim matches
what the realm returns.

The PAT dashboard is mounted under a prefix that the Gateway strips, so the
service re-adds it to every `Location` header and inlines it into the
dashboard HTML for client-side fetches (`URL_PREFIX`).

## Consequences

- One certificate and one DNS name per site.
- The local development profile keeps host-based `*.localhost` routing,
  because on a developer machine it is simpler. The two profiles therefore
  cannot share route definitions — see ADR 0002.
- Anything that must not be public cannot simply be "a different hostname"; it
  needs no route at all. Grafana and Langfuse are exposed through operator
  NodePorts instead.
- Path prefixes are now a security control: everything under `/sso` is
  published unless a longer, more specific rule rejects it. See
  [docs/security](../security/README.md).
