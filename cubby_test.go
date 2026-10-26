// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEvictsLeastRecentlyUsedOverCap(t *testing.T) {
	dir := t.TempDir()
	const capBytes = 3 << 20
	testScopes = map[testJob]Scope{
		"main": {
			Tenant: "tenant-a", Repo: "o/r",
			WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
			CapBytes: capBytes,
		},
	}
	var tick int64
	srv, err := New(Options{
		Dir:     dir,
		Resolve: testResolve,
		Now:     func() time.Time { tick++; return time.Unix(1_700_000_000+tick, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := srv

	saveEntry(t, h, "main", "k1", miB(1))
	saveEntry(t, h, "main", "k2", miB(1))
	if _, ok := restoreBytes(t, h, "main", "k1", nil); !ok {
		t.Fatal("k1 missing")
	}
	saveEntry(t, h, "main", "k3", miB(1))
	saveEntry(t, h, "main", "k4", miB(1))

	if _, ok := restoreBytes(t, h, "main", "k2", nil); ok {
		t.Fatal("k2 should be evicted")
	}
	for _, key := range []string{"k1", "k3", "k4"} {
		if _, ok := restoreBytes(t, h, "main", key, nil); !ok {
			t.Fatalf("%s missing", key)
		}
	}
	if srv.Usage("tenant-a") > capBytes {
		t.Fatalf("usage %d > cap", srv.Usage("tenant-a"))
	}
}

func TestScopeIsolation(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"default": {
			Tenant: "t1", Repo: "o/r",
			WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
			CapBytes: 10 << 20,
		},
		"feature": {
			Tenant: "t1", Repo: "o/r",
			WriteRef: "refs/heads/feat", ReadRefs: []string{"refs/heads/feat", "refs/heads/main"},
			CapBytes: 10 << 20,
		},
		"pr": {
			Tenant: "t1", Repo: "o/r",
			WriteRef: "refs/pull/1/merge", ReadRefs: []string{"refs/pull/1/merge", "refs/heads/main"},
			CapBytes: 10 << 20,
		},
		"sibling": {
			Tenant: "t1", Repo: "o/r",
			WriteRef: "refs/heads/other", ReadRefs: []string{"refs/heads/other", "refs/heads/main"},
			CapBytes: 10 << 20,
		},
		"other-repo": {
			Tenant: "t1", Repo: "o/other",
			WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
			CapBytes: 10 << 20,
		},
		"other-tenant": {
			Tenant: "t2", Repo: "o/r",
			WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
			CapBytes: 10 << 20,
		},
	}
	h := newTestServer(t, dir, nil)
	payload := []byte("default-branch")
	saveEntry(t, h, "default", "shared", payload)
	saveEntry(t, h, "feature", "feat-only", []byte("feat"))
	saveEntry(t, h, "pr", "pr-only", []byte("pr"))

	if b, ok := restoreBytes(t, h, "feature", "shared", nil); !ok || !bytes.Equal(b, payload) {
		t.Fatal("feature should read default")
	}
	if _, ok := restoreBytes(t, h, "default", "feat-only", nil); ok {
		t.Fatal("default must not read feature")
	}
	if _, ok := restoreBytes(t, h, "default", "pr-only", nil); ok {
		t.Fatal("default must not read pr")
	}
	if b, ok := restoreBytes(t, h, "pr", "shared", nil); !ok || !bytes.Equal(b, payload) {
		t.Fatal("pr should read base")
	}
	if _, ok := restoreBytes(t, h, "sibling", "feat-only", nil); ok {
		t.Fatal("sibling must not read feature")
	}
	if _, ok := restoreBytes(t, h, "sibling", "pr-only", nil); ok {
		t.Fatal("sibling must not read pr")
	}
	if _, ok := restoreBytes(t, h, "other-repo", "shared", nil); ok {
		t.Fatal("other repo isolated")
	}
	if _, ok := restoreBytes(t, h, "other-tenant", "shared", nil); ok {
		t.Fatal("other tenant isolated")
	}

	// Download URL outside ReadRefs -> 403
	saveEntry(t, h, "feature", "secret", []byte("x"))
	url := downloadURLForKey(t, h, "feature", "secret")
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Test-Job", "default")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestTwirpJSONShapes(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"j": {Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"}, CapBytes: 1 << 20},
	}
	h := newTestServer(t, dir, nil)

	rawCreate := `{"key":"my-key","version":"v1","metadata":{"foo":"bar"}}`
	req := httptest.NewRequest(http.MethodPost, cacheServicePrefix+"CreateCacheEntry", strings.NewReader(rawCreate))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Job", "j")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}
	var createResp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}
	if _, ok := createResp["signed_upload_url"]; !ok {
		t.Fatal("missing signed_upload_url")
	}

	payload := []byte("data")
	uploadURL := strings.Trim(string(createResp["signed_upload_url"]), `"`)
	put := httptest.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(payload))
	put.Header.Set("X-Test-Job", "j")
	put.Header.Set("x-ms-blob-type", "BlockBlob")
	putRec := httptest.NewRecorder()
	h.ServeHTTP(putRec, put)

	rawFin := `{"key":"my-key","version":"v1","size_bytes":"4"}`
	req = httptest.NewRequest(http.MethodPost, cacheServicePrefix+"FinalizeCacheEntryUpload", strings.NewReader(rawFin))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Job", "j")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if !strings.Contains(string(body), `"entry_id":"1"`) && !strings.Contains(string(body), `"entry_id": "1"`) {
		var fin map[string]string
		if err := json.Unmarshal(body, &fin); err != nil {
			t.Fatal(err)
		}
		if fin["entry_id"] != "1" {
			t.Fatalf("entry_id string: %s", body)
		}
	}
}

