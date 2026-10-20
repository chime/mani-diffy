package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// loadOptions parses the arguments and reads the config file the way main does,
// in a temporary working directory with the given config file contents, if any.
func loadOptions(t *testing.T, config string, args ...string) (*options, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	if config != "" {
		if err := os.WriteFile(".mani-diffy.yaml", []byte(config), 0644); err != nil {
			t.Fatal(err)
		}
	}
	opts, err := parseOptions(args)
	if err != nil {
		t.Fatal(err)
	}
	return opts, opts.loadConfig()
}

const configWithEverything = `hashStore: json
ignoreSuffix: -console
skipRenderKey: appTag
ignoreValueFile: overrides/apptags
maskRandomValues: true
roots:
  - name: use1-stable
    root: bootstrap/child-applications/us-east-1/stable
    output: zz.auto-generated/root-use1-stable
  - name: use1-prod
    root: bootstrap/child-applications/us-east-1/prod
    output: zz.auto-generated/root-use1-prod
`

var configRoots = []rootOptions{
	{name: "use1-stable", root: "bootstrap/child-applications/us-east-1/stable", output: "zz.auto-generated/root-use1-stable"},
	{name: "use1-prod", root: "bootstrap/child-applications/us-east-1/prod", output: "zz.auto-generated/root-use1-prod"},
}

func TestOptionsWithoutConfigFile(t *testing.T) {
	opts, err := loadOptions(t, "")

	if err != nil {
		t.Fatal(err)
	}
	if want := []rootOptions{{root: "bootstrap", output: ".zz.auto-generated"}}; !slices.Equal(opts.roots, want) {
		t.Errorf("Expected roots %v, got %v", want, opts.roots)
	}
	if opts.hashStore != "sumfile" || opts.ignoreSuffix != "-ignore" || opts.skipRenderKey != "do-not-render" || opts.ignoreValueFile != "overrides-to-ignore" || opts.maskRandomValues {
		t.Errorf("Expected the flag defaults, got %+v", opts)
	}
}

func TestOptionsFromConfigFile(t *testing.T) {
	opts, err := loadOptions(t, configWithEverything)

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opts.roots, configRoots) {
		t.Errorf("Expected roots %v, got %v", configRoots, opts.roots)
	}
	if opts.hashStore != "json" || opts.ignoreSuffix != "-console" || opts.skipRenderKey != "appTag" || opts.ignoreValueFile != "overrides/apptags" || !opts.maskRandomValues {
		t.Errorf("Expected the config file's settings, got %+v", opts)
	}
}

func TestOptionsFromFlagsOverrideConfigFile(t *testing.T) {
	opts, err := loadOptions(t, configWithEverything,
		"-hash-store", "sumfile", "-ignore-suffix", "-ignore", "-skip-render-key", "tag", "-ignore-value-file", "tags", "-mask-random-values=false")

	if err != nil {
		t.Fatal(err)
	}
	if opts.hashStore != "sumfile" || opts.ignoreSuffix != "-ignore" || opts.skipRenderKey != "tag" || opts.ignoreValueFile != "tags" || opts.maskRandomValues {
		t.Errorf("Expected the flags' settings, got %+v", opts)
	}
	if !slices.Equal(opts.roots, configRoots) {
		t.Errorf("Expected roots %v, got %v", configRoots, opts.roots)
	}
}

func TestOptionsRootFlagsReplaceConfigRoots(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want rootOptions
	}{
		{args: []string{"-root", "apps", "-output", "rendered"}, want: rootOptions{root: "apps", output: "rendered"}},
		{args: []string{"-root", "apps"}, want: rootOptions{root: "apps", output: ".zz.auto-generated"}},
		{args: []string{"-output", "rendered"}, want: rootOptions{root: "bootstrap", output: "rendered"}},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			opts, err := loadOptions(t, configWithEverything, tt.args...)

			if err != nil {
				t.Fatal(err)
			}
			if want := []rootOptions{tt.want}; !slices.Equal(opts.roots, want) {
				t.Errorf("Expected roots %v, got %v", want, opts.roots)
			}
			if opts.hashStore != "json" {
				t.Errorf("Expected the other settings to come from the config file, got %+v", opts)
			}
		})
	}
}

func TestOptionsFromConfigFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("other.yaml", []byte("hashStore: json\n"), 0644); err != nil {
		t.Fatal(err)
	}
	opts, err := parseOptions([]string{"-config", "other.yaml"})
	if err != nil {
		t.Fatal(err)
	}

	if err := opts.loadConfig(); err != nil {
		t.Fatal(err)
	}

	if opts.hashStore != "json" {
		t.Errorf("Expected the settings of other.yaml, got %+v", opts)
	}
}

