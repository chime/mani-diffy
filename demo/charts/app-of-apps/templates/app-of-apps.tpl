{{- define "app-of-apps" -}}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ .child }}
spec:
  destination:
    namespace: argocd
    server: https://kubernetes.default.svc
  project: default
  source:
    repoURL: https://github.com/1debit/mani-diffy.git
    targetRevision: HEAD

    path: charts/app-of-apps
    helm:
      parameters:
        - name: renderBaseDir
          value: {{ .root.renderBaseDir }}
{{- with .root.parameters }}
{{ . | toYaml | indent 8 }}
{{- end }}
{{ .childParams.parameters | toYaml | indent 8 }}
      valueFiles:
        - ../../overrides/app-of-apps/{{ .child }}.yaml

  syncPolicy:
    automated: {}
{{ end }}