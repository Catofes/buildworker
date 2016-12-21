package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/crypto/openpgp"

	"github.com/caddyserver/buildworker"
)

var addr = "127.0.0.1:2017"

func init() {
	setAPICredentials()
	setSigningKey()
}

func main() {
	addRoute := func(method, path string, h http.HandlerFunc) {
		http.HandleFunc(path, methodHandler(method, maxSizeHandler(authHandler(h))))
	}

	addRoute("POST", "/deploy-caddy", func(w http.ResponseWriter, r *http.Request) {
		var info buildworker.DeployRequest
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(Error{Message: err.Error(), Log: be.Log.String()})
			return
		}
		defer be.Close()

		err = be.Deploy()
		if err != nil {
			log.Printf("deploying Caddy: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(Error{Message: err.Error(), Log: be.Log.String()})
			return
		}
	})

	addRoute("POST", "/deploy-plugin", func(w http.ResponseWriter, r *http.Request) {
		var info buildworker.DeployRequest
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(Error{Message: err.Error(), Log: be.Log.String()})
			return
		}
		defer be.Close()

		err = be.Deploy()
		if err != nil {
			log.Printf("deploying plugin: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(Error{Message: err.Error(), Log: be.Log.String()})
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

		httpBuild(w, info.BuildConfig.CaddyVersion, info.BuildConfig.Plugins, info.Platform)
	})

	addRoute("GET", "/supported-platforms", func(w http.ResponseWriter, r *http.Request) {
		sup, err := buildworker.SupportedPlatforms(buildworker.UnsupportedPlatforms)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(sup)
	})

	fmt.Println("Build worker serving on", addr)
	http.ListenAndServe(addr, nil)
}

type Error struct {
	Message string
	Log     string
}

// httpBuild builds Caddy according to the configuration in cfg
// and plat, and immediately streams the binary into the response
// body of w.
func httpBuild(w http.ResponseWriter, caddyVersion string, plugins []buildworker.CaddyPlugin, plat buildworker.Platform) {
	internalErr := func(intro string, err error) {
		log.Printf("%s: %v", intro, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}

	// make a temporary folder where the result of the build will go
	tmpdir, err := ioutil.TempDir("", "caddy_build_")
	if err != nil {
		internalErr("error getting temporary directory", err)
		return
	}
	defer os.RemoveAll(tmpdir)

	// TODO: This does a deep copy of all plugins including their
	// testdata folders and test files. We might be able to
	// add parameters to an alternate Open function so that it can be configured
	// to only copy certain things if we want it to...
	be, err := buildworker.Open(caddyVersion, plugins)
	if err != nil {
		log.Printf("creating build env: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Error{Message: err.Error(), Log: be.Log.String()})
		return
	}
	defer be.Close()

	outputFile, err := be.Build(plat, tmpdir)
	if err != nil {
		log.Printf("build: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Error{Message: err.Error(), Log: be.Log.String()})
		return
	}
	defer outputFile.Close()
	name := filepath.Base(outputFile.Name())

	signatureBuf, err := buildworker.Sign(outputFile)
	if err != nil {
		internalErr("signing archive", err)
		return
	}
	signatureName := name + ".asc"

	_, err = outputFile.Seek(0, 0)
	if err != nil {
		internalErr("seeking to beginning of file", err)
		return
	}

	writer := multipart.NewWriter(w)
	w.Header().Set("Content-Type", writer.FormDataContentType())
	part, err := writer.CreateFormFile("signature", signatureName)
	if err != nil {
		internalErr("creating signature form file", err)
		return
	}
	_, err = io.Copy(part, signatureBuf)
	if err != nil {
		internalErr("copying signature into form", err)
		return
	}
	part, err = writer.CreateFormFile("archive", name)
	if err != nil {
		internalErr("creating archive form file", err)
		return
	}
	_, err = io.Copy(part, outputFile)
	if err != nil {
		internalErr("copying archive into form", err)
		return
	}
	err = writer.Close()
	if err != nil {
		internalErr("closing form writer", err)
		return
	}

	return
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

func setAPICredentials() {
	apiUsername = os.Getenv("BUILDSERVER_ID")
	hash := sha1.New()
	hash.Write([]byte(os.Getenv("BUILDSERVER_KEY")))
	apiPassword = hash.Sum(nil)
}

func setSigningKey() {
	signingKeyFile := defaultSigningKeyFile
	keyPasswordFile := defaultKeyPasswordFile

	if custom := os.Getenv("SIGNING_KEY_FILE"); custom != "" {
		signingKeyFile = custom
	}
	if custom := os.Getenv("KEY_PASSWORD_FILE"); custom != "" {
		keyPasswordFile = custom
	}

	// open key file
	privKeyFile, err := os.Open(signingKeyFile)
	if err != nil {
		if os.IsNotExist(err) && signingKeyFile == defaultKeyPasswordFile {
			return // no signing enabled, but not a problem
		}
		log.Fatalf("unable to load signing key file: %v", err)
	}

	// read key file
	entities, err := openpgp.ReadArmoredKeyRing(privKeyFile)
	if err != nil {
		log.Fatalf("reading key file: %v", err)
	}
	if len(entities) < 1 {
		log.Fatal("no entities loaded")
	}
	buildworker.Signer = entities[0]

	if buildworker.Signer.PrivateKey.Encrypted {
		// open and read password file; trim any edge whitespace
		passBytes, err := ioutil.ReadFile(keyPasswordFile)
		if err != nil {
			log.Fatalf("unable to load key password file: %v", err)
		}
		passphrase := bytes.TrimSpace(passBytes)

		// decrypt private key
		err = buildworker.Signer.PrivateKey.Decrypt(passphrase)
		if err != nil {
			log.Fatalf("decrypting private key: %v", err)
		}
	}
}

// BuildRequest is a request for a build of Caddy.
type BuildRequest struct {
	buildworker.Platform
	buildworker.BuildConfig
}

const (
	// The maximum query string length allowed by requests.
	MaxQueryStringLength = 1024 * 100

	// The maximum size allowed for request bodies.
	MaxBodyBytes = 1024 * 1024 * 10
)

// Credentials for accessing the API
var (
	apiUsername string
	apiPassword []byte // hashed
)

// Key for signing binaries/archives
const (
	defaultSigningKeyFile  = "private_key.asc"
	defaultKeyPasswordFile = "private_key_password.txt"
)
