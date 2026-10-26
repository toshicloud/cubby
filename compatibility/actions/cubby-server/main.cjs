// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Starts or stops the cubby CLI for one compatibility experiment.
//
// start reads ACTIONS_RESULTS_URL as the runner's Node handler set it, so the
// step must run with NODE_OPTIONS empty; the value is passed to `cubby serve
// --upstream` and never printed. The server child gets RUNNER_TRACKING_ID
// cleared and its own session, so it outlives this action process until a
// cleanup step stops it. This lifecycle is prepared for a live runner proof;
// it is not yet proven against the runner's own orphan cleanup.
//
// cleanup stops only the process recorded in this step's private metadata. The
// start-time and argument checks guard against an accidentally reused pid;
// metadata is written 0600 by this action, and these checks are not a trust
// boundary against another process that can write to RUNNER_TEMP.
'use strict';

const fs = require('node:fs');
const net = require('node:net');
const path = require('node:path');
const { spawn } = require('node:child_process');

// Fixed example scope: one listener belongs to one job. Root's workflow owns
// which job runs here; the scope keeps every experiment on one tenant.
const TENANT = 'compat';
const REPO = 'example/cache';
const WRITE_REF = 'refs/heads/main';

const START_TIMEOUT_MS = 15000;
const STOP_TIMEOUT_MS = 5000;
const POLL_MS = 100;

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

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function parsePort(value) {
  if (!/^[0-9]{1,5}$/.test(value)) {
    throw new Error('port must be a number');
  }
  const port = Number(value);
  if (port < 1 || port > 65535) {
    throw new Error('port must be between 1 and 65535');
  }
  return port;
}

function parseCap(value) {
  if (!/^-?[0-9]+$/.test(value)) {
    throw new Error('cap_bytes must be an integer');
  }
  const cap = Number(value);
  if (!Number.isSafeInteger(cap)) {
    throw new Error('cap_bytes is out of range');
  }
  return cap;
}

function resolvePath(value) {
  const base = process.env.GITHUB_WORKSPACE || process.cwd();
  return path.resolve(base, value);
}

function runnerTemp() {
  const dir = process.env.RUNNER_TEMP;
  if (!dir) {
    throw new Error('RUNNER_TEMP is not set; run this action inside a GitHub Actions job');
  }
  return dir;
}

function metadataPath(port) {
  return path.join(runnerTemp(), `cubby-server-${port}.json`);
}

function logPath(port) {
  return path.join(runnerTemp(), `cubby-server-${port}.log`);
}

function processStartTime(pid) {
  const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8');
  const close = stat.lastIndexOf(')');
  if (close < 0) {
    throw new Error('unreadable process stat');
  }
  const fields = stat.slice(close + 2).split(' ');
  // After "pid (comm) " the first field is state (field 3); starttime is 22.
  return fields[19];
}

function processArgs(pid) {
  const raw = fs.readFileSync(`/proc/${pid}/cmdline`, 'utf8');
  if (raw === '') {
    return [];
  }
  return raw.split('\0').filter((part) => part !== '');
}

// verify returns null when meta names a live cubby serve this action started,
// or a short reason why it does not. It is called before every signal; the
// checks guard against pid reuse, not a hostile metadata writer.
function verify(meta) {
  if (!Number.isInteger(meta.pid) || meta.pid <= 1) {
    return 'metadata has no usable pid';
  }
  if (meta.pid === process.pid) {
    return 'metadata names this process';
  }
  let start;
  let argv;
  try {
    start = processStartTime(meta.pid);
    argv = processArgs(meta.pid);
  } catch {
    return 'process is not running';
  }
  if (typeof meta.starttime !== 'string' || meta.starttime === '') {
    return 'metadata has no process start time';
  }
  if (start !== meta.starttime) {
    return 'pid now belongs to another process';
  }
  if (argv[0] !== meta.binary) {
    return 'command does not match the recorded binary';
  }
  const args = argv.slice(1);
  if (!args.includes('serve')) {
    return 'command is not cubby serve';
  }
  const dir = args.indexOf('--dir');
  if (dir < 0 || args[dir + 1] !== meta.storage) {
    return 'storage directory does not match the metadata';
  }
  const listen = args.indexOf('--listen');
  if (listen < 0 || args[listen + 1] !== `127.0.0.1:${meta.port}`) {
    return 'listener does not match the metadata';
  }
  return null;
}

function isRunning(meta) {
  try {
    return processStartTime(meta.pid) === meta.starttime;
  } catch {
    return false;
  }
}

function removeMetadata(file) {
  try {
    fs.unlinkSync(file);
  } catch (err) {
    if (err.code !== 'ENOENT') {
      throw err;
    }
  }
}

