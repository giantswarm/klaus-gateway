{{/* vim: set filetype=mustache: */}}
{{- define "name" -}}
{{- $name := .Values.fullnameOverride | default .Chart.Name | trunc 63 -}}
{{- regexReplaceAll "[^a-zA-Z0-9]+$" $name "" -}}
{{- end -}}

{{- define "chart" -}}
{{/* Long dev versions truncate at 63 chars, which can land on a "." or "-";
     a label value must end alphanumeric, so strip every trailing symbol. */}}
{{- $chart := printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 -}}
{{- regexReplaceAll "[^a-zA-Z0-9]+$" $chart "" -}}
{{- end -}}

{{- define "labels.common" -}}
{{ include "labels.selector" . }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
giantswarm.io/managed-by: {{ include "name" . | quote }}
giantswarm.io/service-type: {{ .Values.serviceType | quote }}
helm.sh/chart: {{ include "chart" . | quote }}
{{- end -}}

{{- define "labels.selector" -}}
app.kubernetes.io/name: {{ include "name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "image.tag" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end -}}

{{/* obo.secretStore renders "true" when the OBO link store is the Kubernetes
     Secret backend (obo.store: secret); empty otherwise, so it works in `if`. */}}
{{- define "obo.secretStore" -}}
{{- if and .Values.obo.enabled (eq .Values.obo.store "secret") -}}true{{- end -}}
{{- end -}}

{{/* obo.boltVolume renders "true" when the bolt link store lives on a
     PersistentVolumeClaim mounted into the pod: the bolt backend on a durable
     volume, or the Secret backend still reading that volume for the import.
     A mounted ReadWriteOnce claim is what forces the Recreate strategy. */}}
{{- define "obo.boltVolume" -}}
{{- if and .Values.obo.enabled .Values.obo.storePath .Values.obo.persistence.enabled -}}true{{- end -}}
{{- end -}}

{{/* obo.storeVolume renders "true" when a volume is mounted at the bolt path at
     all: always for the bolt backend (PVC or emptyDir), only the PVC for the
     Secret backend (there is nothing to import from an emptyDir). */}}
{{- define "obo.storeVolume" -}}
{{- if and .Values.obo.enabled .Values.obo.storePath (or .Values.obo.persistence.enabled (not (include "obo.secretStore" .))) -}}true{{- end -}}
{{- end -}}

{{/* obo.linksSecretName is the Secret the Secret backend keeps the links in. */}}
{{- define "obo.linksSecretName" -}}
{{- .Values.obo.storeSecretName | default (printf "%s-obo-links" (include "resource.default.name" .)) -}}
{{- end -}}
