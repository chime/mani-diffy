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
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/chime/mani-diffy/pkg/kustomize"
	"github.com/chime/mani-diffy/pkg/randomvalues"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

// cliKubeVersion makes templates see the same capabilities as when rendered by
// the helm CLI release build of the linked helm.sh/helm/v3, and returns the
// Kubernetes version to render with.
var cliKubeVersion = sync.OnceValues(func() (chartutil.KubeVersion, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return chartutil.KubeVersion{}, errors.New("build info is not available")
	}
	helmVersion, kubeVersion, err := cliVersions(info)
	if err != nil {
		return chartutil.KubeVersion{}, err
	}
	chartutil.DefaultCapabilities.HelmVersion.Version = helmVersion
	return kubeVersion, nil
})

// cliVersions returns the versions that the helm CLI release build stamps into
// template capabilities: helm's own version, and the Kubernetes version
// matching its k8s.io/client-go dependency (client-go v0.32.x is Kubernetes
// 1.32).
func cliVersions(info *debug.BuildInfo) (string, chartutil.KubeVersion, error) {
	modules := map[string]string{}
	for _, dep := range info.Deps {
		modules[dep.Path] = dep.Version
		if dep.Replace != nil {
			modules[dep.Path] = dep.Replace.Version
		}
	}

	helmVersion, ok := modules["helm.sh/helm/v3"]
	if !ok {
		return "", chartutil.KubeVersion{}, errors.New("build info does not list helm.sh/helm/v3")
	}
	clientGoVersion, ok := modules["k8s.io/client-go"]
	if !ok {
		return "", chartutil.KubeVersion{}, errors.New("build info does not list k8s.io/client-go")
	}

	var major, minor int
	if _, err := fmt.Sscanf(clientGoVersion, "v%d.%d.", &major, &minor); err != nil {
		return "", chartutil.KubeVersion{}, fmt.Errorf("unexpected k8s.io/client-go version %s: %w", clientGoVersion, err)
	}
	kubeMajor, kubeMinor := strconv.Itoa(major+1), strconv.Itoa(minor)
	return helmVersion, chartutil.KubeVersion{
		Version: fmt.Sprintf("v%s.%s.0", kubeMajor, kubeMinor),
		Major:   kubeMajor,
		Minor:   kubeMinor,
	}, nil
}

var settings = cli.New()

// notRendered replaces the values that manifests leave out: the value of the
// skip-render key, and random values.
const notRendered = "CONSCIOUSLY_NOT_RENDERED"

// dependencyUpdates serializes dependency updates so that applications sharing
// a chart do not download its dependencies concurrently.
var dependencyUpdates sync.Mutex

// chartFiles caches the files of each chart directory by absolute path, so
// that the applications rendering the same chart read it from disk once per
// run.
var chartFiles sync.Map

// pathHashes caches the hash of each chart directory and value file by
// absolute path, so that the applications using them read them from disk once
// per run.
var pathHashes sync.Map

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
	setValues := ""

	for i := 0; i < len(helmParameters); i++ {
		setValues += fmt.Sprintf("%s=%s", helmParameters[i].Name, helmParameters[i].Value)
		if i != len(helmParameters)-1 {
			setValues += ","
		}

	}

	return setValues, strings.Join(valueFiles(payload, ignoreValueFile), ",")
}

