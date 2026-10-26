// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package nodeoptions

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const (
	wantPrefix = "--import=data:text/javascript;base64,"
	wantGuard  = "if(process.env.ACTIONS_CACHE_SERVICE_V2&&process.env.ACTIONS_RESULTS_URL)process.env.ACTIONS_RESULTS_URL="
)

func decodeJS(t *testing.T, arg string) string {
	t.Helper()
	if !strings.HasPrefix(arg, wantPrefix) {
		t.Fatalf("argument %q does not start with %q", arg, wantPrefix)
	}
	js, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(arg, wantPrefix))
	if err != nil {
		t.Fatalf("argument is not base64: %v", err)
	}
	return string(js)
}

func TestForURLNormalizes(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8095",
		"http://127.0.0.1:8095/",
		"https://cache.example.com",
		"https://cache.example.com:8443/",
		"http://[::1]:8095",
	} {
		arg, err := ForURL(raw)
		if err != nil {
			t.Fatalf("ForURL(%q): %v", raw, err)
		}
		if strings.ContainsAny(arg, " \t\n\"'") {
			t.Errorf("ForURL(%q) = %q: the argument must not contain whitespace or quotes", raw, arg)
		}
		js := decodeJS(t, arg)
		if !strings.HasPrefix(js, wantGuard) {
			t.Fatalf("ForURL(%q) module = %q: guard is not exact", raw, js)
		}
		var got string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(js, wantGuard)), &got); err != nil {
			t.Fatalf("ForURL(%q) module value is not JSON: %v", raw, err)
		}
		if got != strings.TrimSuffix(raw, "/")+"/" {
			t.Errorf("ForURL(%q) sets ACTIONS_RESULTS_URL to %q, want one trailing slash", raw, got)
		}
	}
}

func TestForURLRejects(t *testing.T) {
	for _, raw := range []string{
		"",
		"127.0.0.1:8095",
		"//127.0.0.1:8095",
		"ftp://127.0.0.1:8095/",
		"http://",
		"https:cache.example.com",
		"http://user:secret@127.0.0.1:8095/",
		"http://127.0.0.1:8095/?a=b",
		"http://127.0.0.1:8095/?",
		"http://127.0.0.1:8095/#fragment",
		"http://127.0.0.1:8095/#",
		"http://127.0.0.1:8095/cache",
		"http://127.0.0.1:8095/cache/",
		"http://127.0.0.1:8095/\n",
	} {
		if arg, err := ForURL(raw); err == nil {
			t.Errorf("ForURL(%q) = %q, want an error", raw, arg)
		}
	}
}
