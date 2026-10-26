#!/usr/bin/env bash
# Copyright 2026 The cubby contributors
# SPDX-License-Identifier: Apache-2.0
# Driver for the cubby @actions/cache client proof.
#
# Requires an explicit scratch root:
#   PROOF_ROOT=/path/to/scratch ./run.sh
# VERSIONS defaults to "4.0.5 5.1.0 6.1.0"; LOG_DIR defaults to
# $PROOF_ROOT/logs. It builds cubby (through `slot` when installed, plain
# `go build` otherwise), installs each pinned client into its own isolated
# directory (npm --ignore-scripts, package-lock kept), and runs the Node proof
# once per client against cubby on loopback behind the logging proxy.
#
# The proxy is reusable by other harnesses as-is. Its environment contract is
#   PROXY_PORT  loopback port to listen on
#   CUBBY_PORT  loopback port of the cubby server to forward to
#   PROXY_LOG   JSONL file; one line per request with method, path, comp,
#               request body bytes and status - no headers, tokens or bodies
#
# Scratch data stays under PROOF_ROOT; logs use LOG_DIR.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/../.." && pwd)"
VERSIONS="${VERSIONS:-4.0.5 5.1.0 6.1.0}"
CAP_BYTES="${CAP_BYTES:-4294967296}"

if [ -z "${PROOF_ROOT:-}" ]; then
  echo "PROOF_ROOT is required: an explicit scratch directory, for example" >&2
  echo "  PROOF_ROOT=\"\$HOME/cubby-proof\" $0" >&2
  exit 2
fi

