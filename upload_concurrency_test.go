// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Overlapping retries must publish complete requests, never share a temporary
// inode that another request can rename while it is still being written.
func TestConcurrentUploadRetriesPublishWholeRequests(t *testing.T) {
	for _, block := range []bool{false, true} {
		name := "blob"
		if block {
			name = "block"
		}
		t.Run(name, func(t *testing.T) {
			oneJob(t, 1<<20)
			h := newTestServer(t, t.TempDir(), nil)
			uploadURL, _ := createUpload(t, h, "j", "retry")
			target := uploadURL
			if block {
				target += "?comp=block&blockid=YQ=="
			}
			put := func(body io.Reader) int {
				r := httptest.NewRequest(http.MethodPut, target, body)
				r.Header.Set("X-Test-Job", "j")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Code
			}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			done := make(chan int, 1)
			go func() { done <- put(reader) }()
			if _, err := writer.Write([]byte("first")); err != nil {
				t.Fatal(err)
			}
			if code := put(strings.NewReader("second")); code != http.StatusCreated {
				t.Fatalf("second upload: %d", code)
			}
			if _, err := writer.Write([]byte("-tail")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != http.StatusCreated {
				t.Fatalf("first upload finishing last: %d; want 201", code)
			}
			if block {
				if w := commitBlockList(t, h, "j", uploadURL, "YQ=="); w.Code != http.StatusCreated {
					t.Fatalf("commit: %d %s", w.Code, w.Body.String())
				}
			}
			finalizeEntry(t, h, "j", "retry", int64(len("first-tail")))
			if got, ok := restoreBytes(t, h, "j", "retry", nil); !ok || string(got) != "first-tail" {
				t.Fatalf("restored %q, hit %v; want first-tail", got, ok)
			}
		})
	}
}
