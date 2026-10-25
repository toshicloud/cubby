// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveTenant: RemoveTenant drops a tenant's store and files, and a
// missing directory is a success.
func TestRemoveTenant(t *testing.T) {
	dir := t.TempDir()
	testScopes = map[testJob]Scope{
		"main": {
			Tenant: "tenant-rm", Repo: "o/r",
			WriteRef: "refs/heads/main", ReadRefs: []string{"refs/heads/main"},
			CapBytes: 10 << 20,
		},
	}
	srv, err := New(Options{Dir: dir, Resolve: testResolve})
	if err != nil {
		t.Fatal(err)
	}
	saveEntry(t, srv, "main", "k1", miB(1))
	if srv.Usage("tenant-rm") == 0 {
		t.Fatal("usage is zero after a save")
	}
	tenantDir := filepath.Join(dir, tenantDirName("tenant-rm"))
	if _, err := os.Stat(tenantDir); err != nil {
		t.Fatalf("tenant dir: %v", err)
	}

	if err := srv.RemoveTenant("tenant-rm"); err != nil {
		t.Fatalf("RemoveTenant: %v", err)
	}
	if got := srv.Usage("tenant-rm"); got != 0 {
		t.Fatalf("usage after remove = %d, want 0", got)
	}
	if _, err := os.Stat(tenantDir); !os.IsNotExist(err) {
		t.Fatalf("tenant dir still present: %v", err)
	}

	// A missing directory is a success.
	if err := srv.RemoveTenant("tenant-rm"); err != nil {
		t.Fatalf("RemoveTenant missing: %v", err)
	}
}
