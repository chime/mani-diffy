package helm

import (
	"encoding/hex"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
)

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

	t.Run("ResolvesTo", func(t *testing.T) {
		scenarios := []struct {
			name        string
			expected    string
			file        string
			isDirectory bool
		}{
			{
				name:        "symlinked file resolves to its target",
				expected:    "crdData_override_testfile.yaml",
				file:        "crdData_override_testfile_sym_link.yaml",
				isDirectory: false,
			},
			{
				name:        "regular directory returns own name",
				expected:    "nonSymDir",
				file:        "nonSymDir",
				isDirectory: true,
			},
			{
				name:        "symlinked directory returns its target",
				expected:    "nonSymDir",
				file:        "SymDir",
				isDirectory: true,
			},
			/*
				{
					name:        "fail to find",
					expected:    "nonSymDir",
					file:        "phantom",
					isDirectory: true,
				},
			*/
		}

		for _, tt := range scenarios {
			t.Run(tt.name, func(t *testing.T) {
				dataGot, err := resolvesTo(tt.file)
				if err != nil {
					t.Errorf("failed to resolve file err: %v", err)
				}
				if dataGot.fileName != tt.expected {
					t.Errorf("resolved files do not match. got: %s wanted: %s", dataGot.fileName, tt.expected)
				}
				if dataGot.isDir != tt.isDirectory {
					t.Errorf("failed checking directory status. got: %t wanted: %t", dataGot.isDir, tt.isDirectory)
				}
			})
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
		actualHash := "13aa148adefa3d633e5ce95584d3c95297a4417977837040cd67f0afbca17b5a"
		if h != actualHash {
			t.Errorf("Failed to generate a generic hash on a chart. got: %s wanted: %s", h, actualHash)
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
