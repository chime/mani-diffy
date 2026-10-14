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

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
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

	ignoreSuffix string
}

// Walk walks a directory tree looking for Argo applications and renders them
func (w *Walker) Walk(inputPath, outputPath string, maxDepth int, hashes HashStore) error {
	visited := make(map[string]*v1alpha1.Application)

	if err := w.walk(inputPath, outputPath, maxDepth, visited, hashes); err != nil {
		return err
	}

	if err := hashes.Save(); err != nil {
		return err
	}

	if maxDepth == InfiniteDepth {
		return pruneUnvisited(visited, outputPath)
	}

	return nil
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
	depth      int
	storedHash string
}

// result is the outcome of processing a job.
type result struct {
	job      job
	rendered bool
	hash     string
	children []*v1alpha1.Application
	err      error
}

// walk renders the Argo applications found in inputPath and, recursively, the
// Argo applications they render. Up to Parallelism workers process
// applications while this goroutine hands out jobs and collects results. Only
// this goroutine reads or writes visited and hashes.
func (w *Walker) walk(inputPath, outputPath string, maxDepth int, visited map[string]*v1alpha1.Application, hashes HashStore) error {
	roots, err := w.findApplications(inputPath)
	if err != nil {
		return err
	}

	jobs := make(chan job)
	results := make(chan result)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(jobs)
	for range max(w.Parallelism, 1) {
		workers.Go(func() {
			for j := range jobs {
				results <- w.process(j, outputPath, maxDepth)
			}
		})
	}

	var pending []job
	enqueue := func(apps []*v1alpha1.Application, depth int) error {
		for _, app := range apps {
			firstVisit, err := visit(visited, outputPath, app)
			if err != nil {
				return err
			}
			if !firstVisit {
				continue
			}

			hash, err := hashes.Get(app.ObjectMeta.Name)
			if err != nil {
				return err
			}
			pending = append(pending, job{app: app, depth: depth, storedHash: hash})
		}
		return nil
	}

	// After the first error no new jobs are handed out, but the jobs in
	// progress are waited for.
	firstErr := enqueue(roots, 0)
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
			if r.err == nil && r.rendered {
				r.err = hashes.Add(r.job.app.ObjectMeta.Name, r.hash)
			}
			if r.err == nil {
				r.err = enqueue(r.children, r.job.depth+1)
			}
			if firstErr == nil {
				firstErr = r.err
			}
		}
	}

	return firstErr
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
func (w *Walker) process(j job, outputPath string, maxDepth int) result {
	r := result{job: j}
	path := filepath.Join(outputPath, j.app.ObjectMeta.Name)

	// COMPARE HASHES HERE. STEP INTO RENDER IF NO MATCH
	hash, err := w.GenerateHash(j.app)
	if err != nil {
		if !errors.Is(err, kustomize.ErrNotSupported) {
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
			if !errors.Is(err, kustomize.ErrNotSupported) {
				r.err = err
			}
			return r
		}
		r.rendered = true
		r.hash = hash
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
