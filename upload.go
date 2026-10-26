// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

// Upload protocol reference: @actions/cache 4.0.5, lib/internal/uploadUtils.js,
// and its @azure/storage-blob 12.13.0 dependency; see PROTOCOL.md.

import (
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Validate the server-issued ID format before using it in filesystem paths.
var uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
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
	uploadID := strings.TrimPrefix(r.URL.Path, uploadPrefix)
	if !uploadIDPattern.MatchString(uploadID) {
		http.NotFound(w, r)
		return
	}

	ts, err := s.tenantFor(scope)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "store")
		return
	}

	ts.mu.Lock()
	u := ts.findUploadByID(uploadID)
	if u == nil {
		ts.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if u.Tenant != scope.Tenant || u.Repo != scope.Repo || u.Ref != scope.WriteRef {
		ts.mu.Unlock()
		writeTwirpError(w, http.StatusForbidden, "permission_denied", "forbidden")
		return
	}
	// A committed block list already names the bytes of the upload, so no
	// later write may change them: not a restaged block, not a second list.
	if _, err := os.Stat(ts.uploadManifestPath(uploadID)); err == nil {
		ts.mu.Unlock()
		writeTwirpError(w, http.StatusConflict, "conflict", "upload already committed")
		return
	}
	// In-progress uploads share the tenant's cap, so a tenant holds at most
	// twice its cap on disk: its entries plus its uploads. Concurrent blocks
	// may overshoot by what is in flight.
	used, err := ts.uploadsBytes()
	ts.mu.Unlock()
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "cap check")
		return
	}
	remaining := scope.CapBytes - used

	comp := r.URL.Query().Get("comp")
	now := s.now()

	switch comp {
	case "":
		if _, err := streamToFileLimit(ts.uploadBlobPath(uploadID), r.Body, remaining); err != nil {
			s.uploadFailed(w, ts, uploadID, err)
			return
		}
		writeAzureCreated(w, now)
	case "block":
		blockID := r.URL.Query().Get("blockid")
		if blockID == "" || len(blockID) > maxBlockIDLen {
			http.Error(w, "bad blockid", http.StatusBadRequest)
			return
		}
		blockPath := ts.uploadBlockPath(uploadID, blockID)
		part, err := streamToPart(blockPath, r.Body, remaining)
		if err != nil {
			s.uploadFailed(w, ts, uploadID, err)
			return
		}
		switch err := ts.publishBlock(uploadID, blockPath, part); {
		case errors.Is(err, errUploadCommitted):
			writeTwirpError(w, http.StatusConflict, "conflict", "upload already committed")
			return
		case errors.Is(err, errUploadGone):
			http.NotFound(w, r)
			return
		case err != nil:
			writeTwirpError(w, http.StatusInternalServerError, "internal", "write block")
			return
		}
		writeAzureCreated(w, now)
	case "blocklist":
		var list blockList
		if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, maxBlockListBody)).Decode(&list); err != nil {
			http.Error(w, "bad block list", http.StatusBadRequest)
			return
		}
		ids := list.blockIDs()
		if len(ids) == 0 {
			http.Error(w, "empty block list", http.StatusBadRequest)
			return
		}
		// The commit holds the upload lock from the size checks to the
		// publish: a block that is streaming meanwhile can only publish
		// before the manifest exists or be refused after it.
		ts.mu.Lock()
		if _, err := os.Stat(ts.uploadManifestPath(uploadID)); err == nil {
			ts.mu.Unlock()
			writeTwirpError(w, http.StatusConflict, "conflict", "upload already committed")
			return
		}
		// The commit publishes an ordered manifest over the staged block
		// files; the bytes stay where they are. Offsets come from the sizes
		// recorded here, so the list order decides the entry's bytes and a
		// repeated ID adds its bytes again.
		segments := make([]manifestSegment, 0, len(ids))
		var total int64
		for _, id := range ids {
			fi, err := os.Stat(ts.uploadBlockPath(uploadID, id))
			if err != nil || !fi.Mode().IsRegular() {
				ts.mu.Unlock()
				http.Error(w, "missing block", http.StatusBadRequest)
				return
			}
			segments = append(segments, manifestSegment{
				Block: hex.EncodeToString([]byte(id)),
				Size:  fi.Size(),
			})
			total += fi.Size()
		}
		manifest := entryManifest{Version: manifestVersion, Size: total, Segments: segments}
		if err := writeManifest(ts.uploadDir(uploadID), manifest); err != nil {
			ts.mu.Unlock()
			writeTwirpError(w, http.StatusInternalServerError, "internal", "commit manifest")
			return
		}
		// Azure discards the blocks a list does not name on commit, and so
		// does this store: an unlisted block would otherwise sit inside the
		// entry, uncounted by the cap and never served.
		ts.dropUnlistedBlocks(uploadID, manifest)
		ts.mu.Unlock()
		writeAzureCreated(w, now)
	default:
		http.Error(w, "unknown comp", http.StatusBadRequest)
	}
}

// uploadFailed answers a failed blob or block write. Over the cap, the whole
// upload is dropped and the client gets 413, which it does not retry.
func (s *Server) uploadFailed(w http.ResponseWriter, ts *tenantStore, uploadID string, err error) {
	if errors.Is(err, errOverCap) {
		ts.mu.Lock()
		_ = os.RemoveAll(ts.uploadDir(uploadID))
		ts.removeUploadRecord(uploadID)
		_ = ts.persistIndex()
		ts.mu.Unlock()
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	writeTwirpError(w, http.StatusInternalServerError, "internal", "write upload")
}

var errOverCap = fmt.Errorf("cubby: over cap")

var (
	errUploadCommitted = errors.New("cubby: upload already committed")
	errUploadGone      = errors.New("cubby: upload gone")
)

// publishBlock moves a block that has finished streaming into place. The upload
// lock is held so this excludes a commit: the manifest check and the rename are
// one step, so a block whose upload crossed a commit is refused instead of
// written, and a committed list's bytes cannot change.
func (t *tenantStore) publishBlock(uploadID, blockPath, part string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.findUploadByID(uploadID) == nil {
		_ = os.Remove(part)
		return errUploadGone
	}
	if _, err := os.Stat(t.uploadManifestPath(uploadID)); err == nil {
		_ = os.Remove(part)
		return errUploadCommitted
	}
	if err := os.Rename(part, blockPath); err != nil {
		_ = os.Remove(part)
		return err
	}
	return nil
}

type blockList struct {
	XMLName     xml.Name `xml:"BlockList"`
	Latest      []string `xml:"Latest"`
	Committed   []string `xml:"Committed"`
	Uncommitted []string `xml:"Uncommitted"`
}

func (b blockList) blockIDs() []string {
	if len(b.Latest) > 0 {
		return b.Latest
	}
	if len(b.Committed) > 0 {
		return b.Committed
	}
	return b.Uncommitted
}
