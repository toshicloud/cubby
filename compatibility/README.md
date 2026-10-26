# Compatibility checks

These experiments distinguish local client behavior from a complete GitHub
Actions runner integration. A passing local script does not establish support
for a particular runner, container setup or action release.

## Local client loop

The client proof installs exact `@actions/cache` versions into an external
scratch directory and exercises a built Cubby command through its generated
`NODE_OPTIONS` preload. It uses synthetic credentials and loopback listeners.
The observer records request methods, paths, upload selectors, byte counts and
statuses; it does not record credentials, headers or bodies.

Prerequisites: Linux, Go 1.27, Node.js 24, npm, Bash, curl, tar and zstd. Choose a scratch
directory outside this checkout with room for the generated cache archives:

```sh
PROOF_ROOT="$HOME/cubby-proof" compatibility/client-proof/run.sh
```

Logs default to `$PROOF_ROOT/logs`. `VERSIONS=6.1.0` selects one client.
The driver resets its own per-version storage and fixtures on each run, and
stops the processes it starts. It keeps npm lockfiles in the scratch directory;
retain those files when reproducing a particular dependency resolution.
The Go library and command gain no npm dependencies. The directly exercised
`@actions/cache` and `@azure/storage-blob` packages declare MIT licences.

Each client must save and restore a small file, restore a fallback key, refuse
a duplicate without warning annotations, and save a large random file whose
compressed archive exceeds the client's single-PUT threshold. Large exact and
fallback restores must match the original SHA256. The observer must see block
uploads and a block-list commit with no failed transfers.

## Recorded local result

On 26 October 2026, the Go implementation at `8f45025` passed the client loop:

| Client | Node.js | Azure SDK resolved | Small exact/fallback | Large exact/fallback | Duplicate save |
|---|---|---|---|---|---|
| `@actions/cache` 4.0.5 | 24.22.1 | 12.34.0 | SHA256 matched | SHA256 matched | `-1`, no warning |
| `@actions/cache` 5.1.0 | 24.22.1 | 12.34.0 | SHA256 matched | SHA256 matched | `-1`, no warning |
| `@actions/cache` 6.1.0 | 24.22.1 | 12.34.0 | SHA256 matched | SHA256 matched | `-1`, no warning |

For every client, a 138,412,032-byte random payload produced a
138,416,766-byte archive, above the 134,217,728-byte single-PUT threshold read
from that installed release. The observer recorded three block PUTs and one
block-list PUT. Both large restores matched the original bytes after the input
file had been deleted. Downloads used full responses with status 200; this
experiment did not exercise ranged downloads. Node.js 20 was not tested in
this local client run.

## Stock-runner evidence still required

The remaining experiment uses a disposable Linux VM and an unmodified runner:
separate save and restore jobs, pinned action releases, an explicit
`NODE_OPTIONS` override, real artifact forwarding and a bridge container
boundary probe. Expected digests and cache data must survive between jobs
outside the checkout and the runner's temporary directory. The VM and its
runner registrations are disposable.

No stock-runner or container result is recorded yet. Publish the workflow run
and its assertions before turning these cases into compatibility claims.
