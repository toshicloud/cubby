# Protocol sources and compatibility work

These references document the initial implementation. Reading a client release
or passing local tests does not establish compatibility with a real runner.

## Cache client baseline

[`actions/cache` v4.2.4, `package-lock.json`](https://github.com/actions/cache/blob/v4.2.4/package-lock.json)
pins `@actions/cache` 4.0.5 and `@azure/storage-blob` 12.13.0. The files below
were read from the published
[`@actions/cache` 4.0.5 package](https://registry.npmjs.org/@actions/cache/-/cache-4.0.5.tgz)
on 25 October 2026. Paths are relative to that package's root.

| Topic | Source file | Contract used by Cubby |
|---|---|---|
| Cache service selection | `lib/internal/config.js` | Cache service v2 uses `ACTIONS_RESULTS_URL`; the client selects v2 using `ACTIONS_CACHE_SERVICE_V2` outside GHES. |
| Twirp routes | `lib/internal/shared/cacheTwirpClient.js` | Requests use `/twirp/<service>/<method>` relative to the results URL. |
| Methods and JSON names | `lib/generated/results/api/v1/cache.twirp-client.js`, `lib/generated/results/api/v1/cache.js` | `CreateCacheEntry`, `FinalizeCacheEntryUpload`, `GetCacheEntryDownloadURL`; JSON uses proto field names such as `size_bytes` and `signed_download_url`. |
| Restore and save | `lib/cache.js`, `restoreCacheV2` and `saveCacheV2` | Lookup sends the primary key, version and ordered restore keys; the client uploads to the create response URL and then finalizes. A lookup response with `ok: false` is a miss. |
| Blob transfers | `lib/internal/uploadUtils.js`, `lib/internal/downloadUtils.js` | The client uses the Azure storage SDK for the upload URL and can use it for downloads. |

The published [`@actions/cache` 5.1.0 package](https://registry.npmjs.org/@actions/cache/-/cache-5.1.0.tgz)
and [`6.1.0 package](https://registry.npmjs.org/@actions/cache/-/cache-6.1.0.tgz)
were also inspected on 25 October 2026. In each package's `lib/cache.js`,
`saveCacheV2` emits a workflow warning when `CreateCacheEntry` returns
`ok: false` with a non-empty message that lacks the `cache write denied:`
prefix. Cubby omits messages for ordinary duplicate and in-progress
reservations. Other refusal messages retain their existing behavior. This is
source evidence for the response shape, not a live compatibility result.

The matching SDK release's
[`blockBlob.ts`](https://github.com/Azure/azure-sdk-for-js/blob/adef2d36f8c055171b7bdcf8fd73c68cc4cf6b06/sdk/storage/storage-blob/src/generated/src/operations/blockBlob.ts)
defines the whole-blob PUT, staged-block PUT and block-list PUT operations.
[`blob.ts`](https://github.com/Azure/azure-sdk-for-js/blob/adef2d36f8c055171b7bdcf8fd73c68cc4cf6b06/sdk/storage/storage-blob/src/generated/src/operations/blob.ts)
defines blob download and properties operations. Cubby's implementation is in
`upload.go`, `download.go` and `util.go`; it implements the subset exercised by
its tests, not the complete Azure Blob service.

The server searches each readable ref for an exact primary key, then a
primary-key prefix, then the supplied restore keys. Primary-key prefix lookup
is specified in `@actions/cache` 4.0.5's `restoreCacheV2` documentation and in
[GitHub's cache-key matching reference](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#cache-key-matching).
The resolver supplies the read-ref order; Cubby does not infer branch ancestry.

## Runner preload

[`actions/runner` v2.338.1, `NodeScriptActionHandler.cs`](https://github.com/actions/runner/blob/v2.338.1/src/Runner.Worker/Handlers/NodeScriptActionHandler.cs)
sets `ACTIONS_RESULTS_URL` from the job's service connection in the Node action
environment. It also sets `ACTIONS_CACHE_SERVICE_V2` when enabled by the job.
Cubby's preload changes the URL inside Node after those values exist.

The generated argument uses a base64 data URL so its JavaScript is one
`NODE_OPTIONS` argument. A local Node check verifies the guard and URL change;
this does not verify how every runner setup passes the variable to actions.
The generator normalizes a trailing slash. In the baseline client,
`cacheTwirpClient.js` uses URL resolution for the Twirp route, rather than
concatenating the route to the results URL.

## Initial implementation limits

- No live compatibility run has been recorded for any `actions/cache` release,
  setup action, artifact action or Docker Buildx.
- Confirm a stock runner's `.env` configuration, job-level `NODE_OPTIONS`
  replacement, container jobs and upstream forwarding with real jobs. The Node
  preload does not redirect arbitrary executables.
- The returned upload and download URLs use HTTP and the request's Host.
  They are unsigned; `Resolve` authorises each transfer. The standalone command
  supports loopback TCP and Unix sockets for an isolated job, not public hosting.
- The library implements cache service v2 only. GHES is not supported by the
  baseline client configuration described above.
- Storage is owned by one server process. Atomic renames protect publication;
  full power-loss durability and transactional index rollback are not promised.
- Upload budgets are checked as writes start. Concurrent transfers and entries
  pinned by downloads can delay enforcement of the nominal disk cap.
- Newer clients may add protocol fields or cache modes. Check their actual
  releases and runner requirements before claiming support.

## Custom import host

`docs/vanity/cubby.html` is the prepared discovery page. Serve it over HTTPS at
`go.toshicloud.io/cubby`, including requests with `?go-get=1`, after configuring
DNS. Its `go-import` metadata maps the module to the GitHub repository. It is
not deployed by this change. See the
[Go module discovery rules](https://go.dev/ref/mod#vcs-find).
