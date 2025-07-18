package vcr

import (
	"fmt"
	"io/fs"
	"magician/provider"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Result struct {
	PassedTests     []string
	SkippedTests    []string
	FailedTests     []string
	PassedSubtests  []string
	SkippedSubtests []string
	FailedSubtests  []string
	Panics          []string
}

type Mode int

const (
	Replaying Mode = iota
	Recording
)

const numModes = 2

func (m Mode) Lower() string {
	switch m {
	case Replaying:
		return "replaying"
	case Recording:
		return "recording"
	}
	return "unknown"
}

func (m Mode) Upper() string {
	return strings.ToUpper(m.Lower())
}

type logKey struct {
	mode    Mode
	version provider.Version
}

type Tester struct {
	env            map[string]string           // shared environment variables for running tests
	rnr            ExecRunner                  // for running commands and manipulating files
	cassetteBucket string                      // name of GCS bucket to store cassettes
	logBucket      string                      // name of GCS bucket to store logs
	baseDir        string                      // the directory in which this tester was created
	saKeyPath      string                      // where sa_key.json is relative to baseDir
	cassettePaths  map[provider.Version]string // where cassettes are relative to baseDir by version
	logPaths       map[logKey]string           // where logs are relative to baseDir by version and mode
	repoPaths      map[provider.Version]string // relative paths of already cloned repos by version
}

const accTestParallelism = 32
const parallelJobs = 16

const replayingTimeout = "240m"

var testResultsExpression = regexp.MustCompile(`(?m:^--- (PASS|FAIL|SKIP): (TestAcc\w+))`)

var subtestResultsExpression = regexp.MustCompile(`(?m:^    --- (PASS|FAIL|SKIP): (TestAcc\w+)/(\w+))`)

var testPanicExpression = regexp.MustCompile(`^panic: .*`)

var safeToLog = map[string]bool{
	"ACCTEST_PARALLELISM":                        true,
	"COMMIT_SHA":                                 true,
	"GITHUB_TOKEN":                               false,
	"GITHUB_TOKEN_CLASSIC":                       false,
	"GITHUB_TOKEN_DOWNSTREAMS":                   false,
	"GITHUB_TOKEN_MAGIC_MODULES":                 false,
	"GOCACHE":                                    true,
	"GOOGLE_APPLICATION_CREDENTIALS":             false,
	"GOOGLE_BILLING_ACCOUNT":                     false,
	"GOOGLE_CHRONICLE_INSTANCE_ID":               true,
	"GOOGLE_CREDENTIALS":                         false,
	"GOOGLE_CUST_ID":                             true,
	"GOOGLE_IDENTITY_USER":                       true,
	"GOOGLE_MASTER_BILLING_ACCOUNT":              false,
	"GOOGLE_ORG":                                 true,
	"GOOGLE_ORG_2":                               true,
	"GOOGLE_ORG_DOMAIN":                          true,
	"GOOGLE_PROJECT":                             true,
	"GOOGLE_PROJECT_NUMBER":                      true,
	"GOOGLE_PUBLIC_AVERTISED_PREFIX_DESCRIPTION": true,
	"GOOGLE_REGION":                              true,
	"GOOGLE_SERVICE_ACCOUNT":                     true,
	"GOOGLE_TEST_DIRECTORY":                      true,
	"GOOGLE_VMWAREENGINE_PROJECT":                true,
	"GOOGLE_ZONE":                                true,
	"GOPATH":                                     true,
	"HOME":                                       true,
	"PATH":                                       true,
	"SA_KEY":                                     false,
	"TF_ACC":                                     true,
	"TF_LOG":                                     true,
	"TF_LOG_CORE":                                true,
	"TF_LOG_PATH_MASK":                           true,
	"TF_LOG_SDK_FRAMEWORK":                       true,
	"TF_SCHEMA_PANIC_ON_ERROR":                   true,
	"USER":                                       true,
	"VCR_MODE":                                   true,
	"VCR_PATH":                                   true,
} // true if shown, false if hidden (default false)

// Create a new tester in the current working directory and write the service account key file.
func NewTester(env map[string]string, cassetteBucket, logBucket string, rnr ExecRunner) (*Tester, error) {
	var saKeyPath string
	if saKeyVal, ok := env["SA_KEY"]; ok {
		saKeyPath = "sa_key.json"
		if err := rnr.WriteFile(saKeyPath, saKeyVal); err != nil {
			return nil, err
		}
	}
	return &Tester{
		env:            env,
		rnr:            rnr,
		cassetteBucket: cassetteBucket,
		logBucket:      logBucket,
		baseDir:        rnr.GetCWD(),
		saKeyPath:      saKeyPath,
		cassettePaths:  make(map[provider.Version]string, provider.NumVersions),
		logPaths:       make(map[logKey]string, provider.NumVersions*numModes),
		repoPaths:      make(map[provider.Version]string, provider.NumVersions),
	}, nil
}

func (vt *Tester) SetRepoPath(version provider.Version, repoPath string) {
	vt.repoPaths[version] = repoPath
}

// Fetch the cassettes for the current version if not already fetched.
// Should be run from the base dir.
func (vt *Tester) FetchCassettes(version provider.Version, baseBranch, head string) error {
	if _, cassettesAlreadyFetched := vt.cassettePaths[version]; cassettesAlreadyFetched {
		return nil
	}
	cassettePath := filepath.Join(vt.baseDir, "cassettes", version.String())
	vt.rnr.Mkdir(cassettePath)
	if baseBranch != "FEATURE-BRANCH-major-release-6.0.0" {
		// pull main cassettes (major release uses branch specific casssettes as primary ones)
		bucketPath := fmt.Sprintf("gs://%s/%sfixtures/*", vt.cassetteBucket, version.BucketPath())
		if err := vt.fetchBucketPath(bucketPath, cassettePath); err != nil {
			fmt.Println("Error fetching cassettes: ", err)
		}
	}
	if baseBranch != "main" {
		bucketPath := fmt.Sprintf("gs://%s/%srefs/branches/%s/fixtures/*", vt.cassetteBucket, version.BucketPath(), baseBranch)
		if err := vt.fetchBucketPath(bucketPath, cassettePath); err != nil {
			fmt.Println("Error fetching cassettes: ", err)
		}
	}
	if head != "" {
		bucketPath := fmt.Sprintf("gs://%s/%srefs/heads/%s/fixtures/*", vt.cassetteBucket, version.BucketPath(), head)
		if err := vt.fetchBucketPath(bucketPath, cassettePath); err != nil {
			fmt.Println("Error fetching cassettes: ", err)
		}
	}
	vt.cassettePaths[version] = cassettePath
	return nil
}

func (vt *Tester) fetchBucketPath(bucketPath, cassettePath string) error {
	// Fetch the cassettes.
	args := []string{"-m", "-q", "cp", bucketPath, cassettePath}
	fmt.Println("Fetching cassettes:\n", "gsutil", strings.Join(args, " "))
	if _, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		return fmt.Errorf("error running gsutil: %v", err)
	}
	return nil
}

