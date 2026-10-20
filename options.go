package main

import (
	"flag"
	"os"
	"runtime"
)

// options configure a run of mani-diffy.
type options struct {
	workdir         string
	roots           []rootOptions
	maxDepth        int
	hashStore       string
	hashStrategy    string
	ignoreSuffix    string
	skipRenderKey   string
	ignoreValueFile string
	postRenderer    string
	parallelism     int
}

// rootOptions is a directory of Argo applications to render into an output
// directory.
type rootOptions struct {
	root   string
	output string
}

// parseOptions parses the command line arguments, without the program name.
func parseOptions(args []string) (*options, error) {
	flags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	root := flags.String("root", "bootstrap", "Directory to initially look for k8s manifests containing Argo applications. The root of the tree.")
	workdir := flags.String("workdir", ".", "Directory to run the command in.")
	renderDir := flags.String("output", ".zz.auto-generated", "Path to store the compiled Argo applications.")
	maxDepth := flags.Int("max-depth", InfiniteDepth, "Maximum depth for the depth first walk.")
	hashStore := flags.String("hash-store", "sumfile", "The hashing backend to use. Can be `sumfile` or `json`.")
	hashStrategy := flags.String("hash-strategy", HashStrategyReadWrite, "Whether to read + write, or just read hashes. Can be `readwrite` or `read`.")
	ignoreSuffix := flags.String("ignore-suffix", "-ignore", "Suffix used to identify apps to ignore")
	skipRenderKey := flags.String("skip-render-key", "do-not-render", "Key to not render")
	ignoreValueFile := flags.String("ignore-value-file", "overrides-to-ignore", "Override file to ignore based on filename")
	postRenderer := flags.String("post-renderer", "", "When provided, binary will be called after an application is rendered.")
	parallelism := flags.Int("parallelism", runtime.NumCPU(), "Maximum number of applications rendered at once.")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	return &options{
		workdir:         *workdir,
		roots:           []rootOptions{{root: *root, output: *renderDir}},
		maxDepth:        *maxDepth,
		hashStore:       *hashStore,
		hashStrategy:    *hashStrategy,
		ignoreSuffix:    *ignoreSuffix,
		skipRenderKey:   *skipRenderKey,
		ignoreValueFile: *ignoreValueFile,
		postRenderer:    *postRenderer,
		parallelism:     *parallelism,
	}, nil
}
