// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

// Protocol reference: @actions/cache 4.0.5,
// lib/generated/results/api/v1/cache{,.twirp-client}.js; see PROTOCOL.md.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) handleCacheTwirp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "POST required")
		return
	}
	scope, ok := s.requireScope(w, r)
	if !ok {
		return
	}
	method := strings.TrimPrefix(r.URL.Path, cacheServicePrefix)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTwirpBody))
	if err != nil {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "read body")
		return
	}

	switch method {
	case "CreateCacheEntry":
		s.twirpCreateCacheEntry(w, r, scope, body)
	case "FinalizeCacheEntryUpload":
		s.twirpFinalizeCacheEntryUpload(w, r, scope, body)
	case "GetCacheEntryDownloadURL":
		s.twirpGetCacheEntryDownloadURL(w, r, scope, body)
	default:
		writeTwirpError(w, http.StatusNotFound, "bad_route", "unknown method")
	}
}

type createCacheEntryReq struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type createCacheEntryResp struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url,omitempty"`
	Message         string `json:"message,omitempty"`
}

func (s *Server) twirpCreateCacheEntry(w http.ResponseWriter, r *http.Request, scope Scope, body []byte) {
	var req createCacheEntryReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "invalid json")
		return
	}
	resp := createCacheEntryResp{OK: false}
	if scope.CapBytes <= 0 {
		resp.Message = "cache disabled"
		writeTwirpJSON(w, resp)
		return
	}
	if !validCacheKey(req.Key) {
		resp.Message = "invalid key"
		writeTwirpJSON(w, resp)
		return
	}
	ts, err := s.tenantFor(scope)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "store")
		return
	}
	now := s.now()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.purgeStaleUploads(now, s.log.Info)

	if ts.findEntry(scope.Tenant, scope.Repo, scope.WriteRef, req.Key, req.Version) != nil {
		resp.Message = "duplicate entry"
		writeTwirpJSON(w, resp)
		return
	}
	if u := ts.findUpload(scope.Tenant, scope.Repo, scope.WriteRef, req.Key, req.Version); u != nil {
		resp.Message = "upload in progress"
		writeTwirpJSON(w, resp)
		return
	}

	uploadID, err := randomHex(16)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "upload id")
		return
	}
	if err := os.MkdirAll(ts.uploadDir(uploadID), 0o700); err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "upload dir")
		return
	}
	rec := uploadRecord{
		ID:      uploadID,
		Tenant:  scope.Tenant,
		Repo:    scope.Repo,
		Ref:     scope.WriteRef,
		Key:     req.Key,
		Version: req.Version,
		Started: now,
	}
	ts.index.Uploads = append(ts.index.Uploads, rec)
	if err := ts.persistIndex(); err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "index")
		return
	}

	resp.OK = true
	resp.SignedUploadURL = hostURL(r, uploadPrefix+uploadID)
	writeTwirpJSON(w, resp)
}

type finalizeReq struct {
	Key       string          `json:"key"`
	Version   string          `json:"version"`
	SizeBytes json.RawMessage `json:"size_bytes"`
}

