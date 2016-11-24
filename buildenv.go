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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/tools/go/ast/astutil"
)

// BuildEnv defines a configuration for a build environment.
type BuildEnv struct {
	// The path to the workspace where GOPATHs are made
	Workspace Workspace

	// The cumulative log of activities and command outputs
	Log LogBuffer

	// The PATH variable from our parent environment
	EnvPath string

	// The absolute GOPATH being used, as found in Workspace
	Gopath string

	// The configuration to use for builds
	BuildCfg BuildConfig
}

func newBuildEnv(caddyVersion string, plugins []CaddyPlugin) (BuildEnv, error) {
	be := BuildEnv{
		Workspace: workspace,
		Log:       LogBuffer{Writer: os.Stdout}, //LogBuffer{Writer: new(bytes.Buffer)},
		EnvPath:   os.Getenv("PATH"),
		BuildCfg: BuildConfig{
			CaddyVersion: caddyVersion,
			Plugins:      plugins,
		},
	}

	// make folder in workspace that will serve as the GOPATH
	be.Log.Println("Making new GOPATH")
	err := makeNewGopath(&be)
	if err != nil {
		return be, fmt.Errorf("making GOPATH: %v", err)
	}

	return be, nil
}

// RepoPath is the full, absolute path to pkg as found in BuildEnv's GOPATH.
func (be BuildEnv) RepoPath(pkg string) string {
	return filepath.Join(be.Gopath, "src", pkg)
}

// CaddyRepoPath is the full, absolute path to the cloned Caddy repo.
func (be BuildEnv) CaddyRepoPath() string {
	return filepath.Join(be.Gopath, "src", CaddyPackage)
}

// goGet runs `go get -d -t -x $pkg/...`.
func (be BuildEnv) goGet(pkg string) error {
	cmd := be.makeCommand(true, "go", "get", "-d", "-t", "-x", pkg+"/...")
	return cmd.Run()
}

// goVet runs `go vet $pkg/...`.
func (be BuildEnv) goVet(pkg string) error {
	cmd := be.makeCommand(false, "go", "vet", pkg+"/...")
	return cmd.Run()
}

// goTest runs `go test -race $pkg/...`.
// TODO: This should be done in a container
func (be BuildEnv) goTest(pkg string) error {
	cmd := be.makeCommand(true, "go", "test", "-race", pkg+"/...")
	return cmd.Run()
}

// TODO: support go generate... and should also happen in a container

// gitCheckout runs `git checkout $version` from the repoPath dir.
func (be BuildEnv) gitCheckout(repoPath, version string) error {
	cmd := be.makeCommand(false, "git", "checkout", version)
	cmd.Dir = repoPath
	return cmd.Run()
}

// setup runs `go get` and `git checkout` to put caddy and
// the plugin (if any) in the build environment's GOPATH
// at the right version, so it's ready for use.
func (be BuildEnv) setup() error {
	// go get caddy
	// TODO: replace these logs with something when we run commands that logs the command being run
	// TODO: we might be able to just clone caddy and plugin initially with limited depth...
	// because we checkout a specific version and run go get after that anyway.
	be.Log.Println("go getting Caddy")
	err := be.goGet(CaddyPackage)
	if err != nil {
		return fmt.Errorf("go get caddy: %v", err)
	}

	// go get the plugins
	for _, plugin := range be.BuildCfg.Plugins {
		be.Log.Printf("go getting %s", plugin.Package)
		err = be.goGet(plugin.Package)
		if err != nil {
			return fmt.Errorf("go get plugin: %v", err)
		}
	}

	// checkout the desired version of Caddy
	be.Log.Println("Checking out version of Caddy")
	err = be.gitCheckout(be.CaddyRepoPath(), be.BuildCfg.CaddyVersion)
	if err != nil {
		return fmt.Errorf("checking out caddy: %v", err)
	}

	// checkout the desired version of each plugin
	for _, plugin := range be.BuildCfg.Plugins {
		be.Log.Println("Checking out version of plugin")
		err = be.gitCheckout(be.RepoPath(plugin.Package), plugin.Version)
		if err != nil {
			return fmt.Errorf("checking out plugin: %v", err)
		}
	}

	// run go get on Caddy and each plugin again since the version
	// we checked out might have different dependencies than tip
	be.Log.Println("go getting Caddy (any remaining dependencies)")
	err = be.goGet(CaddyPackage)
	if err != nil {
		return fmt.Errorf("go get caddy: %v", err)
	}
	for _, plugin := range be.BuildCfg.Plugins {
		err = be.goGet(plugin.Package)
		if err != nil {
			return fmt.Errorf("go get plugin: %v", err)
		}
	}

	return nil
}

