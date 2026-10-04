# QoS tests

`scripts/inference-smoke-test` verifies the currently configured global limit:
three requests per minute for each distinct Keycloak `sub` on the LLM route.
It creates two users and verifies that exhausting one user's bucket does not
limit the other user.

## Open WebUI fair-share regression (ADR 0022 stage 3)

Open WebUI chat now reaches the gateway through pat-service's private entry
point instead of the fallback band `0`.
`TestPrivateEntryMatchesPATFairShareHeaders` in
`pat-service/cmd/pat-service/private_test.go` sends the same chat body
through the PAT entry point and the private one and requires identical
`X-Llm-D-Inference-Fairness-Id` (the Keycloak subject),
`X-Llm-D-Inference-Objective`, `X-User-Id` and `X-User-Name` at the gateway,
and one ledger owner for both. `TestPrivateEntryDerivesIdentityFromToken`
proves client-sent identity, band and session headers are replaced.

On the cluster, after `make helm-up`: run `make inference-bypass-test`, then
repeat the two-user contention run from `baseline.md` with one user in Open
WebUI and one on a PAT. Both must appear as separate tenants in the EPP
flow-control metrics, and the Open WebUI requests must no longer be counted
in band `0`.
