{{- define "strait.labels" -}}
app.kubernetes.io/name: strait
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{/*
The Strait image. image.digest pins it when set. A released chart carries the digest that its release pushed, in the
annotation strait.tako.id/image-digest, which counts while image.tag and image.digest are both empty (RFC0007).
*/}}
{{- define "strait.image" -}}
{{- $digest := .Values.image.digest -}}
{{- if and (not $digest) (not .Values.image.tag) -}}
{{- $digest = index (.Chart.Annotations | default dict) "strait.tako.id/image-digest" -}}
{{- end -}}
{{- if and $digest (not (regexMatch "^sha256:[0-9a-f]{64}$" $digest)) -}}
{{- fail (printf "image digest %q is not sha256: and 64 lowercase hex digits" $digest) -}}
{{- end -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}{{ with $digest }}@{{ . }}{{ end }}
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
