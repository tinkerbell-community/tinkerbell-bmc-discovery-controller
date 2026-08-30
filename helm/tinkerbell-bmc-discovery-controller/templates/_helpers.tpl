{{- define "bmc-discovery.name" -}}
{{ .Chart.Name }}
{{- end }}

{{- define "bmc-discovery.labels" -}}
app.kubernetes.io/name: {{ include "bmc-discovery.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "bmc-discovery.selectorLabels" -}}
app.kubernetes.io/name: {{ include "bmc-discovery.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
