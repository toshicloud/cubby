// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockSpec is one staged block: its Azure block ID and the bytes it holds.
type blockSpec struct {
	id   string
	data []byte
}

// blockFileName is the on-disk name of a staged block.
func blockFileName(id string) string { return hex.EncodeToString([]byte(id)) }

// tenantPath is the directory of one tenant under the cache root.
func tenantPath(root, tenant string) string { return filepath.Join(root, tenantDirName(tenant)) }

func oneJob(t *testing.T, capBytes int64) {
	t.Helper()
	testScopes = map[testJob]Scope{
		"j": {Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"}, CapBytes: capBytes},
	}
}

// createUpload starts an upload and returns its URL and ID.
func createUpload(t *testing.T, h http.Handler, job testJob, key string) (uploadURL, uploadID string) {
	t.Helper()
	code, body := postTwirp(t, h, job, "CreateCacheEntry", map[string]string{"key": key, "version": testVersion})
	if code != http.StatusOK {
		t.Fatalf("create %s: %d %s", key, code, body)
	}
	var created struct {
		OK              bool   `json:"ok"`
		SignedUploadURL string `json:"signed_upload_url"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if !created.OK {
		t.Fatalf("create %s: %s", key, created.Message)
	}
	parsed, err := url.Parse(created.SignedUploadURL)
	if err != nil {
		t.Fatal(err)
	}
	return created.SignedUploadURL, strings.TrimPrefix(parsed.Path, uploadPrefix)
}

func stageBlock(t *testing.T, h http.Handler, job testJob, uploadURL, id string, data []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, uploadURL+"?comp=block&blockid="+id, bytes.NewReader(data))
	req.Header.Set("X-Test-Job", string(job))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("stage block %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

func commitBlockList(t *testing.T, h http.Handler, job testJob, uploadURL string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	var list strings.Builder
	list.WriteString("<BlockList>")
	for _, id := range ids {
		list.WriteString("<Latest>" + id + "</Latest>")
	}
	list.WriteString("</BlockList>")
	req := httptest.NewRequest(http.MethodPut, uploadURL+"?comp=blocklist", strings.NewReader(list.String()))
	req.Header.Set("X-Test-Job", string(job))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func finalizeEntry(t *testing.T, h http.Handler, job testJob, key string, size int64) {
	t.Helper()
	code, body := postTwirp(t, h, job, "FinalizeCacheEntryUpload", map[string]string{
		"key": key, "version": testVersion, "size_bytes": fmt.Sprintf("%d", size),
	})
	if code != http.StatusOK {
		t.Fatalf("finalize %s: %d %s", key, code, body)
	}
	var fin struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &fin); err != nil {
		t.Fatal(err)
	}
	if !fin.OK {
		t.Fatalf("finalize %s: %s", key, fin.Message)
	}
}

// uploadBlocks stages blocks in the order given, commits the list in listIDs
// order and finalizes the entry at the total size.
func uploadBlocks(t *testing.T, h http.Handler, job testJob, key string, staged []blockSpec, listIDs ...string) {
	t.Helper()
	uploadURL, _ := createUpload(t, h, job, key)
	size := int64(0)
	byID := map[string][]byte{}
	for _, b := range staged {
		byID[b.id] = b.data
		stageBlock(t, h, job, uploadURL, b.id, b.data)
	}
	for _, id := range listIDs {
		data, ok := byID[id]
		if !ok {
			t.Fatalf("block %s is not staged", id)
		}
		size += int64(len(data))
	}
	if rec := commitBlockList(t, h, job, uploadURL, listIDs...); rec.Code != http.StatusCreated {
		t.Fatalf("commit %s: %d %s", key, rec.Code, rec.Body.String())
	}
	finalizeEntry(t, h, job, key, size)
}

// getWithRange requests a download URL with an optional Range header.
func getWithRange(t *testing.T, h http.Handler, job testJob, url, method, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	req.Header.Set("X-Test-Job", string(job))
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCommitWritesOnlyTheManifest pins the point of the change: a block list
// commits a manifest and leaves the staged blocks where they are. No blob is
// built, so the commit copies no bytes.
func TestCommitWritesOnlyTheManifest(t *testing.T) {
	root := t.TempDir()
	oneJob(t, 10<<20)
	h := newTestServer(t, root, nil)

	uploadURL, uploadID := createUpload(t, h, "j", "only-manifest")
	stageBlock(t, h, "j", uploadURL, "YQ==", []byte("aaaa"))
	stageBlock(t, h, "j", uploadURL, "Yg==", []byte("bbbb"))
	if rec := commitBlockList(t, h, "j", uploadURL, "YQ==", "Yg=="); rec.Code != http.StatusCreated {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body.String())
	}

	dir := filepath.Join(tenantPath(root, "t"), "uploads", uploadID)
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(names))
	for _, e := range names {
		got = append(got, e.Name())
	}
	if want := []string{"blocks", manifestName}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("upload dir holds %v, want %v", got, want)
	}
	fi, err := os.Stat(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 512 {
		t.Fatalf("manifest is %d bytes, want only the list", fi.Size())
	}
	for id, data := range map[string]string{"YQ==": "aaaa", "Yg==": "bbbb"} {
		block, err := os.ReadFile(filepath.Join(dir, blocksDirName, blockFileName(id)))
		if err != nil {
			t.Fatal(err)
		}
		if string(block) != data {
			t.Fatalf("block %s = %q, want %q", id, block, data)
		}
	}
}

// TestBlockListDecidesOrderAndDuplicates pins that the list's order decides the
// entry's bytes (not the upload order) and that a repeated ID adds its bytes
// again.
func TestBlockListDecidesOrderAndDuplicates(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)

	uploadBlocks(t, h, "j", "ordered",
		[]blockSpec{{id: "Yw==", data: []byte("cccc")}, {id: "YQ==", data: []byte("aaaa")}, {id: "Yg==", data: []byte("bbbb")}},
		"Yg==", "YQ==", "Yg==")

	got, ok := restoreBytes(t, h, "j", "ordered", nil)
	if !ok {
		t.Fatal("entry missing")
	}
	if want := "bbbbaaaabbbb"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRestagedBlockIsTheCommittedBlock pins that a block staged again before
// the commit replaces the earlier bytes, at their new size.
func TestRestagedBlockIsTheCommittedBlock(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)

	uploadBlocks(t, h, "j", "restaged",
		[]blockSpec{{id: "YQ==", data: []byte("one")}, {id: "YQ==", data: []byte("two!")}},
		"YQ==")

	got, ok := restoreBytes(t, h, "j", "restaged", nil)
	if !ok {
		t.Fatal("entry missing")
	}
	if string(got) != "two!" {
		t.Fatalf("got %q, want the restaged block %q", got, "two!")
	}
}

// TestBlockAfterCommitIsRefused pins that a committed block list is final: a
// later Put Block gets 409 and the downloaded bytes do not change.
func TestBlockAfterCommitIsRefused(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)

	uploadURL, _ := createUpload(t, h, "j", "final")
	stageBlock(t, h, "j", uploadURL, "YQ==", []byte("original"))
	if rec := commitBlockList(t, h, "j", uploadURL, "YQ=="); rec.Code != http.StatusCreated {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPut, uploadURL+"?comp=block&blockid=YQ==", strings.NewReader("replaced"))
	req.Header.Set("X-Test-Job", "j")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("block after commit: %d, want 409", rec.Code)
	}
	finalizeEntry(t, h, "j", "final", int64(len("original")))

	got, ok := restoreBytes(t, h, "j", "final", nil)
	if !ok {
		t.Fatal("entry missing")
	}
	if string(got) != "original" {
		t.Fatalf("got %q, want the committed bytes", got)
	}
}

// TestManifestEntryMatchesSingleFile pins that a manifest entry and a
// single-file entry answer the same requests identically, including a range
// that crosses a segment boundary, an open-ended range at a boundary, a
// zero-length range at the end, a suffix range, and HEAD.
func TestManifestEntryMatchesSingleFile(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)

	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	saveEntry(t, h, "j", "single", payload)
	uploadBlocks(t, h, "j", "manifest", []blockSpec{
		{id: "YQ==", data: payload[:64]},
		{id: "Yg==", data: payload[64:128]},
		{id: "Yw==", data: payload[128:]},
	}, "YQ==", "Yg==", "Yw==")

	single := downloadURLForKey(t, h, "j", "single")
	manifest := downloadURLForKey(t, h, "j", "manifest")

	for _, tc := range []struct {
		name   string
		method string
		rng    string
	}{
		{"full", http.MethodGet, ""},
		{"first byte", http.MethodGet, "bytes=0-0"},
		{"across a segment boundary", http.MethodGet, "bytes=63-64"},
		{"from a boundary to the end", http.MethodGet, "bytes=128-"},
		{"zero-length range at the end", http.MethodGet, "bytes=256-"},
		{"suffix", http.MethodGet, "bytes=-16"},
		{"two ranges", http.MethodGet, "bytes=0-1,200-201"},
		{"whole body as a range", http.MethodGet, "bytes=0-"},
		{"unsatisfiable", http.MethodGet, "bytes=999-1000"},
		{"head", http.MethodHead, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := getWithRange(t, h, "j", single, tc.method, tc.rng)
			got := getWithRange(t, h, "j", manifest, tc.method, tc.rng)
			if got.Code != want.Code {
				t.Fatalf("status %d, want %d", got.Code, want.Code)
			}
			// A multi-range answer is multipart with a random boundary, so its
			// parts are compared by count rather than by bytes.
			multiRange := strings.HasPrefix(want.Header().Get("Content-Type"), "multipart/byteranges")
			if multiRange {
				if !strings.HasPrefix(got.Header().Get("Content-Type"), "multipart/byteranges") {
					t.Fatalf("content type %q", got.Header().Get("Content-Type"))
				}
				if a, b := strings.Count(want.Body.String(), "Content-Range:"), strings.Count(got.Body.String(), "Content-Range:"); a != b {
					t.Fatalf("%d parts, want %d", b, a)
				}
				return
			}
			if !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
				t.Fatalf("body %d bytes, want %d", got.Body.Len(), want.Body.Len())
			}
			for _, header := range []string{"Content-Range", "Content-Length", "Accept-Ranges"} {
				if got.Header().Get(header) != want.Header().Get(header) {
					t.Fatalf("%s = %q, want %q", header, got.Header().Get(header), want.Header().Get(header))
				}
			}
		})
	}
}

// TestOrphanEntryDirectoryIsCleared pins that an entry directory left by a
// crash between a finalize's rename and its index write neither serves nor
// blocks the id it occupies.
func TestOrphanEntryDirectoryIsCleared(t *testing.T) {
	root := t.TempDir()
	oneJob(t, 1<<20)
	orphan := filepath.Join(tenantPath(root, "t"), "entries", "1", blocksDirName)
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, blockFileName("YQ==")), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newTestServer(t, root, nil)
	uploadBlocks(t, h, "j", "after-orphan", []blockSpec{{id: "YQ==", data: []byte("fresh")}}, "YQ==")
	got, ok := restoreBytes(t, h, "j", "after-orphan", nil)
	if !ok || string(got) != "fresh" {
		t.Fatalf("got %q, ok=%v", got, ok)
	}
}

// TestConditionalRequestMatchesSingleFile pins that a conditional request is
// answered the same way as for a single-file entry, so the full-body path did
// not take it.
func TestConditionalRequestMatchesSingleFile(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)
	payload := []byte("conditional")
	saveEntry(t, h, "j", "single", payload)
	uploadBlocks(t, h, "j", "manifest", []blockSpec{{id: "YQ==", data: payload}}, "YQ==")

	since := time.Unix(1_700_000_000, 0).UTC().Format(http.TimeFormat)
	for _, url := range []string{downloadURLForKey(t, h, "j", "single"), downloadURLForKey(t, h, "j", "manifest")} {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("X-Test-Job", "j")
		req.Header.Set("If-Modified-Since", since)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("304 carried %d body bytes", rec.Body.Len())
		}
	}
}

// TestCommitDropsUnlistedBlocks pins that a commit keeps only the blocks its
// list names, so no uncounted bytes ride into the entry, and that the entry
// directory holds nothing else.
func TestCommitDropsUnlistedBlocks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		list       []string
		wantBlocks []string
	}{
		{"unlisted block", []string{"YQ==", "Yg=="}, []string{blockFileName("YQ=="), blockFileName("Yg==")}},
		{"duplicate block", []string{"YQ==", "YQ=="}, []string{blockFileName("YQ==")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			oneJob(t, 10<<20)
			h := newTestServer(t, root, nil)
			uploadBlocks(t, h, "j", "trimmed", []blockSpec{
				{id: "YQ==", data: []byte("aaaa")},
				{id: "Yg==", data: []byte("bbbb")},
				{id: "Yw==", data: bytes.Repeat([]byte("c"), 1<<20)},
			}, tc.list...)

			entryDir := filepath.Join(tenantPath(root, "t"), "entries", "1")
			names, err := os.ReadDir(entryDir)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, n := range names {
				got = append(got, n.Name())
			}
			if want := []string{blocksDirName, manifestName}; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("entry holds %v, want %v", got, want)
			}
			blocks, err := os.ReadDir(filepath.Join(entryDir, blocksDirName))
			if err != nil {
				t.Fatal(err)
			}
			var blockNames []string
			var onDisk int64
			for _, b := range blocks {
				blockNames = append(blockNames, b.Name())
				fi, err := b.Info()
				if err != nil {
					t.Fatal(err)
				}
				onDisk += fi.Size()
			}
			if strings.Join(blockNames, ",") != strings.Join(tc.wantBlocks, ",") {
				t.Fatalf("entry blocks %v, want %v", blockNames, tc.wantBlocks)
			}
			m, err := readManifest(filepath.Join(entryDir, manifestName))
			if err != nil {
				t.Fatal(err)
			}
			if onDisk > m.Size {
				t.Fatalf("%d block bytes on disk, entry size %d", onDisk, m.Size)
			}
		})
	}
}

// TestBlockStreamingAcrossCommitIsRefused pins that a Put Block which began
// before a commit cannot publish its bytes afterwards: the commit and the
// block's rename exclude each other.
func TestBlockStreamingAcrossCommitIsRefused(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)
	uploadURL, _ := createUpload(t, h, "j", "raced")
	stageBlock(t, h, "j", uploadURL, "YQ==", []byte("original"))

	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPut, uploadURL+"?comp=block&blockid=YQ==", pr)
	req.Header.Set("X-Test-Job", "j")
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	// This write returns once the handler has read it, so the block is in
	// flight when the commit below runs.
	if _, err := pw.Write([]byte("replaced")); err != nil {
		t.Fatal(err)
	}
	if rec := commitBlockList(t, h, "j", uploadURL, "YQ=="); rec.Code != http.StatusCreated {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body.String())
	}
	_, _ = pw.Write([]byte(" and more"))
	_ = pw.Close()
	if code := <-done; code != http.StatusConflict {
		t.Fatalf("block published after the commit: %d, want 409", code)
	}
	finalizeEntry(t, h, "j", "raced", int64(len("original")))

	got, ok := restoreBytes(t, h, "j", "raced", nil)
	if !ok {
		t.Fatal("entry missing")
	}
	if string(got) != "original" {
		t.Fatalf("got %q, want the committed bytes", got)
	}
}

// TestPublishBlockRefusesACommittedUpload pins the publish half of the
// exclusion directly: once the manifest exists, a streamed block file is
// removed rather than renamed over a committed block.
func TestPublishBlockRefusesACommittedUpload(t *testing.T) {
	oneJob(t, 1<<20)
	srv := newTestServer(t, t.TempDir(), nil)
	uploadURL, uploadID := createUpload(t, srv, "j", "late")
	stageBlock(t, srv, "j", uploadURL, "YQ==", []byte("original"))
	if rec := commitBlockList(t, srv, "j", uploadURL, "YQ=="); rec.Code != http.StatusCreated {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body.String())
	}

	ts, err := srv.tenantFor(testScopes["j"])
	if err != nil {
		t.Fatal(err)
	}
	blockPath := ts.uploadBlockPath(uploadID, "YQ==")
	if err := os.WriteFile(blockPath+".part", []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ts.publishBlock(uploadID, blockPath, blockPath+".part"); !errors.Is(err, errUploadCommitted) {
		t.Fatalf("publishBlock = %v, want errUploadCommitted", err)
	}
	if _, err := os.Stat(blockPath + ".part"); !os.IsNotExist(err) {
		t.Fatalf("the refused block file was kept: %v", err)
	}
	body, err := os.ReadFile(blockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original" {
		t.Fatalf("block is %q, want the committed bytes", body)
	}
}

// TestRangeOverSegmentsReturnsUploadedBytes pins the bytes of a range that
// spans three segments.
func TestRangeOverSegmentsReturnsUploadedBytes(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)

	payload := make([]byte, 60)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	uploadBlocks(t, h, "j", "spans", []blockSpec{
		{id: "YQ==", data: payload[:10]},
		{id: "Yg==", data: payload[10:30]},
		{id: "Yw==", data: payload[30:]},
	}, "YQ==", "Yg==", "Yw==")
	url := downloadURLForKey(t, h, "j", "spans")

	rec := getWithRange(t, h, "j", url, http.MethodGet, "bytes=5-34")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[5:35]) {
		t.Fatalf("range body mismatch: %v", rec.Body.Bytes())
	}
}

// TestLegacyEntryFromAnOldIndexIsServed pins that an entry written as one file,
// with an index persisted before this layout, still serves.
func TestLegacyEntryFromAnOldIndexIsServed(t *testing.T) {
	root := t.TempDir()
	oneJob(t, 1<<20)
	payload := []byte("written by the older code")
	dir := tenantPath(root, "t")
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entries", "1"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`{"next_id":2,"entries":[{"id":1,"tenant":"t","repo":"o/r","ref":"refs/heads/main","key":"legacy","version":%q,"size":%d,"created":"2026-01-01T00:00:00Z","last_used":"2026-01-01T00:00:00Z"}],"uploads":[]}`, testVersion, len(payload))
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newTestServer(t, root, nil)
	got, ok := restoreBytes(t, h, "j", "legacy", nil)
	if !ok {
		t.Fatal("legacy entry not served")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
}

// TestMissingOrShortBlockIsAMiss pins that an entry whose block file is gone,
// or is not the size the manifest records, is dropped and never served again.
func TestMissingOrShortBlockIsAMiss(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, blockFile string)
	}{
		{"missing", func(t *testing.T, blockFile string) {
			if err := os.Remove(blockFile); err != nil {
				t.Fatal(err)
			}
		}},
		{"short", func(t *testing.T, blockFile string) {
			if err := os.WriteFile(blockFile, []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			oneJob(t, 1<<20)
			h := newTestServer(t, root, nil)
			uploadBlocks(t, h, "j", "fragile", []blockSpec{
				{id: "YQ==", data: []byte("aaaa")},
				{id: "Yg==", data: []byte("bbbb")},
			}, "YQ==", "Yg==")
			url := downloadURLForKey(t, h, "j", "fragile")

			entryDir := filepath.Join(tenantPath(root, "t"), "entries", "1")
			tc.break_(t, filepath.Join(entryDir, blocksDirName, blockFileName("YQ==")))

			rec := getWithRange(t, h, "j", url, http.MethodGet, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("download of a %s block: %d, want 404", tc.name, rec.Code)
			}
			if _, err := os.Stat(entryDir); !os.IsNotExist(err) {
				t.Fatalf("entry files still on disk: %v", err)
			}
			if _, ok := restoreBytes(t, h, "j", "fragile", nil); ok {
				t.Fatal("the dropped entry is still restorable")
			}
		})
	}
}

// TestCapCountsBlocksAndPurgeDeletesThem pins that staged blocks count against
// the cap, that abandoned uploads and their blocks are purged, and that an
// evicted entry takes its block files with it.
func TestCapCountsBlocksAndPurgeDeletesThem(t *testing.T) {
	root := t.TempDir()
	oneJob(t, 5<<20)
	now := time.Unix(1_700_000_000, 0)
	srv, err := New(Options{Dir: root, Resolve: testResolve, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stagedBytes := func() int64 {
		t.Helper()
		ts, err := srv.tenantFor(testScopes["j"])
		if err != nil {
			t.Fatal(err)
		}
		ts.mu.Lock()
		defer ts.mu.Unlock()
		n, err := ts.uploadsBytes()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// An upload abandoned with two megabytes of staged blocks: they count.
	uploadURL, abandonedID := createUpload(t, srv, "j", "abandoned")
	stageBlock(t, srv, "j", uploadURL, "YQ==", miB(2))
	if got := stagedBytes(); got != 2<<20 {
		t.Fatalf("staged blocks count %d bytes, want %d", got, 2<<20)
	}
	abandonedDir := filepath.Join(tenantPath(root, "t"), "uploads", abandonedID)

	// Once stale, the next create purges the upload and its block files.
	now = now.Add(uploadMaxAge + time.Minute)
	uploadBlocks(t, srv, "j", "first", []blockSpec{{id: "YQ==", data: miB(2)}}, "YQ==")
	if _, err := os.Stat(abandonedDir); !os.IsNotExist(err) {
		t.Fatalf("abandoned upload still on disk after purge: %v", err)
	}
	if got := stagedBytes(); got != 0 {
		t.Fatalf("staged bytes after purge and finalize = %d, want 0", got)
	}

	// Over the cap, the least recently used entry goes with its blocks.
	testScopes["j"] = Scope{
		Tenant: "t", Repo: "o/r",
		WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
		CapBytes: 3 << 20,
	}
	uploadBlocks(t, srv, "j", "second", []blockSpec{{id: "YQ==", data: miB(2)}}, "YQ==")
	if got := srv.Usage("t"); got > 3<<20 {
		t.Fatalf("usage %d > cap %d", got, 3<<20)
	}
	entries, err := os.ReadDir(filepath.Join(tenantPath(root, "t"), "entries"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries on disk after eviction, want 1", len(entries))
	}
	if _, ok := restoreBytes(t, srv, "j", "first", nil); ok {
		t.Fatal("the evicted entry is still restorable")
	}
	if _, ok := restoreBytes(t, srv, "j", "second", nil); !ok {
		t.Fatal("the kept entry is missing")
	}
}
