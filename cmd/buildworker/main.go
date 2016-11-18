package main

import (
	"log"
	"net/http"

	"github.com/caddyserver/buildworker"
)

func main() {
	http.HandleFunc("/deploy-plugin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// TODO: MaxBytesReader

		err := r.ParseForm()
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		repo, commit := r.Form.Get("repo"), r.Form.Get("commit")
		if repo == "" || commit == "" {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}

		err = buildworker.DeployPlugin(repo, commit)
		if err != nil {
			log.Printf("checking plugin: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	})

	http.ListenAndServe("127.0.0.1:2017", nil)
}
