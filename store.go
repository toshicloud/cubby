// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

type tenantStore struct {
	dir   string // Dir/<tenantDir>
	name  string // tenantDir (hex)
	mu    sync.Mutex
	index tenantIndex
	// serving counts the downloads reading each entry. An entry being served
	// is not evicted: its block files must stay until the request is done.
	serving map[int64]int
}

func (t *tenantStore) indexPath() string {
	return filepath.Join(t.dir, "index.json")
}

func (t *tenantStore) entriesDir() string {
	return filepath.Join(t.dir, "entries")
}

func (t *tenantStore) uploadsDir() string {
	return filepath.Join(t.dir, "uploads")
}

func (t *tenantStore) entryPath(id int64) string {
	return filepath.Join(t.entriesDir(), fmt.Sprintf("%d", id))
}

// validateEntry checks the storage behind a candidate before lookup returns its
// URL. Missing or invalid bytes are misses; other filesystem errors are left
// intact so a temporary failure cannot cause a valid entry to be deleted.
func (t *tenantStore) validateEntry(ent *cacheEntry) error {
	path := t.entryPath(ent.ID)
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		if !fi.Mode().IsRegular() || fi.Size() != ent.Size {
			return fmt.Errorf("%w: invalid blob size or type", errEntryMiss)
		}
		return nil
	}
	m, err := readManifest(filepath.Join(path, manifestName))
	if err != nil {
		return err
	}
	if m.Size != ent.Size {
		return fmt.Errorf("%w: manifest and index sizes differ", errEntryMiss)
	}
	reader, err := openSegmentReader(path, m)
	if err != nil {
		return err
	}
	return reader.Close()
}

// dropEntryLocked removes a stale index record and its unpinned files. The
// caller holds ts.mu; an active download's release removes the remaining files.
func (s *Server) dropEntryLocked(ts *tenantStore, id int64, reason string) error {
	ent := ts.findEntryByID(id)
	if ent == nil {
		return nil
	}
	// removeEntry shifts the slice containing ent.
	tenant := ent.Tenant
	ts.removeEntry(id)
	err := ts.persistIndex()
	if ts.serving[id] == 0 {
		_ = os.RemoveAll(ts.entryPath(id))
	}
	s.setUsage(tenant, ts.totalEntryBytes())
	s.log.Info("dropped cache entry", "entry", id, "reason", reason)
	return err
}

func (t *tenantStore) uploadDir(uploadID string) string {
	return filepath.Join(t.uploadsDir(), uploadID)
}

func (t *tenantStore) uploadBlobPath(uploadID string) string {
	return filepath.Join(t.uploadDir(uploadID), "blob")
}

func (t *tenantStore) uploadBlockPath(uploadID, blockID string) string {
	// Block IDs are base64 and may hold '/', so they are hex-encoded.
	return filepath.Join(t.uploadDir(uploadID), blocksDirName, hex.EncodeToString([]byte(blockID)))
}

// uploadManifestPath is the manifest a Put Block List publishes; a file there
// means the upload's blocks are committed and may not change.
func (t *tenantStore) uploadManifestPath(uploadID string) string {
	return filepath.Join(t.uploadDir(uploadID), manifestName)
}

