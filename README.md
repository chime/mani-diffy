# mani-diffy

![Tests](https://github.com/1debit/mani-diffy/actions/workflows/tests.yaml/badge.svg)

This program walks a hierarchy of Argo CD Application templates, renders Kubernetes manifests from the input templates, and posts the rendered files back for the user to review and validate. 

It is designed to be called from a CI job within a pull request, enabling the author to update templates and see the resulting manifests directly within the pull request before the changes are applied to the Kubernetes cluster. 

The rendered manifests are kept within the repository, making diffs between revisions easy to parse, dramatically improving safety when updating complex application templates.

---
## How it works:
1. A user makes their desired change to the application's templates (charts, overrides, etc) and submits a PR with the change.
2. A CI job executes `mani-diffy`, rendering all manifests affected by the change.
3. Any updated manifests are submitted back to the same PR as a new commit.
4. The author and any reviewers will be able to review the diff between the new changes and the previous version of the manifests.

## Post Renderers

`mani-diffy` also supports something called a "post renderer". This is a command that will be called immediately after an Application is rendered. This can be used to run linting, or alter the output of the generated manifest.

```
mani-diffy -post-renderer="bin/post-render" -output=.zz-auto-generated
```

The command will be called with the output directory as the first argument (e.g. `.zz-auto-generated/<application name>`)

---

## Pre-requisites 

This is for a new user that is looking to use mani-diffy on a new repo.

In order to make use of mani-diffy on the repo that holds all of your ArgoCD applications the pre-requisites are:

- You have a "root" Application
- All of your charts and Application manifests live in the same repo.

`mani-diffy` itself makes no assumptions about how the repo is structured, as long as it can successfully render the charts it encounters while walking the Application tree.

However, you may find it useful to organize your repo similarly to the demo app, with 3 key directories : 

1. a "root" or "bootstrap" directory that holds all the ArgoCD applications manifests.
2. a "charts" directory that contains all the helm charts needed for the ArgoCD applications.
3. a "rendered" or "generated" directory, where all rendered charts will be committed.

You can see an example of that in the [`demo`](demo/README.md) directory.

# FAQ

Q: Is ArgoCD using the rendered manifests in `.zz.auto-generated` ?

A: No, ArgoCD renders the charts itself. There is no expected discrepancy between the manifest files rendered by mani-diffy and by ArgoCD as long as they are using the same version of Helm.




