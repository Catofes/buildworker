Build Worker
============

[![Documentation](https://img.shields.io/badge/godoc-reference-blue.svg?style=flat-square)](https://godoc.org/github.com/caddyserver/buildworker)

Build Worker is the Caddy build server. It maintains a cache of source code repositories in a local GOPATH so that it can build Caddy with desired plugins on demand.

While the `buildworker` package can be used by other programs that wish to build Caddy in dependable ways, the `main` package here adds HTTP handlers so that the Caddy website can request jobs. The build server's handlers are not directly exposed to the Internet, and all requests must be authenticated.

The build server is entirely stateless. It will rebuild its GOPATH (considered merely a cache) if necessary.

Build Worker will assume the GOPATH environment variable is the absolute path to the _master_ GOPATH, which is the GOPATH that will be maintained as new releases are made and from which new builds will be produced. As part of this maintenance, Build Worker will get dependencies with `go get` (for builds) and `go get -u` (for deploys) in `$GOPATH`. If you are not comfortable running these commands in your GOPATH, set the GOPATH environment variable to something else for Build Worker. It will be created from scratch if it does not exist.

When creating a build or running checks to do a new release/deploy, Build Worker creates a temporary directory as a separate GOPATH, copies the requested packages (plugins) into it from the master GOPATH (including the Caddy core packages, of course), and does `git checkout` in that temporary workspace before running tests or builds. This ensures that the tests and builds are using the versions of Caddy and plugins that are desired.

The build worker is optimized for fast, on-demand builds. Deploys (a.k.a. releases) can take a little longer, even several minutes.

The command of this repository is the production build server, and the library is also used by the [Caddy releaser](https://github.com/caddyserver/releaser) tool. The [Caddy developer portal](https://github.com/caddyserver/devportal), which is the backend to the Caddy website, makes requests to this build server.


## Command Usage

Most basic use:

```bash
$ BUILDWORKER_CLIENT_ID=username BUILDWORKER_CLIENT_KEY=password buildworker
```

Replace the credentials with your own secret values. This will start buildworker listening on 127.0.0.1:2017 (you can change the address with the `-addr` option). All requests to buildworker must be authenticated using HTTP Basic Auth with the credentials you've specified.

The `buildworker` command will automatically try to load the OpenPGP private key in `private_key.asc` and decrypt it with the password in `private_key_password.txt` so that builds can be signed. You can change these file paths with the `SIGNING_KEY_FILE` and `KEY_PASSWORD_FILE` environment variables, respectively.

Remember to set the `GOPATH` environment variable to something else if you don't want to run updates in your working GOPATH.


## HTTP Endpoints

### GET /supported-platforms

Get a list of platforms supported for building.

**Example:**

```bash
curl --request GET \
  --url http://localhost:2017/supported-platforms \
  --header 'authorization: Basic ZGV2OmhhcHB5cGFzczEyMw=='
```

### POST /deploy-caddy

Invoke a deploy of Caddy.

**Example:**

```bash
curl --request POST \
  --url http://localhost:2017/deploy-caddy \
  --header 'authorization: Basic ZGV2OmhhcHB5cGFzczEyMw==' \
  --header 'content-type: application/json' \
  --data '{"caddy_version": "master"}'
```

### POST /deploy-plugin

Invoke a deploy of a Caddy plugin.

**Example:**

```bash
curl --request POST \
  --url http://localhost:2017/deploy-plugin \
  --header 'authorization: Basic ZGV2OmhhcHB5cGFzczEyMw==' \
  --header 'content-type: application/json' \
  --data '{
	"caddy_version": "v0.9.4",
	"plugin_package": "github.com/xuqingfeng/caddy-rate-limit",
	"plugin_version": "v1.2"
}'
```


### POST /build

Produce a build of Caddy, optionally with plugins.

**Example:**

```bash
curl --request POST \
  --url http://localhost:2017/build \
  --header 'authorization: Basic ZGV2OmhhcHB5cGFzczEyMw==' \
  --header 'content-type: application/json' \
  --data '{
	"caddy_version": "v0.9.4",
	"GOOS": "darwin",
	"GOARCH": "amd64",
	"plugins": [
		{
			"package": "github.com/xuqingfeng/caddy-rate-limit",
			"version": "164fb914fa8c8d7c9e8d59290cdb0831ace2daef"
		},
		{
			"package": "github.com/abiosoft/caddy-git",
			"version": "v1.3"
		}
	]
}'
```

