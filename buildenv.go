package buildworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mholt/archiver"

	"golang.org/x/tools/go/ast/astutil"
)

// BuildEnv is a build environment. A build environment
// is comprised of a master GOPATH from which sources
// originate, a temporary GOPATH where some repositories
// are copied to and modified, and a list of packages and
// their versions at which to build. A build environment
// should be "closed" after it is opened successfully
// to clean up any temporary assets.
type BuildEnv struct {
	masterGopath string
	tmpGopath    string
	pkgs         map[string]string // map of package to version
	Log          *log.Logger
}

// Open creates a new, provisioned build environment with caddy
// and the specified plugins at their associated versions. It
// uses the master GOPATH (from environment) to provision itself
// efficiently.
func Open(caddyVersion string, plugins []CaddyPlugin) (BuildEnv, error) {
	tmpGopath, err := newTemporaryGopath()
	if err != nil {
		return BuildEnv{}, err
	}
	be := BuildEnv{
		masterGopath: os.Getenv("GOPATH"),
		tmpGopath:    tmpGopath,
		pkgs:         make(map[string]string),
		Log:          log.New(os.Stdout, "", log.Ldate|log.Ltime), // TODO: new(bytes.Buffer) or something, instead of os.Stdout
	}
	for _, plugin := range plugins {
		be.pkgs[plugin.Package] = plugin.Version
	}
	if caddyVersion == "" {
		caddyVersion = "master"
	}
	be.pkgs[CaddyPackage] = caddyVersion
	err = be.provision()
	if err != nil {
		os.RemoveAll(tmpGopath)
		return be, fmt.Errorf("provisioning build environment: %v", err)
	}
	return be, nil
}

// provision fills in the master GOPATH as needed
// (non-destructive use of `go get`), and then
// fills in the temporary GOPATH by copying repos
// over and checking out the versions indicated
// in the configuration of the BuildEnv.
func (be BuildEnv) provision() error {
	// make temporary GOPATH if not already there
	if !dirExists(be.tmpGopath) {
		err := os.MkdirAll(be.tmpGopath, 0755)
		if err != nil {
			return err
		}
	}

	// before provisioning the temporary GOPATH,
	// we run `go get` (not -u) in the master GOPATH
	// to ensure that no packages are missing.
	err := be.fillMasterGopath()
	if err != nil {
		return err
	}

	rlock(be.masterGopath)
	defer runlock(be.masterGopath)

	// copy each package from master GOPATH into temporary GOPATH
	// and run `git fetch` to ensure we can checkout any version,
	// then checkout that version in the temporary GOPATH.
	for pkg, version := range be.pkgs {
		err := deepCopy(be.Path(pkg), be.TemporaryPath(pkg), false, false)
		if err != nil {
			return fmt.Errorf("copying %s: %v", pkg, err)
		}
		err = be.gitFetch(be.TemporaryPath(pkg))
		if err != nil {
			return fmt.Errorf("git fetch %s: %v", pkg, err)
		}
		err = be.gitCheckout(be.TemporaryPath(pkg), version)
		if err != nil {
			return fmt.Errorf("git checkout %s @ %s: %v", pkg, version, err)
		}
		// run `go get` since the version we just checked out
		// might have previously-unseen dependencies
		err = be.goGet(pkg)
		if err != nil {
			return fmt.Errorf("go get %s: %v", pkg, err)
		}
	}

	return nil
}

// goGet runs `go get -d -t -x $pkg/...`.
// It uses both master and temporary GOPATHs.
func (be BuildEnv) goGet(pkg string) error {
	cmd := be.newCommand("go", "get", "-d", "-t", "-x", pkg+"/...")
	return be.runCommand(cmd)
}

// goVet runs `go vet $pkg/...`.
// It uses both master and temporary GOPATHs.
func (be BuildEnv) goVet(pkg string) error {
	// see goTest() for an explanation of why we
	// use "./..." and change the dir of the command
	cmd := be.newCommand("go", "vet", "./...")
	cmd.Dir = be.TemporaryPath(pkg)
	return be.runCommand(cmd)
}

