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

{{/*
The gateway's controlPort, exportPorts, importPorts and peerPort, as JSON, for the gateway's network policy (RFC0007).
With site.create true, from site.spec.gateway, and the Site CRD's defaults where it sets none: the chart reads them from
its own crds/, so they are stated once. With site.create false, from networkPolicy.gateway.ports, which must hold all
four.
*/}}
{{- define "strait.gatewayPorts" -}}
{{- $fields := list "controlPort" "exportPorts" "importPorts" "peerPort" -}}
{{- $set := .Values.networkPolicy.gateway.ports | default dict -}}
{{- $out := dict -}}
{{- if .Values.site.create -}}
{{- if $set -}}
{{- fail "networkPolicy.gateway.ports applies with site.create false only: with site.create true, the chart reads the gateway's ports from site.spec.gateway" -}}
{{- end -}}
{{- $crd := .Files.Get "crds/strait.tako.id_sites.yaml" | fromYaml -}}
{{- $defaults := dict -}}
{{- range $crd.spec.versions -}}
{{- if .storage -}}
{{- $defaults = .schema.openAPIV3Schema.properties.spec.properties.gateway.properties -}}
{{- end -}}
{{- end -}}
{{- $spec := .Values.site.spec.gateway | default dict -}}
{{- range $fields -}}
{{- if hasKey $spec . -}}
{{- $_ := set $out . (index $spec .) -}}
{{- else -}}
{{- $_ := set $out . (index $defaults . "default") -}}
{{- end -}}
{{- end -}}
{{- else -}}
{{- range $fields -}}
{{- if not (index $set .) -}}
{{- fail (printf "networkPolicy.gateway.ports.%s is required with networkPolicy.gateway.enabled and site.create false: copy it from the Site's spec.gateway, or from the CRD's default where the Site sets none" .) -}}
{{- end -}}
{{- $_ := set $out . (index $set .) -}}
{{- end -}}
{{- range list "exportPorts" "importPorts" -}}
{{- $r := index $out . -}}
{{- if or (not (kindIs "map" $r)) (not $r.first) (not $r.last) -}}
{{- fail (printf "networkPolicy.gateway.ports.%s needs first and last" .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/* One NetworkPolicy port: a port, or a range of them. */}}
{{- define "strait.policyPorts" -}}
{{- if eq (int .first) (int .last) -}}
{port: {{ int .first }}}
{{- else -}}
{port: {{ int .first }}, endPort: {{ int .last }}}
{{- end -}}
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