// CassettePath returns the local cassette path.
func (vt *Tester) CassettePath(version provider.Version) string {
	return vt.cassettePaths[version]
}

// LogPath returns the local log path.
func (vt *Tester) LogPath(mode Mode, version provider.Version) string {
	lgky := logKey{mode, version}
	return vt.logPaths[lgky]
}

type RunOptions struct {
	Mode     Mode
	Version  provider.Version
	TestDirs []string
	Tests    []string
}

// Run the vcr tests in the given mode and provider version and return the result.
// This will overwrite any existing logs for the given mode and version.
func (vt *Tester) Run(opt RunOptions) (Result, error) {
	logPath, err := vt.makeLogPath(opt.Mode, opt.Version)
	if err != nil {
		return Result{}, err
	}

	repoPath, ok := vt.repoPaths[opt.Version]
	if !ok {
		return Result{}, fmt.Errorf("no repo cloned for version %s in %v", opt.Version, vt.repoPaths)
	}
	if err := vt.rnr.PushDir(repoPath); err != nil {
		return Result{}, err
	}
	if len(opt.TestDirs) == 0 {
		var err error
		opt.TestDirs, err = vt.googleTestDirectory()
		if err != nil {
			return Result{}, err
		}

	}

	cassettePath := filepath.Join(vt.baseDir, "cassettes", opt.Version.String())
	switch opt.Mode {
	case Replaying:
		cassettePath, ok = vt.cassettePaths[opt.Version]
		if !ok {
			return Result{}, fmt.Errorf("cassettes not fetched for version %s", opt.Version)
		}
	case Recording:
		if err := vt.rnr.RemoveAll(cassettePath); err != nil {
			return Result{}, fmt.Errorf("error removing cassettes: %v", err)
		}
		if err := vt.rnr.Mkdir(cassettePath); err != nil {
			return Result{}, fmt.Errorf("error creating cassette dir: %v", err)
		}
		vt.cassettePaths[opt.Version] = cassettePath
	}

	args := []string{"test"}
	args = append(args, opt.TestDirs...)
	args = append(args,
		"-parallel",
		strconv.Itoa(accTestParallelism),
		"-v",
		"-run=TestAcc",
		"-timeout",
		replayingTimeout,
		"-ldflags=-X=github.com/hashicorp/terraform-provider-google-beta/version.ProviderVersion=acc",
		"-vet=off",
	)
	env := map[string]string{
		"VCR_PATH":                 cassettePath,
		"VCR_MODE":                 opt.Mode.Upper(),
		"ACCTEST_PARALLELISM":      strconv.Itoa(accTestParallelism),
		"GOOGLE_CREDENTIALS":       vt.env["SA_KEY"],
		"GOOGLE_TEST_DIRECTORY":    strings.Join(opt.TestDirs, " "),
		"TF_LOG":                   "DEBUG",
		"TF_LOG_CORE":              "WARN",
		"TF_LOG_SDK_FRAMEWORK":     "INFO",
		"TF_LOG_PATH_MASK":         filepath.Join(logPath, "%s.log"),
		"TF_ACC":                   "1",
		"TF_SCHEMA_PANIC_ON_ERROR": "1",
	}
	if vt.saKeyPath != "" {
		env["GOOGLE_APPLICATION_CREDENTIALS"] = filepath.Join(vt.baseDir, vt.saKeyPath)
	}
	for ev, val := range vt.env {
		env[ev] = val
	}
	var printedEnv string
	for ev, val := range env {
		if !safeToLog[ev] {
			val = "{hidden}"
		}
		printedEnv += fmt.Sprintf("%s=%s\n", ev, val)
	}
	fmt.Printf(`Running go:
	env:
%v
	args:
%s
`, printedEnv, strings.Join(args, " "))
	output, testErr := vt.rnr.Run("go", args, env)
	if testErr != nil {
		// Use error as output for log.
		output = fmt.Sprintf("Error %s tests:\n%v", opt.Mode.Lower(), testErr)
	}
	// Leave repo directory.
	if err := vt.rnr.PopDir(); err != nil {
		return Result{}, err
	}

	logFileName := filepath.Join(vt.baseDir, "testlogs", fmt.Sprintf("%s_test.log", opt.Mode.Lower()))
	// Write output (or error) to test log.
	// Append to existing log file.
	allOutput, _ := vt.rnr.ReadFile(logFileName)
	if allOutput != "" {
		allOutput += "\n"
	}
	allOutput += output
	if err := vt.rnr.WriteFile(logFileName, allOutput); err != nil {
		return Result{}, fmt.Errorf("error writing log: %v, test output: %v", err, allOutput)
	}
	return collectResult(output), testErr
}