function writePrivateJson(file, value) {
  const tmp = `${file}.tmp-${process.pid}`;
  fs.writeFileSync(tmp, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  fs.chmodSync(tmp, 0o600);
  fs.renameSync(tmp, file);
}

function canConnect(port) {
  return new Promise((resolve) => {
    const socket = net.connect({ host: '127.0.0.1', port });
    const finish = (ok) => {
      socket.destroy();
      resolve(ok);
    };
    socket.setTimeout(250);
    socket.once('connect', () => finish(true));
    socket.once('timeout', () => finish(false));
    socket.once('error', () => finish(false));
  });
}

async function waitForReady(child, port) {
  let exited = null;
  child.once('exit', (code, signal) => {
    exited = { code, signal };
  });
  const deadline = Date.now() + START_TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (exited) {
      const how = exited.signal ? `signal ${exited.signal}` : `code ${exited.code}`;
      throw new Error(`cubby exited during startup (${how}); see its log`);
    }
    if (await canConnect(port)) {
      return;
    }
    await delay(POLL_MS);
  }
  throw new Error('cubby did not accept connections; see its log');
}

async function stopProcess(meta) {
  try {
    process.kill(meta.pid, 'SIGTERM');
  } catch (err) {
    if (err.code === 'ESRCH') {
      return 'already stopped';
    }
    throw err;
  }
  const deadline = Date.now() + STOP_TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (!isRunning(meta)) {
      return 'stopped';
    }
    await delay(POLL_MS);
  }
  // A graceful stop timed out: confirm the pid is still ours before a forced
  // signal, then kill only that process.
  if (verify(meta) !== null) {
    return 'left running: it no longer matches the metadata';
  }
  try {
    process.kill(meta.pid, 'SIGKILL');
  } catch (err) {
    if (err.code !== 'ESRCH') {
      throw err;
    }
  }
  return 'killed after the graceful timeout';
}

// stopOwned stops only the process this start created, identified by its pid
// and /proc start time. Every failure after spawn uses it, so a child is never
// left behind.
async function stopOwned(pid, starttime) {
  try {
    if (processStartTime(pid) !== starttime) {
      return;
    }
    process.kill(pid, 'SIGTERM');
  } catch {
    return;
  }
  const deadline = Date.now() + STOP_TIMEOUT_MS;
  while (Date.now() < deadline) {
    try {
      if (processStartTime(pid) !== starttime) {
        return;
      }
    } catch {
      return;
    }
    await delay(POLL_MS);
  }
  try {
    if (processStartTime(pid) === starttime) {
      process.kill(pid, 'SIGKILL');
    }
  } catch {
    // The process is already gone.
  }
}

