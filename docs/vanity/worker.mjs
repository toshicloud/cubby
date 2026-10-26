// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Serve Go module discovery for Cubby and redirect browser visits to its source.
const repository = "https://github.com/toshicloud/cubby";
const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="go-import" content="go.toshicloud.io/cubby git ${repository}">
<title>cubby</title></head>
<body><a href="${repository}">cubby source repository</a></body></html>
`;

export default {
  fetch(request) {
    const url = new URL(request.url);
    if (url.pathname !== "/cubby" && !url.pathname.startsWith("/cubby/")) {
      return new Response("not found\n", { status: 404 });
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response(null, { status: 405, headers: { allow: "GET, HEAD" } });
    }
    if (url.searchParams.get("go-get") !== "1") {
      return Response.redirect(repository, 302);
    }
    return new Response(request.method === "HEAD" ? null : page, {
      headers: {
        "content-type": "text/html; charset=utf-8",
        "cache-control": "public, max-age=300",
      },
    });
  },
};
