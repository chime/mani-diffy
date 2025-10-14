package helm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/chime/mani-diffy/pkg/digest"
	"github.com/chime/mani-diffy/pkg/kustomize"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func VerifyRenderDir(autoGenerationPath string) error {
	if _, err := os.Stat(autoGenerationPath); errors.Is(err, os.ErrNotExist) {
		if err := CreateDir(autoGenerationPath); err != nil {
			return fmt.Errorf("error creating render directory: %w", err)
		}
	}
	return nil
}

func CreateDir(dirName string) error {
	err := os.MkdirAll(dirName, os.ModePerm)
	if err != nil {
		return fmt.Errorf("error creating directory: %s %w", dirName, err)
	}
	return nil
}

func buildParams(payload *v1alpha1.Application, ignoreValueFile string) (string, string) {
	helmParameters := payload.Spec.Source.Helm.Parameters
	helmFiles := payload.Spec.Source.Helm.ValueFiles
	setValues := ""
	fileValues := ""

	for i := 0; i < len(helmParameters); i++ {
		setValues += fmt.Sprintf("%s=%s", helmParameters[i].Name, helmParameters[i].Value)
		if i != len(helmParameters)-1 {
			setValues += ","
		}

	}
	for i := 0; i < len(helmFiles); i++ {
		if !ignoredValueFile(payload, helmFiles[i], ignoreValueFile) {
			fileValues += fmt.Sprintf("%s,", helmFiles[i])
		}
	}
	fileValues = strings.TrimRight(fileValues, ",")

	return setValues, fileValues
}

func createTempFile(payload string) (string, error) {
	// create a temp file with the results of a yaml block:
	tmpYamlFile, err := os.CreateTemp("", "temp.*.yaml")
	if err != nil {
		return "", fmt.Errorf("error creating temp file: %w", err)
	}

	if _, err := tmpYamlFile.Write([]byte(payload)); err != nil {
		return "", err
	}
	if err := tmpYamlFile.Close(); err != nil {
		return "", err
	}

	return tmpYamlFile.Name(), nil
}

func IsMissingDependencyErr(err error) bool {
	return strings.Contains(err.Error(), "found in requirements.yaml, but missing in charts") ||
		strings.Contains(err.Error(), "found in Chart.yaml, but missing in charts/ directory")
}

func installDependencies(chartDirectory string) error {
	log.Println("Updating dependencies for " + chartDirectory)
	cmd := exec.Command(
		"helm",
		"dependency",
		"update",
	)
	cmd.Dir = chartDirectory
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error updating dependencies for %s: %w", chartDirectory, err)
	}

	return nil

}

func template(helmInfo *v1alpha1.Application, skipRenderKey string, ignoreValueFile string) ([]byte, error) {

	chartPath := strings.Split(helmInfo.Spec.Source.Path, "/")
	chart := fmt.Sprint("../" + chartPath[len(chartPath)-1])

	setValues, fileValues := buildParams(helmInfo, ignoreValueFile)

	tmpFile := ""
	if helmInfo.Spec.Source.Helm.Values != "" {
		dataFile, err := createTempFile(helmInfo.Spec.Source.Helm.Values)
		defer os.Remove(dataFile)
		if err != nil {
			log.Println(err)
		}
		tmpFile = dataFile
	}

	cmd := exec.Command(
		"helm",
		"template",
		chart,
		"--set",
		setValues,
		"-f",
		fileValues,
		"-f",
		tmpFile,
		"-n",
		helmInfo.Spec.Destination.Namespace,
	)

	if skipRenderKey != "" {
		cmd.Args = append(cmd.Args, "--set", fmt.Sprintf("%s=%s", skipRenderKey, "CONSCIOUSLY_NOT_RENDERED"))
	}

	cmd.Dir = helmInfo.Spec.Source.Path

	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	if err := cmd.Run(); err != nil {
		if IsMissingDependencyErr(errors.New(errb.String())) {
			if err := installDependencies(helmInfo.Spec.Source.Path); err != nil {
				return template(helmInfo, skipRenderKey, ignoreValueFile)
			}
		} else {
			return []byte{}, fmt.Errorf("error templating manifest: %w %v", err, errb.String())
		}
	}

	return outb.Bytes(), nil
}

