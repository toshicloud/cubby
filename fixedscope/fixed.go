// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Package fixedscope resolves every request on a dedicated listener to one
// operator-configured scope. The listener must be isolated to that job.
package fixedscope

import (
	"fmt"
	"net/http"
	"strings"

	"go.toshicloud.io/cubby"
)

// New returns a resolver for a listener dedicated to one job. It snapshots
// scope, searches WriteRef first, and never trusts request-controlled identity.
// Anyone who can reach the listener receives this scope's permissions.
func New(scope cubby.Scope) (func(*http.Request) (cubby.Scope, bool), error) {
	if strings.TrimSpace(scope.Tenant) == "" || strings.TrimSpace(scope.Repo) == "" || strings.TrimSpace(scope.WriteRef) == "" {
		return nil, fmt.Errorf("fixedscope: tenant, repository and write ref are required")
	}
	refs := []string{scope.WriteRef}
	seen := map[string]bool{scope.WriteRef: true}
	for _, ref := range scope.ReadRefs {
		if strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("fixedscope: empty read ref")
		}
		if !seen[ref] {
			refs = append(refs, ref)
			seen[ref] = true
		}
	}
	scope.ReadRefs = refs
	return func(*http.Request) (cubby.Scope, bool) {
		result := scope
		result.ReadRefs = append([]string(nil), refs...)
		return result, true
	}, nil
}
