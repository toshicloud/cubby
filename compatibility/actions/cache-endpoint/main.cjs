// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Classifies the results endpoint visible to this Node action without
// printing it. A workflow step cannot read ACTIONS_RESULTS_URL, which the
// runner's Node handler sets only for Node actions, so the NODE_OPTIONS
// override experiment needs an action to observe it.
'use strict';

const fs = require('node:fs');

const LOOPBACK_HOSTS = new Set(['127.0.0.1', '::1', '[::1]', 'localhost']);
const CLASSES = new Set(['cubby-loopback', 'original-https', 'other', 'invalid', 'missing']);

function getInput(name) {
  const key = 'INPUT_' + name.toUpperCase().replace(/ /g, '_');
  return (process.env[key] || '').trim();
}

function setOutput(name, value) {
  const file = process.env.GITHUB_OUTPUT;
  if (!file) {
    return;
  }
  fs.appendFileSync(file, `${name}=${value}\n`);
}

// classify returns one of CLASSES and never includes the URL or any part of
// it in its result.
function classify(raw, expectedPort) {
  if (!raw) {
    return 'missing';
  }
  let url;
  try {
    url = new URL(raw);
  } catch {
    return 'invalid';
  }
  if (url.username || url.password || url.hash || !url.hostname) {
    return 'invalid';
  }
  const host = url.hostname.toLowerCase();
  const port = url.port || (url.protocol === 'https:' ? '443' : url.protocol === 'http:' ? '80' : '');
  if (url.protocol === 'https:') {
    return LOOPBACK_HOSTS.has(host) ? 'other' : 'original-https';
  }
  if (url.protocol === 'http:' && LOOPBACK_HOSTS.has(host) && port === expectedPort) {
    return 'cubby-loopback';
  }
  return 'other';
}

function main() {
  const expectedPort = getInput('port') || '8095';
  if (!/^[0-9]{1,5}$/.test(expectedPort)) {
    throw new Error('port must be a number');
  }
  const endpoint = classify(process.env.ACTIONS_RESULTS_URL || '', expectedPort);
  if (!CLASSES.has(endpoint)) {
    throw new Error('unexpected classification');
  }
  console.log(`results endpoint: ${endpoint}`);
  setOutput('endpoint', endpoint);
}

try {
  main();
} catch (err) {
  console.error(`::error::${err.message}`);
  process.exit(1);
}
