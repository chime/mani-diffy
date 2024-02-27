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
	if maxDepth != InfiniteDepth {
		// If we've reached the max depth, stop walking
		if depth > maxDepth {
			return nil
		}
	}

	log.Println("Dropping into", inputPath)

	fi, err := os.ReadDir(inputPath)
	if err != nil {
		return err
	}
	for _, file := range fi {
		if !strings.Contains(file.Name(), ".yaml") {
			continue
		}

		crds, err := helm.Read(filepath.Join(inputPath, file.Name()))
		if err != nil {
			return err
		}
		for _, crd := range crds {
			if crd.Kind != "Application" {
				continue
			}

			if strings.HasSuffix(crd.ObjectMeta.Name, w.ignoreSuffix) {
				continue
			}

			path := filepath.Join(outputPath, crd.ObjectMeta.Name)
			visited[path] = true

			hash, err := hashes.Get(crd.ObjectMeta.Name)
			// COMPARE HASHES HERE. STEP INTO RENDER IF NO MATCH
			if err != nil {
				return err
			}
			hashGenerated, err := w.GenerateHash(crd)
			if err != nil {
				return err
			}

			emptyManifest, err := helm.EmptyManifest(filepath.Join(path, "manifest.yaml"))
			if err != nil {
				return err
			}
			if hashGenerated != hash || emptyManifest {
				log.Printf("No match detected. Render: %s\n", crd.ObjectMeta.Name)
				if err := w.Render(crd, path); err != nil {
					if strings.Contains(err.Error(), "not supported") {
						continue
					}
					return err
				}

				if err := hashes.Add(crd.ObjectMeta.Name, hashGenerated); err != nil {
					return err
				}
			}

			if err := w.walk(path, outputPath, depth+1, maxDepth, visited, hashes); err != nil {
				return err
			}
		}
	}
	return nil
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
		return fmt.Errorf("kustomize not supported")
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
	workdir := flag.String("workdir", ".", "Directory to run the command in.")
	renderDir := flag.String("output", ".zz.auto-generated", "Path to store the compiled Argo applications.")
	maxDepth := flag.Int("max-depth", InfiniteDepth, "Maximum depth for the depth first walk.")
	hashStore := flag.String("hash-store", "sumfile", "The hashing backend to use. Can be `sumfile` or `json`.")
	hashStrategy := flag.String("hash-strategy", HashStrategyReadWrite, "Whether to read + write, or just read hashes. Can be `readwrite` or `read`.")
	ignoreSuffix := flag.String("ignore-suffix", "-ignore", "Suffix used to identify apps to ignore")
	skipRenderKey := flag.String("skip-render-key", "do-not-render", "Key to not render")
	ignoreValueFile := flag.String("ignore-value-file", "overrides-to-ignore", "Override file to ignore based on filename")
	postRenderer := flag.String("post-renderer", "", "When provided, binary will be called after an application is rendered.")
	flag.Parse()

	// Runs the command in the specified directory
	err := os.Chdir(*workdir)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	if err := helm.VerifyRenderDir(*renderDir); err != nil {
		log.Fatal(err)
	}

	h, err := getHashStore(*hashStore, *hashStrategy, *renderDir)
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

var hashStores = map[string]func(string, string) (HashStore, error){
	"sumfile": func(outputPath, hashStrategy string) (HashStore, error) { //nolint:unparam
		return NewSumFileStore(outputPath, hashStrategy), nil
	},
	"json": func(outputPath, hashStrategy string) (HashStore, error) {
		return NewJSONHashStore(filepath.Join(outputPath, "hashes.json"), hashStrategy)
	},
}

func getHashStore(hashStore, hashStrategy, outputPath string) (HashStore, error) {
	if fn, ok := hashStores[hashStore]; ok {
		return fn(outputPath, hashStrategy)
	}
	return nil, fmt.Errorf("Invalid hash store: %v", hashStore)
}
