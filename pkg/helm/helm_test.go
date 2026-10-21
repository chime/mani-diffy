package helm

import (
	"bytes"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

func mapKeys(m map[string][32]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestHelm(t *testing.T) {
	// Set up tests to use current package's testdata as the working directory
	oldWD, _ := os.Getwd()
	_ = os.Chdir("testdata")
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	t.Run("Read", func(t *testing.T) {
		data, err := Read("crdData_testfile.yaml")
		if err != nil {
			t.Error(err)
		}

		for _, crd := range data {
			if crd.Kind != "Application" {
				t.Error("Kind attribute did not match Application")
			}

			if crd.Spec.Source.Helm.ValueFiles[0] != "../../overrides/bootstrap/prod-cluster.yaml" {
				t.Error(("Failed to parse ValuesFiles from yaml"))
			}
		}
	})

	t.Run("ReadMultipleCrds", func(t *testing.T) {
		data, err := Read("crdData_multiple_crd_testfile.yaml")
		if err != nil {
			t.Error(err)
		}

		if len(data) != 2 {
			t.Error("Failed to get correct number of crds")
			t.Errorf("%s", data)
		}
	})

	t.Run("BuildParameters", func(t *testing.T) {
		data, err := Read("crdData_testfile.yaml")
		if err != nil {
			t.Error(err)
		}
		crd := data[0]
		setValues, fileValues := buildParams(crd, "")

		if setValues != "region=us-east-1" {
			t.Error("setValues is not correct")
		}

		if fileValues != "../../overrides/bootstrap/prod-cluster.yaml" {
			t.Error("fileValues is not correct")
		}
	})

	t.Run("BuildParameters2", func(t *testing.T) {
		data, err := Read("crdData_testfile_2.yaml")
		if err != nil {
			t.Error(err)
		}
		crd := data[0]
		setValues, fileValues := buildParams(crd, "")

		if setValues != "region=us-east-1,testName=testValue" {
			t.Error("setValues is not correct")
		}

		if fileValues != "../../overrides/bootstrap/prod-cluster.yaml,../../overrides/bootstrap/fake_file.yaml" {
			t.Error("fileValues is not correct")
		}
	})

	t.Run("BuildParametersIgnoreValueFile", func(t *testing.T) {
		data, err := Read("crdData_testfile_3.yaml")
		if err != nil {
			t.Error(err)
		}
		crd := data[0]
		setValues, fileValues := buildParams(crd, "overrides/service/bar/test.yaml")

		if setValues != "env=test" {
			t.Error("setValues is not correct")
		}

		if fileValues != "../../overrides/service/bar/base.yaml" {
			t.Error("fileValues is not correct")
		}
	})

	t.Run("BuildParametersIgnoreMissingFile", func(t *testing.T) {
		data, err := Read("crdData_testfile_4.yaml")
		if err != nil {
			t.Error(err)
		}
		crd := data[0]
		setValues, fileValues := buildParams(crd, "")

		if setValues != "env=test" {
			t.Error("setValues is not correct")
		}

		want := "../../overrides/service/bar/base.yaml,../../overrides/service/bar/test.yaml"
		got := fileValues

		if want != got {
			t.Errorf("fileValues is not correct, want: %q, got: %q", want, got)
		}
	})

	t.Run("CreateTempFile", func(t *testing.T) {
		fileContent := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: prod-cluster
  namespace: argocd
	`
		fileName, err := createTempFile(fileContent)
		if err != nil {
			t.Errorf("failure during file creation: %v", err)
		}

		t.Run("Test creating a file from values.", func(t *testing.T) {
			_, err = os.Stat(fileName)
			if err != nil {
				t.Errorf("failed to create a temporary file: %v", err)
			}
		})

		t.Run("Verify the content of the temp file", func(t *testing.T) {
			got, _ := os.ReadFile(fileName)
			want := fileContent
			if string(got) != want {
				t.Errorf("file contents didn't match got %s wanted %s", got, want)
			}
		})

		defer os.Remove(fileName)

		t.Run("Verify the file is cleaned up", func(t *testing.T) {
			_, err = os.Stat(fileName)
			if os.IsNotExist(err) {
				t.Errorf("failed to clean up the temp file: %v", err)
			}

		})
	})

	t.Run("Template", func(t *testing.T) {
		data, err := Read("crdData_testfile.yaml")
		if err != nil {
			t.Error(err)
		}
		crdSpec := data[0]
		_, _, err = template(crdSpec, "", "", false)
		if err != nil {
			log.Println(err)
			t.Error("Template failed to render a template")
		}
	})

	t.Run("TemplateContent", func(t *testing.T) {
		data, err := Read("crdData_testfile.yaml")
		if err != nil {
			t.Error(err)
		}
		crdSpec := data[0]

		var comparisonString = `---
# Source: app-of-apps/templates/apps.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
`

		manifest, _, _ := template(crdSpec, "", "", false)
		if strings.Contains(string(manifest), comparisonString) != true {
			t.Error("Template failed to render a template with expected content")
		}
	})

	t.Run("TemplateContentSkipRenderKey", func(t *testing.T) {
		data, err := Read("crdData_testfile_3.yaml")
		if err != nil {
			t.Error(err)
		}
		app := data[0]

		// Call template with a key to override
		manifest, _, _ := template(app, "appTag", "", false)

		// Verify the rendered manifest contains the override
		if !strings.Contains(string(manifest), "appTag: CONSCIOUSLY_NOT_RENDERED") {
			t.Errorf("Expected override not found in rendered manifest")

		}
	})

	// needsDependency returns an Application for a copy of a chart whose
	// dependency is declared but not vendored.
	needsDependency := func(t *testing.T) *v1alpha1.Application {
		chartsDir := t.TempDir()
		if err := os.CopyFS(chartsDir, os.DirFS("charts")); err != nil {
			t.Fatal(err)
		}
		apps, err := Read("crdData_testfile_3.yaml")
		if err != nil {
			t.Fatal(err)
		}
		app := apps[0]
		app.Spec.Source.Path = chartsDir + "/needs-dependency"
		app.Spec.Source.Helm.ValueFiles = nil
		return app
	}

	t.Run("TemplateInstallsMissingDependencies", func(t *testing.T) {
		manifest, _, err := template(needsDependency(t), "", "", false)
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(manifest), "name: release-name-dependency-source") {
			t.Errorf("Expected the dependency to be rendered, got:\n%s", manifest)
		}
	})

	t.Run("TemplateInstallsMissingDependenciesConcurrently", func(t *testing.T) {
		app := needsDependency(t)

		const renders = 8
		results := make(chan string, renders)
		errs := make(chan error, renders)
		for range renders {
			go func() {
				manifest, _, err := template(app, "", "", false)
				errs <- err
				results <- string(manifest)
			}()
		}

		for range renders {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			if manifest := <-results; !strings.Contains(manifest, "name: release-name-dependency-source") {
				t.Errorf("Expected the dependency to be rendered, got:\n%s", manifest)
			}
		}
	})

	t.Run("TemplateMatchesHelmCLI", func(t *testing.T) {
		// Golden files were rendered by the helm v4.2.1 CLI.
		scenarios := []struct {
			application string
			golden      string
		}{
			{application: "crdData_cli_parity.yaml", golden: "cli_parity.golden.yaml"},
			{application: "crdData_cli_parity_no_namespace.yaml", golden: "cli_parity_no_namespace.golden.yaml"},
		}

		for _, tt := range scenarios {
			t.Run(tt.application, func(t *testing.T) {
				apps, err := Read(tt.application)
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(tt.golden)
				if err != nil {
					t.Fatal(err)
				}

				got, _, err := template(apps[0], "appTag", "", false)
				if err != nil {
					t.Fatal(err)
				}

				if string(got) != string(want) {
					t.Errorf("rendered manifest differs from helm CLI output.\ngot:\n%s\nwant:\n%s", got, want)
				}
			})
		}
	})

	t.Run("RunMasksRandomValues", func(t *testing.T) {
		scenarios := []struct {
			application string
			golden      string
		}{
			{application: "crdData_random_values.yaml", golden: "random_values.golden.yaml"},
			{application: "crdData_tpl_values.yaml", golden: "tpl_values.golden.yaml"},
		}

		for _, tt := range scenarios {
			t.Run(tt.application, func(t *testing.T) {
				apps, err := Read(tt.application)
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(tt.golden)
				if err != nil {
					t.Fatal(err)
				}
				output := t.TempDir()

				rendering, err := Run(apps[0], output, "", "", true)
				if err != nil {
					t.Fatal(err)
				}
				if !rendering.Twice {
					t.Error("Expected the chart to be rendered twice")
				}

				got, err := os.ReadFile(filepath.Join(output, "manifest.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Errorf("Expected random values to be masked.\ngot:\n%s\nwant:\n%s", got, want)
				}
			})
		}
	})

	t.Run("TemplateRendersEachApplicationOfAChartWithItsOwnValues", func(t *testing.T) {
		apps, err := Read("crdData_optional_sub.yaml")
		if err != nil {
			t.Fatal(err)
		}

		for range 2 {
			withoutSub, _, err := template(apps[0], "", "", false)
			if err != nil {
				t.Fatal(err)
			}
			withSub, _, err := template(apps[1], "", "", false)
			if err != nil {
				t.Fatal(err)
			}

			if strings.Contains(string(withoutSub), "name: sub") {
				t.Errorf("Expected the disabled subchart not to be rendered, got:\n%s", withoutSub)
			}
			if !strings.Contains(string(withSub), "name: sub") {
				t.Errorf("Expected the subchart to be rendered, got:\n%s", withSub)
			}
		}
	})

	t.Run("CLIVersions", func(t *testing.T) {
		info := &debug.BuildInfo{Deps: []*debug.Module{
			{Path: "helm.sh/helm/v4", Version: "v4.2.1"},
			{Path: "k8s.io/client-go", Version: "v0.35.0", Replace: &debug.Module{Path: "k8s.io/client-go", Version: "v0.36.1"}},
		}}

		helm, kube, err := cliVersions(info)

		if err != nil {
			t.Fatal(err)
		}
		if helm != "v4.2.1" {
			t.Errorf("Expected helm version v4.2.1, got %s", helm)
		}
		if kube.String() != "v1.36.0" || kube.Major != "1" || kube.Minor != "36" {
			t.Errorf("Expected kube version v1.36.0, got %+v", kube)
		}
	})

	t.Run("CLIVersionsWithoutHelm", func(t *testing.T) {
		_, _, err := cliVersions(&debug.BuildInfo{Deps: []*debug.Module{{Path: "k8s.io/client-go", Version: "v0.36.1"}}})

		if err == nil || !strings.Contains(err.Error(), "helm.sh/helm/v4") {
			t.Errorf("Expected an error about the missing helm module, got: %v", err)
		}
	})

	t.Run("TemplateConcurrently", func(t *testing.T) {
		apps, err := Read("crdData_cli_parity.yaml")
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile("cli_parity.golden.yaml")
		if err != nil {
			t.Fatal(err)
		}

		const renders = 8
		results := make(chan string, renders)
		errs := make(chan error, renders)
		for range renders {
			go func() {
				got, _, err := template(apps[0], "appTag", "", false)
				errs <- err
				results <- string(got)
			}()
		}

		for range renders {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			if got := <-results; got != string(want) {
				t.Errorf("concurrent render differs from helm CLI output:\n%s", got)
			}
		}
	})

	t.Run("TemplateError", func(t *testing.T) {
		apps, err := Read("crdData_cli_parity.yaml")
		if err != nil {
			t.Fatal(err)
		}
		app := apps[0]
		app.Spec.Source.Helm.Parameters[0].Name = "replicas"
		app.Spec.Source.Helm.Parameters[0].Value = "not-a-number"

		_, _, err = template(app, "", "", false)
		if err == nil || !strings.Contains(err.Error(), "replicas") {
			t.Errorf("Expected a schema validation error about replicas, got: %v", err)
		}
	})

	t.Run("RunLogsTheErrorWhenRenderingFails", func(t *testing.T) {
		apps, err := Read("crdData_cli_parity.yaml")
		if err != nil {
			t.Fatal(err)
		}
		app := apps[0]
		app.Spec.Source.Helm.Parameters[0].Name = "replicas"
		app.Spec.Source.Helm.Parameters[0].Value = "not-a-number"
		var logged bytes.Buffer
		log.SetOutput(&logged)
		defer log.SetOutput(os.Stderr)

		if _, err := Run(app, t.TempDir(), "", "", false); err == nil {
			t.Fatal("Expected rendering to fail")
		}

		if !strings.Contains(logged.String(), "replicas") {
			t.Errorf("Expected the log to say why rendering failed, got: %q", logged.String())
		}
	})

	t.Run("GeneralHashFunction", func(t *testing.T) {
		testFiles := []struct {
			name string
			file string
			hash string
		}{
			{
				name: "Generating hash on symlinked files",
				file: "crdData_override_testfile_sym_link.yaml",
				hash: "a1d62704739d8af3fcaca8f8b13602fc4d4e656b87d773089df3c626c2f37b5d",
			},
			{
				name: "Generate hash on non symlinked file",
				file: "crdData_override_testfile.yaml",
				hash: "a1d62704739d8af3fcaca8f8b13602fc4d4e656b87d773089df3c626c2f37b5d",
			},
		}

		for _, tt := range testFiles {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				hash, err := generalHashFunction(tt.file) //nolint:govet
				h := hex.EncodeToString(hash)
				expectedHash := tt.hash //nolint:govet
				if err != nil || h != expectedHash {
					t.Errorf("Failed to generate a correct hash on an overrides. got: %s wanted %s", h, expectedHash)
				}
			})
		}
	})

	t.Run("GenerateHashOnCrd", func(t *testing.T) {
		data, err := Read("crdData_testfile.yaml")
		if err != nil {
			t.Error(err)
		}
		crd := data[0]

		hash, err := generateHashOnCrd(crd)
		if err != nil || hash != "15875562a99becd18489077b6ac2a11407e3df36fd40f43d1580bf0b9f34610c" {
			t.Errorf("Failed to generate correctly, got %s", hash)
		}
	})

	t.Run("SumFilesFollowsDirSymlink", func(t *testing.T) {
		symMap, err := sha256Dir("sym_chart")
		if err != nil {
			t.Fatalf("sha256Dir(sym_chart): %v", err)
		}
		expandedMap, err := sha256Dir("sym_chart_expanded")
		if err != nil {
			t.Fatalf("sha256Dir(sym_chart_expanded): %v", err)
		}

		// Compare per-file content sums under matching relative paths.
		// generalHashFunction folds the absolute key into its hash, so we
		// can't just compare hex hashes - we have to strip the root prefix.
		stripPrefix := func(m map[string][32]byte, prefix string) map[string][32]byte {
			out := make(map[string][32]byte, len(m))
			for k, v := range m {
				rel, err := filepath.Rel(prefix, k)
				if err != nil {
					t.Fatalf("filepath.Rel(%q, %q): %v", prefix, k, err)
				}
				out[rel] = v
			}
			return out
		}
		got := stripPrefix(symMap, "sym_chart")
		want := stripPrefix(expandedMap, "sym_chart_expanded")

		gotKeys := mapKeys(got)
		wantKeys := mapKeys(want)
		if len(got) != len(want) {
			t.Fatalf("file count mismatch: sym_chart=%v expanded=%v", gotKeys, wantKeys)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("hash mismatch at %s: sym_chart=%x expanded=%x", k, got[k], v)
			}
		}
	})

	t.Run("SumFilesHandlesSymlinkCycle", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			_, err := sha256Dir("cycle_chart")
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("expected nil error for cycle_chart, got: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("sha256Dir on cycle_chart did not terminate within 5s - cycle detection broken")
		}
	})

	t.Run("SumFilesBrokenSymlinkErrors", func(t *testing.T) {
		_, err := sha256Dir("broken_symlink_chart")
		if err == nil {
			t.Error("expected error for broken symlink, got nil")
		}
	})

	t.Run("DifferenceInTwoDifferentFiles", func(t *testing.T) {
		data, err := Read("crdData_testfile.yaml")
		if err != nil {
			t.Error(err)
		}
		data2, err2 := Read("crdData_testfile_2.yaml")
		if err2 != nil {
			t.Error(err2)
		}

		crd1Hash, _ := generateHashOnCrd(data[0])
		crd2Hash, _ := generateHashOnCrd(data2[0])
		if crd1Hash == crd2Hash {
			t.Error("Failed to generate two different hashes")
		}
	})

	t.Run("GenerateHashOnChart", func(t *testing.T) {
		hash, _ := generalHashFunction("demo/charts/app-of-apps")
		h := hex.EncodeToString(hash)
		actualHash := "f57ffb63de221520249492e247beb6d6f61cd378e7c246909cca9c5110a6bb28"
		if h != actualHash {
			t.Errorf("Failed to generate a generic hash on a chart. got: %s wanted: %s", h, actualHash)
		}
	})

	t.Run("GenerateHash", func(t *testing.T) {
		testFiles := []struct {
			name            string
			file            string
			ignoreValueFile string
			hash            string
		}{
			{
				name:            "Generating hash for an Application",
				file:            "crdData_testfile_3.yaml",
				ignoreValueFile: "",
				hash:            "049f52ddf4e141f2374a8947d8221d4a55452a9c13d07c31d5a6c12fe6209849",
			},
			{
				name:            "Generate hash for an application with ignoreMissingValueFiles and a missing value file",
				file:            "crdData_testfile_4.yaml",
				ignoreValueFile: "",
				hash:            "654dde61d7c50f71a75d23f20950636750af3c7fac6b8a75de619499acdd3991",
			},
		}

		for _, tt := range testFiles {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				data, err := Read(tt.file)
				if err != nil {
					t.Error(err)
				}
				if len(data) != 1 {
					t.Fatal("This test can work only with one Application at a time")
				}
				crd := data[0]

				got, err := GenerateHash(crd, tt.ignoreValueFile)
				if err != nil {
					t.Error(err)
				}

				want := tt.hash //nolint:govet
				if got != want {
					t.Errorf("Failed to generate a correct hash on an overrides. got: %s want %s", got, want)
				}
			})
		}
	})

	t.Run("EmptyManifest", func(t *testing.T) {
		manifestErrors := []struct {
			manifest string
			name     string
			expected bool
			err      error
		}{
			{
				name:     "Check known empty file",
				manifest: "empty_manifest.yaml",
				expected: true,
				err:      nil,
			},
			{
				name:     "Check known non empty file",
				manifest: "crdData_multiple_crd_testfile.yaml",
				expected: false,
				err:      nil,
			},
			{
				name:     "Check missing file",
				manifest: "i_dont_exist.yaml",
				expected: false,
				err:      errors.New("stat i_dont_exist.yaml: no such file or directory"),
			},
		}

		for _, tt := range manifestErrors {
			t.Run(tt.name, func(t *testing.T) {
				got, err := EmptyManifest(tt.manifest)
				if !errors.Is(err, tt.err) {
					if !strings.Contains(err.Error(), "no such file or directory") {
						t.Errorf("unexpected error got: %v wanted: %v", tt.err, err)
					}
				}
				if got != tt.expected {
					t.Errorf("got: %t wanted: %t", got, tt.expected)
				}
			})
		}
	})
}