async function start() {
  if (process.env.NODE_OPTIONS) {
    throw new Error(
      'NODE_OPTIONS must be empty for the setup step so the original results endpoint is visible'
    );
  }
  if (!getInput('binary')) {
    throw new Error('binary is required in start mode');
  }
  if (!getInput('storage_dir')) {
    throw new Error('storage_dir is required in start mode');
  }
  const binary = resolvePath(getInput('binary'));
  const storage = resolvePath(getInput('storage_dir'));
  const port = parsePort(getInput('port') || '8095');
  const cap = parseCap(getInput('cap_bytes') || '1073741824');

  let stat;
  try {
    stat = fs.statSync(binary);
  } catch {
    throw new Error('binary does not exist; build it before this step');
  }
  if (!stat.isFile()) {
    throw new Error('binary is not a file');
  }
  try {
    fs.accessSync(binary, fs.constants.X_OK);
  } catch {
    throw new Error('binary is not executable');
  }
  if (fs.existsSync(storage) && !fs.statSync(storage).isDirectory()) {
    throw new Error('storage_dir exists and is not a directory');
  }

  // The original results endpoint is the only value taken from the runner's
  // Node handler. It goes to the child's argv and is never printed.
  const rawEndpoint = process.env.ACTIONS_RESULTS_URL || '';
  if (!rawEndpoint) {
    throw new Error('ACTIONS_RESULTS_URL is not set; run this step inside a GitHub Actions job');
  }
  let endpoint;
  try {
    endpoint = new URL(rawEndpoint);
  } catch {
    throw new Error('ACTIONS_RESULTS_URL is not a valid URL');
  }
  if (endpoint.protocol !== 'https:') {
    throw new Error('ACTIONS_RESULTS_URL must be https');
  }
  if (endpoint.username || endpoint.password) {
    throw new Error('ACTIONS_RESULTS_URL must not carry credentials');
  }
  if (endpoint.hash) {
    throw new Error('ACTIONS_RESULTS_URL must not carry a fragment');
  }
  if (!endpoint.hostname) {
    throw new Error('ACTIONS_RESULTS_URL must have a host');
  }

  const url = `http://127.0.0.1:${port}/`;
  const metaFile = metadataPath(port);
  if (fs.existsSync(metaFile)) {
    let meta;
    try {
      meta = JSON.parse(fs.readFileSync(metaFile, 'utf8'));
    } catch {
      throw new Error('existing cubby metadata is unreadable; stop it or remove the file');
    }
    const problem = verify(meta);
    if (problem === null) {
      // A live server from an earlier start may hold a different storage,
      // upstream or cap. Never reuse it: the cleanup step is the only way to
      // replace the endpoint this job talks to.
      throw new Error(
        `a cubby server from an earlier start is still running on port ${port}; run this action with mode: cleanup first`
      );
    }
    console.log(`replacing stale cubby metadata: ${problem}`);
  }

  const serverLog = logPath(port);
  // Never record a child we did not actually get: a foreign listener on the
  // port would otherwise make readiness look successful.
  if (await canConnect(port)) {
    throw new Error(`port ${port} is already in use; stop the process that owns it first`);
  }
  const logFd = fs.openSync(serverLog, 'a', 0o600);
  fs.fchmodSync(logFd, 0o600);
  const args = [
    'serve',
    '--dir', storage,
    '--listen', `127.0.0.1:${port}`,
    '--tenant', TENANT,
    '--repo', REPO,
    '--write-ref', WRITE_REF,
    '--read-ref', WRITE_REF,
    '--cap', String(cap),
    '--upstream', rawEndpoint,
  ];
  let child;
  try {
    child = spawn(binary, args, {
      detached: true,
      stdio: ['ignore', logFd, logFd],
      // Clear the runner's tracking id on this child only, so the runner does
      // not treat the server as an orphan of the finished step, and keep this
      // process's environment out of the server.
      env: {
        PATH: process.env.PATH || '/usr/local/bin:/usr/bin:/bin',
        RUNNER_TRACKING_ID: '',
      },
    });
  } finally {
    fs.closeSync(logFd);
  }
  if (!child.pid) {
    throw new Error('cubby did not start');
  }
  child.unref();
  let childStart;
  try {
    childStart = processStartTime(child.pid);
  } catch {
    throw new Error('cubby did not stay alive after starting');
  }
  try {
    await waitForReady(child, port);
  } catch (err) {
    // Stop only the child we started: it must still carry this start time.
    await stopOwned(child.pid, childStart);
    throw err;
  }
  try {
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error('cubby exited right after starting; see its log');
    }
    let starttime;
    try {
      starttime = processStartTime(child.pid);
    } catch {
      throw new Error('cubby exited right after starting; see its log');
    }
    if (starttime !== childStart) {
      throw new Error('cubby process changed during startup');
    }
    writePrivateJson(metaFile, {
      version: 1,
      pid: child.pid,
      starttime,
      binary,
      storage,
      port,
      url,
      log: serverLog,
      created: new Date().toISOString(),
    });
    console.log(`cubby listening on ${url}`);
    console.log(`cubby log: ${serverLog}`);
    setOutput('url', url);
  } catch (err) {
    // Metadata, logging or the step output failed after spawn: stop the child
    // this start owns and remove any metadata it wrote.
    await stopOwned(child.pid, childStart);
    removeMetadata(metaFile);
    throw err;
  }
}

async function cleanup() {
  const port = parsePort(getInput('port') || '8095');
  const metaFile = metadataPath(port);
  if (!fs.existsSync(metaFile)) {
    console.log('no cubby server metadata; nothing to stop');
    return;
  }
  let meta;
  try {
    meta = JSON.parse(fs.readFileSync(metaFile, 'utf8'));
  } catch {
    throw new Error('cubby metadata is unreadable; refusing to guess a pid');
  }
  const problem = verify(meta);
  if (problem !== null) {
    removeMetadata(metaFile);
    console.log(`cubby is not running: ${problem}; removed stale metadata`);
    return;
  }
  const outcome = await stopProcess(meta);
  removeMetadata(metaFile);
  console.log(`cubby ${outcome}`);
}

async function main() {
  const mode = getInput('mode') || 'start';
  if (mode === 'start') {
    await start();
    return;
  }
  if (mode === 'cleanup') {
    await cleanup();
    return;
  }
  throw new Error(`unknown mode ${mode}; want start or cleanup`);
}

main().catch((err) => {
  console.error(`::error::${err.message}`);
  process.exit(1);
});
