package buildworker

import (
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func init() {
	var err error
	absoluteMasterGopath, err = filepath.Abs(masterGopath)
	if err != nil {
		log.Fatal(err)
	}
}

// A Workspace is a folder path which will hold one or more GOPATHs.
type Workspace string

// workspace is where we do concurrent builds and tests.
const workspace = Workspace("./workspace")

// masterGopath is where we store our master copy of repos.
const masterGopath = "./gopath"

// masterGopathMu protects the masterGopath from concurrent writes.
var masterGopathMu sync.RWMutex

// absoluteMasterGopath is the absolute form of masterGopath.
var absoluteMasterGopath string

type LogBuffer struct{ io.Writer }

const logTimeFormat = "2006/01/02 15:04:05"

func (b LogBuffer) Println(a ...interface{}) {
	fmt.Fprintf(b, "%s ", time.Now().Format(logTimeFormat))
	fmt.Fprintln(b, a...)
}

func (b LogBuffer) Printf(format string, a ...interface{}) {
	fmt.Fprintf(b, "%s ", time.Now().Format(logTimeFormat))
	fmt.Fprintf(b, format, a...)
	if !strings.HasSuffix(format, "\n") {
		fmt.Fprintln(b)
	}
}

// CaddyPlugin holds information about a Caddy plugin to build.
type CaddyPlugin struct {
	Repo    string `json:"repo"`    // git clone URL
	Package string `json:"package"` // fully qualified package import path
	Version string `json:"version"` // commit, tag, or branch to checkout
}

// BuildConfig holds information to conduct a build of some
// version of Caddy and a number of plugins.
type BuildConfig struct {
	CaddyVersion string        `json:"caddy_version"`
	Plugins      []CaddyPlugin `json:"plugins"`
}

// DeployCaddy begins the pipeline that deploys
// an update to the main Caddy repo.
func DeployCaddy(version string, allPlugins []CaddyPlugin) error {
	be, err := newBuildEnv(version, nil)
	if err != nil {
		return err
	}
	defer be.cleanup()

	err = be.setup()
	if err != nil {
		return err
	}

	deployEnv := BuildEnv{
		Log:     be.Log,
		EnvPath: be.EnvPath,
		Gopath:  absoluteMasterGopath,
		BuildCfg: BuildConfig{
			CaddyVersion: version,
			Plugins:      allPlugins,
		},
	}

	return deploy(CaddyPackage, deployEnv)
}

// DeployPlugin begins the pipeline that deploys a new plugin.
// It blocks until the plugin is finished deploying, or an error
// occurs.
func DeployPlugin(pkg, version string, allPlugins []CaddyPlugin) error {
	be, err := newBuildEnv("master", []CaddyPlugin{ // TODO: which version is currently deployed...?
		{Package: pkg, Version: version}, // TODO: repo URL?
	})
	if err != nil {
		return err
	}
	defer be.cleanup()

	err = checkPlugin(be)
	if err != nil {
		return err
	}

	deployEnv := BuildEnv{
		Log:     be.Log,
		EnvPath: be.EnvPath,
		Gopath:  absoluteMasterGopath,
		BuildCfg: BuildConfig{
			CaddyVersion: be.BuildCfg.CaddyVersion,
			Plugins:      allPlugins,
		},
	}

	return deploy(pkg, deployEnv)
}

func deploy(pkg string, deployEnv BuildEnv) error {
	masterGopathMu.Lock()
	defer masterGopathMu.Unlock()

	for _, plugin := range deployEnv.BuildCfg.Plugins {
		err := deployEnv.gitCheckout(deployEnv.RepoPath(plugin.Package), "-")
		if err != nil {
			return fmt.Errorf("resetting git checkout: %v", err)
		}
	}

	// run `go get -u` to get the latest into the master GOPATH.
	cmd := deployEnv.makeCommand(true, "go", "get", "-u", "-d", "-x", pkg+"/...")
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("go get into deploy env: %v", err)
	}

	// the `go get -u` we just ran might have overwritten the versions
	// of any plugins we had checked out; running 'setup' should be
	// able to restore them to the versions we intend to use because
	// it does not use the -u flag when running `go get`.
	err = deployEnv.setup()
	if err != nil {
		return fmt.Errorf("restoring deploy env: %v", err)
	}

	return nil
}

func CheckPlugin(pkg, version string) error {
	be, err := newBuildEnv("master", []CaddyPlugin{ // TODO: latest caddy version...?
		{Package: pkg, Version: version}, // TODO: repo URL?
	})
	if err != nil {
		return err
	}
	defer be.cleanup()
	return checkPlugin(be)
}

func checkPlugin(be BuildEnv) error {
	err := be.setup()
	if err != nil {
		return err
	}

	// go vet the plugin
	be.Log.Println("go vet the plugins")
	for _, plugin := range be.BuildCfg.Plugins {
		err = be.goVet(plugin.Package)
		if err != nil {
			return fmt.Errorf("go vet plugin: %v", err)
		}
	}

	// go test the plugin
	be.Log.Println("go test the plugins")
	for _, plugin := range be.BuildCfg.Plugins {
		err = be.goTest(plugin.Package)
		if err != nil {
			return fmt.Errorf("go test plugin: %v", err)
		}
	}

	// go build
	be.Log.Println("go build checks")
	for _, plugin := range be.BuildCfg.Plugins {
		err = be.goBuildChecks(plugin.Package)
		if err != nil {
			return fmt.Errorf("go build: %v", err)
		}
	}

	// plug in the plugin
	be.Log.Println("Plugging in the plugins")
	for _, plugin := range be.BuildCfg.Plugins {
		err = be.plugInThePlugin(plugin)
		if err != nil {
			return fmt.Errorf("plugging in plugin: %v", err)
		}
	}

	// go test Caddy with the plugin installed
	be.Log.Println("go test Caddy with plugin installed")
	err = be.goTest(CaddyPackage)
	if err != nil {
		return fmt.Errorf("go test caddy with plugin: %v", err)
	}

	return nil
}
