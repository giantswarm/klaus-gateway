{{/* vim: set filetype=mustache: */}}
{{- define "resource.default.name" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | replace "." "-" | trunc 47 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "resource.default.namespace" -}}
{{ .Release.Namespace }}
{{- end -}}
