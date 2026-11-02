{{- define "waterwheel.labels" -}}
app.kubernetes.io/name: waterwheel
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{- define "waterwheel.image" -}}
{{ .root.Values.image.registry }}{{ .name }}:{{ .root.Values.image.tag }}
{{- end }}
