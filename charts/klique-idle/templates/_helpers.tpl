{{- define "kic.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kic.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kic.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "kic.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kic.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kic.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Selector of the collector DaemonSet; distinct from the hub's. */}}
{{- define "kic.collectorSelectorLabels" -}}
{{ include "kic.selectorLabels" . }}
app.kubernetes.io/component: collector
{{- end -}}

{{- define "kic.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kic.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kic.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/*
Secret holding the token, or "" when there is none. With the hub and no token
given, the chart generates one, so the hub never accepts anonymous reports or
serves /v1/nodes to any pod in the cluster.
*/}}
{{- define "kic.secretName" -}}
{{- if .Values.existingSecret.name -}}
{{- .Values.existingSecret.name -}}
{{- else if or .Values.token (and .Values.hub.enabled (not .Values.endpoint)) -}}
{{- include "kic.fullname" . -}}
{{- end -}}
{{- end -}}

{{/* The token to store: the given one, the one already stored (upgrades), or a new random one. */}}
{{- define "kic.tokenValue" -}}
{{- if .Values.token -}}
{{- .Values.token -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kic.fullname" .) -}}
{{- if and $existing $existing.data (index $existing.data "token") -}}
{{- index $existing.data "token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kic.secretKey" -}}
{{- if .Values.existingSecret.name -}}{{ .Values.existingSecret.key }}{{- else -}}token{{- end -}}
{{- end -}}

{{- define "kic.hubSelectorLabels" -}}
{{ include "kic.selectorLabels" . }}
app.kubernetes.io/component: hub
{{- end -}}

{{/* Where collectors send reports: `endpoint`, else the hub, else the pod logs. */}}
{{- define "kic.endpoint" -}}
{{- if .Values.endpoint -}}
{{- .Values.endpoint -}}
{{- else if .Values.hub.enabled -}}
{{- printf "http://%s-hub.%s.svc:8080/v1/reports" (include "kic.fullname" .) .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{/* Secret holding the Klique Cloud API key, or "" when forwarding is off. */}}
{{- define "kic.cloudSecretName" -}}
{{- if .Values.cloud.existingSecret.name -}}
{{- .Values.cloud.existingSecret.name -}}
{{- else if .Values.cloud.apiKey -}}
{{- printf "%s-cloud" (include "kic.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "kic.cloudSecretKey" -}}
{{- if .Values.cloud.existingSecret.name -}}{{ .Values.cloud.existingSecret.key }}{{- else -}}api-key{{- end -}}
{{- end -}}

{{/*
RuntimeClass for NVIDIA driver access. "auto" picks "nvidia" if that
RuntimeClass exists in the cluster (lookup is empty under `helm template`).
*/}}
{{- define "kic.runtimeClassName" -}}
{{- $rc := .Values.nvidia.runtimeClassName -}}
{{- if ne .Values.collector.gpu "nvml" -}}
{{- else if eq $rc "auto" -}}
{{- if (lookup "node.k8s.io/v1" "RuntimeClass" "" "nvidia") -}}nvidia{{- end -}}
{{- else -}}
{{- $rc -}}
{{- end -}}
{{- end -}}

{{/* labels map as "k1=v1,k2=v2", sorted by key. */}}
{{- define "kic.labelList" -}}
{{- $out := list -}}
{{- range $k := keys . | sortAlpha -}}
{{- $out = append $out (printf "%s=%s" $k (get $ $k)) -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}
