package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// application returns an Argo application that renders one of the charts in
// testdata/walker/charts with the given values.
func application(t *testing.T, name, chart, values string) string {
	t.Helper()
	app := v1alpha1.Application{
		TypeMeta:   metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ApplicationSpec{
			Destination: v1alpha1.ApplicationDestination{Namespace: "default"},
			Source: &v1alpha1.ApplicationSource{
				Path: "charts/" + chart,
				Helm: &v1alpha1.ApplicationSourceHelm{Values: values},
			},
		},
	}
	content, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	return string(content) + "\n"
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
	return walkWithPostRenderer(t, root, output, parallelism, "")
}

func walkWithPostRenderer(t *testing.T, root, output string, parallelism int, postRenderer string) error {
	t.Helper()
	return walkWith(t, root, output, parallelism, postRenderer, false)
}

func walkWith(t *testing.T, root, output string, parallelism int, postRenderer string, maskRandomValues bool) error {
	t.Helper()
	_, err := walkCounting(t, root, output, parallelism, postRenderer, maskRandomValues)
	return err
}

func walkCounting(t *testing.T, root, output string, parallelism int, postRenderer string, maskRandomValues bool) (Stats, error) {
	t.Helper()
	if err := os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	hashes, err := NewJSONHashStore(filepath.Join(output, "hashes.json"), HashStrategyReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	w := newWalker("", "", "-ignore", parallelism, maskRandomValues)
	if postRenderer != "" {
		w.PostRender = PostRender(postRenderer)
	}
	return w.Walk([]Root{{Input: root, Output: output, Hashes: hashes}}, InfiniteDepth)
}

func isRendered(output, name string) bool {
	_, err := os.Stat(filepath.Join(output, name, "manifest.yaml"))
	return err == nil
}

// postRenderer writes a post-renderer script and returns its path. The script
// gets the rendered application's output directory as "$1" and can append to
// a log at "$RENDER_LOG", whose path is returned too.
func postRenderer(t *testing.T, script string) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "post-render")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(dir, "render.log")
	t.Setenv("RENDER_LOG", log)
	return path, log
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
		apps.WriteString(application(t, fmt.Sprintf("app-%d", i), "app", ""))
	}
	writeFile(t, filepath.Join(root, "apps.yaml"), apps.String())
	script, log := postRenderer(t, `echo start >> "$RENDER_LOG"
sleep 0.3
echo end >> "$RENDER_LOG"
`)

	if err := walkWithPostRenderer(t, root, output, 2, script); err != nil {
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
		t.Errorf("Expected at most 2 renders at once, got %d", maxRunning)
	}
}

