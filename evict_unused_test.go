// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"testing"
	"time"
)

// TestEvictUnused: an entry no save or restore used for the age goes, from
// the index on disk too; one restored since stays, and usage drops by the
// bytes removed.
func TestEvictUnused(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"main": {
			Tenant: "tenant-age", Repo: "o/r",
			WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
			CapBytes: 10 << 20,
		},
	}
	now := time.Unix(1_700_000_000, 0)
	opts := Options{Dir: dir, Resolve: testResolve, Now: func() time.Time { return now }}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	saveEntry(t, srv, "main", "old", miB(1))
	saveEntry(t, srv, "main", "used", miB(1))
	now = now.Add(6 * 24 * time.Hour)
	if _, ok := restoreBytes(t, srv, "main", "used", nil); !ok {
		t.Fatal("used missing")
	}
	now = now.Add(2 * 24 * time.Hour)

	freed, err := srv.EvictUnused(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("EvictUnused: %v", err)
	}
	if freed != int64(len(miB(1))) {
		t.Fatalf("freed %d bytes, want %d", freed, len(miB(1)))
	}
	if got := srv.Usage("tenant-age"); got != int64(len(miB(1))) {
		t.Fatalf("usage = %d, want %d", got, len(miB(1)))
	}
	reopened, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restoreBytes(t, reopened, "main", "old", nil); ok {
		t.Fatal("old entry survived EvictUnused")
	}
	if _, ok := restoreBytes(t, reopened, "main", "used", nil); !ok {
		t.Fatal("used entry was evicted")
	}
}
