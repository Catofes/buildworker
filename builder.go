package buildworker

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mholt/archiver"
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
	Repo    string `json:"repo"`    // git clone URL -- TODO: used?
	Package string `json:"package"` // fully qualified package import path
	Version string `json:"version"` // commit, tag, or branch to checkout
}

// BuildConfig holds information to conduct a build of some
// version of Caddy and a number of plugins.
type BuildConfig struct {
	CaddyVersion string        `json:"caddy_version"`
	Plugins      []CaddyPlugin `json:"plugins"`
}

func Build(w http.ResponseWriter, cfg BuildConfig, plat Platform) error {
	if w == nil || plat.OS == "" || plat.Arch == "" {
		return fmt.Errorf("missing required information: response writer, OS, or arch")
	}

	masterGopathMu.RLock()
	defer masterGopathMu.RUnlock()

	be, err := newBuildEnv(cfg.CaddyVersion, cfg.Plugins)
	if err != nil {
		return fmt.Errorf("creating build env: %v", err)
	}
	defer be.cleanup()

	// TODO: This does a deep copy of all plugins including their .git
	// folders and testdata folders and test files. We might be able to
	// add parameters to this setup function so that it can be configured
	// to only copy certain things if we want it to...
	err = be.setup()
	if err != nil {
		return fmt.Errorf("setting up build env: %v", err)
	}

	for _, plugin := range cfg.Plugins {
		err := be.plugInThePlugin(plugin)
		if err != nil {
			return fmt.Errorf("plugging in %s: %v", plugin.Package, err)
		}
	}

	caddyVer := be.BuildCfg.CaddyVersion
	if !strings.HasPrefix(caddyVer, "v") && len(caddyVer) > 8 {
		caddyVer = caddyVer[:8]
	}
	outputName := "caddy_" + plat.OS + "_" + plat.Arch
	if plat.Arch == "arm" {
		outputName += plat.ARM
	}
	outputName += "_" + caddyVer + "_custom"

	binaryOutputName := outputName
	if plat.OS == "windows" {
		binaryOutputName += ".exe"
	}

	err = be.buildCaddy(plat, binaryOutputName)
	if err != nil {
		return fmt.Errorf("building caddy: %v", err)
	}

	// by default, we'll use a .tar.gz archive, but for
	// some OSes, .zip is more regular.
	compressZip := plat.OS == "windows" || plat.OS == "darwin"

	fileList := []string{
		filepath.Join(be.CaddyRepoPath(), "dist", "README.txt"),
		filepath.Join(be.CaddyRepoPath(), "dist", "LICENSES.txt"),
		filepath.Join(be.CaddyRepoPath(), "dist", "CHANGES.txt"),
		filepath.Join(be.CaddyRepoPath(), "dist", "init"),
		filepath.Join(be.CaddyRepoPath(), "caddy", binaryOutputName),
	}

	finalOutputName := filepath.Join(be.CaddyRepoPath(), "dist", outputName)

	if compressZip {
		finalOutputName += ".zip"
		err = archiver.Zip.Make(finalOutputName, fileList)
	} else {
		finalOutputName += ".tar.gz"
		err = archiver.TarGz.Make(finalOutputName, fileList)
	}
	if err != nil {
		return fmt.Errorf("error compressing: %v", err)
	}

	// Write the file to the response, so we can delete it when
	// the function returns and cleans up its temporary GOPATH.
	w.Header().Set("Content-Disposition", `attachment; filename="`+finalOutputName+`"`)
	file, err := os.Open(finalOutputName)
	if err != nil {
		return fmt.Errorf("opening archive file: %v", err)
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	if err != nil {
		return fmt.Errorf("copying archive file: %v", err)
	}

	return nil
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

func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return !os.IsNotExist(err)
	}
	return info.IsDir()
}

func deploy(pkg string, deployEnv BuildEnv) error {
	masterGopathMu.Lock()
	defer masterGopathMu.Unlock()

	// reset all plugins to their tip so `go get -u` will succeed
	for _, plugin := range deployEnv.BuildCfg.Plugins {
		pluginRepo := deployEnv.RepoPath(plugin.Package)
		if dirExists(pluginRepo) {
			err := deployEnv.gitCheckout(pluginRepo, "-")
			if err != nil {
				return fmt.Errorf("resetting git checkout: %v", err)
			}
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

	// go build on various platforms
	be.Log.Println("go build checks")
	for _, plugin := range be.BuildCfg.Plugins {
		err = be.goBuildChecks(plugin.Package)
		if err != nil {
			return fmt.Errorf("go build: %v", err)
		}
	}

	return nil
}

// deepCopy makes a deep file copy of src into dest, overwriting any existing files.
// If an error occurs, not all files were copied successfully. This function blocks.
// If skipHidden is true, files and folders with names beginning with "." are skipped.
// If skipTestFiles is true, files ending with "_test.go" and folders named "testdata"
// are skipped.
func deepCopy(src string, dest string, skipHidden, skipTestFiles bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		// error accessing current file
		if err != nil {
			return err
		}

		// skip files/folders without a name
		if info.Name() == "" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// skip hidden folders, if requested
		if skipHidden && info.Name()[0] == '.' {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// skip testdata folders and _test.go files, if requested
		if skipTestFiles {
			if info.IsDir() && info.Name() == "testdata" {
				return filepath.SkipDir
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), "_test.go") {
				return nil
			}
		}

		// if directory, create destination directory
		if info.IsDir() {
			subdir := strings.TrimPrefix(path, src)
			destdir := filepath.Join(dest, subdir)
			return os.MkdirAll(destdir, info.Mode()&os.ModePerm)
		}

		// open source file
		fsrc, err := os.Open(path)
		if err != nil {
			return err
		}

		// open destination file
		destpath := filepath.Join(dest, strings.TrimPrefix(path, src))
		fdest, err := os.OpenFile(destpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode()&os.ModePerm)
		if err != nil {
			fsrc.Close()
			return err
		}

		// copy the file and ensure it gets flushed to disk
		if _, err = io.Copy(fdest, fsrc); err != nil {
			fsrc.Close()
			fdest.Close()
			return err
		}
		if err = fdest.Sync(); err != nil {
			fsrc.Close()
			fdest.Close()
			return err
		}

		// close both files
		if err = fsrc.Close(); err != nil {
			fdest.Close()
			return err
		}
		if err = fdest.Close(); err != nil {
			return err
		}
		return nil
	})
}
