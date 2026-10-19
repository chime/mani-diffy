{{- define "cli-parity.name" -}}
{{ .Release.Name }}-{{ .Chart.Name }}
{{- end -}}
