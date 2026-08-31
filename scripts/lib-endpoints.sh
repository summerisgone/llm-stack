# Shared endpoint derivation for the smoke tests. Source this; do not execute.
#
# The stack has two routing profiles and the smoke tests must work against
# both, so no script may hardcode a hostname or a path layout:
#
#   path mode  (STACK_BASE_URL set)   remote profile. One public origin, routes
#                                     selected by path only. Keycloak lives
#                                     under /sso, the PAT dashboard under
#                                     /platform, the OpenAI API under /v1 and
#                                     Open WebUI is the catch-all. Grafana and
#                                     Langfuse have no public route: set
#                                     GRAFANA_BASE_URL / LANGFUSE_BASE_URL to
#                                     their operator NodePort URLs to include
#                                     them, otherwise their checks are skipped.
#
#   host mode  (STACK_BASE_URL unset) local-mac profile. Host-based
#                                     *.localhost routes on the development
#                                     edge :8080. STACK_TUNNEL_PORT redirects
#                                     those hostnames at a local tunnel port.
#
# Keycloak always runs under the /sso relative path (KC_HTTP_RELATIVE_PATH in
# k8s/base/applications.yaml), in both profiles.

stack_realm=${STACK_REALM:-ai-stack}
stack_base_url=${STACK_BASE_URL:-}

if [ -n "$stack_base_url" ]; then
  stack_mode=path
  stack_base_url=${stack_base_url%/}
  sso_base=$stack_base_url/sso
  issuer=$sso_base/realms/$stack_realm
  admin_issuer=$sso_base/realms/master
  admin_api=$sso_base/admin/realms/$stack_realm
  webui=$stack_base_url
  # Must track pat-service's own URL_PREFIX env var (helm/airgap-stack
  # resources.yaml) -- the dashboard's redirects are built as
  # urlPrefix+"/auth/login" server-side, so this is what the server actually
  # sends, not just where the client happens to send its first request.
  dashboard_prefix=/platform
  dashboard=$stack_base_url$dashboard_prefix
  api=$stack_base_url/v1
  grafana=${GRAFANA_BASE_URL:-}
  langfuse=${LANGFUSE_BASE_URL:-}
else
  stack_mode=host
  sso_base=http://sso.ai.localhost:8080/sso
  issuer=$sso_base/realms/$stack_realm
  admin_issuer=$sso_base/realms/master
  admin_api=$sso_base/admin/realms/$stack_realm
  webui=http://ai.localhost:8080
  # tokens.ai.localhost is pat-service's own hostname in this profile (see
  # k8s/overlays/local-mac/routes.yaml), so pat-service runs with no
  # URL_PREFIX and its redirects come back unprefixed.
  dashboard_prefix=
  dashboard=http://tokens.ai.localhost:8080$dashboard_prefix
  api=http://api.ai.localhost:8080/v1
  grafana=${GRAFANA_BASE_URL:-http://grafana.localhost:8080}
  langfuse=${LANGFUSE_BASE_URL:-http://langfuse.localhost:8080}
fi

# In host mode STACK_TUNNEL_PORT points the development hostnames at a local
# SSH tunnel. In path mode the origin is already absolute, so the tunnel port
# is irrelevant and must not rewrite anything.
stack_curl() {
  if [ "$stack_mode" = host ] && [ -n "${STACK_TUNNEL_PORT:-}" ]; then
    curl \
      --connect-to "sso.ai.localhost:8080:127.0.0.1:$STACK_TUNNEL_PORT" \
      --connect-to "ai.localhost:8080:127.0.0.1:$STACK_TUNNEL_PORT" \
      --connect-to "grafana.localhost:8080:127.0.0.1:$STACK_TUNNEL_PORT" \
      --connect-to "langfuse.localhost:8080:127.0.0.1:$STACK_TUNNEL_PORT" \
      --connect-to "tokens.ai.localhost:8080:127.0.0.1:$STACK_TUNNEL_PORT" \
      --connect-to "api.ai.localhost:8080:127.0.0.1:$STACK_TUNNEL_PORT" \
      "$@"
  else
    curl "$@"
  fi
}

# Percent-encode a value for comparison against a query string.
urlencode() {
  STACK_URLENCODE_VALUE="$1" python3 -c '
import os, urllib.parse
print(urllib.parse.quote(os.environ["STACK_URLENCODE_VALUE"], safe=""))
'
}
