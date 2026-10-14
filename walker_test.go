package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// application returns an Argo application that renders one of the charts in
// testdata/walker/charts with the given values.
func application(name, chart, values string) string {
	if values == "" {
		values = "{}"
	}
	indented := "        " + strings.ReplaceAll(strings.TrimSpace(values), "\n", "\n        ")
	return fmt.Sprintf(`---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
spec:
  destination:
    namespace: default
  source:
    path: charts/%s
    helm:
      values: |
%s
`, name, chart, indented)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// newWorkspace changes into a temporary copy of testdata/walker and returns
// the bootstrap and output directories to walk.
func newWorkspace(t *testing.T) (root, output string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/walker")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return "bootstrap", "output"
}

func walk(t *testing.T, root, output string, parallelism int) error {
	t.Helper()
	if err := os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	hashes, err := NewJSONHashStore(filepath.Join(output, "hashes.json"), HashStrategyReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	return newWalker("", "", "-ignore", parallelism).Walk(root, output, InfiniteDepth, hashes)
}

func isRendered(output, name string) bool {
	_, err := os.Stat(filepath.Join(output, name, "manifest.yaml"))
	return err == nil
}

// wrapHelm puts a helm executable first on PATH that runs script. The script
// can run the real helm as "$REAL_HELM".
func wrapHelm(t *testing.T, script string) {
	t.Helper()
	realHelm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REAL_HELM", realHelm)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// logHelm records every helm run in a log, one line per event, and returns
// the log's path.
func logHelm(t *testing.T, script string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "helm.log")
	t.Setenv("HELM_LOG", log)
	wrapHelm(t, script)
	return log
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(content))
}

func TestWalkRendersInParallelUpToLimit(t *testing.T) {
	root, output := newWorkspace(t)
	var apps strings.Builder
	for i := range 6 {
		apps.WriteString(application(fmt.Sprintf("app-%d", i), "app", ""))
	}
	writeFile(t, filepath.Join(root, "apps.yaml"), apps.String())
	log := logHelm(t, `echo start >> "$HELM_LOG"
sleep 0.3
"$REAL_HELM" "$@"; status=$?
echo end >> "$HELM_LOG"
exit $status
`)

	if err := walk(t, root, output, 2); err != nil {
		t.Fatal(err)
	}

	for i := range 6 {
		if name := fmt.Sprintf("app-%d", i); !isRendered(output, name) {
			t.Errorf("Expected %s to be rendered", name)
		}
	}
	running, maxRunning := 0, 0
	for _, event := range readLines(t, log) {
		if event == "start" {
			running++
			maxRunning = max(maxRunning, running)
		} else {
			running--
		}
	}
	if maxRunning != 2 {
		t.Errorf("Expected at most 2 helm processes at once, got %d", maxRunning)
	}
}

func TestWalkRendersChildApplications(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "root.yaml"), application("parent", "apps", `
apps:
- name: child-a
  chart: apps
  values:
    apps:
    - name: grandchild
      chart: app
- name: child-b
  chart: app
`))

	if err := walk(t, root, output, 4); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"parent", "child-a", "child-b", "grandchild"} {
		if !isRendered(output, name) {
			t.Errorf("Expected %s to be rendered", name)
		}
	}
}

func TestWalkRendersDuplicateApplicationOnce(t *testing.T) {
	root, output := newWorkspace(t)
	shared := `
apps:
- name: shared
  chart: app
`
	writeFile(t, filepath.Join(root, "a.yaml"), application("parent-a", "apps", shared))
	writeFile(t, filepath.Join(root, "b.yaml"), application("parent-b", "apps", shared))
	log := logHelm(t, `basename "$(pwd -P)" >> "$HELM_LOG"
exec "$REAL_HELM" "$@"
`)

	if err := walk(t, root, output, 4); err != nil {
		t.Fatal(err)
	}

	if !isRendered(output, "shared") {
		t.Error("Expected shared to be rendered")
	}
	renders := 0
	for _, chart := range readLines(t, log) {
		if chart == "app" {
			renders++
		}
	}
	if renders != 1 {
		t.Errorf("Expected shared to be rendered once, got %d renders", renders)
	}
}

func TestWalkPrunesUnvisitedApplications(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application("current", "app", ""))
	writeFile(t, filepath.Join(output, "stale", "manifest.yaml"), "kind: ConfigMap\n")

	if err := walk(t, root, output, 2); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(output, "stale")); !os.IsNotExist(err) {
		t.Errorf("Expected stale output to be pruned, got: %v", err)
	}
	if !isRendered(output, "current") {
		t.Error("Expected current to be rendered")
	}
}

func TestWalkRejectsConflictingDuplicateApplications(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "a.yaml"), application("shared", "app", ""))
	writeFile(t, filepath.Join(root, "b.yaml"), application("shared", "app", "name: other"))

	err := walk(t, root, output, 2)

	if err == nil || !strings.Contains(err.Error(), "application shared is defined more than once with different specs") {
		t.Errorf("Expected conflicting duplicate error, got: %v", err)
	}
}

func TestWalkStopsRenderingAfterError(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application("bad", "broken", "")+application("after", "app", ""))

	err := walk(t, root, output, 1)

	if err == nil || !strings.Contains(err.Error(), "broken chart") {
		t.Errorf("Expected render error for bad, got: %v", err)
	}
	for _, name := range []string{"bad", "after"} {
		if _, err := os.Stat(filepath.Join(output, name)); !os.IsNotExist(err) {
			t.Errorf("Expected %s not to be rendered, got: %v", name, err)
		}
	}
}

func TestWalkRendersApplicationWhoseOutputIsMissing(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application("app", "app", ""))
	if err := walk(t, root, output, 2); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(output, "app")); err != nil {
		t.Fatal(err)
	}

	if err := walk(t, root, output, 2); err != nil {
		t.Fatal(err)
	}

	if !isRendered(output, "app") {
		t.Error("Expected app to be rendered again")
	}
}

func TestWalkWaitsForRendersInProgressAfterError(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application("slow", "slow", "")+application("bad", "broken", ""))
	wrapHelm(t, `[ "$(basename "$(pwd -P)")" = slow ] && sleep 1
exec "$REAL_HELM" "$@"
`)

	err := walk(t, root, output, 2)

	if err == nil || !strings.Contains(err.Error(), "broken chart") {
		t.Errorf("Expected render error for bad, got: %v", err)
	}
	if !isRendered(output, "slow") {
		t.Error("Expected slow to be rendered")
	}
	if _, err := os.Stat(filepath.Join(output, "bad")); !os.IsNotExist(err) {
		t.Errorf("Expected bad not to be rendered, got: %v", err)
	}
}
