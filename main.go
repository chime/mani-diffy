package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"

	"path/filepath"
	"time"

	"github.com/chime/mani-diffy/pkg/helm"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

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

// newWalker returns a Walker that renders Argo applications with Helm, or by
// copying their source when they don't use Helm.
func newWalker(skipRenderKey, ignoreValueFile, ignoreSuffix string, parallelism int) *Walker {
	return &Walker{
		CopySource: CopySource,
		HelmTemplate: func(application *v1alpha1.Application, output string) error {
			return helm.Run(application, output, skipRenderKey, ignoreValueFile)
		},
		GenerateHash: func(application *v1alpha1.Application) (string, error) {
			return helm.GenerateHash(application, ignoreValueFile)
		},
		Parallelism:  parallelism,
		ignoreSuffix: ignoreSuffix,
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
	parallelism := flag.Int("parallelism", runtime.NumCPU(), "Maximum number of applications rendered at once.")
	flag.Parse()

	// Runs the command in the specified directory
	err := os.Chdir(*workdir)
	if err != nil {
		log.Fatal("Could not set workdir: ", err)
	}

	start := time.Now()
	if err := helm.VerifyRenderDir(*renderDir); err != nil {
		log.Fatal(err)
	}

	h, err := getHashStore(*hashStore, *hashStrategy, *renderDir)
	if err != nil {
		log.Fatal(err)
	}

	w := newWalker(*skipRenderKey, *ignoreValueFile, *ignoreSuffix, *parallelism)

	if *postRenderer != "" {
		w.PostRender = PostRender(*postRenderer)
	}

	if err := w.Walk([]Root{{Input: *root, Output: *renderDir, Hashes: h}}, *maxDepth); err != nil {
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
