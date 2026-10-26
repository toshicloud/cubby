// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Scope is what one job may read and write, mirroring GitHub's cache
// isolation: a job saves to its own ref and restores from its own ref, then
// (for a pull request) the base branch, then the default branch.
type Scope struct {
	Tenant   string   // tenant ID; every entry belongs to one tenant
	Repo     string   // "owner/name"
	WriteRef string   // ref saves go to, e.g. "refs/heads/feat", "refs/pull/12/merge"
	ReadRefs []string // refs restores search, in order; WriteRef is first
	CapBytes int64    // the tenant's size cap; <= 0 means the cache is off
}

// Options configures a cache server.
type Options struct {
	Dir            string                              // root directory, created if missing
	Resolve        func(r *http.Request) (Scope, bool) // the job behind a request; false -> 403
	Upstream       *url.URL                            // GitHub results service, for every non-cache twirp call; nil -> 404
	UpstreamClient *http.Client                        // nil -> http.DefaultClient
	Log            *slog.Logger                        // nil -> discard
	Now            func() time.Time                    // nil -> time.Now
}
