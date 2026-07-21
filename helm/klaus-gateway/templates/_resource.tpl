{{/* vim: set filetype=mustache: */}}
{{- define "resource.default.name" -}}
{{- $name := .Values.fullnameOverride | default (.Chart.Name | replace "." "-") | trunc 63 -}}
{{- regexReplaceAll "[^a-zA-Z0-9]+$" $name "" -}}
{{- end -}}

{{- define "resource.default.namespace" -}}
{{ .Release.Namespace }}
{{- end -}}