func (vt *Tester) RunParallel(opt RunOptions) (Result, error) {
	logPath, err := vt.makeLogPath(opt.Mode, opt.Version)
	if err != nil {
		return Result{}, err
	}
	if err := vt.rnr.Mkdir(filepath.Join(vt.baseDir, "testlogs", opt.Mode.Lower()+"_build")); err != nil {
		return Result{}, err
	}
	repoPath, ok := vt.repoPaths[opt.Version]
	if !ok {
		return Result{}, fmt.Errorf("no repo cloned for version %s in %v", opt.Version, vt.repoPaths)
	}
	if err := vt.rnr.PushDir(repoPath); err != nil {
		return Result{}, err
	}
	if len(opt.TestDirs) == 0 {
		var err error
		opt.TestDirs, err = vt.googleTestDirectory()
		if err != nil {
			return Result{}, err
		}
	}

	cassettePath := filepath.Join(vt.baseDir, "cassettes", opt.Version.String())
	switch opt.Mode {
	case Replaying:
		cassettePath, ok = vt.cassettePaths[opt.Version]
		if !ok {
			return Result{}, fmt.Errorf("cassettes not fetched for version %s", opt.Version)
		}
	case Recording:
		if err := vt.rnr.RemoveAll(cassettePath); err != nil {
			return Result{}, fmt.Errorf("error removing cassettes: %v", err)
		}
		if err := vt.rnr.Mkdir(cassettePath); err != nil {
			return Result{}, fmt.Errorf("error creating cassette dir: %v", err)
		}
		vt.cassettePaths[opt.Version] = cassettePath
	}

	running := make(chan struct{}, parallelJobs)
	outputs := make(chan string, len(opt.TestDirs)*len(opt.Tests))
	wg := &sync.WaitGroup{}
	wg.Add(len(opt.TestDirs) * len(opt.Tests))
	errs := make(chan error, len(opt.TestDirs)*len(opt.Tests)*2)
	for _, testDir := range opt.TestDirs {
		for _, test := range opt.Tests {
			running <- struct{}{}
			go vt.runInParallel(opt.Mode, opt.Version, testDir, test, logPath, cassettePath, running, wg, outputs, errs)
		}
	}

	wg.Wait()

	close(outputs)
	close(errs)

	// Leave repo directory.
	if err := vt.rnr.PopDir(); err != nil {
		return Result{}, err
	}
	var output string
	for otpt := range outputs {
		output += otpt
	}
	logFileName := filepath.Join(vt.baseDir, "testlogs", fmt.Sprintf("%s_test.log", opt.Mode.Lower()))
	if err := vt.rnr.WriteFile(logFileName, output); err != nil {
		return Result{}, err
	}
	var testErr error
	for err := range errs {
		if err != nil {
			testErr = err
			break
		}
	}
	return collectResult(output), testErr
}

