package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
)

func application(name string) string {
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
    helm: {}
`, name, name)
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

// fakeHelm renders each Application into the manifest listed in children,
// keyed by Application name, and records how renders overlap.
type fakeHelm struct {
	children map[string]string
	delay    time.Duration
	fail     string

	mu            sync.Mutex
	rendered      []string
	inFlight      int
	maxConcurrent int
}

func (f *fakeHelm) Render(app *v1alpha1.Application, output string) error {
	f.mu.Lock()
	f.rendered = append(f.rendered, app.Name)
	f.inFlight++
	if f.inFlight > f.maxConcurrent {
		f.maxConcurrent = f.inFlight
	}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	time.Sleep(f.delay)

	if app.Name == f.fail {
		return errors.New("render failed for " + app.Name)
	}

	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	manifest := f.children[app.Name]
	if manifest == "" {
		manifest = "kind: ConfigMap\n"
	}
	return os.WriteFile(filepath.Join(output, "manifest.yaml"), []byte(manifest), 0644)
}

func newTestWalker(helm *fakeHelm, parallelism int) *Walker {
	return &Walker{
		HelmTemplate: helm.Render,
		GenerateHash: func(app *v1alpha1.Application) (string, error) {
			return "hash-" + app.Name, nil
		},
		Parallelism:  parallelism,
		ignoreSuffix: "-ignore",
	}
}

func walk(t *testing.T, w *Walker, root, output string) error {
	t.Helper()
	if err := os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	hashes, err := NewJSONHashStore(filepath.Join(output, "hashes.json"), HashStrategyReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	return w.Walk(root, output, InfiniteDepth, hashes)
}

func TestWalkRendersInParallelUpToLimit(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	var apps strings.Builder
	for i := range 6 {
		apps.WriteString(application(fmt.Sprintf("app-%d", i)))
	}
	writeFile(t, filepath.Join(root, "apps.yaml"), apps.String())

	helm := &fakeHelm{delay: 50 * time.Millisecond}
	if err := walk(t, newTestWalker(helm, 2), root, output); err != nil {
		t.Fatal(err)
	}

	if len(helm.rendered) != 6 {
		t.Errorf("Expected 6 renders, got %d: %v", len(helm.rendered), helm.rendered)
	}
	if helm.maxConcurrent != 2 {
		t.Errorf("Expected 2 concurrent renders, got %d", helm.maxConcurrent)
	}
}

func TestWalkRendersChildApplications(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(root, "root.yaml"), application("parent"))

	helm := &fakeHelm{children: map[string]string{
		"parent":  application("child-a") + application("child-b"),
		"child-a": application("grandchild"),
	}}
	if err := walk(t, newTestWalker(helm, 4), root, output); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"parent", "child-a", "child-b", "grandchild"} {
		if _, err := os.Stat(filepath.Join(output, name, "manifest.yaml")); err != nil {
			t.Errorf("Expected %s to be rendered: %v", name, err)
		}
	}
	if len(helm.rendered) != 4 {
		t.Errorf("Expected 4 renders, got %v", helm.rendered)
	}
}

func TestWalkRendersDuplicateApplicationOnce(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(root, "a.yaml"), application("parent-a"))
	writeFile(t, filepath.Join(root, "b.yaml"), application("parent-b"))

	helm := &fakeHelm{children: map[string]string{
		"parent-a": application("shared"),
		"parent-b": application("shared"),
	}}
	if err := walk(t, newTestWalker(helm, 4), root, output); err != nil {
		t.Fatal(err)
	}

	renders := 0
	for _, name := range helm.rendered {
		if name == "shared" {
			renders++
		}
	}
	if renders != 1 {
		t.Errorf("Expected shared to be rendered once, got %d renders: %v", renders, helm.rendered)
	}
}

func TestWalkReturnsRenderError(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(root, "apps.yaml"), application("good")+application("bad"))

	helm := &fakeHelm{fail: "bad"}
	err := walk(t, newTestWalker(helm, 2), root, output)

	if err == nil || !strings.Contains(err.Error(), "render failed for bad") {
		t.Errorf("Expected render error for bad, got: %v", err)
	}
}

func TestWalkPrunesUnvisitedApplications(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(root, "apps.yaml"), application("current"))
	writeFile(t, filepath.Join(output, "stale", "manifest.yaml"), "kind: ConfigMap\n")

	if err := walk(t, newTestWalker(&fakeHelm{}, 2), root, output); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(output, "stale")); !os.IsNotExist(err) {
		t.Errorf("Expected stale output to be pruned, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "current", "manifest.yaml")); err != nil {
		t.Errorf("Expected current to be rendered: %v", err)
	}
}

func TestWalkRejectsConflictingDuplicateApplications(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	conflicting := strings.Replace(application("shared"), "path: charts/shared", "path: charts/other", 1)
	writeFile(t, filepath.Join(root, "a.yaml"), application("shared"))
	writeFile(t, filepath.Join(root, "b.yaml"), conflicting)

	err := walk(t, newTestWalker(&fakeHelm{}, 2), root, output)

	if err == nil || !strings.Contains(err.Error(), "application shared is defined more than once with different specs") {
		t.Errorf("Expected conflicting duplicate error, got: %v", err)
	}
}

func TestWalkStopsRenderingAfterError(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(root, "apps.yaml"), application("bad")+application("after"))

	helm := &fakeHelm{fail: "bad"}
	if err := walk(t, newTestWalker(helm, 1), root, output); err == nil {
		t.Fatal("Expected render error for bad")
	}

	if len(helm.rendered) != 1 || helm.rendered[0] != "bad" {
		t.Errorf("Expected only bad to be rendered, got %v", helm.rendered)
	}
}

func TestWalkRendersApplicationWhoseOutputIsMissing(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "bootstrap")
	output := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(root, "apps.yaml"), application("app"))
	if err := walk(t, newTestWalker(&fakeHelm{}, 2), root, output); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(output, "app")); err != nil {
		t.Fatal(err)
	}

	helm := &fakeHelm{}
	if err := walk(t, newTestWalker(helm, 2), root, output); err != nil {
		t.Fatal(err)
	}

	if len(helm.rendered) != 1 || helm.rendered[0] != "app" {
		t.Errorf("Expected app to be rendered again, got %v", helm.rendered)
	}
}