// goTest runs `go test -race $pkg/...`.
// It uses both master and temporary GOPATHs.
// TODO: This should be done in a container.
func (be BuildEnv) goTest(pkg string) error {
	// Note that we run tests on ./... and change the cwd of
	// the command to the package in the temporary GOPATH.
	// This is because specifying the package name instead of
	// using "." apparently causes `go test` to look in
	// either GOPATH, not necessarily the first one first,
	// for the package; I found that it was running tests
	// from the master GOPATH instead of the temporary one,
	// which was causing the tests to fail since, in that
	// case, the repo at master wasn't passing tests.
	// (After running go test -x here, I found that it
	// sets WORK=/temporary/gopath and then runs
	// `mkdir -p $WORK/github.com/user/repo/folder/that/doesn't/
	// exist/in/temp/gopath/_test/github.com/user/repo/same/folder/
	// -- very unexpected!)
	cmd := be.newCommand("go", "test", "-race", "./...")
	cmd.Dir = be.TemporaryPath(pkg)
	return be.runCommand(cmd)
}

// gitCheckout runs `git checkout $version` from the directory repoPath.
func (be BuildEnv) gitCheckout(repoPath, version string) error {
	cmd := be.newCommand("git", "checkout", version)
	cmd.Dir = repoPath
	return be.runCommand(cmd)
}

// gitFetch runs `git fetch` in the directory repoPath.
func (be BuildEnv) gitFetch(repoPath string) error {
	cmd := be.newCommand("git", "fetch")
	cmd.Dir = repoPath
	return be.runCommand(cmd)
}

// fillMasterGopath runs `go get` (without -u
// and without specifying subpackages) in the
// master GOPATH only to ensure that no packages
// needed by this build environment are missing.
func (be BuildEnv) fillMasterGopath() error {
	lock(be.masterGopath)
	defer unlock(be.masterGopath)
	for pkg := range be.pkgs {
		if pkg == CaddyPackage {
			// the caddy package is a special case because of its
			// plugin architecture and the fact that it's the package
			// we're building into a command; so we also want to
			// go get its main package and all its dependencies.
			pkg += "/..."
		}
		cmd := be.newCommand("go", "get", "-d", "-t", "-x", pkg)
		setEnvGopath(cmd.Env, be.masterGopath)
		err := be.runCommand(cmd)
		if err != nil {
			return err
		}
	}
	return nil
}

// Close deletes the temporary GOPATH from disk.
func (be BuildEnv) Close() error {
	return os.RemoveAll(be.tmpGopath)
}

// TemporaryPath returns the path to pkg's source
// folder in the temporary GOPATH.
func (be BuildEnv) TemporaryPath(pkg string) string {
	return filepath.Join(be.tmpGopath, "src", pkg)
}

// Path returns the path to pkg's source folder in the
// master GOPATH.
func (be BuildEnv) Path(pkg string) string {
	return filepath.Join(be.masterGopath, "src", pkg)
}

// newTemporaryGopath creates a new gopath folder
// in a temporary location. It is the caller's
// responsibility to remove the gopath when finished.
func newTemporaryGopath() (string, error) {
	ts := time.Now().Format(MonthDayHourMin)
	return ioutil.TempDir("", fmt.Sprintf("gopath_%s.", ts))
}

// setEnvGopath sets the GOPATH variable in env
// to the given path (do not prefix with 'GOPATH=').
func setEnvGopath(env []string, to string) {
	for i := 0; i < len(env); i++ {
		if strings.HasPrefix(env[i], "GOPATH=") {
			env[i] = "GOPATH=" + to
			return
		}
	}
}

