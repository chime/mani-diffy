package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"

	"path/filepath"
	"strings"
	"time"

	"github.com/1debit/mani-diffy/pkg/helm"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
)

const InfiniteDepth = -1

// Renderer is a function that can render an Argo application.
type Renderer func(*v1alpha1.Application, string) error

// PostRenderer is a function that can be called after an Argo application is rendered.
type PostRenderer func(string) error

// Walker walks a directory tree looking for Argo applications and renders them
// using a depth first search.
type Walker struct {
	// HelmTemplate is a function that can render an Argo application using Helm
	HelmTemplate Renderer

	// CopySource is a function that can copy an Argo application to a directory
	CopySource Renderer

	// PostRender is a function that can be called after an Argo application is rendered.
	PostRender PostRenderer

	// GenerateHash is used to generate a cache key for an Argo application
	GenerateHash func(*v1alpha1.Application) (string, error)

	ignoreSuffix string
}

type HashStores map[string]func(string, string) (HashStore, error)

// Walk walks a directory tree looking for Argo applications and renders them
func (w *Walker) Walk(inputPath, outputPath string, maxDepth int, hashes HashStore) error {
	visited := make(map[string]bool)

	if err := w.walk(inputPath, outputPath, 0, maxDepth, visited, hashes); err != nil {
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

func pruneUnvisited(visited map[string]bool, outputPath string) error {
	files, err := os.ReadDir(outputPath)
	if err != nil {
		return err
	}

	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		path := filepath.Join(outputPath, f.Name())
		if visited[path] {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}

	return nil
}

func (w *Walker) walk(inputPath, outputPath string, depth, maxDepth int, visited map[string]bool, hashes HashStore) error {
	if maxDepth != InfiniteDepth && depth > maxDepth {
		return nil
	}

	log.Println("Dropping into", inputPath)
	files, err := os.ReadDir(inputPath)
	if err != nil {
		return err
	}

	for _, file := range files {
		if !strings.Contains(file.Name(), ".yaml") {
			continue
		}
		if err := w.processFile(file, inputPath, outputPath, depth, maxDepth, visited, hashes); err != nil {
			return err
		}
	}

	return nil
}

func (w *Walker) processFile(file os.DirEntry, inputPath, outputPath string, depth, maxDepth int, visited map[string]bool, hashes HashStore) error {
	apps, err := helm.Read(filepath.Join(inputPath, file.Name()))
	if err != nil {
		return err
	}

	for _, app := range apps {
		if err := w.processApps(app, outputPath, depth, maxDepth, visited, hashes); err != nil {
			return err
		}
	}

	return nil
}

func (w *Walker) processApps(app *v1alpha1.Application, outputPath string, depth, maxDepth int, visited map[string]bool, hashes HashStore) error {
	if app.Kind != "Application" || strings.HasSuffix(app.ObjectMeta.Name, w.ignoreSuffix) {
		return nil
	}

	path := filepath.Join(outputPath, app.ObjectMeta.Name)
	visited[path] = true

	emptyManifest, err := helm.EmptyManifest(filepath.Join(path, "manifest.yaml"))
	if err != nil {
		return err
	}

	existingHash, appHash, err := w.generateHashes(app, hashes)
	if err != nil {
		return err
	}

	if appHash != existingHash || emptyManifest {
		if err := w.renderAndUpdateHashes(app, path, appHash, hashes); err != nil {
			return err
		}
	}

	return w.walk(path, outputPath, depth+1, maxDepth, visited, hashes)
}

func (w *Walker) generateHashes(app *v1alpha1.Application, hashes HashStore) (string, string, error) {
	existingHash, err := hashes.Get(app.ObjectMeta.Name)
	if err != nil {
		return "", "", err
	}
	generatedHash, err := w.GenerateHash(app)
	return existingHash, generatedHash, err
}

func (w *Walker) renderAndUpdateHashes(app *v1alpha1.Application, path, generatedHash string, hashes HashStore) error {
	log.Printf("No match detected. Render: %s\n", app.ObjectMeta.Name)
	if err := w.Render(app, path); err != nil {
		return err
	}
	return hashes.Add(app.ObjectMeta.Name, generatedHash)
}

func (w *Walker) Render(application *v1alpha1.Application, output string) error {
	log.Println("Render", application.ObjectMeta.Name)

	var render Renderer

	// Figure out what renderer to use
	if application.Spec.Source.Helm != nil {
		render = w.HelmTemplate
	} else {
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

func HelmTemplate(application *v1alpha1.Application, output string) error {
	return helm.Run(application, output, "", "")
}

func CopySource(application *v1alpha1.Application, output string) error {
	cmd := exec.Command("cp", "-r", application.Spec.Source.Path+"/.", output)
	return cmd.Run()
}

func PostRender(command string) PostRenderer {
	return func(output string) error {
		cmd := exec.Command(command, output)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
}

func main() {
	root := flag.String("root", "bootstrap", "Directory to initially look for k8s manifests containing Argo applications. The root of the tree.")
	renderDir := flag.String("output", ".zz.auto-generated", "Path to store the compiled Argo applications.")
	maxDepth := flag.Int("max-depth", InfiniteDepth, "Maximum depth for the depth first walk.")
	hashStore := flag.String("hash-store", "sumfile", "The hashing backend to use. Can be `sumfile` or `json`.")
	hashStrategy := flag.String("hash-strategy", HashStrategyReadWrite, "Whether to read + write, or just read hashes. Can be `readwrite` or `read`.")
	ignoreSuffix := flag.String("ignore-suffix", "-ignore", "Suffix used to identify apps to ignore")
	skipRenderKey := flag.String("skip-render-key", "do-not-render", "Key to not render")
	ignoreValueFile := flag.String("ignore-value-file", "overrides-to-ignore", "Override file to ignore based on filename")
	postRenderer := flag.String("post-renderer", "", "When provided, binary will be called after an application is rendered.")
	flag.Parse()

	start := time.Now()
	if err := helm.VerifyRenderDir(*renderDir); err != nil {
		log.Fatal(err)
	}

	hashStores := HashStores{
		"sumfile": func(outputPath, hashStrategy string) (HashStore, error) { //nolint:unparam
			return NewSumFileStore(outputPath, hashStrategy), nil
		},
		"json": func(outputPath, hashStrategy string) (HashStore, error) {
			return NewJSONHashStore(filepath.Join(outputPath, "hashes.json"), hashStrategy)
		},
	}

	h, err := getHashStore(hashStores, *hashStore, *hashStrategy, *renderDir)
	if err != nil {
		log.Fatal(err)
	}

	w := &Walker{
		CopySource: CopySource,
		HelmTemplate: func(application *v1alpha1.Application, output string) error {
			return helm.Run(application, output, *skipRenderKey, *ignoreValueFile)
		},
		GenerateHash: func(application *v1alpha1.Application) (string, error) {
			return helm.GenerateHash(application, *ignoreValueFile)
		},
		ignoreSuffix: *ignoreSuffix,
	}

	if *postRenderer != "" {
		w.PostRender = PostRender(*postRenderer)
	}

	if err := w.Walk(*root, *renderDir, *maxDepth, h); err != nil {
		log.Fatal(err)
	}
	log.Printf("mani-diffy took %v to run", time.Since(start))
}

func getHashStore(hashStores HashStores, hashStore, hashStrategy, outputPath string) (HashStore, error) {
	if fn, ok := hashStores[hashStore]; ok {
		return fn(outputPath, hashStrategy)
	}
	return nil, fmt.Errorf("Invalid hash store: %v", hashStore)
}
