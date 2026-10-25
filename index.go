// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cacheEntry is one finalized blob in the tenant index.
type cacheEntry struct {
	ID       int64     `json:"id"`
	Tenant   string    `json:"tenant"`
	Repo     string    `json:"repo"`
	Ref      string    `json:"ref"`
	Key      string    `json:"key"`
	Version  string    `json:"version"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
}

// uploadRecord is an in-progress upload bound to a cache key.
type uploadRecord struct {
	ID      string    `json:"id"`
	Tenant  string    `json:"tenant"`
	Repo    string    `json:"repo"`
	Ref     string    `json:"ref"`
	Key     string    `json:"key"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
}

type tenantIndex struct {
	NextID  int64          `json:"next_id"`
	Entries []cacheEntry   `json:"entries"`
	Uploads []uploadRecord `json:"uploads"`
}

func loadIndex(path string) (tenantIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return tenantIndex{NextID: 1}, nil
		}
		return tenantIndex{}, fmt.Errorf("cubby: read index: %w", err)
	}
	var idx tenantIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return tenantIndex{}, fmt.Errorf("cubby: parse index: %w", err)
	}
	if idx.NextID < 1 {
		idx.NextID = 1
	}
	return idx, nil
}

func saveIndex(path string, idx tenantIndex) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cubby: mkdir index dir: %w", err)
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("cubby: marshal index: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("cubby: write index temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cubby: rename index: %w", err)
	}
	return nil
}
