{{/*
Expand the name of the chart.
*/}}
{{- define "mt-mcp-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "mt-mcp-proxy.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version as used by the chart label.
*/}}
{{- define "mt-mcp-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "mt-mcp-proxy.labels" -}}
helm.sh/chart: {{ include "mt-mcp-proxy.chart" . }}
{{ include "mt-mcp-proxy.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "mt-mcp-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mt-mcp-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The env var name a tenant's credential is mapped into (TENANT_<ID>_CREDENTIAL,
uppercased, non-alphanumerics → underscore). Used to keep secrets out of the
rendered ConfigMap: the config references ${VAR}, resolved from this env var.
Call with the tenant id string, e.g. {{ include "mt-mcp-proxy.credEnv" .id }}.
*/}}
{{- define "mt-mcp-proxy.credEnv" -}}
{{- $raw := printf "TENANT_%s_CREDENTIAL" . | upper -}}
{{- regexReplaceAll "[^A-Z0-9_]" $raw "_" -}}
{{- end }}

{{/*
The env var name a backend's own credential is mapped into
(BACKEND_<NAME>_CREDENTIAL, uppercased, non-alphanumerics → underscore). Used
to keep secrets out of the rendered ConfigMap: the config references ${VAR},
resolved from this env var. Call with the backend name string, e.g.
{{ include "mt-mcp-proxy.backendCredEnv" .name }}.
*/}}
{{- define "mt-mcp-proxy.backendCredEnv" -}}
{{- $raw := printf "BACKEND_%s_CREDENTIAL" . | upper -}}
{{- regexReplaceAll "[^A-Z0-9_]" $raw "_" -}}
{{- end }}
