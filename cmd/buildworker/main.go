package main

import (
	"crypto/sha1"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/caddyserver/buildworker"
)

var addr = "127.0.0.1:2017"

// Credentials for accessing the API
var (
	apiUsername string
	apiPassword []byte // hashed
)

func init() {
	apiUsername = os.Getenv("BUILDSERVER_ID")
	hash := sha1.New()
	hash.Write([]byte(os.Getenv("BUILDSERVER_KEY")))
	apiPassword = hash.Sum(nil)
}

func main() {
	addRoute := func(method, path string, h http.HandlerFunc) {
		http.HandleFunc(path, methodHandler(method, maxSizeHandler(authHandler(h))))
	}

	addRoute("POST", "/deploy-plugin", func(w http.ResponseWriter, r *http.Request) {
		var info DeployRequest
		err := json.NewDecoder(r.Body).Decode(&info)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if info.CaddyVersion == "" || info.PluginPackage == "" || info.PluginVersion == "" {
			http.Error(w, "missing required field(s)", http.StatusBadRequest)
			return
		}

		be, err := buildworker.Open(info.CaddyVersion, []buildworker.CaddyPlugin{
			{Package: info.PluginPackage, Version: info.PluginVersion},
		})
		if err != nil {
			log.Printf("setting up deploy environment: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer be.Close()

		err = be.Deploy()
		if err != nil {
			log.Printf("deploying plugin: %v", err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	})

	addRoute("POST", "/deploy-caddy", func(w http.ResponseWriter, r *http.Request) {
		var info DeployRequest
		err := json.NewDecoder(r.Body).Decode(&info)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if info.CaddyVersion == "" {
			http.Error(w, "missing required field", http.StatusBadRequest)
			return
		}

		be, err := buildworker.Open(info.CaddyVersion, nil)
		if err != nil {
			log.Printf("setting up deploy environment: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer be.Close()

		err = be.Deploy()
		if err != nil {
			log.Printf("deploying caddy: %v", err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	})

	addRoute("POST", "/build", func(w http.ResponseWriter, r *http.Request) {
		var info BuildRequest
		err := json.NewDecoder(r.Body).Decode(&info)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if info.Platform.OS == "" || info.Platform.Arch == "" {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}

		err = httpBuild(w, info.BuildConfig.CaddyVersion, info.BuildConfig.Plugins, info.Platform)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	})

	fmt.Println("Build worker serving on", addr)
	http.ListenAndServe(addr, nil)
}

// httpBuild builds Caddy according to the configuration in cfg
// and plat, and immediately streams the binary into the response
// body of w.
func httpBuild(w http.ResponseWriter, caddyVersion string, plugins []buildworker.CaddyPlugin, plat buildworker.Platform) error {
	if w == nil {
		return fmt.Errorf("missing ResponseWriter value")
	}

	// make a temporary folder where the result of the build will go
	tmpdir, err := ioutil.TempDir("", "caddy_build_")
	if err != nil {
		return fmt.Errorf("error getting temporary directory: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	// TODO: This does a deep copy of all plugins including their
	// testdata folders and test files. We might be able to
	// add parameters to an alternate Open function so that it can be configured
	// to only copy certain things if we want it to...
	be, err := buildworker.Open(caddyVersion, plugins)
	if err != nil {
		return fmt.Errorf("creating build env: %v", err)
	}
	defer be.Close()

	outputFile, err := be.Build(plat, tmpdir)
	if err != nil {
		return err
	}
	defer outputFile.Close()

	name := filepath.Base(outputFile.Name())

	// Write the file to the response, so we can delete it when
	// the function returns and cleans up its temporary GOPATH.
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, err = io.Copy(w, outputFile)
	if err != nil {
		return fmt.Errorf("copying archive file: %v", err)
	}

	return nil
}

func methodHandler(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	}
}

func maxSizeHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.RawQuery) > MaxQueryStringLength {
			http.Error(w, "query string exceeded length limit", http.StatusRequestURITooLong)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		h.ServeHTTP(w, r)
	}
}

func authHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != apiUsername || !correctPassword(password) {
			truncPass := password
			if len(password) > 5 {
				truncPass = password[:5]
			}
			log.Printf("Wrong credentials: user=%s pass=%s...", username, truncPass)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}
}

func correctPassword(pwd string) bool {
	hash := sha1.New()
	hash.Write([]byte(pwd))
	sum := hash.Sum(nil)
	return subtle.ConstantTimeCompare(sum, apiPassword) == 1
}

type BuildRequest struct {
	buildworker.Platform
	buildworker.BuildConfig
}

type DeployRequest struct {
	CaddyVersion  string `json:"caddy_version"`
	PluginPackage string `json:"plugin_package"`
	PluginVersion string `json:"plugin_version"`
}

const (
	MaxQueryStringLength = 1024 * 100
	MaxBodyBytes         = 1024 * 1024 * 10
)
