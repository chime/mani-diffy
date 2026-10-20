package main

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// options configure a run of mani-diffy.
type options struct {
	workdir          string
	roots            []rootOptions
	maxDepth         int
	hashStore        string
	hashStrategy     string
	ignoreSuffix     string
	skipRenderKey    string
	ignoreValueFile  string
	postRenderer     string
	parallelism      int
	maskRandomValues bool

	// configFile is the config file to read the options that weren't given
	// by flags from.
	configFile string
	// only are the names of the config file's roots to render, or empty to
	// render all of them.
	only string
	// listRoots is whether to print the names of the roots instead of
	// rendering them.
	listRoots bool
	// verbose is whether to print the progress of every application instead
	// of only a summary.
	verbose bool
	// flags are the names of the flags that were given.
	flags map[string]bool
}

// rootOptions is a directory of Argo applications to render into an output
// directory.
type rootOptions struct {
	name   string
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
	maskRandomValues := flags.Bool("mask-random-values", false, "Render twice the charts that call random functions, and replace the values that differ between the renders with CONSCIOUSLY_NOT_RENDERED.")
	configFile := flags.String("config", defaultConfigFile, "Config file with the roots to render and settings for the flags that aren't given, relative to the workdir. Giving -root or -output renders only that root instead of the config file's roots.")
	only := flags.String("only", "", "Comma separated names of the config file's roots to render, instead of all of them.")
	verbose := flags.Bool("v", false, "Print the progress of every application, and Helm's warnings, instead of only a summary.")
	listRoots := flags.Bool("list-roots", false, "Print the names of the config file's roots that would be rendered, one per line, instead of rendering them.")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })

	return &options{
		workdir:          *workdir,
		roots:            []rootOptions{{root: *root, output: *renderDir}},
		maxDepth:         *maxDepth,
		hashStore:        *hashStore,
		hashStrategy:     *hashStrategy,
		ignoreSuffix:     *ignoreSuffix,
		skipRenderKey:    *skipRenderKey,
		ignoreValueFile:  *ignoreValueFile,
		postRenderer:     *postRenderer,
		parallelism:      *parallelism,
		maskRandomValues: *maskRandomValues,
		configFile:       *configFile,
		only:             *only,
		listRoots:        *listRoots,
		verbose:          *verbose,
		flags:            given,
	}, nil
}

const defaultConfigFile = ".mani-diffy.yaml"

// config is the contents of a config file. Settings it doesn't have are nil.
type config struct {
	HashStore        *string      `yaml:"hashStore"`
	IgnoreSuffix     *string      `yaml:"ignoreSuffix"`
	SkipRenderKey    *string      `yaml:"skipRenderKey"`
	IgnoreValueFile  *string      `yaml:"ignoreValueFile"`
	MaskRandomValues *bool        `yaml:"maskRandomValues"`
	Roots            []configRoot `yaml:"roots"`
}

type configRoot struct {
	Name   string `yaml:"name"`
	Root   string `yaml:"root"`
	Output string `yaml:"output"`
}

// loadConfig sets the options that weren't given by flags from the config
// file, then selects the roots given by -only. A missing config file is only
// an error when it was given by a flag.
func (o *options) loadConfig() error {
	content, err := os.ReadFile(o.configFile)
	if errors.Is(err, fs.ErrNotExist) && !o.flags["config"] {
		return o.selectOnly()
	}
	if err != nil {
		return err
	}
	c, err := parseConfig(content)
	if err != nil {
		return fmt.Errorf("error reading %s: %w", o.configFile, err)
	}

	setString := func(flag string, value *string, option *string) {
		if value != nil && !o.flags[flag] {
			*option = *value
		}
	}
	setBool := func(flag string, value *bool, option *bool) {
		if value != nil && !o.flags[flag] {
			*option = *value
		}
	}
	setString("hash-store", c.HashStore, &o.hashStore)
	setString("ignore-suffix", c.IgnoreSuffix, &o.ignoreSuffix)
	setString("skip-render-key", c.SkipRenderKey, &o.skipRenderKey)
	setString("ignore-value-file", c.IgnoreValueFile, &o.ignoreValueFile)
	setBool("mask-random-values", c.MaskRandomValues, &o.maskRandomValues)

	if len(c.Roots) > 0 && !o.flags["root"] && !o.flags["output"] {
		o.roots = nil
		for _, r := range c.Roots {
			o.roots = append(o.roots, rootOptions{name: r.Name, root: r.Root, output: r.Output})
		}
	}
	return o.selectOnly()
}

// selectOnly keeps only the roots named by -only, in the config file's order.
func (o *options) selectOnly() error {
	if o.only == "" {
		return nil
	}
	if o.flags["root"] || o.flags["output"] {
		return errors.New("-only selects roots of the config file, which -root and -output replace")
	}

	selected := map[string]bool{}
	for _, name := range strings.Split(o.only, ",") {
		name = strings.TrimSpace(name)
		if !slices.ContainsFunc(o.roots, func(r rootOptions) bool { return r.name == name }) {
			var names []string
			for _, r := range o.roots {
				if r.name != "" {
					names = append(names, r.name)
				}
			}
			return fmt.Errorf("-only: no root named %s in %s, which has %s", name, o.configFile, cmp.Or(strings.Join(names, ", "), "no named roots"))
		}
		selected[name] = true
	}
	o.roots = slices.DeleteFunc(o.roots, func(r rootOptions) bool { return !selected[r.name] })
	return nil
}

func parseConfig(content []byte) (*config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	c := &config{}
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	names := map[string]int{}
	outputs := map[string]int{}
	for i, r := range c.Roots {
		root := fmt.Sprintf("roots[%d] %s", i, r.Name)
		switch {
		case r.Name == "":
			return nil, fmt.Errorf("roots[%d]: name is required", i)
		case r.Root == "":
			return nil, fmt.Errorf("%s: root is required", root)
		case r.Output == "":
			return nil, fmt.Errorf("%s: output is required", root)
		}
		if other, ok := names[r.Name]; ok {
			return nil, fmt.Errorf("%s: name is already used by roots[%d]", root, other)
		}
		names[r.Name] = i
		output := filepath.Clean(r.Output)
		if other, ok := outputs[output]; ok {
			return nil, fmt.Errorf("%s: output %s is already used by roots[%d] %s", root, r.Output, other, c.Roots[other].Name)
		}
		outputs[output] = i
	}
	return c, nil
}

// writeRootNames writes the names of the roots, one per line. Roots given by
// flags have no name.
func writeRootNames(w io.Writer, roots []rootOptions) {
	for _, r := range roots {
		if r.name != "" {
			fmt.Fprintln(w, r.name)
		}
	}
}