func TestBlockBlobUpload(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"j": {Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"}, CapBytes: 10 << 20},
	}
	h := newTestServer(t, dir, nil)
	want := miB(1)

	// Single-shot PUT
	saveEntry(t, h, "j", "single", want)
	got, ok := restoreBytes(t, h, "j", "single", nil)
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("single PUT restore failed")
	}

	// Block upload
	code, body := postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "blocks", "version": testVersion})
	if code != http.StatusOK {
		t.Fatal(code)
	}
	var created struct {
		OK              bool   `json:"ok"`
		SignedUploadURL string `json:"signed_upload_url"`
	}
	_ = json.Unmarshal(body, &created)
	blockA := "YmxvY2th" // base64 ids
	blockB := "YmxvY2ti"
	partA := want[:len(want)/2]
	partB := want[len(want)/2:]
	for _, pair := range []struct {
		id string
		p  []byte
	}{
		{blockB, partB},
		{blockA, partA},
	} {
		u := created.SignedUploadURL + "?comp=block&blockid=" + pair.id
		req := httptest.NewRequest(http.MethodPut, u, bytes.NewReader(pair.p))
		req.Header.Set("X-Test-Job", "j")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("block upload: %d", rec.Code)
		}
	}
	list := `<BlockList><Latest>` + blockA + `</Latest><Latest>` + blockB + `</Latest></BlockList>`
	req := httptest.NewRequest(http.MethodPut, created.SignedUploadURL+"?comp=blocklist", strings.NewReader(list))
	req.Header.Set("X-Test-Job", "j")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("blocklist: %d", rec.Code)
	}
	if code, body := postTwirp(t, h, "j", "FinalizeCacheEntryUpload", map[string]string{
		"key": "blocks", "version": testVersion, "size_bytes": fmt.Sprintf("%d", len(want)),
	}); code != http.StatusOK {
		t.Fatal(code, body)
	}
	got, ok = restoreBytes(t, h, "j", "blocks", nil)
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("block restore failed")
	}
}

