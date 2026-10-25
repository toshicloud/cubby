// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStorageReadFailurePreservesEntry(t *testing.T) {
	oneJob(t, 1<<20)
	root := t.TempDir()
	h := newTestServer(t, root, nil)
	uploadBlocks(t, h, "j", "cached", []blockSpec{{id: "YQ==", data: []byte("data")}}, "YQ==")
	url := downloadURLForKey(t, h, "j", "cached")
	manifest := filepath.Join(tenantPath(root, "t"), "entries", "1", manifestName)
	if err := os.Chmod(manifest, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifest, 0o600) })
	if _, err := os.ReadFile(manifest); !os.IsPermission(err) {
		t.Skip("filesystem does not deny reads after chmod")
	}
	code, body := postTwirp(t, h, "j", "GetCacheEntryDownloadURL", map[string]string{
		"key": "cached", "version": testVersion,
	})
	if code != http.StatusInternalServerError {
		t.Fatalf("lookup with unreadable storage = %d %s; want 500", code, body)
	}
	if rec := getWithRange(t, h, "j", url, http.MethodGet, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("download with unreadable storage = %d; want 500", rec.Code)
	}
	if err := os.Chmod(manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := restoreBytes(t, h, "j", "cached", nil); !ok || string(got) != "data" {
		t.Fatalf("restore after read access returns = %q, hit %v", got, ok)
	}
}

type pausedDownloadWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (w *pausedDownloadWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.resume
	})
	return w.ResponseRecorder.Write(p)
}

func TestStaleEntryCleanupWaitsForDownload(t *testing.T) {
	oneJob(t, 1<<20)
	root := t.TempDir()
	h := newTestServer(t, root, nil)
	uploadBlocks(t, h, "j", "cached", []blockSpec{
		{id: "YQ==", data: []byte("first")},
		{id: "Yg==", data: []byte("second")},
	}, "YQ==", "Yg==")
	url := downloadURLForKey(t, h, "j", "cached")
	w := &pausedDownloadWriter{
		ResponseRecorder: httptest.NewRecorder(),
		started:          make(chan struct{}),
		resume:           make(chan struct{}),
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Test-Job", "j")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req)
	}()
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(w.resume) }); <-done }
	defer resume()
	select {
	case <-w.started:
	case <-done:
		t.Fatal("download finished before writing any bytes")
	}
	entryDir := filepath.Join(tenantPath(root, "t"), "entries", "1")
	if err := os.Remove(filepath.Join(entryDir, blocksDirName, blockFileName("Yg=="))); err != nil {
		t.Fatal(err)
	}
	if _, ok := restoreBytes(t, h, "j", "cached", nil); ok {
		t.Fatal("lookup returned the incomplete entry")
	}
	if _, err := os.Stat(filepath.Join(entryDir, blocksDirName, blockFileName("YQ=="))); err != nil {
		t.Fatalf("lookup removed an active download's files: %v", err)
	}
	resume()
	if _, err := os.Stat(entryDir); !os.IsNotExist(err) {
		t.Fatalf("stale files remain after the last download: %v", err)
	}
}
