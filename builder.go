package buildworker

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/tools/go/ast/astutil"
)

// A Workspace is a folder path which will hold GOPATHs.
type Workspace string

var workspace = Workspace("./workspace")

type BuildEnv struct {
	// The path to the workspace where GOPATHs are made
	Workspace Workspace

	// The fully qualified package name of the plugin to deploy
	Package string

	// The commit or tag of the plugin to deploy
	Commit string

	// The leaf folder name of the GOPATH
	GopathName string

	// The full, absolute GOPATH
	Gopath string

	// The PATH variable from the environment
	EnvPath string

	// The full, absolute path to the Caddy repo
	CaddyRepoPath string
}

// DeployCaddy begins the pipeline that deploys
// an update to the main Caddy repo. It blocks
// until Caddy is finished deploying or an error
// occurs.
func DeployCaddy(commit string) error {
	// TODO: Run tests for each version of Go?

	be := BuildEnv{
		Workspace: workspace,
		Package:   CaddyPackage,
		EnvPath:   os.Getenv("PATH"),
	}

	// Make new, empty GOPATH
	log.Println("Making new GOPATH")
	err := makeNewGopath(&be)
	if err != nil {
		return fmt.Errorf("making GOPATH: %v", err)
	}

	log.Println("go getting Caddy")
	err = goGetCaddy(be)
	if err != nil {
		return fmt.Errorf("go get caddy: %v", err)
	}

	// checkout the tag/commit of current release of Caddy
	log.Println("Checking out commit of Caddy")
	err = checkoutCaddy(be, commit)
	if err != nil {
		return fmt.Errorf("checking out caddy: %v", err)
	}

	// TODO...

	return nil
}

// DeployPlugin begins the pipeline that deploys a new plugin.
// It blocks until the plugin is finished deploying, or an error
// occurs.
func DeployPlugin(repo, commit string) error {
	err := CheckPlugin(repo, commit)
	if err != nil {
		return err
	}

	// TODO: Perform deploy

	return nil
}

// CheckPlugin checks repo at commit for any
// build or test errors.
func CheckPlugin(repo, commit string) error {
	// TODO: Run tests for each version of Go?

	be := BuildEnv{
		Workspace: workspace,
		Package:   repo,
		Commit:    commit,
		EnvPath:   os.Getenv("PATH"),
	}

	// Make new, empty GOPATH
	log.Println("Making new GOPATH")
	err := makeNewGopath(&be)
	if err != nil {
		return fmt.Errorf("making new GOPATH: %v", err)
	}

	// go get the plugin
	log.Println("go getting the plugin")
	err = goGetPlugin(be)
	if err != nil {
		return fmt.Errorf("go get plugin: %v", err)
	}

	log.Println("go getting Caddy (any remaining dependencies)")
	err = goGetCaddy(be)
	if err != nil {
		return fmt.Errorf("go get caddy: %v", err)
	}

	// checkout the tag/commit of current release of Caddy
	log.Println("Checking out commit of Caddy")
	err = checkoutCaddy(be, "v0.9.3") // TODO: use current version/tag/commit of Caddy
	if err != nil {
		return fmt.Errorf("checking out caddy: %v", err)
	}

	// checkout the tag/commit to deploy
	log.Println("Checking out commit of plugin")
	err = checkoutPlugin(be)
	if err != nil {
		return fmt.Errorf("checking out plugin: %v", err)
	}

	// go vet the plugin
	log.Println("go vet the plugin")
	err = goVetPlugin(be)
	if err != nil {
		return fmt.Errorf("go vet plugin: %v", err)
	}

	// go test the plugin
	log.Println("go test the plugin")
	err = goTestPlugin(be)
	if err != nil {
		return fmt.Errorf("go test plugin: %v", err)
	}

	// plug in the plugin
	log.Println("Plugging in the plugin")
	err = plugInThePlugin(be)
	if err != nil {
		return fmt.Errorf("plugging in plugin: %v", err)
	}

	// go test Caddy with the plugin installed
	log.Println("go test Caddy with plugin installed")
	err = goTestCaddyWithPlugin(be)
	if err != nil {
		return fmt.Errorf("go test caddy with plugin: %v", err)
	}

	// try building on all platforms
	log.Println("go build checks")
	err = goBuildChecks(be)
	if err != nil {
		return fmt.Errorf("go build: %v", err)
	}

	return nil
}

// TODO: Keep the workspace cleaned up

func makeNewGopath(be *BuildEnv) error {
	ts := time.Now().Format(YearMonthDayHourMinSec)
	gopathName := fmt.Sprintf("gopath_%s", ts)
	gopath, err := filepath.Abs(filepath.Join(string(be.Workspace), gopathName))
	if err != nil {
		return err
	}
	err = os.MkdirAll(gopath, 0755)
	if err != nil {
		return err
	}
	be.GopathName = gopathName
	be.Gopath = gopath
	be.CaddyRepoPath = filepath.Join(gopath, "src", CaddyPackage)
	return nil
}

