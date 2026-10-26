#!/usr/bin/env node
// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0
//
// Negative boundary probe for the cubby container job.
//
// A failed connection from a container proves nothing if the host listener
// died first, so this helper has two explicit modes and one shared contract:
//
//   host  --state <absolute heartbeat path> [--port 18095] [--interval 2]
//         [--lifetime 900]
//     Starts an HTTP sentinel on 127.0.0.1 only. The sentinel serves a
//     per-process nonce on GET /ready (nowhere else) and self-requests that
//     endpoint on an interval. Only a confirmed self-request with the matching
//     nonce updates the heartbeat: an atomic private 0600 JSON file holding
//     nonce, port and lastReady (RFC 3339). The process stops after its
//     bounded lifetime or on SIGTERM/SIGINT, and removes the heartbeat.
//
//   probe --state <absolute heartbeat path> [--port 18095] [--max-age 15]
//         [--connect-timeout 3]
//     Runs in the container against a read-only copy of the host heartbeat.
//     It requires a fresh, structurally valid heartbeat before a bounded
//     connection attempt to 127.0.0.1:<read-only port>. For "refused" or
//     "timeout" it then observes the heartbeat for up to six seconds (the
//     default host tick is two seconds) and accepts only the SAME nonce with a
//     lastReady strictly later than the moment the attempt finished: the host
//     sentinel must be alive after the container's try, not just before it.
//     That is the negative boundary result. A reachable listener, an
//     unchanged/stale/removed heartbeat, or a nonce change is reported as
//     "not-proven", never as isolation.
//
// Both modes print at most one JSON report line to stdout (probe) or short
// lifecycle lines (host). Neither logs environment values, headers, request
// bodies or filesystem paths. Exit codes:
//   0  host stopped normally; probe proved the negative boundary
//   2  usage or configuration error
//   3  probe failed: loopback reached a listener (or an unexpected outcome)
//   4  probe failed: heartbeat missing/stale/invalid/mismatched before the try
//   5  probe failed: no newer same-nonce heartbeat after the try (unchanged,
//      stale, removed, or nonce changed)
//
// Standard library only. No daemon, no service supervisor, no cache
// compatibility claim.

import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import path from 'node:path';

const DEFAULTS = {
  port: 18095,
  intervalMs: 2000,
  lifetimeMs: 15 * 60 * 1000,
  maxAgeMs: 15 * 1000,
  connectTimeoutMs: 3000,
  selfCheckTimeoutMs: 1000,
};

// AFTER_WAIT_MS bounds the observation after a refused or timed-out
// connection: three default two-second host ticks.
const AFTER_WAIT_MS = 6000;

function usage(message) {
  if (message) {
    process.stderr.write(`container-probe: ${message}\n`);
  }
  process.stderr.write(
    'usage:\n' +
      '  container-probe.mjs host  --state <absolute heartbeat path> [--port 18095] [--interval 2] [--lifetime 900]\n' +
      '  container-probe.mjs probe --state <absolute heartbeat path> [--port 18095] [--max-age 15] [--connect-timeout 3]\n'
  );
  process.exit(2);
}

function positive(value, name) {
  const number = Number(value);
  if (!Number.isFinite(number) || number <= 0) {
    usage(`${name} must be a positive number`);
  }
  return number;
}

function parseArgs(argv) {
  const mode = argv[0];
  if (mode !== 'host' && mode !== 'probe') {
    usage(mode ? `unknown mode ${mode}` : 'a mode is required');
  }
  const opts = { mode, ...DEFAULTS, state: '' };
  for (let i = 1; i < argv.length; i += 1) {
    const flag = argv[i];
    const value = () => {
      if (i + 1 >= argv.length) {
        usage(`missing value for ${flag}`);
      }
      i += 1;
      return argv[i];
    };
    switch (flag) {
      case '--state':
        opts.state = value();
        break;
      case '--port':
        opts.port = positive(value(), '--port');
        break;
      case '--interval':
        opts.intervalMs = positive(value(), '--interval') * 1000;
        break;
      case '--lifetime':
        opts.lifetimeMs = positive(value(), '--lifetime') * 1000;
        break;
      case '--max-age':
        opts.maxAgeMs = positive(value(), '--max-age') * 1000;
        break;
      case '--connect-timeout':
        opts.connectTimeoutMs = positive(value(), '--connect-timeout') * 1000;
        break;
      case '-h':
      case '--help':
        usage();
        break;
      default:
        usage(`unknown option ${flag}`);
    }
  }
  if (!opts.state) {
    usage('--state is required');
  }
  if (!path.isAbsolute(opts.state)) {
    usage('--state must be an absolute path');
  }
  if (!Number.isInteger(opts.port) || opts.port < 1 || opts.port > 65535) {
    usage('--port must be an integer between 1 and 65535');
  }
  return opts;
}

