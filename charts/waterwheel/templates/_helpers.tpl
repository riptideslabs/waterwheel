{{- define "waterwheel.labels" -}}
app.kubernetes.io/name: waterwheel
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{- define "waterwheel.image" -}}
{{- $digest := get (.root.Values.image.digests | default dict) (trimPrefix "waterwheel-" .name) -}}
{{ .root.Values.image.registry }}{{ .name }}:{{ .root.Values.image.tag | default .root.Chart.AppVersion }}{{ with $digest }}@{{ . }}{{ end }}
{{- end }}