func TestCreateRefusesDuplicateAndOverCap(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"j": {Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"}, CapBytes: 100},
	}
	h := newTestServer(t, dir, nil)
	saveEntry(t, h, "j", "dup", []byte("x"))

	// A collision is a plain refusal: @actions/cache 5.1.0 and 6.1.0 warn
	// on non-empty collision messages, so these refusals must carry none.
	assertCollision := func(t *testing.T, body []byte) {
		t.Helper()
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("collision response is not json: %v (%s)", err, body)
		}
		if string(raw["ok"]) != "false" {
			t.Fatalf("collision response ok = %s, want false (%s)", raw["ok"], body)
		}
		if _, has := raw["message"]; has {
			t.Fatalf("collision response carries a message: %s", body)
		}
		if _, has := raw["signed_upload_url"]; has {
			t.Fatalf("collision response carries a signed upload url: %s", body)
		}
	}

	code, body := postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "dup", "version": testVersion})
	if code != http.StatusOK {
		t.Fatalf("duplicate create status = %d, want 200 (%s)", code, body)
	}
	assertCollision(t, body)

	// A matching in-progress upload is the same collision and takes the same
	// response shape.
	var reserved struct {
		OK bool `json:"ok"`
	}
	_, body = postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "pending", "version": testVersion})
	if err := json.Unmarshal(body, &reserved); err != nil || !reserved.OK {
		t.Fatalf("reserve pending upload: %s", body)
	}
	code, body = postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "pending", "version": testVersion})
	if code != http.StatusOK {
		t.Fatalf("in-progress create status = %d, want 200 (%s)", code, body)
	}
	assertCollision(t, body)

	var resp struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}

	testScopes["j"] = Scope{
		Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
		CapBytes: 50,
	}
	big := make([]byte, 80)
	_, body = postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "big", "version": testVersion})
	_ = json.Unmarshal(body, &resp)
	if !resp.OK {
		t.Fatal(body)
	}
	var created struct {
		SignedUploadURL string `json:"signed_upload_url"`
	}
	_ = json.Unmarshal(body, &created)
	put := httptest.NewRequest(http.MethodPut, created.SignedUploadURL, bytes.NewReader(big))
	put.Header.Set("X-Test-Job", "j")
	put.Header.Set("x-ms-blob-type", "BlockBlob")
	putRec := httptest.NewRecorder()
	h.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT over cap = %d, want 413", putRec.Code)
	}

	_, body = postTwirp(t, h, "j", "FinalizeCacheEntryUpload", map[string]string{
		"key": "big", "version": testVersion, "size_bytes": "80",
	})
	_ = json.Unmarshal(body, &resp)
	if resp.OK {
		t.Fatal("finalize over cap should fail")
	}
	// The refused upload is dropped, so the key can be saved again.
	_, body = postTwirp(t, h, "j", "CreateCacheEntry", map[string]string{"key": "big", "version": testVersion})
	_ = json.Unmarshal(body, &resp)
	if !resp.OK {
		t.Fatalf("create after a refused upload: %s", body)
	}
}

func TestForwardsOtherTwirpCallsUpstream(t *testing.T) {
	secret := []byte("super-secret-body-bytes")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, secret) {
			t.Errorf("upstream body mismatch")
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing auth")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	var logBuf bytes.Buffer
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"j": {Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"}, CapBytes: 1 << 20},
	}
	upURL, _ := url.Parse(up.URL)
	srv, err := New(Options{
		Dir:      dir,
		Resolve:  testResolve,
		Upstream: upURL,
		Log:      slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}

	path := "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact"
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(secret))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Test-Job", "j")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("forward: %d", rec.Code)
	}
	if strings.Contains(logBuf.String(), "super-secret-body") {
		t.Fatal("log must not contain body")
	}
}

func TestIndexSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"j": {Tenant: "t", Repo: "o/r", WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"}, CapBytes: 1 << 20},
	}
	h := newTestServer(t, dir, nil)
	want := []byte("persisted")
	saveEntry(t, h, "j", "k", want)

	h2 := newTestServer(t, dir, nil)
	got, ok := restoreBytes(t, h2, "j", "k", nil)
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("entry lost after restart")
	}
}

func TestUnresolvedRequestIsForbidden(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Options{Dir: dir, Resolve: testResolve})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, cacheServicePrefix+"CreateCacheEntry", strings.NewReader(`{"key":"k","version":"v"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
}
