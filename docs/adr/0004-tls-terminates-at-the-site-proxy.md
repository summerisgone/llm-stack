# 0004 — TLS terminates at the site proxy

Status: accepted

## Context

The reference site already runs a TLS-terminating reverse proxy with a valid
certificate for the public origin. Terminating again inside the cluster would
mean managing a second certificate and a second trust store for no gain on a
single host.

## Decision

The site proxy terminates TLS and forwards plaintext HTTP to the Envoy edge
listener. Traffic between the proxy, the host, and every workload inside the
cluster is plaintext. Keycloak runs with `KC_HTTP_ENABLED=true` and
`KC_PROXY_HEADERS=xforwarded`, and relies on the proxy to send
`X-Forwarded-Proto` so it builds correct absolute URLs.

## Consequences

- The proxy and the cluster must stay on the same trusted host or network
  segment. This decision does not survive splitting them across a network you
  do not control.
- The proxy sending a correct `X-Forwarded-Proto` is load-bearing: without it
  Keycloak issues `http://` URLs into an `https://` flow.
- `sslRequired` on the realm must be chosen with this in mind; Keycloak sees
  HTTP regardless of what the browser used.
