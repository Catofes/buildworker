Build Worker
============

[![Documentation](https://img.shields.io/badge/godoc-reference-blue.svg?style=flat-square)](https://godoc.org/github.com/caddyserver/buildworker)

Build Worker is the Caddy build server. It maintains a cache of source code repositories in a local GOPATH so that it can build Caddy with desired plugins on demand.

While the `buildworker` package can be used by other programs that wish to build Caddy in dependable ways, the `main` package here adds HTTP handlers so that the Caddy website can request jobs. The build server's handlers are not directly exposed to the Internet, and all requests must be authenticated.

The build server is entirely stateless. It will rebuild its GOPATH (considered merely a cache) if necessary.

Build Worker will assume the GOPATH environment variable is the absolute path to the _master_ GOPATH, which is the GOPATH that will be maintained as new releases are made and from which new builds will be produced. As part of this maintenance, Build Worker will update dependencies with `go get` (for builds) and `go get -u` (for deploys) in `$GOPATH`. If you are not comfortable running these commands in your GOPATH, set the GOPATH environment variable to something else for Build Worker. It will be created from scratch if it does not exist.

When creating a build or running checks to do a new release/deploy, Build Worker creates a temporary directory as a separate GOPATH, copies the requested packages (plugins) into it from the master GOPATH (including the Caddy core packages, of course), and does `git checkout` in that temporary workspace before running tests or builds. This ensures that the tests and builds are using the versions of Caddy and plugins that are desired.

The build worker is optimized for fast, on-demand builds. Deploys (a.k.a. releases) can take a little longer, even several minutes.