func goGetPlugin(be BuildEnv) error {
	return goGet(be, be.Package)
}

func goGetCaddy(be BuildEnv) error {
	return goGet(be, CaddyPackage)
}

// goGet runs `go get -d -t -x $pkg/...`.
func goGet(be BuildEnv, pkg string) error {
	cmd := makeCommand(be, true, "go", "get", "-d", "-t", "-x", pkg+"/...")
	return cmd.Run()
}

func checkoutCaddy(be BuildEnv, caddyCommit string) error {
	cmd := makeCommand(be, false, "git", "checkout", caddyCommit, ".")
	cmd.Dir = be.CaddyRepoPath
	return cmd.Run()
}

func checkoutPlugin(be BuildEnv) error {
	cmd := makeCommand(be, false, "git", "checkout", be.Commit, ".")
	cmd.Dir = filepath.Join(be.Gopath, "src", be.Package)
	return cmd.Run()
}

func goVetPlugin(be BuildEnv) error {
	cmd := makeCommand(be, false, "go", "vet", be.Package+"/...")
	return cmd.Run()
}

// TODO: This should be done in a container
func goTestPlugin(be BuildEnv) error {
	cmd := makeCommand(be, true, "go", "test", "-v", "-race", be.Package+"/...")
	return cmd.Run()
}

// TODO: go generate? (also in container)

func plugInThePlugin(be BuildEnv) error {
	fset := token.NewFileSet()
	file := filepath.Join(be.CaddyRepoPath, "caddy/caddymain/run.go")
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return fmt.Errorf("parsing file: %v", err)
	}
	astutil.AddNamedImport(fset, f, "_", be.Package)
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

func goTestCaddyWithPlugin(be BuildEnv) error {
	cmd := makeCommand(be, true, "go", "test", "-race", CaddyPackage+"/...")
	return cmd.Run()
}

func goBuildChecks(be BuildEnv) error {
	for _, platform := range []struct {
		os, arch, arm string
	}{
		// problematic builds are commented out
		// TODO: We should be able to get this list
		// dynamically with the go tool, I think...
		{os: "darwin", arch: "386"},
		{os: "darwin", arch: "amd64"},
		{os: "darwin", arch: "arm"},
		//{os: "darwin", arch: "arm64"},
		//{os: "dragonfly", arch: "amd64"},
		{os: "freebsd", arch: "386"},
		{os: "freebsd", arch: "amd64"},
		{os: "freebsd", arch: "arm"},
		{os: "linux", arch: "386"},
		{os: "linux", arch: "amd64"},
		{os: "linux", arch: "arm"},
		{os: "linux", arch: "arm64"},
		{os: "linux", arch: "ppc64"},
		{os: "linux", arch: "ppc64le"},
		{os: "linux", arch: "mips64"},
		//{os: "linux", arch: "mips64le"},
		{os: "netbsd", arch: "386"},
		{os: "netbsd", arch: "amd64"},
		{os: "netbsd", arch: "arm"},
		{os: "openbsd", arch: "386"},
		{os: "openbsd", arch: "amd64"},
		{os: "openbsd", arch: "arm"},
		//{os: "plan9", arch: "386"},
		//{os: "plan9", arch: "amd64"},
		{os: "solaris", arch: "amd64"},
		{os: "windows", arch: "386"},
		{os: "windows", arch: "amd64"},
	} {
		log.Printf("GOOS=%s GOARCH=%s GOARM=%s go build", platform.os, platform.arch, platform.arm)
		cmd := makeCommand(be, true, "go", "build", be.Package+"/...")
		for _, env := range []string{
			"CGO_ENABLED=0",
			"GOOS=" + platform.os,
			"GOARCH=" + platform.arch,
			"GOARM=" + platform.arm,
		} {
			cmd.Env = append(cmd.Env, env)
		}
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("build failed: GOOS=%s GOARCH=%s GOARM=%s: %v",
				platform.os, platform.arch, platform.arm, err)
		}
	}
	return nil
}

func makeCommand(be BuildEnv, withEnvPath bool, command string, args ...string) *exec.Cmd {
	cmd := exec.Command(command, args...)
	cmd.Env = []string{
		"GOPATH=" + be.Gopath,
	}
	if withEnvPath {
		cmd.Env = append(cmd.Env, "PATH="+be.EnvPath)
	}
	cmd.Stdout = os.Stdout // TODO: Probably not needed
	cmd.Stderr = os.Stderr // TODO: Use log
	return cmd
}

const (
	YearMonthDayHourMinSec = "060201150405"
	CaddyPackage           = "github.com/mholt/caddy"
)
