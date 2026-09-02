{{- define "teardown.name" -}}
{{ .Chart.Name }}
{{- end }}

{{- define "teardown.labels" -}}
app.kubernetes.io/name: {{ include "teardown.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "teardown.selectorLabels" -}}
app.kubernetes.io/name: {{ include "teardown.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
