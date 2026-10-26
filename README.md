# cubby

A local cache server for GitHub Actions on self-hosted runners, available as
a Go library and a small command. Cubby stores cache entries on disk and
provides tenant quotas, repository and ref isolation, and least-recently-used
eviction. It uses the Go standard library only.

**Development status:** the initial implementation is under development.
[Local client experiments](compatibility/README.md) exercise small and block
uploads with matching restored bytes. Compatibility with specific GitHub Actions
runner setups is not yet established. There are no releases.

## Build

With Go 1.27 or later, from this checkout:

```sh
go build -o cubby ./cmd/cubby
```

The module path is `go.toshicloud.io/cubby`. The source repository is
[toshicloud/cubby](https://github.com/toshicloud/cubby). The
[import host](https://go.toshicloud.io/) is live. Release publication is still
pending, so build from the checkout for now.

## Run a cache for one isolated job

```sh
./cubby serve \
  --dir ./cache \
  --listen 127.0.0.1:8095 \
  --tenant example \
  --repo example/project \
  --write-ref refs/heads/main \
  --cap 10737418240
```

The command uses a fixed scope: everyone who can reach this listener gets the
configured permissions. Give it a listener isolated to one job. Loopback alone
does not separate jobs sharing a host or network namespace. TCP listeners must
use a literal loopback address; `unix:/absolute/path` is also available for
integrations that provide an HTTP bridge. One process must own a storage
directory at a time.

`--read-ref` is repeatable. The write ref is always searched first, followed by
the configured read refs in order. For a pull request, the provisioner sets the
merge ref as the write ref and supplies the base and default branches as read
refs. The server does not discover job identity or branch relationships.

`--cap` is required and expressed in bytes; a nonpositive value disables cache
operations. The cap covers stored entries. Open uploads have a separate budget,
and concurrent transfers or active downloads can temporarily keep disk use above
the configured cap.

Completed entries have no time-based expiry. Saves trigger LRU eviction within
the tenant's cap. There is no host-wide byte limit; provision disk for the sum of
tenant budgets, staged uploads and transfers in flight.

Generate the runner's `NODE_OPTIONS` value with:

```sh
./cubby node-options --url http://127.0.0.1:8095/ --existing "${NODE_OPTIONS-}"
```

Pass the output as `NODE_OPTIONS` when starting the runner. The preload changes
the results URL only when cache service v2 and the results URL are already set.
An action that replaces `NODE_OPTIONS` will bypass the preload. This targets
Node actions; it is not a redirect for arbitrary processes or Docker Buildx.
The stock runner `.env` setup and client versions still need live-run checks.

The results endpoint is shared with other services. If a job uses artifact
uploads or other results services, set `--upstream` to its original HTTPS results
URL so Cubby can forward non-cache Twirp requests. Without it, those routes
return 404. See [protocol sources and limitations](PROTOCOL.md).

## Embed the library

```go
resolve, err := fixedscope.New(cubby.Scope{
	Tenant:   "example",
	Repo:     "example/project",
	WriteRef: "refs/heads/main",
	CapBytes: 10 << 30,
})
if err != nil {
	return err
}
cache, err := cubby.New(cubby.Options{
	Dir:     "./cache",
	Resolve: resolve,
})
if err != nil {
	return err
}
// Serve cache as an http.Handler on a listener isolated to this job.
```

Import `go.toshicloud.io/cubby` and `go.toshicloud.io/cubby/fixedscope`. For a
shared fleet, supply `Options.Resolve` from trusted job identity instead of
using `fixedscope`. Never trust a job-supplied header to grant another tenant,
repository or ref. Upload and download URLs are unsigned and are authorised
again through the resolver.

`Server.Usage` reports stored entry bytes for a tenant. `Server.RemoveTenant`
removes its cache; the caller should quiesce that tenant's requests first.
`Options.Now` supplies the clock used for cache timestamps and expiry decisions.
The storage directory contains indexes and entries and survives restarts.

## Development

```sh
go vet ./...
go test -race ./...
```

## Licence

[Apache-2.0](LICENSE). See [NOTICE](NOTICE).
