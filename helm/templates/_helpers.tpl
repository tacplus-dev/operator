{{/*
Common labels.
*/}}
{{- define "schematic-operator.labels" -}}
app.kubernetes.io/name: schematic-operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
The namespace the operator runs in: tac-<deployment> when operator.deployment
is set, otherwise operator.namespace.
*/}}
{{- define "schematic-operator.namespace" -}}
{{- if .Values.operator.deployment -}}
tac-{{ .Values.operator.deployment }}
{{- else -}}
{{ .Values.operator.namespace }}
{{- end -}}
{{- end }}

{{/*
The operator's ServiceAccount. A deployment's is named operator: deploymentproxy's
Role in tac-<deployment>, and the RoleBinding it makes in each tenant
namespace, bind exactly that name.
*/}}
{{- define "schematic-operator.serviceAccountName" -}}
{{- if .Values.operator.deployment -}}
operator
{{- else -}}
schematic-operator
{{- end -}}
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