func TestOptionsRequireConfigFileGivenByFlag(t *testing.T) {
	_, err := loadOptions(t, "", "-config", "missing.yaml")

	if err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Errorf("Expected an error about missing.yaml, got: %v", err)
	}
}

func TestOptionsRejectInvalidConfigFile(t *testing.T) {
	for _, tt := range []struct {
		name, config, err string
	}{
		{"unknown key", "hashStor: json\n", "field hashStor not found"},
		{"root without name", "roots:\n  - {root: a, output: out-a}\n", "roots[0]: name is required"},
		{"root without root", "roots:\n  - {name: a, output: out-a}\n", "roots[0] a: root is required"},
		{"root without output", "roots:\n  - {name: a, root: a}\n", "roots[0] a: output is required"},
		{"duplicate names", "roots:\n  - {name: a, root: a, output: out-a}\n  - {name: a, root: b, output: out-b}\n", "roots[1] a: name is already used by roots[0]"},
		{"duplicate outputs", "roots:\n  - {name: a, root: a, output: out}\n  - {name: b, root: b, output: out}\n", "roots[1] b: output out is already used by roots[0] a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadOptions(t, tt.config)

			if err == nil || !strings.Contains(err.Error(), tt.err) || !strings.Contains(err.Error(), ".mani-diffy.yaml") {
				t.Errorf("Expected an error about .mani-diffy.yaml containing %q, got: %v", tt.err, err)
			}
		})
	}
}

func TestOptionsOnlySelectsConfigRoots(t *testing.T) {
	for _, tt := range []struct {
		only string
		want []rootOptions
	}{
		{only: "use1-prod", want: configRoots[1:]},
		{only: "use1-prod,use1-stable", want: configRoots},
		{only: "use1-stable, use1-prod", want: configRoots},
	} {
		t.Run(tt.only, func(t *testing.T) {
			opts, err := loadOptions(t, configWithEverything, "-only", tt.only)

			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(opts.roots, tt.want) {
				t.Errorf("Expected roots %v, got %v", tt.want, opts.roots)
			}
		})
	}
}

func TestOptionsOnlyRejectsUnknownRoots(t *testing.T) {
	_, err := loadOptions(t, configWithEverything, "-only", "use1-stable,use1-dev")

	if err == nil || !strings.Contains(err.Error(), "-only: no root named use1-dev in .mani-diffy.yaml, which has use1-stable, use1-prod") {
		t.Errorf("Expected an error about use1-dev, got: %v", err)
	}
}

func TestOptionsOnlyRejectsRootFlags(t *testing.T) {
	_, err := loadOptions(t, configWithEverything, "-only", "use1-stable", "-root", "apps")

	if err == nil || !strings.Contains(err.Error(), "-only selects roots of the config file, which -root and -output replace") {
		t.Errorf("Expected an error about -root, got: %v", err)
	}
}

func TestOptionsListRootNames(t *testing.T) {
	opts, err := loadOptions(t, configWithEverything, "-list-roots", "-only", "use1-prod")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	writeRootNames(&out, opts.roots)

	if !opts.listRoots {
		t.Error("Expected -list-roots to be set")
	}
	if got := out.String(); got != "use1-prod\n" {
		t.Errorf("Expected the selected root's name, got %q", got)
	}
}

func TestOptionsListNoNamesWithoutConfigRoots(t *testing.T) {
	opts, err := loadOptions(t, "", "-list-roots")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	writeRootNames(&out, opts.roots)

	if got := out.String(); got != "" {
		t.Errorf("Expected no names for the unnamed root of the flags, got %q", got)
	}
}

func TestSummary(t *testing.T) {
	for _, tt := range []struct {
		stats Stats
		want  string
	}{
		{Stats{Rendered: 1204, RenderedTwice: 156, Unchanged: 3780}, "Applications: 1204 rendered (156 twice, to mask random values), 3780 unchanged, in 23.012s"},
		{Stats{Rendered: 3, Unchanged: 1}, "Applications: 3 rendered, 1 unchanged, in 23.012s"},
		{Stats{Rendered: 3, Unsupported: 2}, "Applications: 3 rendered, 0 unchanged, 2 skipped since Kustomize isn't supported, in 23.012s"},
	} {
		if got := summary(tt.stats, 23012345*time.Microsecond); got != tt.want {
			t.Errorf("Expected %q, got %q", tt.want, got)
		}
	}
}

func TestOptionsVerbose(t *testing.T) {
	quiet, err := loadOptions(t, "")
	if err != nil {
		t.Fatal(err)
	}
	verbose, err := loadOptions(t, "", "-v")
	if err != nil {
		t.Fatal(err)
	}

	if quiet.verbose || !verbose.verbose {
		t.Errorf("Expected -v to make the output verbose, got %v without and %v with it", quiet.verbose, verbose.verbose)
	}
}