function hostReady(port, nonce) {
  return new Promise((resolve) => {
    const req = http.get(
      { host: '127.0.0.1', port, path: '/ready', timeout: DEFAULTS.selfCheckTimeoutMs },
      (res) => {
        let body = '';
        res.setEncoding('utf8');
        res.on('data', (chunk) => {
          body += chunk;
        });
        res.on('end', () => {
          try {
            resolve(res.statusCode === 200 && JSON.parse(body).nonce === nonce);
          } catch {
            resolve(false);
          }
        });
      }
    );
    req.on('timeout', () => req.destroy());
    req.on('error', () => resolve(false));
  });
}

async function runHost(opts) {
  const nonce = crypto.randomBytes(16).toString('hex');
  let checks = 0;
  let failing = false;
  let stopped = false;
  let closing = false;

  fs.mkdirSync(path.dirname(opts.state), { recursive: true, mode: 0o700 });

  const server = http.createServer((req, res) => {
    if (req.method === 'GET' && req.url === '/ready') {
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
      res.end(JSON.stringify({ nonce }));
      return;
    }
    res.writeHead(404, { 'content-type': 'text/plain' });
    res.end('not found');
  });

  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(opts.port, '127.0.0.1', resolve);
  }).catch((error) => {
    process.stderr.write(`container-probe: cannot listen on 127.0.0.1:${opts.port}: ${error.code || 'error'}\n`);
    process.exit(2);
  });
  process.stdout.write(`host: listening on 127.0.0.1:${opts.port}\n`);

  const writeHeartbeat = () => {
    const body = JSON.stringify({ nonce, port: opts.port, lastReady: new Date().toISOString() }) + '\n';
    const tmp = `${opts.state}.tmp-${process.pid}`;
    const fd = fs.openSync(tmp, 'w', 0o600);
    try {
      fs.writeFileSync(fd, body);
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(tmp, opts.state);
  };

  const tick = async () => {
    if (stopped) {
      return;
    }
    let ok = false;
    try {
      const ready = await hostReady(opts.port, nonce);
      if (ready && !stopped) {
        writeHeartbeat();
        ok = true;
      }
    } catch {
      ok = false;
    }
    if (ok) {
      checks += 1;
      if (failing) {
        failing = false;
        process.stdout.write('host: self-check recovered\n');
      }
    } else if (!failing) {
      failing = true;
      process.stdout.write('host: self-check failing; heartbeat not updated\n');
    }
  };

  const finish = () => {
    if (closing) {
      return;
    }
    closing = true;
    try {
      fs.rmSync(opts.state, { force: true });
    } catch {
      // A heartbeat that cannot be removed is reported by its absence only.
    }
    process.stdout.write(`host: stopped after ${checks} confirmed self-checks\n`);
    process.exit(0);
  };

  const interval = setInterval(tick, opts.intervalMs);
  const lifetime = setTimeout(() => stop('lifetime'), opts.lifetimeMs);
  function stop(reason) {
    if (stopped) {
      return;
    }
    stopped = true;
    clearInterval(interval);
    clearTimeout(lifetime);
    server.close(finish);
    if (typeof server.closeAllConnections === 'function') {
      server.closeAllConnections();
    }
    // Never wait unboundedly for a socket that will not close.
    setTimeout(finish, 1000).unref();
    process.stdout.write(`host: stopping (${reason})\n`);
  }
  process.on('SIGTERM', () => stop('SIGTERM'));
  process.on('SIGINT', () => stop('SIGINT'));

  await tick();
}

function readHeartbeat(opts) {
  let text;
  try {
    text = fs.readFileSync(opts.state, 'utf8');
  } catch {
    return { ok: false, reason: 'heartbeat missing' };
  }
  let beat;
  try {
    beat = JSON.parse(text);
  } catch {
    return { ok: false, reason: 'heartbeat is not valid JSON' };
  }
  if (!beat || typeof beat !== 'object' || Array.isArray(beat)) {
    return { ok: false, reason: 'heartbeat is not an object' };
  }
  if (typeof beat.nonce !== 'string' || !/^[0-9a-f]{16,128}$/.test(beat.nonce)) {
    return { ok: false, reason: 'heartbeat nonce is invalid' };
  }
  if (!Number.isInteger(beat.port) || beat.port < 1 || beat.port > 65535) {
    return { ok: false, reason: 'heartbeat port is invalid' };
  }
  if (beat.port !== opts.port) {
    return { ok: false, reason: 'heartbeat port does not match --port' };
  }
  const stamp = Date.parse(beat.lastReady);
  if (!Number.isFinite(stamp)) {
    return { ok: false, reason: 'heartbeat lastReady is invalid' };
  }
  const ageMs = Date.now() - stamp;
  if (ageMs > opts.maxAgeMs || ageMs < -opts.maxAgeMs) {
    return { ok: false, reason: 'heartbeat is stale', ageMs, nonce: beat.nonce, lastReadyMs: stamp };
  }
  return { ok: true, nonce: beat.nonce, ageMs, lastReadyMs: stamp };
}

