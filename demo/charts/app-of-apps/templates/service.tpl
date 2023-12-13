{{- define "service" -}}
{{ $appName := .child -}}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ .root.env }}-service-{{ $appName }}
spec:
  destination:
    namespace: argocd
    server: https://kubernetes.default.svc
  project: default
  source:
    repoURL: https://github.com/1debit/mani-diffy.git
    path: charts/service
    helm:
      version: v3
      parameters:
      - name: env
        value: {{ .root.env }}
      valueFiles:
        - ../../overrides/service/{{ $appName }}/base.yaml
        - ../../overrides/service/{{ .child }}/{{ .root.env }}.yaml
  syncPolicy:
    automated: {}
{{ end }}