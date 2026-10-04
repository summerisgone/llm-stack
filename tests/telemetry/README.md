# Telemetry tests

Run `sh tests/telemetry/genai-smoke.sh` against the remote GPU profile with
its model ready. It sends a short non-streaming and streaming chat request
through the private AI Gateway, then asserts that Langfuse ClickHouse stores
a generation with input, output, model name, and the synthetic user identity.
It also asserts that Envoy HTTP spans were not exported to Langfuse.
It creates two test traces and does not exercise browser SSO or PAT issuance.
Service credentials stay inside the PAT pod and are not printed.

Requires `kubectl`, `openssl`, and access to the PAT and ClickHouse pods.
Override `K8S_NAMESPACE` or `LANGFUSE_CLICKHOUSE_POD` if necessary.

## Request ledger and admin console statistics (ADR 0022 stage 2)

`pat-service/cmd/pat-service/ledger_test.go` proves that every proxied chat
or embeddings request writes one `qos_events` row with its outcome (`ok`,
`rejected`, `client_error`, `error`, `cancelled`), usage quality and server
timestamps: received, dispatched, first and last output, finished. First and
last output count only text, reasoning or tool-call deltas, never role-only
or usage-only SSE events, including events split across reads. A client that
disconnects still leaves a `cancelled` row with `usage_quality = unknown`.

`admin_stats_test.go` checks the ADR 0022 section 6 formulas on fixed
intervals: TTFT and TPOT percentiles with sample counts, the output rate as
sum of tokens over sum of durations, active time as the union of
overlapping request intervals clipped to the range, session wall-clock span,
and that rows recorded before timing existed are reported as not
measurable rather than as zero. `topology_test.go` resolves model routes to
pools and engines from fixture objects, checks that engine inventory never
carries env values, API keys or annotations, and that k3d nodes on one
machine share one host key so their GPU is listed once.

```sh
cd pat-service
PATDB_TEST_DSN='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable' go test ./...
```

After deployment, open `/platform/admin/inference`, `/nodes` and `/routes`
as an `ai-admin` user. Panels whose source is missing show N/A and the
failing source is named at the top of the page; `/platform/admin/users/<sub>`
shows "timing recorded from" the first request served by this version.