func writeToFile(manifest []byte, location string) error {
	if err := CreateDir(location); err != nil {
		return err
	}

	return os.WriteFile(
		fmt.Sprintf(
			"%s/%s",
			location,
			"manifest.yaml",
		),
		manifest,
		0664,
	)
}

func EmptyManifest(manifest string) (bool, error) {
	fileInfo, err := os.Stat(manifest)
	if err != nil {
		if strings.Contains(err.Error(), "manifest.yaml: no such file or directory") {
			// the root dirs don't have manifest.yaml files
			return false, nil
		}
		return false, fmt.Errorf("error checking if %s is empty: %w", manifest, err)
	}

	if fileInfo.Size() == 0 {
		return true, nil
	}

	return false, nil

}

func GenerateHash(crd *v1alpha1.Application, ignoreValueFile string) (string, error) {
	finalHash := sha256.New()

	if _, err := finalHash.Write([]byte(crd.String())); err != nil {
		return "", err
	}

	if crd.Spec.Source.Kustomize != nil {
		return "", kustomize.ErrNotSupported
	}

	if crd.Spec.Source.Path != "" {
		chartHash, err := generalHashFunction(crd.Spec.Source.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(finalHash, "%x\n", chartHash)
	}

	if crd.Spec.Source.Helm != nil && len(crd.Spec.Source.Helm.ValueFiles) > 0 {
		oHash := sha256.New()
		overrideFiles := crd.Spec.Source.Helm.ValueFiles
		for i := 0; i < len(overrideFiles); i++ {
			if !ignoredValueFile(crd, overrideFiles[i], ignoreValueFile) {
				oHashReturned, err := generalHashFunction(path.Join(crd.Spec.Source.Path, overrideFiles[i]))
				if err != nil {
					return "", err
				}
				fmt.Fprintf(oHash, "%x\n", oHashReturned)
			}
		}
		overrideHash := oHash.Sum(nil)
		fmt.Fprintf(finalHash, "%x\n", overrideHash)
	}

	return hex.EncodeToString(finalHash.Sum(nil)), nil
}

func generalHashFunction(dirFilepath string) ([]byte, error) {
	h := sha256.New()
	if err := digest.Digest(dirFilepath, h); err != nil {
		log.Println(err)
		return []byte{}, err
	}
	return h.Sum(nil), nil
}

func Run(crd *v1alpha1.Application, output string, skipRenderKey string, ignoreValueFile string) error {
	manifest, err := template(crd, skipRenderKey, ignoreValueFile)
	if err != nil {
		log.Printf(
			"error generating manifest for %s error: %v\n",
			crd.ObjectMeta.Name,
			string(manifest),
		)
		return err
	}
	err = writeToFile(manifest, output)
	return err
}

func Read(inputCRD string) ([]*v1alpha1.Application, error) {
	crdSpecs := make([]*v1alpha1.Application, 0)
	yamlFile, err := os.ReadFile(inputCRD)
	if err != nil {
		// log.Fatalf("Error reading crd: %s %v", inputCRD, err)
		return crdSpecs, fmt.Errorf("error reading crd: %s %w", inputCRD, err)
	}

	dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(yamlFile), 1000)
	for {
		app := v1alpha1.Application{}
		if err := dec.Decode(&app); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// panic(fmt.Errorf("document decode failed: %w", err))
			return crdSpecs, fmt.Errorf("document decode failed: %w", err)
		}
		crdSpecs = append(crdSpecs, &app)
	}

	return crdSpecs, nil
}

func ignoredValueFile(crd *v1alpha1.Application, f string, ignoreValueFile string) bool {
	isExplicitlyIgnored := ignoreValueFile != "" && strings.Contains(f, ignoreValueFile)
	isMissingAndShouldBeIgnored :=
		!fileExists(path.Join(crd.Spec.Source.Path, f)) &&
			crd.Spec.Source.Helm.IgnoreMissingValueFiles

	return isExplicitlyIgnored || isMissingAndShouldBeIgnored
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
