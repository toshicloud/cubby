// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

// Download protocol reference: @actions/cache 4.0.5,
// lib/internal/downloadUtils.js; see PROTOCOL.md.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.requireScope(w, r)
	if !ok {
		return
	}
	if scope.CapBytes <= 0 {
		writeTwirpError(w, http.StatusForbidden, "permission_denied", "cache disabled")
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, downloadPrefix)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}

	ts, err := s.tenantFor(scope)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "store")
		return
	}

	ts.mu.Lock()
	ent := ts.findEntryByID(id)
	if ent == nil {
		ts.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if ent.Tenant != scope.Tenant || ent.Repo != scope.Repo {
		ts.mu.Unlock()
		writeTwirpError(w, http.StatusForbidden, "permission_denied", "forbidden")
		return
	}
	if !slices.Contains(scope.ReadRefs, ent.Ref) {
		ts.mu.Unlock()
		writeTwirpError(w, http.StatusForbidden, "permission_denied", "forbidden")
		return
	}
	path := ts.entryPath(id)
	// While this request reads the entry, an eviction must leave its files
	// alone: a block file is opened only when its bytes are first needed.
	ts.serving[id]++
	ts.mu.Unlock()
	defer ts.release(id)

	if fi, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		writeTwirpError(w, http.StatusInternalServerError, "internal", "open")
		return
	} else if fi.IsDir() {
		s.serveManifestEntry(w, r, ts, id, path)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		writeTwirpError(w, http.StatusInternalServerError, "internal", "open")
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, idStr, s.now(), f)
}

// serveManifestEntry serves an entry built from a block list: one stream over
// its block files, handed to http.ServeContent so Range, If-Range and the
// conditional headers keep their behaviour. An entry whose blocks are missing
// or no longer the size the manifest records is dropped, never served.
func (s *Server) serveManifestEntry(w http.ResponseWriter, r *http.Request, ts *tenantStore, id int64, dir string) {
	m, err := readManifest(filepath.Join(dir, manifestName))
	if err != nil {
		s.dropEntry(ts, id, "unreadable manifest")
		http.NotFound(w, r)
		return
	}
	reader, err := openSegmentReader(dir, m)
	if err != nil {
		if errors.Is(err, errEntryMiss) {
			s.dropEntry(ts, id, "missing block")
			http.NotFound(w, r)
			return
		}
		writeTwirpError(w, http.StatusInternalServerError, "internal", "open")
		return
	}
	defer func() { _ = reader.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	// A plain GET reads block files directly, which keeps the kernel's
	// sendfile path that the reader type would lose: one code path for the
	// whole body, one for a single byte range. Everything else (several
	// ranges, suffix ranges, conditional headers, HEAD) goes to ServeContent
	// unchanged.
	if r.Method == http.MethodGet && noConditionalHeader(r) {
		if r.Header.Get("Range") == "" {
			s.serveFullBody(w, r, id, m, dir)
			return
		}
		if start, length, ok := simpleRange(r.Header.Get("Range"), m.Size); ok {
			s.serveRange(w, r, id, m, dir, start, length)
			return
		}
	}
	http.ServeContent(w, r, strconv.FormatInt(id, 10), s.now(), io.NewSectionReader(reader, 0, m.Size))
}

// noConditionalHeader reports whether the request carries none of the headers
// that change an answer beyond Range.
func noConditionalHeader(r *http.Request) bool {
	for _, header := range []string{"If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if r.Header.Get(header) != "" {
			return false
		}
	}
	return true
}

// serveFullBody writes a whole entry as a 200, one io.Copy per block file.
func (s *Server) serveFullBody(w http.ResponseWriter, _ *http.Request, id int64, m entryManifest, dir string) {
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", s.now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.WriteHeader(http.StatusOK)
	s.copySegments(w, id, m, dir, 0, m.Size)
}

// serveRange writes one byte range as a 206, one io.Copy per block file, so a
// client's parallel range restores keep the kernel's sendfile path.
func (s *Server) serveRange(w http.ResponseWriter, _ *http.Request, id int64, m entryManifest, dir string, start, length int64) {
	end := start + length - 1
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", s.now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, m.Size))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	s.copySegments(w, id, m, dir, start, length)
}

// copySegments writes length bytes of the entry from start, opening one block
// file at a time. The sizes were checked when the entry was opened, so a block
// that changed under us can only shorten the body, never fake it.
func (s *Server) copySegments(w http.ResponseWriter, id int64, m entryManifest, dir string, start, length int64) {
	var segStart int64
	pos, remaining := start, length
	for _, seg := range m.Segments {
		segEnd := segStart + seg.Size - 1
		if remaining <= 0 {
			return
		}
		if pos > segEnd {
			segStart += seg.Size
			continue
		}
		off := pos - segStart
		n := min(remaining, seg.Size-off)
		f, err := os.Open(filepath.Join(dir, blocksDirName, seg.Block))
		if err != nil {
			s.log.Warn("cubby: open block while serving", "entry", id, "block", seg.Block, "err", err)
			return
		}
		if off > 0 {
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				_ = f.Close()
				s.log.Warn("cubby: seek block while serving", "entry", id, "block", seg.Block, "err", err)
				return
			}
		}
		_, err = io.Copy(w, io.LimitReader(f, n))
		_ = f.Close()
		if err != nil {
			return
		}
		pos += n
		remaining -= n
		segStart += seg.Size
	}
}

// simpleRange returns the start and length of one explicit byte range
// (`bytes=start-end` or `bytes=start-`). Anything else, including a suffix
// range, several ranges, an unsatisfiable one or bad syntax, returns false and
// stays with http.ServeContent so its answer is unchanged.
func simpleRange(value string, size int64) (start, length int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(value, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found || strings.TrimSpace(first) == "" {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	last = strings.TrimSpace(last)
	if last == "" {
		return start, size - start, true
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true
}

// dropEntry removes an entry whose bytes cannot be served from the index, so it
// is never served again, and deletes its files unless another download is
// still reading them.
func (s *Server) dropEntry(ts *tenantStore, id int64, reason string) {
	ts.mu.Lock()
	ent := ts.findEntryByID(id)
	tenant := ""
	if ent != nil {
		// findEntryByID points into the slice that removeEntry shifts.
		tenant = ent.Tenant
		ts.removeEntry(id)
		_ = ts.persistIndex()
	}
	total := ts.totalEntryBytes()
	path := ts.entryPath(id)
	unused := ts.serving[id] <= 1
	ts.mu.Unlock()

	if unused {
		_ = os.RemoveAll(path)
	}
	if tenant != "" {
		s.setUsage(tenant, total)
	}
	s.log.Info("dropped cache entry", "entry", id, "reason", reason)
}
