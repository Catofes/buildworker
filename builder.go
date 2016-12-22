package buildworker

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/openpgp"
)

// Signer is the entity which can sign builds.
// Its private key must be decrypted.
var Signer *openpgp.Entity

// TODO: Maintain master gopath (when? master gopaths are
// scoped to individual BuildEnvs) by pruning unused packages...

var gopathLocks = make(map[string]*sync.RWMutex)

func lock(gopath string) {
	if _, ok := gopathLocks[gopath]; !ok {
		gopathLocks[gopath] = new(sync.RWMutex)
	}
	gopathLocks[gopath].Lock()
}

func unlock(gopath string) {
	gopathLocks[gopath].Unlock()
}

func rlock(gopath string) {
	if _, ok := gopathLocks[gopath]; !ok {
		gopathLocks[gopath] = new(sync.RWMutex)
	}
	gopathLocks[gopath].RLock()
}

func runlock(gopath string) {
	gopathLocks[gopath].RUnlock()
}

// CaddyPlugin holds information about a Caddy plugin to build.
type CaddyPlugin struct {
	Package string `json:"package"` // fully qualified package import path
	Version string `json:"version"` // commit, tag, or branch to checkout
	Repo    string `json:"repo"`    // git clone URL -- TODO: used?
	Name    string `json:"-"`       // name of plugin: not used here, but used by devportal
	ID      string `json:"-"`       // ID of plugin: not used here, but used by devportal
}

// BuildConfig holds information to conduct a build of some
// version of Caddy and a number of plugins.
type BuildConfig struct {
	CaddyVersion string        `json:"caddy_version"`
	Plugins      []CaddyPlugin `json:"plugins"`
}

const ldFlagVarPkg = "github.com/mholt/caddy/caddy/caddymain"

// makeLdFlags makes a string to pass in as ldflags when building Caddy.
// This automates proper versioning, so it uses git to get information
// about the current version of Caddy.
func makeLdFlags(repoPath string) (string, error) {
	run := func(cmd *exec.Cmd, ignoreError bool) (string, error) {
		cmd.Dir = repoPath
		out, err := cmd.Output()
		if err != nil && !ignoreError {
			return string(out), err
		}
		return strings.TrimSpace(string(out)), nil
	}

	var ldflags []string

	for _, ldvar := range []struct {
		name  string
		value func() (string, error)
	}{
		// Timestamp of build
		{
			name: "buildDate",
			value: func() (string, error) {
				return time.Now().UTC().Format("Mon Jan 02 15:04:05 MST 2006"), nil
			},
		},

		// Current tag, if HEAD is on a tag
		{
			name: "gitTag",
			value: func() (string, error) {
				// OK to ignore error since HEAD may not be at a tag
				return run(exec.Command("git", "describe", "--exact-match", "HEAD"), true)
			},
		},

		// Nearest tag on branch
		{
			name: "gitNearestTag",
			value: func() (string, error) {
				return run(exec.Command("git", "describe", "--abbrev=0", "--tags", "HEAD"), false)
			},
		},

		// Commit SHA
		{
			name: "gitCommit",
			value: func() (string, error) {
				return run(exec.Command("git", "rev-parse", "--short", "HEAD"), false)
			},
		},

		// Summary of uncommitted changes
		{
			name: "gitShortStat",
			value: func() (string, error) {
				return run(exec.Command("git", "diff-index", "--shortstat", "HEAD"), false)
			},
		},

		// List of modified files
		{
			name: "gitFilesModified",
			value: func() (string, error) {
				return run(exec.Command("git", "diff-index", "--name-only", "HEAD"), false)
			},
		},
	} {
		value, err := ldvar.value()
		if err != nil {
			return "", err
		}
		ldflags = append(ldflags, fmt.Sprintf(`-X "%s.%s=%s"`, ldFlagVarPkg, ldvar.name, value))
	}

	return strings.Join(ldflags, " "), nil
}

// dirExists returns true if dir exists and is a
// directory, or false in any other case.
func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return !os.IsNotExist(err)
	}
	return info.IsDir()
}

// deepCopy makes a deep file copy of src into dest, overwriting any existing files.
// If an error occurs, not all files were copied successfully. This function blocks.
// If skipHidden is true, files and folders with names beginning with "." are skipped.
// If skipTestFiles is true, files ending with "_test.go" and folders named "testdata"
// are skipped. If skipSymlinks is true, symbolic links will not be evaluated and will
// be skipped.
func deepCopy(src string, dest string, skipHidden, skipTestFiles, skipSymlinks bool) error {
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

		// skip symlinks, if requested
		if skipSymlinks && (info.Mode()&os.ModeSymlink > 0) {
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

		destpath := filepath.Join(dest, strings.TrimPrefix(path, src))
		fdest, err := os.OpenFile(destpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode()&os.ModePerm)
		if err != nil {
			fsrc.Close()
			if _, err := os.Stat(destpath); err == nil {
				return fmt.Errorf("opening destination (which already exists): %v", err)
			}
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

// DeployRequest represents a request to test an updated
// version of a plugin against a specific Caddy version.
type DeployRequest struct {
	// The version of Caddy into which to plug in.
	CaddyVersion string `json:"caddy_version"`

	// The import (package) path of the plugin, and its version.
	PluginPackage string `json:"plugin_package"`
	PluginVersion string `json:"plugin_version"`

	// The list of platforms on which the plugin(s) must
	// build successfully.
	RequiredPlatforms []Platform `json:"required_platforms"`
}

// BuildRequest is a request for a build of Caddy.
type BuildRequest struct {
	Platform
	BuildConfig
}

// Sign signs the file using the configured PGP private key
// and returns the ASCII-armored bytes, or an error.
func Sign(file *os.File) (*bytes.Buffer, error) {
	if Signer == nil {
		return nil, fmt.Errorf("no signing key loaded")
	}
	buf := new(bytes.Buffer)
	err := openpgp.ArmoredDetachSign(buf, Signer, file, nil)
	if err != nil {
		return nil, fmt.Errorf("signing error: %v", err)
	}
	return buf, nil
}
