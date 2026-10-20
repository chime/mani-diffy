package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"path/filepath"
	"strings"
	"sync"

	"github.com/chime/mani-diffy/pkg/helm"
	"github.com/chime/mani-diffy/pkg/kustomize"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const InfiniteDepth = -1

const skipAnnotation = "mani-diffy.chime.com/skip"

// shouldSkipRender checks if an Application should be skipped based on:
// 1. The application name suffix (ignoreSuffix)
// 2. The mani-diffy.chime.com/skip annotation set to "true"
func shouldSkipRender(app *v1alpha1.Application, ignoreSuffix string) bool {
	// Check if the application name has the ignore suffix
	if strings.HasSuffix(app.ObjectMeta.Name, ignoreSuffix) {
		return true
	}

	// Check if the skip annotation is set to "true"
	if app.ObjectMeta.Annotations != nil {
		if value, ok := app.ObjectMeta.Annotations[skipAnnotation]; ok && value == "true" {
			return true
		}
	}

	return false
}

// Renderer is a function that can render an Argo application.
type Renderer func(*v1alpha1.Application, string) error

// PostRenderer is a function that can be called after an Argo application is rendered.
type PostRenderer func(string) error

// Walker walks a directory tree looking for Argo applications and renders them
// concurrently.
type Walker struct {
	// HelmTemplate is a function that can render an Argo application using Helm
	HelmTemplate Renderer

	// CopySource is a function that can copy an Argo application to a directory
	CopySource Renderer

	// PostRender is a function that can be called after an Argo application is rendered.
	PostRender PostRenderer

	// GenerateHash is used to generate a cache key for an Argo application
	GenerateHash func(*v1alpha1.Application) (string, error)

	// Parallelism is the maximum number of Argo applications processed at once.
	Parallelism int

	// Progress, when set, is called with the index of a root in the roots
	// given to Walk and its progress: for every root before any Argo
	// application is processed, then for a root whenever one of its
	// applications is processed.
	Progress func(i int, root Root, progress RootProgress)

	ignoreSuffix string
}

// Root is a directory tree of Argo applications to render into an output
// directory, with the hashes of the applications last rendered there.
type Root struct {
	// Name identifies the root in what is reported about it.
	Name   string
	Input  string
	Output string
	Hashes HashStore
}

// Stats count what a walk did with the Argo applications it found.
type Stats struct {
	// Rendered is how many applications were rendered.
	Rendered int
	// Unchanged is how many applications weren't rendered, since they didn't
	// change since the last run.
	Unchanged int
	// Unsupported is how many applications weren't rendered, since they use
	// Kustomize, which isn't supported.
	Unsupported int
}

// Walk walks directory trees looking for Argo applications and renders them.
// The applications of all roots are rendered by the same workers.
func (w *Walker) Walk(roots []Root, maxDepth int) (Stats, error) {
	visited := make(map[string]*v1alpha1.Application)

	stats, err := w.walk(roots, maxDepth, visited)
	if err != nil {
		return stats, err
	}

	for _, root := range roots {
		if err := root.Hashes.Save(); err != nil {
			return stats, err
		}

		if maxDepth == InfiniteDepth {
			if err := pruneUnvisited(visited, root.Output); err != nil {
				return stats, err
			}
		}
	}

	return stats, nil
}

func pruneUnvisited(visited map[string]*v1alpha1.Application, outputPath string) error {
	files, err := os.ReadDir(outputPath)
	if err != nil {
		return err
	}

	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		path := filepath.Join(outputPath, f.Name())
		if _, ok := visited[path]; ok {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}

	return nil
}

// job is an Argo application for a worker to process.
type job struct {
	app        *v1alpha1.Application
	root       *Root
	depth      int
	storedHash string
}