type finalizeResp struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entry_id,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) twirpFinalizeCacheEntryUpload(w http.ResponseWriter, r *http.Request, scope Scope, body []byte) {
	var req finalizeReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "invalid json")
		return
	}
	sizeBytes, err := parseJSONInt64(req.SizeBytes)
	if err != nil {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "size_bytes")
		return
	}
	resp := finalizeResp{OK: false}
	if scope.CapBytes <= 0 {
		resp.Message = "cache disabled"
		writeTwirpJSON(w, resp)
		return
	}

	ts, err := s.tenantFor(scope)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "store")
		return
	}
	now := s.now()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	u := ts.findUpload(scope.Tenant, scope.Repo, scope.WriteRef, req.Key, req.Version)
	if u == nil {
		resp.Message = "no upload"
		writeTwirpJSON(w, resp)
		return
	}
	uploadDir := ts.uploadDir(u.ID)
	blobPath := ts.uploadBlobPath(u.ID)
	// A committed block list is a manifest over the staged blocks; a
	// single-shot upload leaves one blob. Either way the size reported by the
	// client is checked against what is actually there.
	var (
		size       int64
		blockEntry bool
		manifest   entryManifest
	)
	if fi, err := os.Stat(ts.uploadManifestPath(u.ID)); err == nil && fi.Mode().IsRegular() {
		m, err := readManifest(ts.uploadManifestPath(u.ID))
		if err != nil {
			writeTwirpError(w, http.StatusInternalServerError, "internal", "manifest")
			return
		}
		if _, err := openSegmentReader(uploadDir, m); err != nil {
			resp.Message = "missing block"
			writeTwirpJSON(w, resp)
			return
		}
		size, blockEntry, manifest = m.Size, true, m
	} else {
		fi, err := os.Stat(blobPath)
		if err != nil || !fi.Mode().IsRegular() {
			resp.Message = "blob missing"
			writeTwirpJSON(w, resp)
			return
		}
		size = fi.Size()
	}
	if size != sizeBytes {
		resp.Message = "size mismatch"
		writeTwirpJSON(w, resp)
		return
	}
	if size > scope.CapBytes {
		_ = os.RemoveAll(uploadDir)
		ts.removeUploadRecord(u.ID)
		_ = ts.persistIndex()
		resp.Message = "over cap"
		writeTwirpJSON(w, resp)
		return
	}

	entryID := ts.index.NextID
	ts.index.NextID++
	ent := cacheEntry{
		ID:       entryID,
		Tenant:   scope.Tenant,
		Repo:     scope.Repo,
		Ref:      scope.WriteRef,
		Key:      req.Key,
		Version:  req.Version,
		Size:     size,
		Created:  now,
		LastUsed: now,
	}
	dest := ts.entryPath(entryID)
	if blockEntry {
		// Moving the upload directory into place publishes the manifest and
		// its blocks in one rename: a reader never sees a manifest without
		// its bytes, and a crash before it leaves an upload that is never
		// served. The blocks are not copied, only their directory entry moves.
		// An entry id no index names can be left on disk by a crash between a
		// finalize and its index write; it is removed here (and at startup)
		// rather than failing the rename, which would wedge every later save.
		_ = os.RemoveAll(dest)
		// Nothing but the manifest and the blocks it names may enter the
		// entry: a blob from a single-shot write that raced the commit, the
		// manifest's temporary file, or a block the list does not name
		// (including a .part file still streaming when the commit ran).
		ts.dropUnlistedBlocks(u.ID, manifest)
		_ = os.Remove(ts.uploadBlobPath(u.ID))
		files, _ := os.ReadDir(uploadDir)
		for _, file := range files {
			if strings.HasPrefix(file.Name(), "blob.") && strings.HasSuffix(file.Name(), ".part") {
				_ = os.Remove(filepath.Join(uploadDir, file.Name()))
			}
		}
		_ = os.Remove(filepath.Join(uploadDir, manifestName+".tmp"))
		if err := os.Rename(uploadDir, dest); err != nil {
			writeTwirpError(w, http.StatusInternalServerError, "internal", "move entry")
			return
		}
		_ = os.Chmod(dest, 0o700)
		if err := syncDir(ts.entriesDir()); err != nil {
			s.log.Warn("cubby: sync entries dir", "err", err)
		}
	} else {
		if err := os.Rename(blobPath, dest); err != nil {
			writeTwirpError(w, http.StatusInternalServerError, "internal", "move entry")
			return
		}
		_ = os.Chmod(dest, 0o600)
		_ = os.RemoveAll(uploadDir)
	}
	ts.removeUploadRecord(u.ID)
	ts.index.Entries = append(ts.index.Entries, ent)

	if err := ts.persistIndex(); err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "index")
		return
	}
	if err := ts.evict(scope.CapBytes, entryID, now, s.log.Info); err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "evict")
		return
	}
	s.setUsage(scope.Tenant, ts.totalEntryBytes())

	resp.OK = true
	resp.EntryID = fmt.Sprintf("%d", entryID)
	writeTwirpJSON(w, resp)
}

type getDownloadReq struct {
	Key         string          `json:"key"`
	Version     string          `json:"version"`
	RestoreKeys []string        `json:"restore_keys"`
	Metadata    json.RawMessage `json:"metadata"`
}

type getDownloadResp struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url,omitempty"`
	MatchedKey        string `json:"matched_key,omitempty"`
}

func (s *Server) twirpGetCacheEntryDownloadURL(w http.ResponseWriter, r *http.Request, scope Scope, body []byte) {
	var req getDownloadReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "invalid json")
		return
	}
	if scope.CapBytes <= 0 {
		writeTwirpJSON(w, getDownloadResp{OK: false})
		return
	}
	ts, err := s.tenantFor(scope)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "store")
		return
	}
	now := s.now()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	ent, matchedKey := findRestoreMatch(ts, scope, req.Key, req.Version, req.RestoreKeys)
	if ent == nil {
		writeTwirpJSON(w, getDownloadResp{OK: false})
		return
	}
	ent.LastUsed = now
	if err := ts.persistIndex(); err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "index")
		return
	}

	resp := getDownloadResp{
		OK:                true,
		SignedDownloadURL: hostURL(r, downloadPrefix+fmt.Sprintf("%d", ent.ID)),
		MatchedKey:        matchedKey,
	}
	writeTwirpJSON(w, resp)
}

// findRestoreMatch searches each readable ref in order. Primary-key prefixes
// precede restore keys, as documented by GitHub's dependency caching reference
// and @actions/cache 4.0.5, lib/cache.js (restoreCacheV2); see PROTOCOL.md.
func findRestoreMatch(ts *tenantStore, scope Scope, key, version string, restoreKeys []string) (*cacheEntry, string) {
	for _, ref := range scope.ReadRefs {
		if e := ts.findEntry(scope.Tenant, scope.Repo, ref, key, version); e != nil {
			return e, key
		}
		if key != "" {
			if e := newestPrefixMatch(ts, scope.Tenant, scope.Repo, ref, key, version); e != nil {
				return e, e.Key
			}
		}
		for _, rk := range restoreKeys {
			if e := ts.findEntry(scope.Tenant, scope.Repo, ref, rk, version); e != nil {
				return e, rk
			}
			if e := newestPrefixMatch(ts, scope.Tenant, scope.Repo, ref, rk, version); e != nil {
				return e, e.Key
			}
		}
	}
	return nil, ""
}

func newestPrefixMatch(ts *tenantStore, tenant, repo, ref, prefix, version string) *cacheEntry {
	var best *cacheEntry
	for i := range ts.index.Entries {
		e := &ts.index.Entries[i]
		if e.Tenant != tenant || e.Repo != repo || e.Ref != ref || e.Version != version {
			continue
		}
		if !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		if best == nil || e.Created.After(best.Created) {
			best = e
		}
	}
	return best
}

func writeTwirpJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
