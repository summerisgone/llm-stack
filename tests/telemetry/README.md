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