// result is the outcome of processing a job.
type result struct {
	job      job
	rendered bool
	// unchanged is whether the application wasn't rendered, since it didn't
	// change since the last run.
	unchanged bool
	// unsupported is whether the application wasn't rendered, since it uses
	// Kustomize.
	unsupported bool
	hash        string
	children    []*v1alpha1.Application
	err         error
}

// walk renders the Argo applications found in the roots' inputs and,
// recursively, the Argo applications they render. Up to Parallelism workers
// process applications while this goroutine hands out jobs and collects
// results. Only this goroutine reads or writes visited and the roots' hashes.
func (w *Walker) walk(roots []Root, maxDepth int, visited map[string]*v1alpha1.Application) (Stats, error) {
	var stats Stats
	progress := make([]RootProgress, len(roots))
	index := make(map[*Root]int, len(roots))
	for i := range roots {
		index[&roots[i]] = i
	}
	report := func(i int) {
		if progress[i].Outstanding == 0 {
			progress[i].Finished = true
		}
		if w.Progress != nil {
			w.Progress(i, roots[i], progress[i])
		}
	}
	rootApps := make([][]*v1alpha1.Application, len(roots))
	for i, root := range roots {
		apps, err := w.findApplications(root.Input)
		if err != nil {
			return stats, err
		}
		rootApps[i] = apps
	}

	jobs := make(chan job)
	results := make(chan result)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(jobs)
	for range max(w.Parallelism, 1) {
		workers.Go(func() {
			for j := range jobs {
				results <- w.process(j, maxDepth)
			}
		})
	}

	var pending []job
	enqueue := func(apps []*v1alpha1.Application, root *Root, depth int) error {
		for _, app := range apps {
			firstVisit, err := visit(visited, root.Output, app)
			if err != nil {
				return err
			}
			if !firstVisit {
				continue
			}

			hash, err := root.Hashes.Get(app.ObjectMeta.Name)
			if err != nil {
				return err
			}
			pending = append(pending, job{app: app, root: root, depth: depth, storedHash: hash})
			progress[index[root]].Outstanding++
		}
		return nil
	}

	// After the first error no new jobs are handed out, but the jobs in
	// progress are waited for.
	var firstErr error
	for i := range roots {
		if firstErr = enqueue(rootApps[i], &roots[i], 0); firstErr != nil {
			break
		}
	}
	if firstErr == nil {
		for i := range roots {
			report(i)
		}
	}
	busy := 0
	for busy > 0 || (firstErr == nil && len(pending) > 0) {
		// Sending on a nil channel blocks forever, which disables that case.
		var next job
		var send chan<- job
		if firstErr == nil && len(pending) > 0 {
			next, send = pending[0], jobs
		}

		select {
		case send <- next:
			pending = pending[1:]
			busy++
		case r := <-results:
			busy--
			if r.err != nil {
				r.err = fmt.Errorf("application %s: %w", r.job.app.ObjectMeta.Name, r.err)
			}
			i := index[r.job.root]
			if r.err == nil {
				stats.count(r)
				progress[i].Stats.count(r)
			}
			if r.err == nil && r.rendered {
				r.err = r.job.root.Hashes.Add(r.job.app.ObjectMeta.Name, r.hash)
			}
			if r.err == nil {
				r.err = enqueue(r.children, r.job.root, r.job.depth+1)
			}
			if firstErr == nil {
				firstErr = r.err
			}
			progress[i].Outstanding--
			if firstErr == nil {
				report(i)
			}
		}
	}

	return stats, firstErr
}

// RootProgress is how far a walk is with the Argo applications of a root.
type RootProgress struct {
	// Stats count what the walk did with the applications processed so far.
	Stats
	// Outstanding is how many of the applications found so far are still to
	// be processed, or in progress.
	Outstanding int
	// Finished is whether all of the applications are processed.
	Finished bool
}

// count counts what processing an application did with it.
func (s *Stats) count(r result) {
	switch {
	case r.rendered:
		s.Rendered++
	case r.unchanged:
		s.Unchanged++
	case r.unsupported:
		s.Unsupported++
	}
}