// newCommand prepares a command to execute related to this
// build environment. It sets a custom environment, including
// a GOPATH variable that uses *both* the master and temporary
// GOPATHs. If this command should only use one GOPATH, be sure
// to call setEnvGopath() to change it.
func (be BuildEnv) newCommand(command string, args ...string) *exec.Cmd {
	cmd := exec.Command(command, args...)
	cmd.Env = []string{
		"GOPATH=" + be.tmpGopath + ":" + be.masterGopath,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// runCommand runs cmd while logging the command being run.
func (be BuildEnv) runCommand(cmd *exec.Cmd) error {
	be.Log.Printf("exec [%s] %s %s\n", cmd.Dir, cmd.Path, strings.Join(cmd.Args[1:], " "))
	return cmd.Run()
}

// Deploy deploys the package that the BuildEnv was
// initialized with. The BuildEnv must have been created
// with either zero plugins or one plugin. If zero, caddy
// will be deployed. If one, the plugin will be deployed.
//
// To "deploy" means that the master GOPATH is updated
// with `go get -u` on the package being deployed.
// Package checks are then run for plugin deployments.
// An error is returned if anything failed, in which case
// you should consider the deployment/release a failure.
func (be BuildEnv) Deploy() error {
	// we only allow deploying caddy itself or
	// a single plugin at a time.
	switch len(be.pkgs) {
	case 0:
		return fmt.Errorf("nothing to deploy")
	case 1, 2:
		if _, ok := be.pkgs[CaddyPackage]; !ok {
			return fmt.Errorf("no caddy package")
		}
	default:
		return fmt.Errorf("too many packages to deploy")
	}

	// backup the master GOPATH in case something
	// goes really wrong (like, if a dependency we
	// aren't tracking gets updated and breaks caddy,
	// that would be bad; we assume the current
	// GOPATH is at least mostly healthy)
	backupGopath, err := be.backupMasterGopath()
	if err != nil {
		return err
	}
	defer os.RemoveAll(backupGopath)

	// run `go get -u` in master GOPATH only, so that
	// dependencies get updated -- crossing fingers!
	err = be.UpdateMasterGopath()
	if err != nil {
		return err
	}

	// run checks and report result
	revert, err := be.RunPluginChecks()
	if err != nil && revert {
		// apparently the caddy tests failed; it _could_ have been
		// because of the plugin's code, but this is rare, because
		// a separate run of that plugin's test code by itself
		// tends to catch most of its bugs. the test failures
		// might be caused because of `go get -u`, so our only
		// hope is to restore the GOPATH to before the update.
		err2 := be.restoreMasterGopath(backupGopath)
		if err2 != nil {
			// well, this is terrible. we now have multiple
			// GOPATHs that don't work. just gonna cry.
			return fmt.Errorf("%v; additionally, error restoring GOPATH: %v", err, err2)
		}
	}

	return err
}

// backupMasterGopath copies the master GOPATH of the build
// environment to a temporary location and returns that location.
// It is the caller's responsibility to delete it when no
// longer needed. If an error is returned, no need to clean up.
func (be BuildEnv) backupMasterGopath() (string, error) {
	rlock(be.masterGopath)
	defer runlock(be.masterGopath)
	tmpdir, err := ioutil.TempDir("", "gopath_backup_")
	if err != nil {
		return tmpdir, err
	}
	err = deepCopy(be.masterGopath, tmpdir, false, false)
	if err != nil {
		os.RemoveAll(tmpdir)
	}
	return tmpdir, err
}

// restoreMasterGopath copies the backup GOPATH at tmpdir
// back into the build environment's master GOPATH; it fully
// replaces the existing master GOPATH with the contents
// of the backup but does not change the path of the master
// GOPATH. An error returned from this function is awful, sorry.
// This function does NOT clean up the tmpdir that is passed in.
func (be BuildEnv) restoreMasterGopath(tmpdir string) error {
	lock(be.masterGopath)
	defer unlock(be.masterGopath)

	// rename the master GOPATH so we have a clean
	// destination to copy into; safer than deleting
	// our master copy before the restore is successful
	suffix := fmt.Sprintf("%d", rand.Intn(9000)+1000)
	tmpPath := be.masterGopath + "_tmp_" + suffix
	err := os.Rename(be.masterGopath, tmpPath)
	if err != nil {
		return err
	}

	// copy the files back over
	err = deepCopy(tmpdir, be.masterGopath, false, false)
	if err != nil {
		return err
	}

	// copy successful, so clean up by deleting the original GOPATH
	err = os.RemoveAll(tmpPath)
	if err != nil {
		return err
	}

	return err
}

// packageToDeploy returns the name of the package
// to deploy (assuming be is used to deploy caddy or
// a plugin). The length of be.pkgs must be either
// 1 or 2 in order to return a value. If len is 1,
// then caddy must be deployed; if 2, a plugin.
func (be BuildEnv) packageToDeploy() string {
	var pkg string
	if len(be.pkgs) == 1 {
		pkg = CaddyPackage
	} else if len(be.pkgs) == 2 {
		for key := range be.pkgs {
			if key != CaddyPackage {
				pkg = key
				break
			}
		}
	}
	return pkg
}

// UpdateMasterGopath runs `go get -u` on only the
// package to be deployed and only in the master
// GOPATH. The package to be deployed is inferred
// from the pkgs map in the BuildEnv. If len 1
// (meaning only caddy is in the list of packages),
// caddy itself (and its dependencies) are updated.
// If len 2, the other package (a plugin, and its)
// dependencies) are updated.
//
// CAUTION. `go get -u` may introduce breaking
// changes in dependencies. While necessary to
// get the latest security updates and bug fixes
// and to keep packages from going stale, it is
// wise to back up the master GOPATH if this is
// a build server environment first. (Local dev
// environments will still be affected, but
// running `go get -u` in local dev is normal.)
//
// Note: This will only update packages in the
// master GOPATH; any packages that were copied
// to the temporary GOPATH in provisioning this
// build environment and checked out to a certain
// version will not be affected.
func (be BuildEnv) UpdateMasterGopath() error {
	pkg := be.packageToDeploy()
	if pkg == CaddyPackage {
		pkg += "/..." // see fillMasterGopath() for why we do this
	}
	cmd := be.newCommand("go", "get", "-u", "-d", "-t", "-x", pkg)
	setEnvGopath(cmd.Env, be.masterGopath) // operate on master GOPATH only
	lock(be.masterGopath)
	defer unlock(be.masterGopath)
	be.Log.Println("Updating master GOPATH: %s", be.masterGopath)
	return be.runCommand(cmd)
}

// RunPluginChecks runs checks (vet, test, etc.)
// on the plugins in this build environment.
// While it will work for checking more than one
// plugin at a time, this kind of use is not
// recommended. It does not check the core Caddy
// packages, only plugins. If the master GOPATH
// should be reverted, the first return value will
// be true; otherwise a revert is not necessary.
func (be BuildEnv) RunPluginChecks() (bool, error) {
	rlock(be.masterGopath)
	defer runlock(be.masterGopath)

	for pkg := range be.pkgs {
		if pkg == CaddyPackage {
			continue
		}

		// go vet the plugin
		err := be.goVet(pkg)
		if err != nil {
			return false, fmt.Errorf("go vet plugin %s: %v", pkg, err)
		}

		// go test the plugin
		err = be.goTest(pkg)
		if err != nil {
			return false, fmt.Errorf("go test plugin %s: %v", pkg, err)
		}

		// plug in the plugin
		// TODO: This does not unplug any previously-plugged-in
		// plugins, but that's okay since we only deploy one
		// plugin at a time, right?
		be.Log.Printf("plugging in %s", pkg)
		err = be.plugInThePlugin(pkg)
		if err != nil {
			return false, fmt.Errorf("plugging in %s: %v", pkg, err)
		}

		// go test Caddy with the plugin installed
		err = be.goTest(CaddyPackage)
		if err != nil {
			return true, fmt.Errorf("go test caddy with plugin: %v", err)
		}

		// go build on various platforms
		err = be.goBuildChecks(pkg)
		if err != nil {
			return false, fmt.Errorf("go build %s: %v", pkg, err)
		}
	}

	return false, nil
}

// RunCaddyChecks performs testsn and checks on
// the caddy package in the build environment.
func (be BuildEnv) RunCaddyChecks() error {
	err := be.goVet(CaddyPackage)
	if err != nil {
		return fmt.Errorf("go vet: %v", err)
	}

	// go test
	err = be.goTest(CaddyPackage)
	if err != nil {
		return fmt.Errorf("go test: %v", err)
	}

	// go build on various platforms
	err = be.goBuildChecks(CaddyPackage)
	if err != nil {
		return fmt.Errorf("go build: %v", err)
	}

	return nil
}

// Build performs a build for the given platform and places the
// resulting file on disk in outputFolder. It returns the
// result open for reading. It is the caller's responsibility
// to clean up the file when finished with it. Builds are
// performed by plugging in all the plugins configured for
// this build environment and bundling all distribution
// assets into an archive with the binary.
func (be BuildEnv) Build(plat Platform, outputFolder string) (*os.File, error) {
	if plat.OS == "" || plat.Arch == "" {
		return nil, fmt.Errorf("missing required information: OS or arch")
	}

	// plug in the plugins
	for pkg := range be.pkgs {
		if pkg == CaddyPackage {
			continue // caddy core is not a plugin
		}
		err := be.plugInThePlugin(pkg)
		if err != nil {
			return nil, fmt.Errorf("plugging in %s: %v", pkg, err)
		}
	}

	caddyVer, ok := be.pkgs[CaddyPackage]
	if !ok { // shouldn't happen, but whatever
		caddyVer = "master"
	}
	if !strings.HasPrefix(caddyVer, "v") && len(caddyVer) > 8 {
		caddyVer = caddyVer[:8]
	}
	outputName := "caddy_" + caddyVer + "_" + plat.OS + "_" + plat.Arch
	if plat.Arch == "arm" {
		outputName += plat.ARM
	}
	if len(be.pkgs) > 1 { // one will be caddy itself
		outputName += "_custom"
	}

	binaryOutputName := outputName
	if plat.OS == "windows" {
		binaryOutputName += ".exe"
	}
	binaryOutputPath := filepath.Join(outputFolder, binaryOutputName)

	err := be.buildCaddy(plat, binaryOutputPath)
	if err != nil {
		return nil, fmt.Errorf("building caddy: %v", err)
	}
	defer os.Remove(binaryOutputPath)

	// choose .tar.gz or .zip format depending on OS
	compressZip := plat.OS == "windows" || plat.OS == "darwin"

	fileList := []string{
		filepath.Join(be.TemporaryPath(CaddyPackage), "dist", "README.txt"),
		filepath.Join(be.TemporaryPath(CaddyPackage), "dist", "LICENSES.txt"),
		filepath.Join(be.TemporaryPath(CaddyPackage), "dist", "CHANGES.txt"),
		filepath.Join(be.TemporaryPath(CaddyPackage), "dist", "init"),
		binaryOutputPath,
	}

	finalOutputPath := filepath.Join(outputFolder, outputName)

	if compressZip {
		finalOutputPath += ".zip"
		err = archiver.Zip.Make(finalOutputPath, fileList)
	} else {
		finalOutputPath += ".tar.gz"
		err = archiver.TarGz.Make(finalOutputPath, fileList)
	}
	if err != nil {
		return nil, fmt.Errorf("error compressing: %v", err)
	}

	return os.Open(finalOutputPath)
}

// plugInThePlugin plugs in the plugin with import
// path of pkg into the copy of caddy in the temporary
// GOPATH.
func (be BuildEnv) plugInThePlugin(pkg string) error {
	fset := token.NewFileSet()
	file := filepath.Join(be.TemporaryPath(CaddyPackage), "caddy/caddymain/run.go")
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return fmt.Errorf("parsing file: %v", err)
	}
	astutil.AddNamedImport(fset, f, "_", pkg)
	var buf bytes.Buffer // write to buffer first in case there's an error
	err = printer.Fprint(&buf, fset, f)
	if err != nil {
		return fmt.Errorf("adding import: %v", err)
	}
	// TODO: Use file mode as already on disk
	err = ioutil.WriteFile(file, buf.Bytes(), os.FileMode(0660))
	if err != nil {
		return fmt.Errorf("saving changed file: %v", err)
	}
	return nil
}

// goBuildChecks cross-compiles pkg on various platforms to
// ensure it works.
func (be BuildEnv) goBuildChecks(pkg string) error {
	platforms, err := SupportedPlatforms(UnsupportedPlatforms)
	if err != nil {
		return err
	}

	for _, platform := range platforms {
		cgo := "CGO_ENABLED=0"
		if platform.OS == "darwin" {
			// TODO.
			// As of Go 1.6, darwin might have some trouble if cgo is disabled.
			// https://www.reddit.com/r/golang/comments/46bd5h/ama_we_are_the_go_contributors_ask_us_anything/d03rmc9
			cgo = "CGO_ENABLED=1"
		}
		log.Printf("GOOS=%s GOARCH=%s GOARM=%s go build", platform.OS, platform.Arch, platform.ARM)
		cmd := be.newCommand("go", "build", "-p", strconv.Itoa(ParallelBuildOps), pkg+"/...")
		for _, env := range []string{
			cgo,
			"GOOS=" + platform.OS,
			"GOARCH=" + platform.Arch,
			"GOARM=" + platform.ARM,
		} {
			cmd.Env = append(cmd.Env, env)
		}
		err := be.runCommand(cmd)
		if err != nil {
			return fmt.Errorf("build failed: GOOS=%s GOARCH=%s GOARM=%s: %v",
				platform.OS, platform.Arch, platform.ARM, err)
		}
	}

	return nil
}

// buildCaddy builds caddy for the given platform and puts the
// binary at outputFile. The outputFile path will be relative
// to the folder where Caddy's main() function is defined (or it
// can be an absolute path).
func (be BuildEnv) buildCaddy(plat Platform, outputFile string) error {
	ldflags, err := makeLdFlags(be.TemporaryPath(CaddyPackage))
	if err != nil {
		return err
	}
	cgo := "CGO_ENABLED=0"
	if plat.OS == "darwin" {
		// TODO.
		// As of Go 1.6, darwin might have some trouble if cgo is disabled.
		// https://www.reddit.com/r/golang/comments/46bd5h/ama_we_are_the_go_contributors_ask_us_anything/d03rmc9
		cgo = "CGO_ENABLED=1"
	}
	cmd := be.newCommand("go", "build", "-ldflags", ldflags, "-o", outputFile)
	cmd.Dir = filepath.Join(be.TemporaryPath(CaddyPackage), "caddy")
	for _, env := range []string{
		cgo,
		"GOOS=" + plat.OS,
		"GOARCH=" + plat.Arch,
		"GOARM=" + plat.ARM,
	} {
		cmd.Env = append(cmd.Env, env)
	}
	return be.runCommand(cmd)
}

// Platform contains information about platforms. The values of
// OS, Arch, and ARM should be the same values to set GOOS,
// GOARCH, and GOARM to, respectively. The values of the json
// struct tags match the output of `go tool dist list -json`.
type Platform struct {
	OS   string `json:"GOOS"`
	Arch string `json:"GOARCH"`
	ARM  string `json:"GOARM"`
	Cgo  bool   `json:"CgoSupported"`
}

func (p Platform) String() string {
	return fmt.Sprintf("%s/%s%s", p.OS, p.Arch, p.ARM)
}

// UnsupportedPlatforms is a list of platforms that we do not
// build for at this time. NOTE: this initial list was only
// attempted from 64-bit darwin (macOS).
var UnsupportedPlatforms = []Platform{
	{OS: "android"},               // linker errors (Go 1.7.3, 11/2016)
	{OS: "darwin", Arch: "arm"},   // runtime.read_tls_fallback: not defined (Go 1.7.3, 11/2016), and for ARM7: clang: error: argument unused during compilation: '-mno-thumb'
	{OS: "darwin", Arch: "arm64"}, // linker errors (Go 1.7.3, 11/2016)
	{OS: "linux", Arch: "s390x"},  // github.com/lucas-clemente/aes12/cipher.go:36: undefined: newCipher (Go 1.7.3, 11/2016)
	{OS: "nacl"},                  // syscall-related compile errors in Caddy (Go 1.7.3, 11/2016)
	{OS: "plan9"},                 // syscall-related compile errors in Caddy (Go 1.7.3, 11/2016)
}

// SupportedPlatforms runs `go tool dist list` to get
// a list of platforms we can build for, sans the ones
// matching any in the skip slice. In order to be skipped,
// the platform must match all specified fields.
func SupportedPlatforms(skip []Platform) ([]Platform, error) {
	out, err := exec.Command("go", "tool", "dist", "list", "-json").Output()
	if err != nil {
		return nil, err
	}

	var platforms []Platform
	err = json.Unmarshal(out, &platforms)
	if err != nil {
		return nil, err
	}

	// manually expand all the ARM platforms to enumerate
	// the versions of ARM we can build for (assume 5, 6, 7).
	for i := len(platforms) - 1; i >= 0; i-- {
		p := platforms[i]
		if p.Arch == "arm" && p.ARM == "" {
			platforms[i].ARM = "5"
			platforms = append(platforms[:i+1], append([]Platform{
				Platform{OS: p.OS, Arch: p.Arch, ARM: "6", Cgo: p.Cgo},
				Platform{OS: p.OS, Arch: p.Arch, ARM: "7", Cgo: p.Cgo},
			}, platforms[i+1:]...)...)
		}
	}

	// remove platforms that we don't build for
	for i := 0; i < len(platforms); i++ {
		p := platforms[i]
		for _, unsup := range skip {
			osMatch := unsup.OS == "" || unsup.OS == p.OS
			archMatch := unsup.Arch == "" || unsup.Arch == p.Arch
			armMatch := unsup.ARM == "" || unsup.ARM == p.ARM
			if osMatch && archMatch && armMatch {
				platforms = append(platforms[:i], platforms[i+1:]...)
				i--
				break
			}
		}
	}

	return platforms, nil
}

const (
	MonthDayHourMin  = "01-02-1504"
	ParallelBuildOps = 4

	CaddyPackage = "github.com/mholt/caddy"

	// the file in which plugins get plugged in
	plugInto = "caddy/caddymain/run.go"
)
