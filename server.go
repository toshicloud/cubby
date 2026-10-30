// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Server is a host-local GitHub Actions-compatible cache HTTP handler.
type Server struct {
	dir      string
	resolve  func(r *http.Request) (Scope, bool)
	upstream *url.URL
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	tenants map[string]*tenantStore // keyed by tenantDir hex
	usage   map[string]int64        // tenant ID -> entry bytes

	proxy *httputil.ReverseProxy
}

// New loads every tenant index from Dir and returns a cache server.
func New(opts Options) (*Server, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("cubby: empty Dir")
	}
	if opts.Resolve == nil {
		return nil, fmt.Errorf("cubby: nil Resolve")
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("cubby: mkdir root: %w", err)
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.UpstreamClient
	if client == nil {
		client = http.DefaultClient
	}

	s := &Server{
		dir:      opts.Dir,
		resolve:  opts.Resolve,
		upstream: opts.Upstream,
		client:   client,
		log:      log,
		now:      now,
		tenants:  make(map[string]*tenantStore),
		usage:    make(map[string]int64),
	}
	if opts.Upstream != nil {
		up := opts.Upstream
		s.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(up)
				pr.Out.Host = up.Host
			},
			Transport: client.Transport,
			ModifyResponse: func(resp *http.Response) error {
				s.log.Debug("upstream twirp", "method", resp.Request.Method, "path", resp.Request.URL.Path, "status", resp.StatusCode)
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				s.log.Debug("upstream twirp error", "method", r.Method, "path", r.URL.Path, "err", err)
				writeTwirpError(w, http.StatusInternalServerError, "internal", "upstream error")
			},
		}
	}

	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("cubby: read root: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) != 32 {
			continue
		}
		ts, err := loadTenantStore(opts.Dir, name)
		if err != nil {
			return nil, err
		}
		s.tenants[name] = ts
		for _, ent := range ts.index.Entries {
			s.usage[ent.Tenant] += ent.Size
		}
	}
	return s, nil
}

// Usage reports the bytes stored for a tenant (entries only).
func (s *Server) Usage(tenant string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage[tenant]
}

// RemoveTenant drops a tenant's in-memory store and removes its directory
// under the root. It holds the server lock, so a request of the
// tenant waits for it, and a missing directory is a success.
func (s *Server) RemoveTenant(tenant string) error {
	name := tenantDirName(tenant)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tenants, name)
	delete(s.usage, tenant)
	if err := os.RemoveAll(filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("cubby: remove tenant: %w", err)
	}
	return nil
}

func (s *Server) tenantFor(scope Scope) (*tenantStore, error) {
	name := tenantDirName(scope.Tenant)
	s.mu.Lock()
	ts, ok := s.tenants[name]
	if !ok {
		var err error
		ts, err = loadTenantStore(s.dir, name)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.tenants[name] = ts
	}
	s.mu.Unlock()
	return ts, nil
}

func (s *Server) setUsage(tenant string, total int64) {
	s.mu.Lock()
	s.usage[tenant] = total
	s.mu.Unlock()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, cacheServicePrefix):
		s.handleCacheTwirp(w, r)
	case strings.HasPrefix(path, "/twirp/"):
		s.handleForwardTwirp(w, r)
	case strings.HasPrefix(path, uploadPrefix):
		s.handleUpload(w, r)
	case strings.HasPrefix(path, downloadPrefix):
		s.handleDownload(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) requireScope(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	scope, ok := s.resolve(r)
	if !ok {
		writeTwirpError(w, http.StatusForbidden, "permission_denied", "forbidden")
		return Scope{}, false
	}
	return scope, true
}

func (s *Server) handleForwardTwirp(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.requireScope(w, r)
	if !ok {
		return
	}
	_ = scope
	if s.proxy == nil || s.upstream == nil {
		http.NotFound(w, r)
		return
	}
	s.log.Debug("forward twirp", "method", r.Method, "path", r.URL.Path)
	s.proxy.ServeHTTP(w, r)
}

// EvictUnused removes, in every tenant, the entries no save or restore has
// used for maxAge, and returns the bytes they held. An entry a download is
// reading stays for a later call. The caller decides how often to call it.
func (s *Server) EvictUnused(maxAge time.Duration) (int64, error) {
	s.mu.Lock()
	tenants := make([]*tenantStore, 0, len(s.tenants))
	for _, ts := range s.tenants {
		tenants = append(tenants, ts)
	}
	s.mu.Unlock()

	now := s.now()
	var freed int64
	var errs []error
	for _, ts := range tenants {
		n, err := s.evictUnusedTenant(ts, now.Add(-maxAge))
		freed += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return freed, errors.Join(errs...)
}

// evictUnusedTenant removes ts's entries last used at or before cutoff.
func (s *Server) evictUnusedTenant(ts *tenantStore, cutoff time.Time) (int64, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var freed int64
	tenant, removed := "", false
	for _, e := range slices.Clone(ts.index.Entries) {
		if e.LastUsed.After(cutoff) || ts.serving[e.ID] > 0 {
			continue
		}
		s.log.Info("evicted unused cache entry", "tenant_dir", ts.name, "repo", e.Repo, "key", e.Key, "size", e.Size, "last_used", e.LastUsed)
		_ = os.RemoveAll(ts.entryPath(e.ID))
		ts.removeEntry(e.ID)
		freed += e.Size
		tenant, removed = e.Tenant, true
	}
	if !removed {
		return 0, nil
	}
	s.setUsage(tenant, ts.totalEntryBytes())
	if err := ts.persistIndex(); err != nil {
		return freed, fmt.Errorf("cubby: evict unused: %w", err)
	}
	return freed, nil
}
