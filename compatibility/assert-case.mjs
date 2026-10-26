#!/usr/bin/env node
// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0
//
// Assertions for the manual compatibility proof.
//
//   assert-case.mjs restore <expected-hit> <hit> <matched> <expected-key> <dir> <expect>
//   assert-case.mjs observer-save --log <file> --from <n> [--whole] [--blocks]
//                                 [--min-bytes <n>] [--min-block-bytes <n>]
//   assert-case.mjs observer-count --log <file> --from <n> --contains <text>
//                                  [--status-class <n>] [--min <n>] [--max <n>]
//
// The observer log holds one JSON line per completed request with method, path,
// comp, bytes and status only; it never holds headers, tokens or bodies. The
// fixture digest check is delegated to the fixture helper that wrote it.

import { spawnSync } from 'node:child_process';
import fs from 'node:fs';

function fail(message) {
  console.error(`::error::${message}`);
  process.exit(1);
}

function option(name, required = false) {
  const index = process.argv.indexOf(`--${name}`);
  if (index === -1) {
    if (required) {
      fail(`--${name} is required`);
    }
    return undefined;
  }
  const value = process.argv[index + 1];
  if (value === undefined || value.startsWith('--')) {
    fail(`--${name} needs a value`);
  }
  return value;
}

function countOption(name) {
  const raw = option(name);
  if (raw === undefined) {
    return undefined;
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < 0) {
    fail(`--${name} must be a non-negative integer`);
  }
  return value;
}

function readRecords() {
  const log = option('log', true);
  const from = countOption('from');
  if (from === undefined) {
    fail('--from is required');
  }
  const lines = fs
    .readFileSync(log, 'utf8')
    .split('\n')
    .filter((line) => line.trim() !== '');
  if (from > lines.length) {
    fail(`--from ${from} is past the ${lines.length} recorded requests`);
  }
  return lines.slice(from).map((line) => {
    try {
      return JSON.parse(line);
    } catch {
      return fail('the observer log holds a line that is not JSON');
    }
  });
}

function succeeded(record) {
  return typeof record.status === 'number' && record.status >= 200 && record.status < 300;
}

function restore() {
  const [expectedHit, hit, matched, expectedKey, dir, expect] = process.argv.slice(3);
  for (const value of [expectedHit, hit, matched, expectedKey, dir, expect]) {
    if (value === undefined || value === '') {
      fail('restore needs six arguments');
    }
  }
  if (expectedHit !== 'true' && expectedHit !== 'false') {
    fail('the expected hit must be true or false');
  }
  if (dir.startsWith('/') || dir.split('/').includes('..')) {
    fail('the fixture path must stay inside the workspace');
  }
  if (hit !== expectedHit) {
    fail(`expected cache-hit ${expectedHit}, got ${hit}`);
  }
  if (matched !== expectedKey) {
    fail('the restored key is not the key that was saved');
  }
  const fixture = process.env.FIXTURE_HELPER || 'compatibility/fixtures/cache-fixture.cjs';
  const result = spawnSync(process.execPath, [fixture, 'verify', '--dir', dir, '--expect', expect], {
    stdio: 'inherit',
  });
  if (result.status !== 0) {
    fail('the restored bytes do not match the recorded digest');
  }
}

function observerSave() {
  const records = readRecords();
  const uploads = records.filter((record) => record.method === 'PUT');
  const whole = uploads.filter((record) => record.comp === null || record.comp === undefined);
  const blocks = uploads.filter((record) => record.comp === 'block');
  const blockLists = uploads.filter((record) => record.comp === 'blocklist');

  if (process.argv.includes('--whole')) {
    const min = countOption('min-bytes') ?? 1;
    const good = whole.filter((record) => succeeded(record) && record.bytes >= min);
    if (good.length === 0) {
      fail(`no successful whole-blob PUT of at least ${min} bytes`);
    }
    console.log(`whole-blob PUTs: ${good.length}, largest ${Math.max(...good.map((r) => r.bytes))} bytes`);
  }

  if (process.argv.includes('--blocks')) {
    const min = countOption('min-block-bytes') ?? 1;
    const goodBlocks = blocks.filter((record) => succeeded(record) && record.bytes > 0);
    const goodLists = blockLists.filter((record) => succeeded(record));
    const total = goodBlocks.reduce((sum, record) => sum + record.bytes, 0);
    if (goodBlocks.length === 0) {
      fail('no successful block PUT');
    }
    if (goodLists.length === 0) {
      fail('no successful block-list PUT');
    }
    if (total < min) {
      fail(`block PUTs carried ${total} bytes, expected at least ${min}`);
    }
    console.log(`block PUTs: ${goodBlocks.length} (${total} bytes), block lists: ${goodLists.length}`);
  }
}

function observerCount() {
  const contains = option('contains', true);
  const statusClass = countOption('status-class');
  const min = countOption('min');
  const max = countOption('max');
  const matched = readRecords().filter((record) => {
    if (!String(record.path || '').includes(contains)) {
      return false;
    }
    if (statusClass === undefined) {
      return true;
    }
    return typeof record.status === 'number' && Math.floor(record.status / 100) === statusClass;
  });
  if (min !== undefined && matched.length < min) {
    fail(`expected at least ${min} matching requests, saw ${matched.length}`);
  }
  if (max !== undefined && matched.length > max) {
    fail(`expected at most ${max} matching requests, saw ${matched.length}`);
  }
  console.log(`matching requests: ${matched.length}`);
}

const command = process.argv[2];
if (command === 'restore') {
  restore();
} else if (command === 'observer-save') {
  observerSave();
} else if (command === 'observer-count') {
  observerCount();
} else {
  fail('usage: assert-case.mjs restore|observer-save|observer-count ...');
}
