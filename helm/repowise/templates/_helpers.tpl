{{- define "repowise.env" -}}
- name: REPOWISE_API_KEY
  valueFrom:
    secretKeyRef: {name: repowise, key: REPOWISE_API_KEY}
# Per-repo .repowise/wiki.db in workspace mode; the image's default points
# every process at one /data/wiki.db ("shared database mode").
- name: REPOWISE_DB_URL
  value: ""
- name: REPOWISE_PROVIDER
  value: openai
- name: REPOWISE_MODEL
  value: {{ .Values.llm.model | quote }}
# Qwen thinks by default and spends get_answer's default 1024-token budget
# on it (empty answer). "off" (enable_thinking=false) is accepted only for
# model names containing "qwen", not for `default`, so thinking stays on and
# the budget and timeout are raised instead.
- name: REPOWISE_REASONING
  value: "auto"
- name: REPOWISE_SYNTHESIS_MAX_TOKENS
  value: "8192"
- name: REPOWISE_ANSWER_TIMEOUT_S
  value: "300"
- name: OPENAI_BASE_URL
  value: {{ .Values.llm.baseURL | quote }}
- name: OPENAI_API_KEY
  valueFrom:
    secretKeyRef: {name: repowise, key: OPENAI_API_KEY}
- name: REPOWISE_EMBEDDER
  value: openai
- name: REPOWISE_EMBEDDING_MODEL
  value: {{ .Values.llm.embeddingModel | quote }}
- name: REPOWISE_EMBEDDING_DECLARED_DIMS
  value: {{ .Values.llm.embeddingDims | quote }}
- name: OPENAI_EMBEDDING_TIMEOUT
  value: {{ .Values.llm.embeddingTimeoutSeconds | quote }}
- name: REPOWISE_TELEMETRY_DISABLED
  value: "1"
- name: DO_NOT_TRACK
  value: "1"
- name: REPOWISE_SKIP_EDITOR_SETUP
  value: "1"
- name: REPOWISE_NO_SAVE_KEY
  value: "1"
- name: HOME
  value: /data/home
{{- end }}

{{- define "repowise.mounts" -}}
- name: data
  mountPath: /data
- name: scripts
  mountPath: /etc/repowise
  readOnly: true
{{- end }}

{{- define "repowise.securityContext" -}}
allowPrivilegeEscalation: false
capabilities:
  drop: ["ALL"]
{{- end }}

{{- define "repowise.publicHost" -}}
{{- $o := required "publicOrigin (REPOWISE_PUBLIC_ORIGIN in .env)" .Values.publicOrigin -}}
{{- regexReplaceAll ":[0-9]+$" (urlParse $o).host "" -}}
{{- end }}

{{- define "repowise.issuer" -}}
{{- printf "%s/sso/realms/%s" (required "stackOrigin (STACK_BASE_URL in .env)" .Values.stackOrigin | trimSuffix "/") .Values.oidc.realm -}}
{{- end }}

{{- define "repowise.jwtProvider" -}}
- name: keycloak
  issuer: {{ include "repowise.issuer" . | quote }}
  remoteJWKS:
    uri: {{ .Values.oidc.jwksURI | quote }}
    cacheDuration: 300s
  extractFrom:
    cookies: [repowise-access-token]
{{- end }}
