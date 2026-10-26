// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package fixedscope

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"go.toshicloud.io/cubby"
)

func TestResolverKeepsConfiguredPermissions(t *testing.T) {
	scope := cubby.Scope{Tenant: "tenant", Repo: "owner/repo", WriteRef: "refs/pull/1/merge", ReadRefs: []string{"refs/heads/dev", "refs/heads/main", "refs/heads/dev"}, CapBytes: 1024}
	resolve, err := New(scope)
	if err != nil {
		t.Fatal(err)
	}
	scope.ReadRefs[0] = "refs/heads/untrusted"
	r := httptest.NewRequest("GET", "/?tenant=other&ref=refs/heads/main", nil)
	r.Header.Set("X-Tenant", "other")
	got, ok := resolve(r)
	want := cubby.Scope{Tenant: "tenant", Repo: "owner/repo", WriteRef: "refs/pull/1/merge", ReadRefs: []string{"refs/pull/1/merge", "refs/heads/dev", "refs/heads/main"}, CapBytes: 1024}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved scope = %+v, %v; want %+v, true", got, ok, want)
	}
	got.ReadRefs[0] = "refs/heads/untrusted"
	if again, _ := resolve(r); !reflect.DeepEqual(again, want) {
		t.Fatalf("returned scope mutated resolver: %+v", again)
	}
}

func TestResolverRejectsMissingIdentity(t *testing.T) {
	for _, scope := range []cubby.Scope{
		{Repo: "owner/repo", WriteRef: "refs/heads/main"},
		{Tenant: "tenant", WriteRef: "refs/heads/main"},
		{Tenant: "tenant", Repo: "owner/repo"},
		{Tenant: "tenant", Repo: "owner/repo", WriteRef: "refs/heads/main", ReadRefs: []string{""}},
	} {
		if _, err := New(scope); err == nil {
			t.Fatalf("accepted incomplete scope: %+v", scope)
		}
	}
}
