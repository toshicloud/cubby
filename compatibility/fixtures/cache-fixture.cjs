// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Creates or verifies one incompressible fixture file for cache experiments.
//
// The expected digest is written outside the cached directory, so a separate
// save job and restore job can share it through the experiment directory and a
// cache round trip is verified after the cached directory is deleted and
// restored. Only sizes and digests are printed; file content is never printed.
//
//   node cache-fixture.cjs create --dir <dir> --bytes <n> --expect <file> [--file <name>]
//   node cache-fixture.cjs verify --dir <dir> --expect <file>
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { once } = require('node:events');

const CHUNK_BYTES = 1 << 20;
const DEFAULT_FILE = 'cubby-fixture.bin';

function usage() {
  console.error(
    'usage: cache-fixture.cjs create --dir <dir> --bytes <n> --expect <file> [--file <name>]\n' +
      '       cache-fixture.cjs verify --dir <dir> --expect <file>'
  );
}

function parseArgs(argv) {
  const [mode, ...rest] = argv;
  if (mode !== 'create' && mode !== 'verify') {
    usage();
    process.exit(2);
  }
  const options = {};
  for (let i = 0; i < rest.length; i += 1) {
    const name = rest[i];
    if (!name.startsWith('--')) {
      usage();
      process.exit(2);
    }
    const value = rest[i + 1];
    if (value === undefined) {
      usage();
      process.exit(2);
    }
    options[name.slice(2)] = value;
    i += 1;
  }
  return { mode, options };
}

function requireOption(options, name) {
  const value = options[name];
  if (!value) {
    throw new Error(`--${name} is required`);
  }
  return value;
}

function parseBytes(value) {
  if (!/^[0-9]+$/.test(value)) {
    throw new Error('--bytes must be a non-negative integer');
  }
  const bytes = Number(value);
  if (!Number.isSafeInteger(bytes)) {
    throw new Error('--bytes is out of range');
  }
  return bytes;
}

// fixtureName keeps the fixture relative: a bare file name inside --dir.
function fixtureName(value) {
  if (path.isAbsolute(value) || value.includes('/') || value.includes('\\') || value === '..') {
    throw new Error('--file must be a plain file name');
  }
  return value;
}

function writeExpected(file, expected) {
  const dir = path.dirname(path.resolve(file));
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(file, `${JSON.stringify(expected, null, 2)}\n`);
}

function readExpected(file) {
  let value;
  try {
    value = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    throw new Error('expected digest file is missing or unreadable');
  }
  if (!Number.isSafeInteger(value.bytes) || value.bytes < 0) {
    throw new Error('expected digest file has no usable byte count');
  }
  if (typeof value.sha256 !== 'string' || !/^[0-9a-f]{64}$/.test(value.sha256)) {
    throw new Error('expected digest file has no usable sha256');
  }
  if (typeof value.file !== 'string' || value.file === '' || fixtureName(value.file) !== value.file) {
    throw new Error('expected digest file names no usable fixture file');
  }
  return value;
}

async function create(options) {
  const dir = requireOption(options, 'dir');
  const expectFile = requireOption(options, 'expect');
  const bytes = parseBytes(requireOption(options, 'bytes'));
  const name = fixtureName(options.file || DEFAULT_FILE);
  fs.mkdirSync(dir, { recursive: true });
  const target = path.join(dir, name);
  const out = fs.createWriteStream(target);
  const hash = crypto.createHash('sha256');
  let remaining = bytes;
  while (remaining > 0) {
    const size = Math.min(CHUNK_BYTES, remaining);
    const chunk = crypto.randomBytes(size);
    hash.update(chunk);
    if (!out.write(chunk)) {
      await once(out, 'drain');
    }
    remaining -= size;
  }
  await new Promise((resolve, reject) => {
    out.end((err) => (err ? reject(err) : resolve()));
  });
  const digest = hash.digest('hex');
  writeExpected(expectFile, { file: name, bytes, sha256: digest });
  console.log(`created file=${name} bytes=${bytes} sha256=${digest}`);
}

async function verify(options) {
  const dir = requireOption(options, 'dir');
  const expected = readExpected(requireOption(options, 'expect'));
  const target = path.join(dir, expected.file);
  let actualBytes = 0;
  let actualDigest = 'missing';
  try {
    await new Promise((resolve, reject) => {
      const hash = crypto.createHash('sha256');
      const input = fs.createReadStream(target);
      input.on('data', (chunk) => {
        actualBytes += chunk.length;
        hash.update(chunk);
      });
      input.once('error', reject);
      input.once('end', () => {
        actualDigest = hash.digest('hex');
        resolve();
      });
    });
  } catch (err) {
    if (err.code !== 'ENOENT') {
      throw err;
    }
  }
  const ok = actualDigest === expected.sha256 && actualBytes === expected.bytes;
  if (!ok) {
    console.log(
      `verify FAILED file=${expected.file} expected_bytes=${expected.bytes} expected_sha256=${expected.sha256} ` +
        `actual_bytes=${actualBytes} actual_sha256=${actualDigest}`
    );
    process.exit(1);
  }
  console.log(`verify ok file=${expected.file} bytes=${actualBytes} sha256=${actualDigest}`);
}

async function main() {
  const { mode, options } = parseArgs(process.argv.slice(2));
  if (mode === 'create') {
    await create(options);
    return;
  }
  await verify(options);
}

main().catch((err) => {
  console.error(`fixture: ${err.message}`);
  process.exit(1);
});
