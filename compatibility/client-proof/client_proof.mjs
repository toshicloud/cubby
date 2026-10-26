// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Client proof for @actions/cache v2 clients (4.0.5, 5.1.0, 6.1.0) against a
// local cubby server.
//
// Environment:
//   CLIENT_VERSION  the pinned @actions/cache release, e.g. 6.1.0
//   CLIENT_DIR      directory holding node_modules/@actions/cache for it
//   PROOF_ROOT      scratch root; one fresh cwd per case is created under it
//   PROXY_LOG       the logging proxy's JSONL file
//
// The driver runs this with ACTIONS_CACHE_SERVICE_V2=true, a synthetic
// ACTIONS_RESULTS_URL and NODE_OPTIONS printed by `cubby node-options`, so the
// redirect under test is the one that points the client at the proxy. It
// prints one CASE line per case and a PROXY summary, and exits non-zero at the
// first failed case, without retries.

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const version = process.env.CLIENT_VERSION;
const clientDir = process.env.CLIENT_DIR;
const root = process.env.PROOF_ROOT;
const proxyLog = process.env.PROXY_LOG;
if (!version || !clientDir || !root || !proxyLog) {
  throw new Error('CLIENT_VERSION, CLIENT_DIR, PROOF_ROOT and PROXY_LOG are required');
}

const cachePkg = path.join(clientDir, 'node_modules', '@actions/cache');
const entry = path.join(cachePkg, 'lib', 'cache.js');
const mod = await import(pathToFileURL(entry).href);
const api = mod.restoreCache ? mod : mod.default;
const { restoreCache, saveCache } = api;

// Read the thresholds the installed release actually uses.
function product(expr) {
  return expr
    .split('*')
    .map((part) => Number(part.trim()))
    .reduce((a, b) => a * b, 1);
}
const uploadSrc = fs.readFileSync(path.join(cachePkg, 'lib', 'internal', 'uploadUtils.js'), 'utf8');
const cacheSrc = fs.readFileSync(path.join(cachePkg, 'lib', 'cache.js'), 'utf8');
const singleShot = uploadSrc.match(/maxSingleShotSize:\s*([0-9\s*]+)/);
const chunk = cacheSrc.match(/uploadChunkSize:\s*([0-9\s*]+),\s*uploadConcurrency: 8/);
const maxSingleShotSize = singleShot ? product(singleShot[1]) : NaN;
const uploadChunkSize = chunk ? product(chunk[1]) : NaN;
const installedCache = JSON.parse(fs.readFileSync(path.join(cachePkg, 'package.json'), 'utf8')).version;
const installedAzure = JSON.parse(
  fs.readFileSync(path.join(clientDir, 'node_modules', '@azure', 'storage-blob', 'package.json'), 'utf8')
).version;

function assert(cond, message) {
  if (!cond) {
    throw new Error(message);
  }
}

assert(
  installedCache === version,
  `installed @actions/cache is ${installedCache}; the proof was asked for ${version}`
);

function sha256(file) {
  return new Promise((resolve, reject) => {
    const hash = crypto.createHash('sha256');
    const stream = fs.createReadStream(file);
    stream.on('data', (data) => hash.update(data));
    stream.on('end', () => resolve(hash.digest('hex')));
    stream.on('error', reject);
  });
}

function writeRandom(file, size) {
  const fd = fs.openSync(file, 'w');
  try {
    let left = size;
    while (left > 0) {
      const n = Math.min(1 << 20, left);
      fs.writeSync(fd, crypto.randomBytes(n));
      left -= n;
    }
  } finally {
    fs.closeSync(fd);
  }
}

function freshCase(name) {
  const dir = path.join(root, 'cases', name);
  fs.rmSync(dir, { recursive: true, force: true });
  fs.mkdirSync(dir, { recursive: true });
  process.chdir(dir);
  return dir;
}

const results = [];
async function run(name, fn) {
  const started = Date.now();
  const detail = await fn();
  const result = { case: name, ok: true, ms: Date.now() - started, ...detail };
  results.push(result);
  process.stdout.write(`CASE ${JSON.stringify(result)}\n`);
}

const keys = {
  exact: `cubby-${version}-small-exact`,
  fallback: `cubby-${version}-small-fallback`,
  duplicate: `cubby-${version}-small-duplicate`,
  large: `cubby-${version}-large-exact`,
  miss: `cubby-${version}-small-miss`,
};