// uploadsBytes sums the bytes held by every upload in progress; the caller
// holds t.mu.
func (t *tenantStore) uploadsBytes() (int64, error) {
	var total int64
	for _, u := range t.index.Uploads {
		n, err := t.uploadBytes(u.ID)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (t *tenantStore) totalEntryBytes() int64 {
	var n int64
	for _, e := range t.index.Entries {
		n += e.Size
	}
	return n
}

func (t *tenantStore) persistIndex() error {
	return saveIndex(t.indexPath(), t.index)
}

func (t *tenantStore) findEntry(tenant, repo, ref, key, version string) *cacheEntry {
	for i := range t.index.Entries {
		e := &t.index.Entries[i]
		if e.Tenant == tenant && e.Repo == repo && e.Ref == ref && e.Key == key && e.Version == version {
			return e
		}
	}
	return nil
}

func (t *tenantStore) findUpload(tenant, repo, ref, key, version string) *uploadRecord {
	for i := range t.index.Uploads {
		u := &t.index.Uploads[i]
		if u.Tenant == tenant && u.Repo == repo && u.Ref == ref && u.Key == key && u.Version == version {
			return u
		}
	}
	return nil
}

func (t *tenantStore) findUploadByID(id string) *uploadRecord {
	for i := range t.index.Uploads {
		if t.index.Uploads[i].ID == id {
			return &t.index.Uploads[i]
		}
	}
	return nil
}

func (t *tenantStore) findEntryByID(id int64) *cacheEntry {
	for i := range t.index.Entries {
		if t.index.Entries[i].ID == id {
			return &t.index.Entries[i]
		}
	}
	return nil
}

func (t *tenantStore) removeUploadRecord(id string) {
	t.index.Uploads = slices.DeleteFunc(t.index.Uploads, func(u uploadRecord) bool {
		return u.ID == id
	})
}

func (t *tenantStore) removeEntry(id int64) {
	t.index.Entries = slices.DeleteFunc(t.index.Entries, func(e cacheEntry) bool {
		return e.ID == id
	})
}

func (t *tenantStore) purgeStaleUploads(now time.Time, log func(string, ...any)) {
	var kept []uploadRecord
	for _, u := range t.index.Uploads {
		if now.Sub(u.Started) >= uploadMaxAge {
			_ = os.RemoveAll(t.uploadDir(u.ID))
			removeUploadRecord := u
			log("purged stale upload", "upload", removeUploadRecord.ID, "repo", removeUploadRecord.Repo, "key", removeUploadRecord.Key)
			continue
		}
		kept = append(kept, u)
	}
	t.index.Uploads = kept
	// A block written while its upload was dropped can recreate the
	// upload's directory; remove directories no record names once stale.
	dirs, _ := os.ReadDir(t.uploadsDir())
	for _, d := range dirs {
		if t.findUploadByID(d.Name()) != nil {
			continue
		}
		if fi, err := d.Info(); err == nil && now.Sub(fi.ModTime()) >= uploadMaxAge {
			_ = os.RemoveAll(filepath.Join(t.uploadsDir(), d.Name()))
		}
	}
}

func (t *tenantStore) uploadBytes(uploadID string) (int64, error) {
	dir := t.uploadDir(uploadID)
	var total int64
	blob := t.uploadBlobPath(uploadID)
	if fi, err := os.Stat(blob); err == nil {
		total += fi.Size()
	}
	blocksDir := filepath.Join(dir, blocksDirName)
	entries, err := os.ReadDir(blocksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return total, nil
		}
		return 0, fmt.Errorf("cubby: list blocks: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			// A block can be renamed into place or removed between the
			// listing and the stat; that entry then holds no bytes here.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("cubby: block stat: %w", err)
		}
		total += fi.Size()
	}
	return total, nil
}

func (t *tenantStore) evict(capBytes int64, keepID int64, now time.Time, log func(string, ...any)) error {
	for t.totalEntryBytes() > capBytes {
		var victim *cacheEntry
		for i := range t.index.Entries {
			e := &t.index.Entries[i]
			if e.ID == keepID {
				continue
			}
			if t.serving[e.ID] > 0 {
				// A download is reading this entry; its block files must
				// outlive the request. Another pass evicts it.
				continue
			}
			if victim == nil {
				victim = e
				continue
			}
			if e.LastUsed.Before(victim.LastUsed) {
				victim = e
				continue
			}
			if e.LastUsed.After(victim.LastUsed) {
				continue
			}
			if e.Created.Before(victim.Created) {
				victim = e
				continue
			}
			if e.Created.After(victim.Created) {
				continue
			}
			if e.ID < victim.ID {
				victim = e
			}
		}
		if victim == nil {
			break
		}
		log("evicted cache entry", "tenant_dir", t.name, "repo", victim.Repo, "key", victim.Key, "size", victim.Size)
		// An entry is a file (single-shot upload) or a directory of block
		// files and a manifest (block list), so the whole path goes.
		_ = os.RemoveAll(t.entryPath(victim.ID))
		t.removeEntry(victim.ID)
		if err := t.persistIndex(); err != nil {
			return err
		}
	}
	return nil
}

// release ends one download and removes stale files after the last reader.
func (t *tenantStore) release(id int64) {
	t.mu.Lock()
	if t.serving[id] <= 1 {
		delete(t.serving, id)
		if t.findEntryByID(id) == nil {
			_ = os.RemoveAll(t.entryPath(id))
		}
	} else {
		t.serving[id]--
	}
	t.mu.Unlock()
}

func (t *tenantStore) ensureDirs() error {
	for _, sub := range []string{t.entriesDir(), t.uploadsDir()} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			return fmt.Errorf("cubby: mkdir %s: %w", sub, err)
		}
	}
	return nil
}

// dropUnlistedBlocks deletes every file in an upload's blocks directory that a
// manifest does not name, including a .part file whose upload is still
// streaming. After a commit those bytes are unreachable, and left in place they
// would sit inside the entry uncounted by the cap.
func (t *tenantStore) dropUnlistedBlocks(uploadID string, m entryManifest) {
	keep := make(map[string]struct{}, len(m.Segments))
	for _, seg := range m.Segments {
		keep[seg.Block] = struct{}{}
	}
	dir := filepath.Join(t.uploadDir(uploadID), blocksDirName)
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, d := range names {
		if d.IsDir() {
			continue
		}
		if _, ok := keep[d.Name()]; ok {
			continue
		}
		_ = os.Remove(filepath.Join(dir, d.Name()))
	}
}

// sweepOrphanEntries removes an entry file or directory that no index record
// names. A crash between a finalize's rename and its index write leaves one;
// it is never served, and its id can be handed out again, so it must not stay.
func (t *tenantStore) sweepOrphanEntries() {
	names, err := os.ReadDir(t.entriesDir())
	if err != nil {
		return
	}
	for _, d := range names {
		id, err := strconv.ParseInt(d.Name(), 10, 64)
		if err != nil || t.findEntryByID(id) != nil {
			continue
		}
		_ = os.RemoveAll(filepath.Join(t.entriesDir(), d.Name()))
	}
}

func loadTenantStore(root, tenantHex string) (*tenantStore, error) {
	dir := filepath.Join(root, tenantHex)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cubby: mkdir tenant: %w", err)
	}
	idx, err := loadIndex(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	ts := &tenantStore{dir: dir, name: tenantHex, index: idx, serving: make(map[int64]int)}
	if err := ts.ensureDirs(); err != nil {
		return nil, err
	}
	ts.sweepOrphanEntries()
	return ts, nil
}
