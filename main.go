package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

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
	opts, err := parseOptions(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		// The error and the usage are already printed.
		os.Exit(2)
	}

	// The progress of every application goes to the standard logger, like
	// Helm's warnings, and is only printed with -v. The summary and errors go
	// to output, and are always printed.
	if !opts.verbose {
		log.SetOutput(io.Discard)
	}

	// Runs the command in the specified directory
	err = os.Chdir(opts.workdir)
	if err != nil {
		output.Fatal("Could not set workdir: ", err)
	}

	if err := opts.loadConfig(); err != nil {
		output.Fatal(err)
	}

	if opts.listRoots {
		writeRootNames(os.Stdout, opts.roots)
		return
	}

	start := time.Now()
	roots := make([]Root, 0, len(opts.roots))
	for _, root := range opts.roots {
		if err := helm.VerifyRenderDir(root.output); err != nil {
			output.Fatal(err)
		}

		h, err := getHashStore(opts.hashStore, opts.hashStrategy, root.output)
		if err != nil {
			output.Fatal(err)
		}
		roots = append(roots, Root{Input: root.root, Output: root.output, Hashes: h})
	}

	w := newWalker(opts.skipRenderKey, opts.ignoreValueFile, opts.ignoreSuffix, opts.parallelism)

	if opts.postRenderer != "" {
		w.PostRender = PostRender(opts.postRenderer)
	}

	stats, err := w.Walk(roots, opts.maxDepth)
	if err != nil {
		output.Fatal(err)
	}
	output.Println(summary(stats, time.Since(start)))
}

// output prints what is always printed: the summary of a run, warnings, and
// errors.
var output = log.New(os.Stderr, "", log.LstdFlags)

// summary describes what a run did with the Argo applications it found.
func summary(stats Stats, took time.Duration) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Applications: %d rendered", stats.Rendered)
	fmt.Fprintf(&s, ", %d unchanged", stats.Unchanged)
	if stats.Unsupported > 0 {
		fmt.Fprintf(&s, ", %d skipped since Kustomize isn't supported", stats.Unsupported)
	}
	fmt.Fprintf(&s, ", in %v", took.Round(time.Millisecond))
	return s.String()
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
