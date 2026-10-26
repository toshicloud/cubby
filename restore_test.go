// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import "testing"

func TestPrimaryKeyPrefixPrecedesRestoreKeys(t *testing.T) {
	oneJob(t, 1<<20)
	h := newTestServer(t, t.TempDir(), nil)
	saveEntry(t, h, "j", "primary-full", []byte("prefix"))
	saveEntry(t, h, "j", "fallback", []byte("fallback"))
	if got, ok := restoreBytes(t, h, "j", "primary", []string{"fallback"}); !ok || string(got) != "prefix" {
		t.Fatalf("restore = %q, hit %v; want primary-key prefix", got, ok)
	}
	saveEntry(t, h, "j", "primary", []byte("exact"))
	if got, ok := restoreBytes(t, h, "j", "primary", []string{"fallback"}); !ok || string(got) != "exact" {
		t.Fatalf("restore = %q, hit %v; want exact primary key", got, ok)
	}
}
