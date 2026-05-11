{{/*
Common labels.
*/}}
{{- define "cavalier-operator.labels" -}}
app.kubernetes.io/name: cavalier-operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
imagePullSecrets block.
*/}}
{{- define "cavalier-operator.imagePullSecrets" -}}
{{- if .Values.operator.imagePullSecret -}}
imagePullSecrets:
- name: {{ .Values.operator.imagePullSecret }}
{{- end }}
{{- end }}