# Refuse a scratch root we must never delete inside: the filesystem root, the
# repository worktree, anything inside it, or anything that contains it.
case "$PROOF_ROOT" in
  /*) ;;
  *)
    echo "PROOF_ROOT must be an absolute path: $PROOF_ROOT" >&2
    exit 2
    ;;
esac
mkdir -p "$PROOF_ROOT"
PROOF_ROOT="$(cd "$PROOF_ROOT" && pwd -P)"
REPO_REAL="$(cd "$REPO" && pwd -P)"
if [ "$PROOF_ROOT" = "/" ]; then
  echo "PROOF_ROOT must not be the filesystem root" >&2
  exit 2
fi
if [ "$PROOF_ROOT" = "$REPO_REAL" ] || [[ "$PROOF_ROOT" == "$REPO_REAL"/* ]]; then
  echo "PROOF_ROOT must not be the worktree or inside it: $PROOF_ROOT" >&2
  exit 2
fi
if [[ "$REPO_REAL" == "$PROOF_ROOT"/* ]]; then
  echo "PROOF_ROOT must not contain the worktree: $PROOF_ROOT" >&2
  exit 2
fi

# Validate every version token before it can reach a path we delete.
for v in $VERSIONS; do
  if ! [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "invalid version token: $v (want N.N.N)" >&2
    exit 2
  fi
done

LOG_DIR="${LOG_DIR:-$PROOF_ROOT/logs}"
case "$LOG_DIR" in
  /*) ;;
  *) LOG_DIR="$PWD/$LOG_DIR" ;;
esac
BIN="$PROOF_ROOT/bin/cubby"

mkdir -p "$PROOF_ROOT"/{bin,clients,storage,cases,tmp} "$LOG_DIR"

# Only PIDs started by this invocation are ever signalled.
CLIENT_PID=""
CUBBY_PID=""
PROXY_PID=""
# Called by the EXIT trap below.
# shellcheck disable=SC2329
cleanup() {
  local code=$? pid
  trap - EXIT INT TERM
  for pid in "$CLIENT_PID" "$PROXY_PID" "$CUBBY_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$CLIENT_PID" "$PROXY_PID" "$CUBBY_PID"; do
    if [ -n "$pid" ]; then wait "$pid" 2>/dev/null || true; fi
  done
  exit "$code"
}
trap 'cleanup' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "== build =="
if command -v slot >/dev/null 2>&1; then
  ( cd "$REPO" && slot -- go build -o "$BIN" ./cmd/cubby )
else
  ( cd "$REPO" && go build -o "$BIN" ./cmd/cubby )
fi

free_port() {
  node -e "const s=require('node:net').createServer();s.listen(0,'127.0.0.1',()=>{console.log(s.address().port);s.close()})"
}

wait_http() {
  local url=$1 code
  for _ in $(seq 1 100); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "$url" 2>/dev/null || true)
    if [ "$code" != "000" ]; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

status=0
for v in $VERSIONS; do
  cdir="$PROOF_ROOT/clients/cache-$v"
  if [ ! -d "$cdir/node_modules/@actions/cache" ]; then
    mkdir -p "$cdir"
    if ! ( cd "$cdir" && npm init -y >/dev/null 2>&1 && npm install --no-audit --no-fund --ignore-scripts --save-exact "@actions/cache@$v" ) >"$LOG_DIR/npm-$v.log" 2>&1; then
      echo "FAIL $v: npm install failed, see $LOG_DIR/npm-$v.log" >&2
      exit 1
    fi
  fi

  # Verify the installed release is the requested one, every run. Read the
  # package.json files directly: newer releases export no './package.json'
  # subpath, so require() of it fails.
  pkg_version() {
    node -e "process.stdout.write(JSON.parse(require('node:fs').readFileSync(process.argv[1], 'utf8')).version)" "$1"
  }
  installed_cache="$(pkg_version "$cdir/node_modules/@actions/cache/package.json")"
  if [ "$installed_cache" != "$v" ]; then
    echo "FAIL $v: installed @actions/cache is $installed_cache" >&2
    exit 1
  fi
  installed_azure="$(pkg_version "$cdir/node_modules/@azure/storage-blob/package.json")"
  echo "[$v] installed @actions/cache $installed_cache, @azure/storage-blob $installed_azure"

  cubby_log="$LOG_DIR/cubby-$v.log"
  proxy_log="$LOG_DIR/proxy-$v.log"
  proxy_out="$LOG_DIR/proxy-$v.out"
  client_log="$LOG_DIR/client-$v.log"
  rm -rf -- "$PROOF_ROOT/storage/$v" "$PROOF_ROOT/cases/$v" "$PROOF_ROOT/tmp/$v"
  mkdir -p "$PROOF_ROOT/storage/$v" "$PROOF_ROOT/cases/$v" "$PROOF_ROOT/tmp/$v"
  : >"$proxy_log"

  cubby_port=$(free_port)
  proxy_port=$(free_port)
  while [ "$proxy_port" = "$cubby_port" ]; do
    proxy_port=$(free_port)
  done

  "$BIN" serve \
    --dir "$PROOF_ROOT/storage/$v" \
    --listen "127.0.0.1:$cubby_port" \
    --tenant proof --repo o/r --write-ref refs/heads/main --read-ref refs/heads/main \
    --cap "$CAP_BYTES" >"$cubby_log" 2>&1 &
  CUBBY_PID=$!

  CUBBY_PORT="$cubby_port" PROXY_PORT="$proxy_port" PROXY_LOG="$proxy_log" \
    node "$SCRIPT_DIR/proxy.mjs" >"$proxy_out" 2>&1 &
  PROXY_PID=$!

  if ! wait_http "http://127.0.0.1:$cubby_port/" || ! wait_http "http://127.0.0.1:$proxy_port/"; then
    echo "FAIL $v: server did not become ready (cubby: $cubby_log, proxy: $proxy_out)" >&2
    exit 1
  fi

  node_options=$("$BIN" node-options --url "http://127.0.0.1:$proxy_port")

  env -i \
    PATH="$PATH" HOME="$HOME" TMPDIR="$PROOF_ROOT/tmp/$v" RUNNER_TEMP="$PROOF_ROOT/tmp/$v" \
    NODE_OPTIONS="$node_options" \
    ACTIONS_CACHE_SERVICE_V2=true \
    ACTIONS_RESULTS_URL="https://example.invalid/" \
    ACTIONS_RUNTIME_TOKEN="local-proof-token" \
    CLIENT_VERSION="$v" \
    CLIENT_DIR="$cdir" \
    PROOF_ROOT="$PROOF_ROOT/cases/$v" \
    PROXY_LOG="$proxy_log" \
    node "$SCRIPT_DIR/client_proof.mjs" >"$client_log" 2>&1 &
  CLIENT_PID=$!
  rc=0
  if wait "$CLIENT_PID"; then
    rc=0
  else
    rc=$?
  fi
  CLIENT_PID=""

  kill "$PROXY_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  PROXY_PID=""
  kill "$CUBBY_PID" 2>/dev/null || true
  wait "$CUBBY_PID" 2>/dev/null || true
  CUBBY_PID=""

  # Record the stored entry shapes: a block upload becomes an entry directory
  # with manifest.json and blocks/, a single-shot upload a plain file.
  {
    echo "storage shape after the @actions/cache $v run"
    for entry in "$PROOF_ROOT/storage/$v"/*/entries/*; do
      [ -e "$entry" ] || continue
      if [ -d "$entry" ]; then
        echo "entry $(basename "$entry"): directory"
        cat "$entry/manifest.json" 2>/dev/null || echo '(no manifest)'
        echo
      else
        echo "entry $(basename "$entry"): file $(stat -c%s "$entry") bytes"
      fi
    done
  } >"$LOG_DIR/storage-$v.txt" 2>&1

  # Unexpected client annotations are failures; the duplicate-save collision is
  # expected to be reported at info level, not as a warning or error.
  annotations=""
  if annotations=$(grep -nE '^::(warning|error)( |::)' "$client_log"); then
    :
  fi
  if [ -n "$annotations" ]; then
    echo "FAIL $v: unexpected client annotations" >&2
    printf '%s\n' "$annotations" >&2
    status=1
  fi

  if [ "$rc" -ne 0 ]; then
    echo "FAIL $v (exit $rc): $client_log"
    status=1
  elif [ -z "$annotations" ]; then
    echo "PASS $v: $client_log (installed @actions/cache $installed_cache, @azure/storage-blob $installed_azure)"
  fi
  grep -E '^(CASE|PROXY|PROOF)' "$client_log" | sed "s/^/[$v] /"
done

exit "$status"