func (vt *Tester) runInParallel(mode Mode, version provider.Version, testDir, test, logPath, cassettePath string, running <-chan struct{}, wg *sync.WaitGroup, outputs chan<- string, errs chan<- error) {
	args := []string{
		"test",
		testDir,
		"-parallel",
		"1",
		"-v",
		"-run=" + test + "$",
		"-timeout",
		replayingTimeout,
		"-ldflags=-X=github.com/hashicorp/terraform-provider-google-beta/version.ProviderVersion=acc",
		"-vet=off",
	}
	env := map[string]string{
		"VCR_PATH":                 cassettePath,
		"VCR_MODE":                 mode.Upper(),
		"ACCTEST_PARALLELISM":      "1",
		"GOOGLE_CREDENTIALS":       vt.env["SA_KEY"],
		"GOOGLE_TEST_DIRECTORY":    testDir,
		"TF_LOG":                   "DEBUG",
		"TF_LOG_CORE":              "WARN",
		"TF_LOG_SDK_FRAMEWORK":     "INFO",
		"TF_LOG_PATH_MASK":         filepath.Join(logPath, "%s.log"),
		"TF_ACC":                   "1",
		"TF_SCHEMA_PANIC_ON_ERROR": "1",
	}
	if vt.saKeyPath != "" {
		env["GOOGLE_APPLICATION_CREDENTIALS"] = filepath.Join(vt.baseDir, vt.saKeyPath)
	}
	for ev, val := range vt.env {
		env[ev] = val
	}
	output, testErr := vt.rnr.Run("go", args, env)
	outputs <- output
	if testErr != nil {
		// Use error as output for log.
		output = fmt.Sprintf("Error %s tests:\n%v", mode.Lower(), testErr)
		errs <- testErr
	}
	logFileName := filepath.Join(vt.baseDir, "testlogs", mode.Lower()+"_build", fmt.Sprintf("%s_%s_test.log", test, mode.Lower()))
	// Write output (or error) to test log.
	// Append to existing log file.
	previousLog, _ := vt.rnr.ReadFile(logFileName)
	if previousLog != "" {
		output = previousLog + "\n" + output
	}
	if err := vt.rnr.WriteFile(logFileName, output); err != nil {
		errs <- fmt.Errorf("error writing log: %v, test output: %v", err, output)
	}
	<-running
	wg.Done()
}

