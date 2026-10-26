# Import host

This Cloudflare Worker serves Go's discovery metadata for
`go.toshicloud.io/cubby` and its subpackages. Ordinary browser requests redirect
to the source repository; unrelated paths return 404. It is independent of the
cache server and stores no cache data.

The domain owner deploys from this directory with an authenticated Wrangler:

```sh
wrangler deploy
```

The configured [Custom Domain](https://developers.cloudflare.com/workers/configuration/routing/custom-domains/)
creates the DNS record and certificate. After deployment, verify the
[Go discovery response](https://go.dev/ref/mod#vcs-find) at both the module root
and a subpackage:

```sh
curl --fail 'https://go.toshicloud.io/cubby?go-get=1'
curl --fail 'https://go.toshicloud.io/cubby/fixedscope?go-get=1'
curl --head 'https://go.toshicloud.io/cubby'
```

After publishing a release tag, check that exact version from a clean module
cache, for example with `GOPROXY=direct go mod download -json
go.toshicloud.io/cubby@v0.1.0`. A prepared configuration does not establish that
the host or version is available.
