#!/usr/bin/env sh
set -eu

# Uses internal service credentials without printing them. Requires the remote
# profile, its running model, and kubectl access to PAT + Langfuse ClickHouse.
namespace=${K8S_NAMESPACE:-airgap-ai-stack}
clickhouse_pod=${LANGFUSE_CLICKHOUSE_POD:-chi-langfuse-clickhouse-main-0-0-0}

for stream in false true; do
  trace_id=$(openssl rand -hex 16)
  parent_id=$(openssl rand -hex 8)
  kubectl -n "$namespace" exec -i deployment/pat-service -- sh -s -- "$trace_id" "$parent_id" "$stream" <<'REMOTE'
set -eu
trace_id=$1
parent_id=$2
stream=$3
token_response=$(wget -qO- --post-data="grant_type=client_credentials&client_id=$AI_GATEWAY_CLIENT_ID&client_secret=$AI_GATEWAY_CLIENT_SECRET" "$OIDC_INTERNAL_ISSUER/protocol/openid-connect/token")
token=$(printf '%s' "$token_response" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
test -n "$token"
body=$(printf '{"model":"qwen-3.8-27b","messages":[{"role":"user","content":"Telemetry smoke %s. Reply with exactly TRACE_OK."}],"max_tokens":64,"temperature":0,"stream":%s,"chat_template_kwargs":{"enable_thinking":false}}' "$trace_id" "$stream")
response=$(wget -qO- -T 120 \
  --header="Authorization: Bearer $token" \
  --header='Content-Type: application/json' \
  --header="traceparent: 00-$trace_id-$parent_id-01" \
  --header='X-User-Name: genai-telemetry-smoke' \
  --header="X-User-Id: telemetry-$trace_id" \
  --header='X-Pat-Token-Id: telemetry-smoke' \
  --post-data="$body" "$AI_GATEWAY_URL/v1/chat/completions")
if [ "$stream" = true ]; then
  printf '%s' "$response" | grep -q 'data: \[DONE\]'
else
  printf '%s' "$response" | grep -q '"choices"'
fi
REMOTE
  # Assert the mapped, persisted Langfuse fields, not just Collector receipt.
  attempt=0
  while :; do
    count=$(kubectl -n "$namespace" exec "$clickhouse_pod" -- clickhouse-client --query "SELECT count() FROM events_full WHERE trace_id = '$trace_id' AND type = 'GENERATION' AND provided_model_name = 'qwen-3.8-27b' AND position(input, '$trace_id') > 0 AND length(output) > 0 AND user_id = 'genai-telemetry-smoke'")
    if [ "$count" -gt 0 ]; then
      transport_count=$(kubectl -n "$namespace" exec "$clickhouse_pod" -- clickhouse-client --query "SELECT count() FROM events_full WHERE trace_id = '$trace_id' AND service_name = 'ai-gateway-private'")
      if [ "$transport_count" -ne 0 ]; then
        printf 'FAIL trace=%s: Envoy HTTP spans reached Langfuse\n' "$trace_id" >&2
        exit 1
      fi
      printf 'PASS stream=%s trace=%s: Langfuse generation has input/output/model/user\n' "$stream" "$trace_id"
      break
    fi
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 12 ]; then
      printf 'FAIL stream=%s trace=%s: missing mapped Langfuse fields\n' "$stream" "$trace_id" >&2
      exit 1
    fi
    sleep 5
  done
done

# ADR 0011: the AI Gateway must map X-Session-Key to Langfuse session.id, so
# two requests carrying the same key group into one session and a request
# with a different key does not. This exercises the Gateway/Collector/
# Langfuse config change only (same direct-to-gateway shape as the loop
# above); PAT's own chain-hash derivation and its Agent-Session-Id stripping
# are covered by pat-service's Go unit tests, not reachable from here since
# this script authenticates as the Gateway client, not through PAT's proxy.
group_marker=$(openssl rand -hex 8)
session_key="sess-$(openssl rand -hex 16)"
other_session_key="sess-$(openssl rand -hex 16)"

session_smoke_request() {
  turn=$1
  session_header=$2
  trace_id=$(openssl rand -hex 16)
  parent_id=$(openssl rand -hex 8)
  kubectl -n "$namespace" exec -i deployment/pat-service -- sh -s -- "$trace_id" "$parent_id" "$group_marker" "$turn" "$session_header" <<'REMOTE2' >&2
set -eu
trace_id=$1
parent_id=$2
marker=$3
turn=$4
session_header=$5
token_response=$(wget -qO- --post-data="grant_type=client_credentials&client_id=$AI_GATEWAY_CLIENT_ID&client_secret=$AI_GATEWAY_CLIENT_SECRET" "$OIDC_INTERNAL_ISSUER/protocol/openid-connect/token")
token=$(printf '%s' "$token_response" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
test -n "$token"
body=$(printf '{"model":"qwen-3.8-27b","messages":[{"role":"user","content":"Session smoke %s turn %s. Reply with exactly TRACE_OK."}],"max_tokens":16,"temperature":0,"stream":false,"chat_template_kwargs":{"enable_thinking":false}}' "$marker" "$turn")
response=$(wget -qO- -T 120 \
  --header="Authorization: Bearer $token" \
  --header='Content-Type: application/json' \
  --header="traceparent: 00-$trace_id-$parent_id-01" \
  --header='X-User-Name: genai-telemetry-smoke' \
  --header="X-Session-Key: $session_header" \
  --post-data="$body" "$AI_GATEWAY_URL/v1/chat/completions")
printf '%s' "$response" | grep -q '"choices"'
REMOTE2
  printf '%s' "$trace_id"
}

trace_a=$(session_smoke_request one "$session_key")
trace_b=$(session_smoke_request two "$session_key")
trace_c=$(session_smoke_request three "$other_session_key")

session_id_for() {
  kubectl -n "$namespace" exec "$clickhouse_pod" -- clickhouse-client --query \
    "SELECT session_id FROM events_full WHERE trace_id = '$1' AND type = 'GENERATION' LIMIT 1"
}

attempt=0
while :; do
  session_a=$(session_id_for "$trace_a")
  session_b=$(session_id_for "$trace_b")
  session_c=$(session_id_for "$trace_c")
  if [ -n "$session_a" ] && [ -n "$session_b" ] && [ -n "$session_c" ]; then
    break
  fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 12 ]; then
    printf 'FAIL session grouping: missing session.id for trace_a=%s(%s) trace_b=%s(%s) trace_c=%s(%s)\n' \
      "$trace_a" "$session_a" "$trace_b" "$session_b" "$trace_c" "$session_c" >&2
    exit 1
  fi
  sleep 5
done

if [ "$session_a" != "$session_key" ] || [ "$session_b" != "$session_key" ]; then
  printf 'FAIL session grouping: shared X-Session-Key did not map to session.id (got %s, %s, want %s)\n' "$session_a" "$session_b" "$session_key" >&2
  exit 1
fi
if [ "$session_c" != "$other_session_key" ]; then
  printf 'FAIL session grouping: distinct X-Session-Key did not get its own session.id (got %s, want %s)\n' "$session_c" "$other_session_key" >&2
  exit 1
fi
printf 'PASS session grouping: shared key %s groups trace_a/trace_b, distinct key %s isolates trace_c\n' "$session_key" "$other_session_key"
