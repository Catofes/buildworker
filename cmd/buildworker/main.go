package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/caddyserver/buildworker"
)

var addr = "127.0.0.1:2017"

func main() {
	http.HandleFunc("/deploy-plugin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if len(r.URL.RawQuery) > MaxQueryStringLength {
			http.Error(w, "query string exceeded length limit", http.StatusRequestURITooLong)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

		err := r.ParseForm()
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		pkg, version := r.Form.Get("package"), r.Form.Get("version")
		if pkg == "" || version == "" {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}

		err = buildworker.DeployPlugin(pkg, version)
		if err != nil {
			log.Printf("deploying plugin: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	})

	fmt.Println("Build worker serving on", addr)
	http.ListenAndServe(addr, nil)
}

const (
	MaxQueryStringLength = 1024 * 100
	MaxBodyBytes         = 1024 * 1024 * 10
)
