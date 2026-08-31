# 0003 — The AI Gateway is cluster-only; programmatic access goes through PATs

Status: accepted

## Context

The inference endpoint has to be reachable by two very different clients:
browsers that have just completed an SSO login, and scripts and agents that
have no browser at all. Handing either a long-lived shared API key would make
per-user accounting impossible and revocation a redeployment.

## Decision

The Envoy AI Gateway is backed by a `ClusterIP` Service and has no public
listener. Two workloads reach it:

- **Open WebUI** sends the signed-in user's Keycloak access token
  (`system_oauth`). Envoy validates it and derives the identity itself, so Open
  WebUI cannot assert a user by setting a header.
- **The PAT service** serves the public `/v1`. It validates the `sk-…` token,
  looks up its owner, and calls the gateway with its own client-credentials
  JWT plus the resolved identity headers. Client-supplied `X-User-*` headers
  are stripped at the edge before it ever sees them.

Tokens are stored only as HMAC hashes, are shown once at creation, and are
revocable by their owner. The per-owner rate limit is a global Envoy limit
keyed on the derived identity, so it stays correct if the edge scales out.

## Consequences

- A compromised PAT is revocable in one click and affects one user.
- `PAT_HASH_KEY` cannot be rotated casually: it invalidates every stored hash.
- The per-owner limit applies to inference calls only; `/v1/models` bypasses
  it, which is why the smoke test exercises the limit with chat requests.
- There is no per-IP limit *before* the PAT lookup, so unauthenticated traffic
  still reaches the service and its database. Tracked in
  [docs/security](../security/README.md).