// valueFiles returns the Application's value files that should be rendered,
// relative to the chart path.
func valueFiles(payload *v1alpha1.Application, ignoreValueFile string) []string {
	var files []string
	for _, f := range payload.Spec.Source.Helm.ValueFiles {
		if !ignoredValueFile(payload, f, ignoreValueFile) {
			files = append(files, f)
		}
	}
	return files
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

// loadedChart is a chart, and the files it was loaded from.
type loadedChart struct {
	*chart.Chart
	source chartSource
}

// loadChart loads a chart, installing its dependencies first if they are
// missing.
func loadChart(chartPath string) (loadedChart, error) {
	c, err := loadCachedChart(chartPath)
	if err != nil {
		return loadedChart{}, err
	}
	if err := checkIfInstallable(c.Chart); err != nil {
		return loadedChart{}, err
	}
	if !missingDependencies(c.Chart) {
		return c, nil
	}

	dependencyUpdates.Lock()
	defer dependencyUpdates.Unlock()

	// Another application using this chart may have installed them already.
	if c, err = readChart(chartPath); err != nil {
		return loadedChart{}, err
	}
	if !missingDependencies(c.Chart) {
		return c, nil
	}
	if err := installDependencies(chartPath); err != nil {
		return loadedChart{}, err
	}
	return readChart(chartPath)
}

// chartSource is the files of a chart directory as read from disk.
type chartSource struct {
	files []*loader.BufferedFile
	// callsRandomFunctions is whether any of the files has a template action
	// that calls a random function.
	callsRandomFunctions bool
}

// load loads a chart from the files.
func (s chartSource) load() (loadedChart, error) {
	c, err := loader.LoadFiles(s.files)
	if err != nil {
		return loadedChart{}, err
	}
	return loadedChart{Chart: c, source: s}, nil
}

// loadCachedChart loads a chart from its files as last read from disk, reading
// them if they haven't been. Each call returns a new chart, since rendering
// modifies it.
func loadCachedChart(chartPath string) (loadedChart, error) {
	key, err := filepath.Abs(chartPath)
	if err != nil {
		return loadedChart{}, err
	}
	read, _ := chartFiles.LoadOrStore(key, sync.OnceValues(func() (chartSource, error) {
		return readChartSource(chartPath)
	}))
	source, err := read.(func() (chartSource, error))()
	if err != nil {
		return loadedChart{}, err
	}
	return source.load()
}

// readChart loads a chart from disk, and caches its files for loadCachedChart.
func readChart(chartPath string) (loadedChart, error) {
	key, err := filepath.Abs(chartPath)
	if err != nil {
		return loadedChart{}, err
	}
	source, err := readChartSource(chartPath)
	if err != nil {
		return loadedChart{}, err
	}
	chartFiles.Store(key, func() (chartSource, error) { return source, nil })
	return source.load()
}

// readChartSource reads the files of a chart from disk.
func readChartSource(chartPath string) (chartSource, error) {
	c, err := loader.Load(chartPath)
	if err != nil {
		return chartSource{}, err
	}
	source := chartSource{files: make([]*loader.BufferedFile, len(c.Raw))}
	for i, f := range c.Raw {
		source.files[i] = &loader.BufferedFile{Name: f.Name, Data: f.Data}
		source.callsRandomFunctions = source.callsRandomFunctions || randomFunctionCall.Match(f.Data)
	}
	return source, nil
}

// randomFunctionCall matches a template action that calls a template function
// whose output is random or depends on the current time.
var randomFunctionCall = regexp.MustCompile(`\{\{[^}]*\b(randAlphaNum|randAlpha|randAscii|randNumeric|randBytes|randInt|shuffle|uuidv4|now|ago|genPrivateKey|genCA|genCAWithKey|genSelfSignedCert|genSelfSignedCertWithKey|genSignedCert|genSignedCertWithKey|bcrypt|htpasswd|encryptAES)\b`)

// valuesCallRandomFunctions reports whether any string in the values calls a
// random function in a template action. Values are plain data, but charts can
// render them as templates with tpl.
func valuesCallRandomFunctions(v interface{}) bool {
	switch v := v.(type) {
	case string:
		return randomFunctionCall.MatchString(v)
	case map[string]interface{}:
		for _, item := range v {
			if valuesCallRandomFunctions(item) {
				return true
			}
		}
	case []interface{}:
		for _, item := range v {
			if valuesCallRandomFunctions(item) {
				return true
			}
		}
	}
	return false
}

func checkIfInstallable(c *chart.Chart) error {
	switch c.Metadata.Type {
	case "", "application":
		return nil
	}
	return fmt.Errorf("%s charts are not installable", c.Metadata.Type)
}

func missingDependencies(c *chart.Chart) bool {
	return c.Metadata.Dependencies != nil && action.CheckDependencies(c, c.Metadata.Dependencies) != nil
}

func installDependencies(chartDirectory string) error {
	log.Println("Updating dependencies for " + chartDirectory)
	registryClient, err := registry.NewClient(
		registry.ClientOptEnableCache(true),
		registry.ClientOptWriter(io.Discard),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return fmt.Errorf("error creating registry client: %w", err)
	}

	manager := &downloader.Manager{
		Out:              io.Discard,
		ChartPath:        chartDirectory,
		Getters:          getter.All(settings),
		RegistryClient:   registryClient,
		RepositoryConfig: settings.RepositoryConfig,
		RepositoryCache:  settings.RepositoryCache,
	}
	if err := manager.Update(); err != nil {
		return fmt.Errorf("error updating dependencies for %s: %w", chartDirectory, err)
	}

	return nil
}

// release is an Application's chart and values, ready to be rendered.
type release struct {
	chartDir  string
	namespace string
	vals      map[string]interface{}
}

// prepareRelease reads the values to render an Application's chart with.
func prepareRelease(helmInfo *v1alpha1.Application, skipRenderKey string, ignoreValueFile string) (*release, error) {
	sourcePath := helmInfo.Spec.Source.Path
	chartPath := strings.Split(sourcePath, "/")
	chartDir := filepath.Join(sourcePath, "..", chartPath[len(chartPath)-1])

	setValues, _ := buildParams(helmInfo, ignoreValueFile)
	valueOpts := &values.Options{Values: []string{setValues}}

	for _, f := range valueFiles(helmInfo, ignoreValueFile) {
		if !filepath.IsAbs(f) {
			f = filepath.Join(sourcePath, f)
		}
		valueOpts.ValueFiles = append(valueOpts.ValueFiles, f)
	}

	if helmInfo.Spec.Source.Helm.Values != "" {
		dataFile, err := createTempFile(helmInfo.Spec.Source.Helm.Values)
		defer os.Remove(dataFile)
		if err != nil {
			log.Println(err)
		}
		valueOpts.ValueFiles = append(valueOpts.ValueFiles, dataFile)
	}

	if skipRenderKey != "" {
		valueOpts.Values = append(valueOpts.Values, fmt.Sprintf("%s=%s", skipRenderKey, notRendered))
	}

	vals, err := valueOpts.MergeValues(getter.All(settings))
	if err != nil {
		return nil, fmt.Errorf("error templating manifest: %w", err)
	}

	namespace := helmInfo.Spec.Destination.Namespace
	if namespace == "" {
		namespace = "default"
	}

	return &release{chartDir: chartDir, namespace: namespace, vals: vals}, nil
}

// render renders the chart, and reports whether its files call a random
// function.
func (r *release) render() (manifest []byte, chartCallsRandomFunctions bool, err error) {
	c, err := loadChart(r.chartDir)
	if err != nil {
		return nil, false, fmt.Errorf("error templating manifest: %w", err)
	}

	kubeVersion, err := cliKubeVersion()
	if err != nil {
		return nil, false, fmt.Errorf("error templating manifest: %w", err)
	}

	client := action.NewInstall(&action.Configuration{Log: func(string, ...interface{}) {}})
	client.DryRun = true
	client.DryRunOption = "true"
	client.ReleaseName = "release-name"
	client.Replace = true
	client.ClientOnly = true
	client.Namespace = r.namespace
	client.KubeVersion = &kubeVersion

	rel, err := client.Run(c.Chart, r.vals)
	if err != nil {
		return nil, false, fmt.Errorf("error templating manifest: %w", err)
	}

	var manifests bytes.Buffer
	fmt.Fprintln(&manifests, strings.TrimSpace(rel.Manifest))
	for _, m := range rel.Hooks {
		fmt.Fprintf(&manifests, "---\n# Source: %s\n%s\n", m.Path, m.Manifest)
	}

	return manifests.Bytes(), c.source.callsRandomFunctions, nil
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

// generalHashFunction returns the hash of a file, or of the files in a
// directory, as read from disk the first time it is asked for in a run.
func generalHashFunction(dirFilepath string) ([]byte, error) {
	key, err := filepath.Abs(dirFilepath)
	if err != nil {
		return nil, err
	}
	hash, _ := pathHashes.LoadOrStore(key, sync.OnceValues(func() ([]byte, error) {
		return hashPath(dirFilepath)
	}))
	return hash.(func() ([]byte, error))()
}

func hashPath(dirFilepath string) ([]byte, error) {
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

// sumFiles starts goroutines to walk the directory tree at root and digest each
// regular file.  These goroutines send the results of the digests on the result
// channel and send the result of the walk on the error channel.  If done is
// closed, sumFiles abandons its work.
func sumFiles(done <-chan struct{}, root string) (<-chan result, <-chan error) {
	c := make(chan result)
	errc := make(chan error, 1)
	go func() {
		var wg sync.WaitGroup

		hashFile := func(keyPath, readPath string) {
			wg.Add(1)
			go func() {
				data, err := os.ReadFile(readPath)
				select {
				case c <- result{keyPath, sha256.Sum256(data), err}:
				case <-done:
				}
				wg.Done()
			}()
		}

		// filepath.Walk does not descend into directory symlinks, so when
		// one is encountered its resolved target is enqueued as its own
		// walk job. The "logical" path (as seen through the symlink) is
		// used for hash keys; the "physical" path (after resolution) is
		// used for cycle detection.
		type job struct{ logical, physical string }
		visited := map[string]struct{}{filepath.Clean(root): {}}
		queue := []job{{logical: root, physical: root}}

		var walkErr error
		for len(queue) > 0 && walkErr == nil {
			j := queue[0]
			queue = queue[1:]

			walkErr = filepath.Walk(j.physical, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				select {
				case <-done:
					return errors.New("walk canceled")
				default:
				}

				rel, err := filepath.Rel(j.physical, path)
				if err != nil {
					return err
				}
				logical := filepath.Join(j.logical, rel)

				// For symlinks, follow once with Stat so symlink-to-file
				// is treated like a regular file. Broken symlinks error
				// here, propagating out and failing the hash.
				target := info
				if info.Mode()&fs.ModeSymlink != 0 {
					target, err = os.Stat(path)
					if err != nil {
						return err
					}
				}

				switch {
				case info.Mode()&fs.ModeSymlink != 0 && target.IsDir():
					resolved, err := filepath.EvalSymlinks(path)
					if err != nil {
						return err
					}
					if _, seen := visited[resolved]; seen {
						log.Printf("sumFiles: skipping symlink cycle at %s -> %s", logical, resolved)
						return nil
					}
					visited[resolved] = struct{}{}
					queue = append(queue, job{logical: logical, physical: resolved})
				case target.Mode().IsRegular():
					hashFile(logical, path)
				}
				return nil
			})
		}

		go func() {
			wg.Wait()
			close(c)
		}()
		errc <- walkErr
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

// Rendering is how an Application's chart was rendered.
type Rendering struct {
	// Twice is whether the chart was rendered twice, to mask its random values.
	Twice bool
}

// Run renders an Application's chart into output. When maskRandomValues is set
// and the chart or its values call a random function, the chart is rendered
// twice, and the values that differ between the renders are masked.
func Run(crd *v1alpha1.Application, output string, skipRenderKey string, ignoreValueFile string, maskRandomValues bool) (Rendering, error) {
	manifest, rendering, err := template(crd, skipRenderKey, ignoreValueFile, maskRandomValues)
	if err != nil {
		log.Printf(
			"error generating manifest for %s error: %v\n",
			crd.ObjectMeta.Name,
			err,
		)
		return Rendering{}, err
	}
	return rendering, writeToFile(manifest, output)
}

// template renders an Application's chart the same way as
// `helm template <chart> -n <namespace> -f <value files> --set <parameters>`
// run from the chart directory.
func template(crd *v1alpha1.Application, skipRenderKey string, ignoreValueFile string, maskRandomValues bool) ([]byte, Rendering, error) {
	r, err := prepareRelease(crd, skipRenderKey, ignoreValueFile)
	if err != nil {
		return nil, Rendering{}, err
	}
	manifest, chartCallsRandomFunctions, err := r.render()
	if err != nil {
		return nil, Rendering{}, err
	}
	if !maskRandomValues || !(chartCallsRandomFunctions || valuesCallRandomFunctions(r.vals)) {
		return manifest, Rendering{}, nil
	}

	second, _, err := r.render()
	if err != nil {
		return nil, Rendering{}, err
	}
	manifest, masked, err := randomvalues.Mask(manifest, second, notRendered)
	if err != nil {
		return nil, Rendering{}, fmt.Errorf("error masking random values in %s: %w", crd.ObjectMeta.Name, err)
	}
	if len(masked) > 0 {
		log.Printf("Masked random values in %s: %s", crd.ObjectMeta.Name, strings.Join(masked, ", "))
	}
	return manifest, Rendering{Twice: true}, nil
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