// visit records that an Argo application was found. It returns false when the
// same application was already found elsewhere in the tree, and an error when
// a different application with the same name was.
func visit(visited map[string]*v1alpha1.Application, outputPath string, app *v1alpha1.Application) (bool, error) {
	path := filepath.Join(outputPath, app.ObjectMeta.Name)

	if previous, ok := visited[path]; ok {
		if previous.String() != app.String() {
			return false, fmt.Errorf("application %s is defined more than once with different specs", app.ObjectMeta.Name)
		}
		return false, nil
	}
	visited[path] = app
	return true, nil
}

// findApplications returns the Argo applications in the YAML files of a
// directory that should be rendered.
func (w *Walker) findApplications(inputPath string) ([]*v1alpha1.Application, error) {
	log.Println("Dropping into", inputPath)

	fi, err := os.ReadDir(inputPath)
	if err != nil {
		return nil, err
	}

	var apps []*v1alpha1.Application
	for _, file := range fi {
		if !strings.Contains(file.Name(), ".yaml") {
			continue
		}

		crds, err := helm.Read(filepath.Join(inputPath, file.Name()))
		if err != nil {
			return nil, err
		}
		for _, crd := range crds {
			if crd.Kind != "Application" {
				continue
			}

			if shouldSkipRender(crd, w.ignoreSuffix) {
				continue
			}

			apps = append(apps, crd)
		}
	}
	return apps, nil
}

// process renders an Argo application if it changed since the last run, then
// finds the Argo applications it renders.
func (w *Walker) process(j job, maxDepth int) result {
	r := result{job: j}
	path := filepath.Join(j.root.Output, j.app.ObjectMeta.Name)

	// COMPARE HASHES HERE. STEP INTO RENDER IF NO MATCH
	hash, err := w.GenerateHash(j.app)
	if err != nil {
		if errors.Is(err, kustomize.ErrNotSupported) {
			r.unsupported = true
		} else {
			r.err = err
		}
		return r
	}

	emptyManifest, err := helm.EmptyManifest(filepath.Join(path, "manifest.yaml"))
	if err != nil {
		r.err = err
		return r
	}

	needsRender := hash != j.storedHash || emptyManifest

	// It is possible for the hashes to match, but the path
	// no longer exists due to an interleaving merge that
	// inadvertently removed it. In this case, we need to
	// re-render.
	if !needsRender {
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				r.err = err
				return r
			}
			needsRender = true
		}
	}

	if needsRender {
		log.Printf("No match detected. Render: %s\n", j.app.ObjectMeta.Name)
		if err := w.Render(j.app, path); err != nil {
			if errors.Is(err, kustomize.ErrNotSupported) {
				r.unsupported = true
			} else {
				r.err = err
			}
			return r
		}
		r.rendered = true
		r.hash = hash
	} else {
		r.unchanged = true
	}

	// If we've reached the max depth, stop walking
	if maxDepth != InfiniteDepth && j.depth+1 > maxDepth {
		return r
	}

	r.children, r.err = w.findApplications(path)
	return r
}

func (w *Walker) Render(application *v1alpha1.Application, output string) error {
	log.Println("Render", application.ObjectMeta.Name)

	var render Renderer

	// Figure out which renderer to use
	switch {
	case application.Spec.Source.Helm != nil:
		render = w.HelmTemplate
	case application.Spec.Source.Kustomize != nil:
		log.Println("WARNING: kustomize not supported")
		return kustomize.ErrNotSupported
	default:
		render = w.CopySource
	}

	// Make sure the directory is empty before rendering.
	if err := os.RemoveAll(output); err != nil {
		return err
	}

	// Render
	if err := render(application, output); err != nil {
		return err
	}

	// Call the post renderer to do any post processing
	if w.PostRender != nil {
		if err := w.PostRender(output); err != nil {
			return fmt.Errorf("post render failed: %w", err)
		}
	}

	return nil
}
