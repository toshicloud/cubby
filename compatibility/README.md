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

## Published action bundles

The published save and restore bundles also passed a separate local check on
26 October 2026 against the same Go implementation. Every bundle ran on
Node.js 24.22.1 with synthetic runtime credentials.

| Action release | Commit | Bundled Azure SDK | Small and large exact/fallback restores |
|---|---|---|---|
| v4.2.4 | [`0400d5f`](https://github.com/actions/cache/tree/0400d5f644dc74513175e3cd8d07132dd4860809) | 12.13.0 | SHA256 and matched keys passed |
| v5.1.0 | [`caa2961`](https://github.com/actions/cache/tree/caa296126883cff596d87d8935842f9db880ef25) | 12.30.0 | SHA256 and matched keys passed |
| v6.1.0 | [`55cc834`](https://github.com/actions/cache/tree/55cc8345863c7cc4c66a329aec7e433d2d1c52a9) | 12.32.0 | SHA256 and matched keys passed |

Each 160 MiB random payload produced an archive above the single-PUT limit,
three block PUTs, one block-list commit and a stored manifest. Exact restores
reported `cache-hit=true`; fallback restores reported `cache-hit=false` and
the expected saved key. The input directory was removed before every restore.
Disabling the redirect made the verification fail even though the save action
exited successfully, confirming that the check required saved and restored data.

The v4 action declares Node.js 20, so its declared runtime remains untested by
these local checks. The v5 and v6 actions declare Node.js 24. None of these
bundle runs used a GitHub Actions runner.

## Manual stock-runner experiment

The [compatibility workflow](../.github/workflows/compatibility.yml) runs only
through `workflow_dispatch`. It must be present on the default branch before
GitHub accepts a dispatch. Supply `runner_label` for a dedicated disposable
Linux x64 VM with Docker, Bash, curl, tar and zstd. Do not attach a shared
development machine or production runner to this experiment.

The workflow has three sequential jobs on the same VM:

1. Save small and large fixtures with pinned v4, v5 and v6 actions, and save
   another fixture through GitHub's original endpoint with `NODE_OPTIONS` cleared.
2. Restore exact and fallback keys, verify the saved SHA256 digests, restore the
   override fixture, and check an artifact upload/download through Cubby's upstream.
3. Run a normal bridge container and require that it cannot reach a live host
   loopback sentinel. A heartbeat must advance after the connection attempt;
   a dead host process cannot satisfy the check.

Use fresh single-job runner registrations while preserving the VM and its
experiment directory across all three jobs. The optional `CUBBY_PROOF_ROOT`
repository variable selects that directory; otherwise the workflow uses
`$RUNNER_TOOL_CACHE/cubby-compatibility`. It must be outside the checkout and
`RUNNER_TEMP`. Each workflow attempt gets its own cache, binary and expected
digests beneath this root. Allow several GiB of scratch space.

Before each runner listener starts, its root `.env` must already contain an
unquoted `NODE_OPTIONS=` line whose value is generated by:

```sh
cubby node-options --url http://127.0.0.1:8123/
```

The workflow starts the request observer on loopback port 8123 and Cubby on
8095. The runner supplies the original HTTPS results endpoint to each server
start for upstream forwarding. The host sentinel uses loopback port 18095.
The VM supervisor must preserve that sentinel between the second and third
jobs; clearing `RUNNER_TRACKING_ID` only avoids the runner's own orphan cleanup.
The sentinel stops after 15 minutes, and the VM must be destroyed after the
experiment or its infrastructure timeout.

The workflow checks actual digests, restore outputs and observed successful
requests. Its observer records no headers, bodies or credentials. Keep runner
registrations and logs private; publish only reviewed results and the workflow
run link.

## Stock-runner evidence still required

The remaining experiment uses a disposable Linux VM and an unmodified runner:
separate save and restore jobs, pinned action releases, an explicit
`NODE_OPTIONS` override, real artifact forwarding and a bridge container
boundary probe. Expected digests and cache data must survive between jobs
outside the checkout and the runner's temporary directory. The VM and its
runner registrations are disposable.

No stock-runner or container result is recorded yet. Publish the workflow run
and its assertions before turning these cases into compatibility claims.