func (be BuildEnv) plugInThePlugin(plugin CaddyPlugin) error {
	fset := token.NewFileSet()
	file := filepath.Join(be.CaddyRepoPath(), "caddy/caddymain/run.go")
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return fmt.Errorf("parsing file: %v", err)
	}
	astutil.AddNamedImport(fset, f, "_", plugin.Package)
	var buf bytes.Buffer // write to buffer first in case there's an error
	err = printer.Fprint(&buf, fset, f)
	if err != nil {
		return fmt.Errorf("adding import: %v", err)
	}
	err = ioutil.WriteFile(file, buf.Bytes(), os.FileMode(0660))
	if err != nil {
		return fmt.Errorf("saving changed file: %v", err)
	}
	return nil
}

func (be BuildEnv) goBuildChecks(pkg string) error {
	platforms, err := SupportedPlatforms()
	if err != nil {
		return err
	}

	for _, platform := range platforms {
		log.Printf("GOOS=%s GOARCH=%s GOARM=%s go build", platform.OS, platform.Arch, platform.ARM)
		cmd := be.makeCommand(true, "go", "build", "-p", strconv.Itoa(ParallelBuildOps), pkg+"/...")
		for _, env := range []string{
			"CGO_ENABLED=0",
			"GOOS=" + platform.OS,
			"GOARCH=" + platform.Arch,
			"GOARM=" + platform.ARM,
		} {
			cmd.Env = append(cmd.Env, env)
		}
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("build failed: GOOS=%s GOARCH=%s GOARM=%s: %v",
				platform.OS, platform.Arch, platform.ARM, err)
		}
	}

	return nil
}

func (be BuildEnv) cleanup() error {
	if be.Workspace != "" { // only remove temporary GOPATHs
		return os.RemoveAll(be.Gopath)
	}
	return nil
}

func (be BuildEnv) makeCommand(withEnvPath bool, command string, args ...string) *exec.Cmd {
	cmd := exec.Command(command, args...)
	cmd.Env = []string{
		"GOPATH=" + be.Gopath,
	}
	if withEnvPath {
		cmd.Env = append(cmd.Env, "PATH="+be.EnvPath)
	}
	cmd.Stdout = be.Log
	cmd.Stderr = be.Log
	return cmd
}

// TODO: Use this...
func (be BuildEnv) runCommand(cmd *exec.Cmd) error {
	be.Log.Printf(cmd.Path, strings.Join(cmd.Args, " "))
	return cmd.Run()
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

// unsupportedPlatforms is a list of platforms that we do not
// build for at this time. NOTE: this initial list was only
// attempted from 64-bit darwin (macOS).
var unsupportedPlatforms = []Platform{
	{OS: "android"},                       // linker errors (Go 1.7.3, 11/2016)
	{OS: "darwin", Arch: "arm", ARM: "5"}, // runtime.read_tls_fallback: not defined (Go 1.7.3, 11/2016)
	{OS: "darwin", Arch: "arm", ARM: "6"}, // runtime.read_tls_fallback: not defined (Go 1.7.3, 11/2016)
	{OS: "darwin", Arch: "arm64"},         // linker errors (Go 1.7.3, 11/2016)
	{OS: "linux", Arch: "s390x"},          // github.com/lucas-clemente/aes12/cipher.go:36: undefined: newCipher (Go 1.7.3, 11/2016)
	{OS: "nacl"},                          // syscall-related compile errors in Caddy (Go 1.7.3, 11/2016)
	{OS: "plan9"},                         // syscall-related compile errors in Caddy (Go 1.7.3, 11/2016)
}

// SupportedPlatforms runs `go tool dist list` to get
// a list of platforms we can build for.
func SupportedPlatforms() ([]Platform, error) {
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
		for _, unsup := range unsupportedPlatforms {
			osMatch := unsup.OS == "" || unsup.OS == p.OS
			archMatch := unsup.Arch == "" || unsup.Arch == p.Arch
			armMatch := unsup.ARM == "" || unsup.ARM == p.ARM
			if osMatch && archMatch && armMatch {
				platforms = append(platforms[:i], platforms[i+1:]...)
				i--
			}
		}
	}

	return platforms, nil
}

func makeNewGopath(be *BuildEnv) error {
	ts := time.Now().Format(MonthDayHourMinSec)
	gopathName := fmt.Sprintf("gopath_%s", ts)
	gopath, err := filepath.Abs(filepath.Join(string(be.Workspace), gopathName))
	if err != nil {
		return err
	}
	err = os.MkdirAll(gopath, 0755)
	if err != nil {
		return err
	}
	be.Gopath = gopath
	return nil
}

const (
	MonthDayHourMinSec = "01-02-150405.00"
	CaddyPackage       = "github.com/mholt/caddy"
	//CaddyMainPackage   = "github.com/mholt/caddy/caddy" // TODO: Probably used when building a binary, right?
	//CaddyRepo = "https://github.com/mholt/caddy.git"
	ParallelBuildOps = 8
)