func TestWalkRendersChildApplications(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "root.yaml"), application(t, "parent", "apps", `
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
	writeFile(t, filepath.Join(root, "a.yaml"), application(t, "parent-a", "apps", shared))
	writeFile(t, filepath.Join(root, "b.yaml"), application(t, "parent-b", "apps", shared))
	script, log := postRenderer(t, `basename "$1" >> "$RENDER_LOG"
`)

	if err := walkWithPostRenderer(t, root, output, 4, script); err != nil {
		t.Fatal(err)
	}

	if !isRendered(output, "shared") {
		t.Error("Expected shared to be rendered")
	}
	renders := 0
	for _, name := range readLines(t, log) {
		if name == "shared" {
			renders++
		}
	}
	if renders != 1 {
		t.Errorf("Expected shared to be rendered once, got %d renders", renders)
	}
}

func TestWalkPrunesUnvisitedApplications(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application(t, "current", "app", ""))
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
	writeFile(t, filepath.Join(root, "a.yaml"), application(t, "shared", "app", ""))
	writeFile(t, filepath.Join(root, "b.yaml"), application(t, "shared", "app", "name: other"))

	err := walk(t, root, output, 2)

	if err == nil || !strings.Contains(err.Error(), "application shared is defined more than once with different specs") {
		t.Errorf("Expected conflicting duplicate error, got: %v", err)
	}
}

func TestWalkStopsRenderingAfterError(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application(t, "bad", "broken", "")+application(t, "after", "app", ""))

	err := walk(t, root, output, 1)

	if err == nil || !strings.Contains(err.Error(), "application bad: ") || !strings.Contains(err.Error(), "broken chart") {
		t.Errorf("Expected render error naming bad, got: %v", err)
	}
	for _, name := range []string{"bad", "after"} {
		if _, err := os.Stat(filepath.Join(output, name)); !os.IsNotExist(err) {
			t.Errorf("Expected %s not to be rendered, got: %v", name, err)
		}
	}
}

func TestWalkRendersApplicationWhoseOutputIsMissing(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application(t, "app", "app", ""))
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
	writeFile(t, filepath.Join(root, "apps.yaml"), application(t, "slow", "app", "")+application(t, "bad", "broken", ""))
	script, _ := postRenderer(t, `[ "$(basename "$1")" = slow ] && sleep 1
touch "$1/post-rendered"
`)

	err := walkWithPostRenderer(t, root, output, 2, script)

	if err == nil || !strings.Contains(err.Error(), "broken chart") {
		t.Errorf("Expected render error for bad, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "slow", "post-rendered")); err != nil {
		t.Errorf("Expected slow to finish rendering: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "bad")); !os.IsNotExist(err) {
		t.Errorf("Expected bad not to be rendered, got: %v", err)
	}
}

func TestWalkRendersAgainWhenMaskingRandomValuesIsTurnedOn(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application(t, "app", "app", ""))
	script, log := postRenderer(t, `basename "$1" >> "$RENDER_LOG"
`)
	if err := walkWith(t, root, output, 2, script, false); err != nil {
		t.Fatal(err)
	}

	if err := walkWith(t, root, output, 2, script, true); err != nil {
		t.Fatal(err)
	}

	if renders := readLines(t, log); len(renders) != 2 {
		t.Errorf("Expected app to be rendered again, got renders %q", renders)
	}
}

// walkRoots walks several roots, each with its own output and hash store, the
// way a config file with several roots does.
func walkRoots(t *testing.T, parallelism int, postRenderer string, roots ...Root) error {
	t.Helper()
	_, err := walkRootsReporting(t, parallelism, postRenderer, false, roots...)
	return err
}

// progressReport is a report of a root's progress.
type progressReport struct {
	i        int
	name     string
	progress RootProgress
}

// walkRootsReporting walks several roots like walkRoots, and returns the
// reports of their progress, in order.
func walkRootsReporting(t *testing.T, parallelism int, postRenderer string, maskRandomValues bool, roots ...Root) ([]progressReport, error) {
	t.Helper()
	for i := range roots {
		if err := os.MkdirAll(roots[i].Output, 0755); err != nil {
			t.Fatal(err)
		}
		hashes, err := NewJSONHashStore(filepath.Join(roots[i].Output, "hashes.json"), HashStrategyReadWrite)
		if err != nil {
			t.Fatal(err)
		}
		roots[i].Hashes = hashes
	}
	w := newWalker("", "", "-ignore", parallelism, maskRandomValues)
	if postRenderer != "" {
		w.PostRender = PostRender(postRenderer)
	}
	var reports []progressReport
	w.Progress = func(i int, root Root, progress RootProgress) {
		reports = append(reports, progressReport{i: i, name: root.Name, progress: progress})
	}
	_, err := w.Walk(roots, InfiniteDepth)
	return reports, err
}

func TestWalkRendersEachRootIntoItsOwnOutput(t *testing.T) {
	newWorkspace(t)
	writeFile(t, filepath.Join("bootstrap-a", "apps.yaml"), application(t, "app", "app", "name: from-a"))
	writeFile(t, filepath.Join("bootstrap-b", "apps.yaml"), application(t, "app", "app", "name: from-b"))
	writeFile(t, filepath.Join("output-a", "stale", "manifest.yaml"), "kind: ConfigMap\n")

	err := walkRoots(t, 2, "", Root{Input: "bootstrap-a", Output: "output-a"}, Root{Input: "bootstrap-b", Output: "output-b"})

	if err != nil {
		t.Fatal(err)
	}
	for output, name := range map[string]string{"output-a": "from-a", "output-b": "from-b"} {
		manifest, err := os.ReadFile(filepath.Join(output, "app", "manifest.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(manifest), "name: "+name) {
			t.Errorf("Expected %s to have the app rendered from its own root, got:\n%s", output, manifest)
		}
		if _, err := os.Stat(filepath.Join(output, "hashes.json")); err != nil {
			t.Errorf("Expected %s to have its own hashes: %v", output, err)
		}
	}
	if _, err := os.Stat(filepath.Join("output-a", "stale")); !os.IsNotExist(err) {
		t.Errorf("Expected stale output to be pruned, got: %v", err)
	}
}

func TestWalkRendersRootsInOneWorkerPool(t *testing.T) {
	newWorkspace(t)
	writeFile(t, filepath.Join("bootstrap-a", "apps.yaml"), application(t, "app-a", "app", ""))
	writeFile(t, filepath.Join("bootstrap-b", "apps.yaml"), application(t, "app-b", "app", ""))
	script, log := postRenderer(t, `echo start >> "$RENDER_LOG"
sleep 0.3
echo end >> "$RENDER_LOG"
`)

	err := walkRoots(t, 2, script, Root{Input: "bootstrap-a", Output: "output-a"}, Root{Input: "bootstrap-b", Output: "output-b"})

	if err != nil {
		t.Fatal(err)
	}
	if events := readLines(t, log); !slices.Equal(events, []string{"start", "start", "end", "end"}) {
		t.Errorf("Expected the apps of both roots to render at the same time, got %q", events)
	}
}

func TestWalkMasksRandomValuesAndRendersChildApplications(t *testing.T) {
	root, output := newWorkspace(t)
	writeFile(t, filepath.Join(root, "apps.yaml"), application(t, "parent", "random-apps", `
apps:
- name: child
  chart: app
`))
	script, log := postRenderer(t, `basename "$1" >> "$RENDER_LOG"
`)

	if err := walkWith(t, root, output, 2, script, true); err != nil {
		t.Fatal(err)
	}

	manifest, err := os.ReadFile(filepath.Join(output, "parent", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `token: "CONSCIOUSLY_NOT_RENDERED"`) {
		t.Errorf("Expected the random value to be masked, got:\n%s", manifest)
	}
	if !isRendered(output, "child") {
		t.Error("Expected child to be rendered")
	}

	if err := walkWith(t, root, output, 2, script, true); err != nil {
		t.Fatal(err)
	}

	renders := readLines(t, log)
	slices.Sort(renders)
	if want := []string{"child", "parent"}; !slices.Equal(renders, want) {
		t.Errorf("Expected each application to be rendered once over both walks, got renders %q", renders)
	}
}

func TestWalkCountsWhatItDidWithApplications(t *testing.T) {
	root, output := newWorkspace(t)
	kustomize := v1alpha1.Application{
		TypeMeta:   metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{Name: "kustomized"},
		Spec: v1alpha1.ApplicationSpec{
			Source: &v1alpha1.ApplicationSource{Path: "charts/app", Kustomize: &v1alpha1.ApplicationSourceKustomize{}},
		},
	}
	content, err := json.Marshal(kustomize)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "apps.yaml"),
		application(t, "plain", "app", "")+application(t, "random", "random-apps", "")+string(content)+"\n")

	first, err := walkCounting(t, root, output, 2, "", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := walkCounting(t, root, output, 2, "", true)
	if err != nil {
		t.Fatal(err)
	}

	if want := (Stats{Rendered: 2, RenderedTwice: 1, Unsupported: 1}); first != want {
		t.Errorf("Expected the first walk to count %+v, got %+v", want, first)
	}
	if want := (Stats{Unchanged: 2, Unsupported: 1}); second != want {
		t.Errorf("Expected the second walk to count %+v, got %+v", want, second)
	}
}

func TestWalkReportsTheProgressOfEachRoot(t *testing.T) {
	newWorkspace(t)
	writeFile(t, filepath.Join("bootstrap-a", "apps.yaml"), application(t, "app", "app", ""))
	writeFile(t, filepath.Join("bootstrap-b", "apps.yaml"), application(t, "parent", "apps", `
apps:
- name: child
  chart: app
`))
	if err := os.MkdirAll("bootstrap-empty", 0755); err != nil {
		t.Fatal(err)
	}

	reports, err := walkRootsReporting(t, 2, "", false,
		Root{Name: "a", Input: "bootstrap-a", Output: "output-a"},
		Root{Name: "b", Input: "bootstrap-b", Output: "output-b"},
		Root{Name: "empty", Input: "bootstrap-empty", Output: "output-empty"},
	)

	if err != nil {
		t.Fatal(err)
	}
	if len(reports) < 3 {
		t.Fatalf("Expected a report for every root before any application is processed, got %v", reports)
	}
	if want := []progressReport{
		{i: 0, name: "a", progress: RootProgress{Outstanding: 1}},
		{i: 1, name: "b", progress: RootProgress{Outstanding: 1}},
		{i: 2, name: "empty", progress: RootProgress{Finished: true}},
	}; !slices.Equal(reports[:3], want) {
		t.Errorf("Expected the first reports to be %v, got %v", want, reports[:3])
	}
	final := map[string]RootProgress{}
	for _, r := range reports {
		if final[r.name].Finished {
			t.Errorf("Expected no reports for %s after it finished, got %v", r.name, r.progress)
		}
		final[r.name] = r.progress
	}
	for name, want := range map[string]Stats{"a": {Rendered: 1}, "b": {Rendered: 2}, "empty": {}} {
		if got := final[name]; !got.Finished || got.Outstanding != 0 || got.Stats != want {
			t.Errorf("Expected %s to finish with %+v, got %+v", name, want, got)
		}
	}
}

func TestWalkNamesTheRootInErrors(t *testing.T) {
	newWorkspace(t)
	writeFile(t, filepath.Join("bootstrap-a", "apps.yaml"), application(t, "app", "app", ""))
	writeFile(t, filepath.Join("bootstrap-b", "apps.yaml"), application(t, "bad", "broken", ""))

	err := walkRoots(t, 2, "", Root{Name: "a", Input: "bootstrap-a", Output: "output-a"}, Root{Name: "b", Input: "bootstrap-b", Output: "output-b"})

	if err == nil || !strings.HasPrefix(err.Error(), "b: application bad: ") || !strings.Contains(err.Error(), "broken chart") {
		t.Errorf("Expected the error to name the root and the application, got: %v", err)
	}
}

func TestWalkReportsTheRootThatFailed(t *testing.T) {
	newWorkspace(t)
	writeFile(t, filepath.Join("bootstrap-a", "apps.yaml"), application(t, "app", "app", ""))
	writeFile(t, filepath.Join("bootstrap-b", "apps.yaml"), application(t, "bad", "broken", ""))

	reports, err := walkRootsReporting(t, 2, "", false, Root{Name: "a", Input: "bootstrap-a", Output: "output-a"}, Root{Name: "b", Input: "bootstrap-b", Output: "output-b"})

	if err == nil {
		t.Fatal("Expected the walk to fail")
	}
	failed := map[string]bool{}
	for _, r := range reports {
		failed[r.name] = failed[r.name] || r.progress.Failed
	}
	if !failed["b"] || failed["a"] {
		t.Errorf("Expected only b to be reported failed, got %v", failed)
	}
}
