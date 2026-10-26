// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestMalformedUploadIDDoesNotOpenStore pins that a PUT whose path id is not
// 32 lower-case hexadecimal characters is answered 404 before the server
// opens a tenant store: a malformed id must not create storage on its way to
// an index miss.
func TestMalformedUploadIDDoesNotOpenStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"32-byte non-hex", strings.Repeat("g", 32)},
		{"32-byte upper-case hex", strings.Repeat("A", 32)},
		{"32-byte containing slash", strings.Repeat("a", 15) + "/" + strings.Repeat("a", 16)},
		{"32-byte containing backslash", strings.Repeat("a", 15) + `\` + strings.Repeat("a", 16)},
		{"32-byte containing dot dot", strings.Repeat("a", 30) + ".."},
		{"32-byte with trailing newline", strings.Repeat("a", 31) + "\n"},
		{"31 lower-case hex", strings.Repeat("a", 31)},
		{"33 lower-case hex", strings.Repeat("a", 33)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			oneJob(t, 1<<20)
			h := newTestServer(t, root, nil)

			req := httptest.NewRequest(http.MethodPut, uploadPrefix+url.PathEscape(tc.id), strings.NewReader("x"))
			req.Header.Set("X-Test-Job", "j")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("PUT id %q = %d; want 404", tc.id, rec.Code)
			}

			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("read cache root: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("cache root holds %v after a malformed upload id; want the request rejected before storage access", names)
			}
		})
	}
}
