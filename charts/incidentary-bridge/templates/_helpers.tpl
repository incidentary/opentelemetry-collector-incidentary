{{/*
Return the chart name, truncated and trimmed to DNS-1123 limits.
*/}}
{{- define "incidentary-bridge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Return the fully qualified release name.
*/}}
{{- define "incidentary-bridge.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart.yaml name-version label.
*/}}
{{- define "incidentary-bridge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard Kubernetes recommended labels.
*/}}
{{- define "incidentary-bridge.labels" -}}
helm.sh/chart: {{ include "incidentary-bridge.chart" . }}
{{ include "incidentary-bridge.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: incidentary
app.kubernetes.io/component: bridge
{{- end -}}

{{/*
Selector labels — stable across upgrades.
*/}}
{{- define "incidentary-bridge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "incidentary-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Service account name.
*/}}
{{- define "incidentary-bridge.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "incidentary-bridge.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
API-key Secret name. Prefers an existing secret when set.
*/}}
{{- define "incidentary-bridge.apiKeySecretName" -}}
{{- if .Values.existingSecretName -}}
{{- .Values.existingSecretName -}}
{{- else -}}
{{- printf "%s-apikey" (include "incidentary-bridge.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
API-key Secret key. Defaults to "token".
*/}}
{{- define "incidentary-bridge.apiKeySecretKey" -}}
{{- default "token" .Values.existingSecretKey -}}
{{- end -}}

{{/*
ConfigMap name for the rendered collector config.
*/}}
{{- define "incidentary-bridge.configMapName" -}}
{{- printf "%s-config" (include "incidentary-bridge.fullname" .) -}}
{{- end -}}
