// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// newestEntryPath returns the on-disk path of a tenant's highest-numbered
// entry, which is the entry finalized last on a fresh server.
func newestEntryPath(t *testing.T, root, tenant string) string {
	t.Helper()
	dir := filepath.Join(tenantPath(root, tenant), "entries")
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	best := int64(-1)
	path := ""
	for _, name := range names {
		id, err := strconv.ParseInt(name.Name(), 10, 64)
		if err != nil {
			continue
		}
		if id > best {
			best, path = id, filepath.Join(dir, name.Name())
		}
	}
	if best < 0 {
		t.Fatal("no entries on disk")
	}
	return path
}

func removeEntryPath(t *testing.T, entryPath string) {
	t.Helper()
	if err := os.Remove(entryPath); err != nil {
		t.Fatalf("remove entry %s: %v", entryPath, err)
	}
}

// TestRestoreSkipsStaleEntries pins that a lookup whose first match cannot
// serve its bytes prunes that entry and keeps matching in the existing exact,
// primary-prefix and restore-key order, instead of handing back a URL that is
// bound to fail.
func TestRestoreSkipsStaleEntries(t *testing.T) {
	const blockID = "YQ==" // base64 "a"
	for _, tc := range []struct {
		name      string
		staleKey  string
		lookupKey string
		blocks    bool
		damage    func(t *testing.T, entryPath string)
	}{
		{
			name: "missing exact single-file entry", staleKey: "broken", lookupKey: "broken",
			damage: removeEntryPath,
		},
		{
			name: "missing primary-prefix entry", staleKey: "primary-full", lookupKey: "primary",
			damage: removeEntryPath,
		},
		{
			name: "missing manifest", staleKey: "broken", lookupKey: "broken", blocks: true,
			damage: func(t *testing.T, entryPath string) {
				t.Helper()
				if err := os.Remove(filepath.Join(entryPath, manifestName)); err != nil {
					t.Fatalf("remove manifest: %v", err)
				}
			},
		},
		{
			name: "malformed manifest", staleKey: "broken", lookupKey: "broken", blocks: true,
			damage: func(t *testing.T, entryPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(entryPath, manifestName), []byte("{"), 0o600); err != nil {
					t.Fatalf("write manifest: %v", err)
				}
			},
		},
		{
			name: "missing block", staleKey: "broken", lookupKey: "broken", blocks: true,
			damage: func(t *testing.T, entryPath string) {
				t.Helper()
				if err := os.Remove(filepath.Join(entryPath, blocksDirName, blockFileName(blockID))); err != nil {
					t.Fatalf("remove block: %v", err)
				}
			},
		},
		{
			name: "shortened block", staleKey: "broken", lookupKey: "broken", blocks: true,
			damage: func(t *testing.T, entryPath string) {
				t.Helper()
				blockPath := filepath.Join(entryPath, blocksDirName, blockFileName(blockID))
				if err := os.Truncate(blockPath, 1); err != nil {
					t.Fatalf("shorten block: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			oneJob(t, 1<<20)
			h := newTestServer(t, root, nil)
			fallback := []byte("fallback-bytes")
			preferred := []byte("preferred-bytes")
			saveEntry(t, h, "j", "fallback", fallback)
			if tc.blocks {
				uploadBlocks(t, h, "j", tc.staleKey, []blockSpec{{id: blockID, data: preferred}}, blockID)
			} else {
				saveEntry(t, h, "j", tc.staleKey, preferred)
			}
			tc.damage(t, newestEntryPath(t, root, "t"))

			got, ok := restoreBytes(t, h, "j", tc.lookupKey, []string{"fallback"})
			if !ok || !bytes.Equal(got, fallback) {
				t.Fatalf("restore %q = %q, hit %v; want the fallback bytes", tc.lookupKey, got, ok)
			}
			if usage := h.Usage("t"); usage != int64(len(fallback)) {
				t.Fatalf("usage = %d; want %d, the stale entry excluded", usage, len(fallback))
			}
			code, body := postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": tc.staleKey, "version": testVersion})
			var created createCacheEntryResp
			if err := json.Unmarshal(body, &created); err != nil {
				t.Fatal(err)
			}
			if code != http.StatusOK || !created.OK {
				t.Fatalf("create %q after the stale entry was pruned = %d %s; want OK", tc.staleKey, code, body)
			}
		})
	}
}

// TestStaleEntryWithoutFallbackIsAPersistedMiss pins that pruning a stale
// entry during a lookup with no fallback leaves a miss that survives a
// restart, so the key can be saved again.
func TestStaleEntryWithoutFallbackIsAPersistedMiss(t *testing.T) {
	root := t.TempDir()
	oneJob(t, 1<<20)
	h := newTestServer(t, root, nil)
	saveEntry(t, h, "j", "only", []byte("bytes"))
	removeEntryPath(t, newestEntryPath(t, root, "t"))

	if got, ok := restoreBytes(t, h, "j", "only", nil); ok {
		t.Fatalf("restore of a stale entry = %q, hit true; want a miss", got)
	}

	h2 := newTestServer(t, root, nil)
	code, body := postTwirp(t, h2, "j", "CreateCacheEntry", map[string]string{"key": "only", "version": testVersion})
	var created createCacheEntryResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || !created.OK {
		t.Fatalf("create after reopen = %d %s; want OK, so the removal was persisted", code, body)
	}
}

// TestMissingSingleFileDownloadPrunesIndex pins that a direct GET of a
// single-file entry whose bytes are gone answers 404 and drops the entry from
// the index, so the key can be saved again.
func TestMissingSingleFileDownloadPrunesIndex(t *testing.T) {
	root := t.TempDir()
	oneJob(t, 1<<20)
	h := newTestServer(t, root, nil)
	saveEntry(t, h, "j", "gone", []byte("bytes"))
	downloadURL := downloadURLForKey(t, h, "j", "gone")
	removeEntryPath(t, newestEntryPath(t, root, "t"))

	r := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	r.Header.Set("X-Test-Job", "j")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("download of a removed entry = %d; want 404", w.Code)
	}

	code, body := postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "gone", "version": testVersion})
	var created createCacheEntryResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || !created.OK {
		t.Fatalf("create after a pruned download = %d %s; want OK", code, body)
	}
}