func (vt *Tester) makeLogPath(mode Mode, version provider.Version) (string, error) {
	lgky := logKey{mode, version}
	logPath, ok := vt.logPaths[lgky]
	if !ok {
		// We've never run this mode and version.
		logPath = filepath.Join(vt.baseDir, "testlogs", mode.Lower(), version.String())
		if err := vt.rnr.Mkdir(logPath); err != nil {
			return "", err
		}
		vt.logPaths[lgky] = logPath
	}
	return logPath, nil
}

type UploadLogsOptions struct {
	Head           string
	BuildID        string
	Parallel       bool
	AfterRecording bool
	Mode           Mode
	Version        provider.Version
}

func (vt *Tester) UploadLogs(opts UploadLogsOptions) error {
	bucketPath := fmt.Sprintf("gs://%s/%s/", vt.logBucket, opts.Version)
	if opts.Head != "" {
		bucketPath += fmt.Sprintf("refs/heads/%s/", opts.Head)
	}
	if opts.BuildID != "" {
		bucketPath += fmt.Sprintf("artifacts/%s/", opts.BuildID)
	}
	lgky := logKey{opts.Mode, opts.Version}
	logPath, ok := vt.logPaths[lgky]
	if !ok {
		return fmt.Errorf("no log path found for mode %s and version %s", opts.Mode.Lower(), opts.Version)
	}
	var suffix string
	if opts.AfterRecording {
		suffix = "_after_recording"
	}
	args := []string{
		"-h",
		"Content-Type:text/plain",
		"-q",
		"cp",
		"-r",
		filepath.Join(vt.baseDir, "testlogs", fmt.Sprintf("%s_test.log", opts.Mode.Lower())),
		fmt.Sprintf("%sbuild-log/%s_test%s.log", bucketPath, opts.Mode.Lower(), suffix),
	}
	fmt.Println("Uploading build log:\n", "gsutil", strings.Join(args, " "))
	if out, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		fmt.Println("Error uploading build log: ", err)
	} else {
		fmt.Println("gsutil output: ", out)
	}
	if opts.Parallel {
		args := []string{
			"-h",
			"Content-Type:text/plain",
			"-m",
			"-q",
			"cp",
			"-r",
			filepath.Join(vt.baseDir, "testlogs", opts.Mode.Lower()+"_build", "*"),
			fmt.Sprintf("%sbuild-log/%s_build%s/", bucketPath, opts.Mode.Lower(), suffix),
		}
		fmt.Println("Uploading build logs:\n", "gsutil", strings.Join(args, " "))
		if _, err := vt.rnr.Run("gsutil", args, nil); err != nil {
			fmt.Println("Error uploading build logs: ", err)
		}
	}
	args = []string{
		"-h",
		"Content-Type:text/plain",
		"-m",
		"-q",
		"cp",
		"-r",
		filepath.Join(logPath, "*"),
		fmt.Sprintf("%s%s%s/", bucketPath, opts.Mode.Lower(), suffix),
	}
	fmt.Println("Uploading logs:\n", "gsutil", strings.Join(args, " "))
	if out, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		fmt.Println("Error uploading logs: ", err)
		vt.printLogs(logPath)
	} else {
		fmt.Println("gsutil output: ", out)
	}
	return nil
}

