package main

import (
	"crypto/sha1"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/caddyserver/buildworker"
)

var addr = "127.0.0.1:2017"

// Credentials for accessing the API
var (
	apiUsername string
	apiPassword []byte // hashed
)

func init() {
	apiUsername = os.Getenv("BUILDWORKER_USERNAME")
	hash := sha1.New()
	hash.Write([]byte(os.Getenv("BUILDWORKER_PASSWORD")))
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

		if info.CaddyVersion == "" || info.PluginPackage == "" ||
			info.PluginVersion == "" || len(info.AllPlugins) == 0 {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}

		err = buildworker.DeployPlugin(info.PluginPackage, info.PluginVersion, info.AllPlugins)
		if err != nil {
			log.Printf("deploying plugin: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
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

		if info.CaddyVersion == "" || len(info.AllPlugins) == 0 {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}

		err = buildworker.DeployCaddy(info.CaddyVersion, info.AllPlugins)
		if err != nil {
			log.Printf("deploying plugin: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
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

		err = buildworker.Build(w, info.BuildConfig, info.Platform)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	})

	fmt.Println("Build worker serving on", addr)
	http.ListenAndServe(addr, nil)
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
	CaddyVersion  string                    `json:"caddy_version"`
	PluginPackage string                    `json:"plugin_package"`
	PluginVersion string                    `json:"plugin_version"`
	AllPlugins    []buildworker.CaddyPlugin `json:"all_plugins"`
}

const (
	MaxQueryStringLength = 1024 * 100
	MaxBodyBytes         = 1024 * 1024 * 10
)
