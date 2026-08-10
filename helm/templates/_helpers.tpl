{{/*
Common labels.
*/}}
{{- define "schematic-operator.labels" -}}
app.kubernetes.io/name: schematic-operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
imagePullSecrets block.
*/}}
{{- define "schematic-operator.imagePullSecrets" -}}
{{- if .Values.operator.imagePullSecret -}}
imagePullSecrets:
- name: {{ .Values.operator.imagePullSecret }}
{{- end }}
{{- end }}
