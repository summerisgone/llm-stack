{{- define "airgap-stack.fullname" -}}
{{- default .Chart.Name .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* backendRef for inference.defaultModel.engine: its AIServiceBackend and
its own model name. Fails on an unknown or disabled engine. */}}
{{- define "airgap-stack.defaultModelBackendRef" -}}
{{- $inf := .Values.inference -}}
{{- $engine := $inf.defaultModel.engine -}}
{{- $backends := dict "pool" "llmd-qwen-test-openai" "ninfer" "ninfer-openai" "strata" "strata-openai" "externalApi" "external-api-openai" "llamacpp" "llamacpp-openai" -}}
{{- $backend := get $backends $engine | required (printf "inference.defaultModel.engine: unknown engine %q" $engine) -}}
{{- $model := $inf.modelName -}}
{{- if ne $engine "pool" -}}
{{- $cfg := get $inf $engine -}}
{{- if not $cfg.enabled -}}
{{- fail (printf "inference.defaultModel.engine is %s, but inference.%s.enabled is false" $engine $engine) -}}
{{- end -}}
{{- $model = $cfg.modelName -}}
{{- end -}}
- name: {{ $backend }}
  modelNameOverride: {{ $model }}
{{- end -}}