try {
  await run('small-exact', async () => {
    freshCase('small-exact');
    writeRandom('payload.bin', 64 * 1024);
    const want = await sha256('payload.bin');
    const cacheId = await saveCache(['payload.bin'], keys.exact);
    assert(cacheId > 0, `saveCache returned ${cacheId}; see the client log for its warning`);
    fs.rmSync('payload.bin');
    const matchedKey = await restoreCache(['payload.bin'], keys.exact);
    assert(matchedKey === keys.exact, `matched key ${matchedKey}; want ${keys.exact}`);
    const got = await sha256('payload.bin');
    assert(got === want, 'restored bytes differ from the saved bytes');
    return { key: keys.exact, cacheId, matchedKey, bytes: 64 * 1024, sha256: want };
  });

  await run('small-fallback', async () => {
    freshCase('small-fallback');
    writeRandom('payload.bin', 64 * 1024);
    const want = await sha256('payload.bin');
    const cacheId = await saveCache(['payload.bin'], keys.fallback);
    assert(cacheId > 0, `saveCache returned ${cacheId}`);
    fs.rmSync('payload.bin');
    const matchedKey = await restoreCache(['payload.bin'], keys.miss, [keys.fallback]);
    assert(matchedKey === keys.fallback, `matched key ${matchedKey}; want the restore key ${keys.fallback}`);
    const got = await sha256('payload.bin');
    assert(got === want, 'restored bytes differ from the saved bytes');
    return { key: keys.fallback, cacheId, matchedKey, bytes: 64 * 1024, sha256: want };
  });

  await run('duplicate-save', async () => {
    freshCase('duplicate-save');
    writeRandom('payload.bin', 64 * 1024);
    const want = await sha256('payload.bin');
    const first = await saveCache(['payload.bin'], keys.duplicate);
    assert(first > 0, `first saveCache returned ${first}`);
    let second;
    let threw = false;
    try {
      second = await saveCache(['payload.bin'], keys.duplicate);
    } catch (error) {
      threw = true;
      second = String(error && error.message);
    }
    assert(!threw, `duplicate saveCache threw: ${second}`);
    assert(second === -1, `duplicate saveCache returned ${second}; want the quiet sentinel -1`);
    fs.rmSync('payload.bin');
    const matchedKey = await restoreCache(['payload.bin'], keys.duplicate);
    assert(matchedKey === keys.duplicate, `the first entry no longer restores (matched ${matchedKey})`);
    const got = await sha256('payload.bin');
    assert(got === want, 'restored bytes differ from the saved bytes');
    return { key: keys.duplicate, first, second, threw, matchedKey, sha256: want };
  });

  await run('large-exact-and-fallback', async () => {
    freshCase('large');
    const bytes = 132 * 1024 * 1024;
    writeRandom('payload.bin', bytes);
    const want = await sha256('payload.bin');
    const cacheId = await saveCache(['payload.bin'], keys.large);
    assert(cacheId > 0, `saveCache returned ${cacheId}`);
    fs.rmSync('payload.bin');
    const exact = await restoreCache(['payload.bin'], keys.large);
    assert(exact === keys.large, `exact restore matched ${exact}`);
    const exactSHA = await sha256('payload.bin');
    assert(exactSHA === want, 'exact restore bytes differ');
    fs.rmSync('payload.bin');
    const fallback = await restoreCache(['payload.bin'], keys.miss, [keys.large]);
    assert(fallback === keys.large, `fallback restore matched ${fallback}`);
    const fallbackSHA = await sha256('payload.bin');
    assert(fallbackSHA === want, 'fallback restore bytes differ');
    return {
      key: keys.large,
      bytes,
      maxSingleShotSize,
      cacheId,
      exact,
      fallback,
      sha256: want,
    };
  });
} catch (error) {
  const failed = { case: 'first-failure', ok: false, error: String(error && error.message) };
  results.push(failed);
  process.stdout.write(`CASE ${JSON.stringify(failed)}\n`);
  process.stdout.write('PROOF FAILED\n');
  process.exit(1);
}

// Let the proxy's last response lines reach the log before reading it.
await new Promise((resolve) => setTimeout(resolve, 300));

// Summarise the proxy log and check the large upload really used blocks.
const rows = fs
  .readFileSync(proxyLog, 'utf8')
  .split('\n')
  .filter(Boolean)
  .map((line) => JSON.parse(line));
const upload = rows.filter((row) => row.path.startsWith('/upload/'));
const single = upload.filter((row) => row.comp === null);
const block = upload.filter((row) => row.comp === 'block');
const blocklist = upload.filter((row) => row.comp === 'blocklist');
const download = rows.filter((row) => row.path.startsWith('/download/'));
const twirp = rows.filter((row) => row.path.startsWith('/twirp/'));
const sum = (list) => list.reduce((total, row) => total + row.bytes, 0);
const countBy = (list, field) =>
  list.reduce((counts, row) => {
    counts[row[field]] = (counts[row[field]] ?? 0) + 1;
    return counts;
  }, {});

const summary = {
  version,
  installedCache,
  installedAzure,
  node: process.version,
  maxSingleShotSize,
  uploadChunkSize,
  singlePut: single.length,
  singlePutBytes: sum(single),
  blockPuts: block.length,
  blockBytes: sum(block),
  blocklistPuts: blocklist.length,
  downloads: download.length,
  downloadStatuses: countBy(download, 'status'),
  twirpCalls: twirp.length,
  badUploads: upload.filter((row) => row.status >= 400).length,
  badTwirp: twirp.filter((row) => row.status !== 200).length,
  badDownloads: download.filter((row) => row.status !== 200 && row.status !== 206).length,
};
process.stdout.write(`PROXY ${JSON.stringify(summary)}\n`);

assert(summary.blockPuts > 0, 'the large save never sent comp=block');
assert(summary.blocklistPuts > 0, 'the large save never sent comp=blocklist');
assert(
  summary.blockBytes > maxSingleShotSize,
  `the observed block bytes (${summary.blockBytes}) do not exceed the installed single-shot threshold (${maxSingleShotSize})`
);
assert(summary.badUploads === 0, `${summary.badUploads} upload request(s) failed`);
assert(summary.badTwirp === 0, `${summary.badTwirp} twirp request(s) did not answer 200`);
assert(summary.badDownloads === 0, `${summary.badDownloads} download request(s) failed`);
process.stdout.write('PROOF PASSED\n');
