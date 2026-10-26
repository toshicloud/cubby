// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

const testVersion = "test-version"

type testJob string

var testScopes = map[testJob]Scope{}

func testResolve(r *http.Request) (Scope, bool) {
	job := testJob(r.Header.Get("X-Test-Job"))
	s, ok := testScopes[job]
	return s, ok
}

func newTestServer(t *testing.T, dir string, upstream *httptest.Server) *Server {
	t.Helper()
	var up *url.URL
	if upstream != nil {
		var err error
		up, err = url.Parse(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(Options{
		Dir:      dir,
		Resolve:  testResolve,
		Upstream: up,
		Now:      func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func postTwirp(t *testing.T, h http.Handler, job testJob, method string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, cacheServicePrefix+method, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Job", string(job))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp, _ := io.ReadAll(rec.Body)
	return rec.Code, resp
}

func saveEntry(t *testing.T, h http.Handler, job testJob, key string, payload []byte) {
	t.Helper()
	code, body := postTwirp(t, h, job, "CreateCacheEntry", map[string]string{
		"key":     key,
		"version": testVersion,
	})
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
		t.Fatalf("create not ok: %s", created.Message)
	}
	req := httptest.NewRequest(http.MethodPut, created.SignedUploadURL, bytes.NewReader(payload))
	req.Header.Set("X-Test-Job", string(job))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload %s: %d", key, rec.Code)
	}
	code, body = postTwirp(t, h, job, "FinalizeCacheEntryUpload", map[string]string{
		"key":        key,
		"version":    testVersion,
		"size_bytes": fmt.Sprintf("%d", len(payload)),
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
		t.Fatalf("finalize not ok: %s", fin.Message)
	}
}

func restoreBytes(t *testing.T, h http.Handler, job testJob, key string, restoreKeys []string) ([]byte, bool) {
	t.Helper()
	reqBody := map[string]any{
		"key":     key,
		"version": testVersion,
	}
	if len(restoreKeys) > 0 {
		reqBody["restore_keys"] = restoreKeys
	}
	code, body := postTwirp(t, h, job, "GetCacheEntryDownloadURL", reqBody)
	if code != http.StatusOK {
		t.Fatalf("get url: %d %s", code, body)
	}
	var got struct {
		OK                bool   `json:"ok"`
		SignedDownloadURL string `json:"signed_download_url"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		return nil, false
	}
	req := httptest.NewRequest(http.MethodGet, got.SignedDownloadURL, nil)
	req.Header.Set("X-Test-Job", string(job))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d", rec.Code)
	}
	out, _ := io.ReadAll(rec.Body)
	return out, true
}

func miB(n int) []byte {
	b := make([]byte, n<<20)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func downloadURLForKey(t *testing.T, h http.Handler, job testJob, key string) string {
	t.Helper()
	code, body := postTwirp(t, h, job, "GetCacheEntryDownloadURL", map[string]string{
		"key":     key,
		"version": testVersion,
	})
	if code != http.StatusOK {
		t.Fatal(code)
	}
	var got struct {
		OK                bool   `json:"ok"`
		SignedDownloadURL string `json:"signed_download_url"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatal("miss")
	}
	return got.SignedDownloadURL
}