function attemptConnection(port, timeoutMs) {
  return new Promise((resolve) => {
    const socket = net.connect({ host: '127.0.0.1', port });
    let settled = false;
    const done = (outcome) => {
      if (settled) {
        return;
      }
      settled = true;
      socket.destroy();
      resolve(outcome);
    };
    socket.setTimeout(timeoutMs);
    socket.once('connect', () => done('connected'));
    socket.once('timeout', () => done('timeout'));
    socket.once('error', (error) => {
      if (error.code === 'ECONNREFUSED') {
        done('refused');
        return;
      }
      done(`error:${error.code || 'unknown'}`);
    });
  });
}

function probeReadyNonce(port, timeoutMs) {
  return new Promise((resolve) => {
    const req = http.get({ host: '127.0.0.1', port, path: '/ready', timeout: timeoutMs }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => {
        body += chunk;
      });
      res.on('end', () => {
        try {
          resolve(res.statusCode === 200 ? JSON.parse(body).nonce : null);
        } catch {
          resolve(null);
        }
      });
    });
    req.on('timeout', () => req.destroy());
    req.on('error', () => resolve(null));
  });
}

async function runProbe(opts) {
  const report = {
    mode: 'probe',
    port: opts.port,
    heartbeatBefore: null,
    connection: null,
    heartbeatAfter: null,
    ok: false,
    verdict: 'not-proven',
  };
  const emit = (code) => {
    process.stdout.write(JSON.stringify(report) + '\n');
    process.exit(code);
  };

  const before = readHeartbeat(opts);
  if (!before.ok) {
    report.heartbeatBefore = { fresh: false, reason: before.reason };
    report.reason = `heartbeat before probe: ${before.reason}`;
    emit(4);
  }
  report.heartbeatBefore = { fresh: true, nonce: before.nonce, ageMs: before.ageMs };

  const outcome = await attemptConnection(opts.port, opts.connectTimeoutMs);
  const attemptEndedAt = Date.now();
  report.connection = outcome;
  report.attemptEndedAt = attemptEndedAt;
  if (outcome === 'connected') {
    report.connectionNonceMatches = (await probeReadyNonce(opts.port, opts.connectTimeoutMs)) === before.nonce;
    const beat = readHeartbeat(opts);
    report.heartbeatAfter = beat.ok
      ? { fresh: true, nonce: beat.nonce, ageMs: beat.ageMs, lastReadyMs: beat.lastReadyMs }
      : { fresh: false, reason: beat.reason };
    report.reason = 'container loopback reached a listener on the probe port';
    emit(3);
  }
  if (outcome !== 'refused' && outcome !== 'timeout') {
    report.reason = `unexpected connection outcome: ${outcome}`;
    emit(3);
  }

  // The host must prove it was alive after this attempt: the same nonce and a
  // lastReady strictly later than the attempt's completion. Six seconds covers
  // three default two-second host ticks.
  const deadline = attemptEndedAt + AFTER_WAIT_MS;
  let after = { ok: false, reason: 'heartbeat has not advanced since the connection attempt' };
  for (;;) {
    const beat = readHeartbeat(opts);
    if (!beat.ok) {
      after = beat;
      if (beat.reason === 'heartbeat missing') {
        break;
      }
    } else if (beat.nonce !== before.nonce) {
      after = { ok: false, reason: 'heartbeat nonce changed during the probe' };
      break;
    } else if (beat.lastReadyMs > attemptEndedAt) {
      after = beat;
      break;
    } else {
      after = {
        ok: false,
        reason: 'heartbeat has not advanced since the connection attempt',
        nonce: beat.nonce,
        ageMs: beat.ageMs,
        lastReadyMs: beat.lastReadyMs,
      };
    }
    if (Date.now() >= deadline) {
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }

  report.heartbeatAfter = after.ok
    ? { fresh: true, nonce: after.nonce, ageMs: after.ageMs, lastReadyMs: after.lastReadyMs }
    : {
        fresh: false,
        reason: after.reason,
        ...(after.lastReadyMs === undefined ? {} : { lastReadyMs: after.lastReadyMs }),
      };
  if (!after.ok) {
    report.reason = `heartbeat after probe: ${after.reason}`;
    emit(5);
  }
  report.waitedMs = Date.now() - attemptEndedAt;
  report.ok = true;
  report.verdict = 'isolated';
  emit(0);
}

const options = parseArgs(process.argv.slice(2));
if (options.mode === 'host') {
  await runHost(options);
} else {
  await runProbe(options);
}
