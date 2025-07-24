package helm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
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
		isExplicitlyIgnored := ignoreValueFile != "" && strings.Contains(helmFiles[i], ignoreValueFile)
		isMissingAndShouldBeIgnored :=
			!fileExists(path.Join(payload.Spec.Source.Path, helmFiles[i])) &&
				payload.Spec.Source.Helm.IgnoreMissingValueFiles

		if !isExplicitlyIgnored && !isMissingAndShouldBeIgnored {
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

	crdHash, err := generateHashOnCrd(crd)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(finalHash, "%x\n", crdHash)

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
		matchDots := regexp.MustCompile(`\.\.\/`)
		for i := 0; i < len(overrideFiles); i++ {
			if ignoreValueFile == "" || !strings.Contains(overrideFiles[i], ignoreValueFile) {
				trimmedFilename := matchDots.ReplaceAllString(overrideFiles[i], "")
				oHashReturned, err := generalHashFunction(trimmedFilename)
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
	m, err := sha256Dir(dirFilepath)
	if err != nil {
		log.Println(err)
		return []byte{}, err
	}
	var paths []string
	for path := range m {
		paths = append(paths, path)
	}
	// Not sure if needed but I'm sorting for deterministic behavior
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		// if a single file, just return the hash
		if len(paths) == 1 {
			value := m[path]
			slice := value[:]
			return slice, nil
		}
		fmt.Fprintf(hash, "%x  %s\n", m[path], path)
	}
	// log.Printf("FINAL HASH: %v\n", hex.EncodeToString(hash.Sum(nil)))
	return hash.Sum(nil), nil
}

// A result is the product of reading and summing a file using MD5.
type result struct {
	path string
	sum  [sha256.Size]byte
	err  error
}

type nonRegularFile struct {
	fileName string
	isDir    bool
}

func resolvesTo(filePath string) (nonRegularFile, error) {
	fileData := nonRegularFile{}
	info, err := os.Lstat(filePath)
	if err != nil {
		return fileData, fmt.Errorf("failed to lstat file: %w", err)
	}

	if info.IsDir() {
		fileData.fileName = filePath
		fileData.isDir = true
		return fileData, nil
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		fileName, err := os.Readlink(filePath)
		if err != nil {
			return fileData, fmt.Errorf("failed to follow symlink: %w", err)
		}
		fileName = strings.ReplaceAll(filePath, info.Name(), fileName)
		fileData.fileName = fileName
		fileInfo, err := os.Lstat(fileName)
		if err != nil {
			return fileData, fmt.Errorf("failed to lstat file: %w", err)
		}
		if fileInfo.IsDir() {
			fileData.isDir = true
			return fileData, nil
		}
	}
	return fileData, nil
}

// sumFiles starts goroutines to walk the directory tree at root and digest each
// regular file.  These goroutines send the results of the digests on the result
// channel and send the result of the walk on the error channel.  If done is
// closed, sumFiles abandons its work.
func sumFiles(done <-chan struct{}, root string) (<-chan result, <-chan error) {
	// For each regular file, start a goroutine that sums the file and sends
	// the result on c.  Send the result of the walk on errc.
	c := make(chan result)
	errc := make(chan error, 1)
	go func() { // HL
		var wg sync.WaitGroup
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return fmt.Errorf("error walking the file path %s: %w", root, err)
			}
			if !info.Mode().IsRegular() {
				resolvedInfo, err := resolvesTo(root)
				if err != nil {
					return err
				}
				if resolvedInfo.isDir {
					// TODO: figure out how to handle dirs and
					// symlinked dirs
					return nil
				}
				path = resolvedInfo.fileName
			}
			wg.Add(1)
			go func() { // HL
				data, err := os.ReadFile(path)
				select {
				case c <- result{path, sha256.Sum256(data), err}: // HL
				case <-done: // HL
				}
				wg.Done()
			}()
			// Abort the walk if done is closed.
			select {
			case <-done: // HL
				return errors.New("walk canceled")
			default:
				return nil
			}
		})
		// Walk has returned, so all calls to wg.Add are done.  Start a
		// goroutine to close c once all the sends are done.
		go func() { // HL
			wg.Wait()
			close(c) // HL
		}()
		// No select needed here, since errc is buffered.
		errc <- err // HL
	}()
	return c, errc
}

// sha256Dir reads all the files in the file tree rooted at root and returns a map
// from file path to the sha256 sum of the file's contents.  If the directory walk
// fails or any read operation fails, sha256Dir returns an error.  In that case,
// sha256Dir does not wait for inflight read operations to complete.
func sha256Dir(root string) (map[string][sha256.Size]byte, error) {
	// sha256Dir closes the done channel when it returns; it may do so before
	// receiving all the values from c and errc.
	done := make(chan struct{}) // HLdone
	defer close(done)           // HLdone

	c, errc := sumFiles(done, root) // HLdone

	m := make(map[string][sha256.Size]byte)
	for r := range c { // HLrange
		if r.err != nil {
			return nil, r.err
		}
		m[r.path] = r.sum
	}
	if err := <-errc; err != nil {
		return nil, err
	}
	return m, nil
}

func generateHashOnCrd(crd *v1alpha1.Application) (string, error) {
	hash := sha256.New()
	crdString := crd.String()
	crdByte := []byte(crdString)
	if _, err := hash.Write(crdByte); err != nil {
		return "", fmt.Errorf("error generating hash for the %s crd: %w", crd.ObjectMeta.Name, err)
	}
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum), nil
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
