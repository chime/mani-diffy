package helm

import (
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
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
		_, err = template(crdSpec, "", "")
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

		manifest, _ := template(crdSpec, "", "")
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
		manifest, _ := template(app, "appTag", "")

		// Verify the rendered manifest contains the override
		if !strings.Contains(string(manifest), "appTag: CONSCIOUSLY_NOT_RENDERED") {
			t.Errorf("Expected override not found in rendered manifest")

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
		if err != nil || hash != "7bfd65e963e76680dc5160b6a55c04c3d9780c84aee1413ae710e4b5279cfe14" {
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
				hash:            "7ce0306f218c7147b388dbb1ec2fa78389e44ebd11ca31b208e085b82158d787",
			},
			{
				name:            "Generate hash for an application with ignoreMissingValueFiles and a missing value file",
				file:            "crdData_testfile_4.yaml",
				ignoreValueFile: "",
				hash:            "349f7da89b1c47362663daedb6fcc28980d017613a9cb9da7d53bb852467fa6c",
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

	t.Run("IsMissingDependencyErr", func(t *testing.T) {
		templateErrors := []struct {
			name       string
			err        error
			dependency bool
		}{
			{
				name: "Missing charts",
				err: errors.New(
					"Error: found in Chart.yaml, but missing in charts/ directory: postgresql",
				),
				dependency: true,
			},
			{
				name: "Missing requirements",
				err: errors.New(
					"Error: found in requirements.yaml, but missing in charts",
				),
				dependency: true,
			},
			{
				name: "Chart error",
				err: errors.New(
					"no such file or directory",
				),
				dependency: false,
			},
		}

		for _, tt := range templateErrors {
			t.Run(tt.name, func(t *testing.T) {
				got := IsMissingDependencyErr(tt.err)
				if got != tt.dependency {
					t.Errorf("%v got %t wanted %t", tt.name, got, tt.dependency)
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
