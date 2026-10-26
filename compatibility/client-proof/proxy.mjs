// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Logging reverse proxy (observer) for the cubby client proof.
//
// Forwards every request unchanged to cubby on 127.0.0.1 and appends one JSON
// line per completed request containing only: method, URL path, the "comp"
// query selector, the request body byte count, and the response status. It
// records no headers, tokens or bodies, and it keeps the client's Host header
// so the URLs cubby generates point back at this proxy.
//
// Reusable as-is by other harnesses; the contract is the environment only:
// PROXY_PORT, CUBBY_PORT and PROXY_LOG. No other configuration and no module
// imports outside Node's standard library.

import fs from 'node:fs';
import http from 'node:http';

const targetPort = Number(process.env.CUBBY_PORT);
const listenPort = Number(process.env.PROXY_PORT);
const logPath = process.env.PROXY_LOG;

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://proxy.invalid');
  let bytes = 0;
  let logged = false;
  req.on('data', (chunk) => {
    bytes += chunk.length;
  });
  const upstream = http.request(
    {
      host: '127.0.0.1',
      port: targetPort,
      method: req.method,
      path: req.url,
      headers: req.headers,
    },
    (up) => {
      res.writeHead(up.statusCode ?? 502, up.headers);
      up.pipe(res);
    }
  );
  upstream.on('error', () => {
    if (!res.headersSent) {
      res.writeHead(502, { 'content-type': 'text/plain' });
    }
    res.end('proxy error');
  });
  const record = () => {
    if (logged) {
      return;
    }
    logged = true;
    fs.appendFileSync(
      logPath,
      JSON.stringify({
        method: req.method,
        path: url.pathname,
        comp: url.searchParams.get('comp'),
        bytes,
        status: res.statusCode,
      }) + '\n'
    );
  };
  res.on('finish', record);
  res.on('close', record);
  req.pipe(upstream);
});

server.listen(listenPort, '127.0.0.1', () => {
  process.stdout.write(`proxy 127.0.0.1:${listenPort} -> cubby 127.0.0.1:${targetPort}\n`);
});
