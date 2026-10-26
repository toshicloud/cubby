// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDisabledCacheRefusesExistingEntries(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)
	saveEntry(t, h, "j", "saved", []byte("cached"))
	downloadURL := downloadURLForKey(t, h, "j", "saved")
	scope := testScopes["j"]
	scope.CapBytes = 0
	testScopes["j"] = scope
	code, body := postTwirp(t, h, "j", "GetCacheEntryDownloadURL", map[string]string{"key": "saved", "version": testVersion})
	var response getDownloadResp
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || response.OK {
		t.Errorf("disabled lookup = %d %s; want a cache miss", code, body)
	}
	r := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	r.Header.Set("X-Test-Job", "j")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("disabled download = %d; want 403", w.Code)
	}
}