func (vt *Tester) UploadCassettes(head string, version provider.Version) error {
	cassettePath, ok := vt.cassettePaths[version]
	if !ok {
		return fmt.Errorf("no cassettes found for version %s", version)
	}
	args := []string{
		"-m",
		"-q",
		"cp",
		filepath.Join(cassettePath, "*"),
		fmt.Sprintf("gs://%s/%s/refs/heads/%s/fixtures/", vt.cassetteBucket, version, head),
	}
	fmt.Println("Uploading cassettes:\n", "gsutil", strings.Join(args, " "))
	if _, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		fmt.Println("Error uploading cassettes: ", err)
	}
	return nil
}

// Deletes the service account key.
func (vt *Tester) Cleanup() error {
	if vt.saKeyPath == "" {
		return nil
	}
	if err := vt.rnr.RemoveAll(vt.saKeyPath); err != nil {
		return err
	}
	return nil
}

// Returns a list of all directories to run tests in.
// Must be called after changing into the provider dir.
func (vt *Tester) googleTestDirectory() ([]string, error) {
	var testDirs []string
	if allPackages, err := vt.rnr.Run("go", []string{"list", "./..."}, nil); err != nil {
		return nil, err
	} else {
		for _, dir := range strings.Split(allPackages, "\n") {
			if !strings.Contains(dir, "github.com/hashicorp/terraform-provider-google-beta/scripts") {
				testDirs = append(testDirs, dir)
			}
		}
	}
	return testDirs, nil
}

// Print all log file names and contents, except for all_tests.log.
// Must be called after running tests.
func (vt *Tester) printLogs(logPath string) {
	vt.rnr.Walk(logPath, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Name() == "all_tests.log" {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		fmt.Println("======= ", info.Name(), " =======")
		if logContent, err := vt.rnr.ReadFile(path); err == nil {
			fmt.Println(logContent)
		}
		return nil
	})
}

func collectResult(output string) Result {
	matches := testResultsExpression.FindAllStringSubmatch(output, -1)
	resultSets := make(map[string]map[string]struct{}, 4)
	for _, submatches := range matches {
		if len(submatches) != 3 {
			fmt.Printf("Warning: unexpected regex match found in test output: %v", submatches)
			continue
		}
		if _, ok := resultSets[submatches[1]]; !ok {
			resultSets[submatches[1]] = make(map[string]struct{})
		}
		resultSets[submatches[1]][submatches[2]] = struct{}{}
	}
	matches = subtestResultsExpression.FindAllStringSubmatch(output, -1)
	subtestResultSets := make(map[string]map[string]struct{}, 4)
	for _, submatches := range matches {
		if len(submatches) != 4 {
			fmt.Printf("Warning: unexpected regex match found in test output: %v", submatches)
			continue
		}
		if _, ok := subtestResultSets[submatches[1]]; !ok {
			subtestResultSets[submatches[1]] = make(map[string]struct{})
		}
		subtestResultSets[submatches[1]][fmt.Sprintf("%s__%s", submatches[2], submatches[3])] = struct{}{}
	}
	results := make(map[string][]string, 4)
	results["PANIC"] = testPanicExpression.FindAllString(output, -1)
	sort.Strings(results["PANIC"])
	subtestResults := make(map[string][]string, 3)
	for _, kind := range []string{"FAIL", "PASS", "SKIP"} {
		for test := range resultSets[kind] {
			results[kind] = append(results[kind], test)
		}
		sort.Strings(results[kind])
		for subtest := range subtestResultSets[kind] {
			subtestResults[kind] = append(subtestResults[kind], subtest)
		}
		sort.Strings(subtestResults[kind])
	}
	return Result{
		FailedTests:     results["FAIL"],
		PassedTests:     results["PASS"],
		SkippedTests:    results["SKIP"],
		FailedSubtests:  subtestResults["FAIL"],
		PassedSubtests:  subtestResults["PASS"],
		SkippedSubtests: subtestResults["SKIP"],
		Panics:          results["PANIC"],
	}
}
