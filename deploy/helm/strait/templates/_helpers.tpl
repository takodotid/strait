{{- define "strait.labels" -}}
app.kubernetes.io/name: strait
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "strait.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "strait.wireguardSecret" -}}
{{- $wg := .Values.site.spec.wireguard | default dict -}}
{{- $ref := $wg.privateKeySecretRef | default dict -}}
{{- $ref.name | default "" -}}
{{- end -}}

{{- define "strait.restricted" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}
