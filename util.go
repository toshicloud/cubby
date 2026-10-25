// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

// Protocol constants follow the @actions/cache 4.0.5 sources and its Azure
// storage SDK dependency recorded in PROTOCOL.md. Local limits are Cubby policy.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cacheServicePrefix = "/twirp/github.actions.results.api.v1.CacheService/"
	uploadPrefix       = "/upload/"
	downloadPrefix     = "/download/"
	maxKeyLen          = 512
	// maxTwirpBody and maxBlockListBody bound the bodies read into memory.
	maxTwirpBody     = 1 << 20
	maxBlockListBody = 4 << 20
	// maxBlockIDLen is Azure's limit on a decoded block ID, 64 bytes, in base64.
	maxBlockIDLen = 88
	uploadMaxAge  = time.Hour
)

func tenantDirName(tenant string) string {
	sum := sha256.Sum256([]byte(tenant))
	return hex.EncodeToString(sum[:])[:32]
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cubby: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func randomRequestID() string {
	id, err := randomHex(16)
	if err != nil {
		return "00000000000000000000000000000000"
	}
	return id
}

func writeAzureCreated(w http.ResponseWriter, now time.Time) {
	w.Header().Set("ETag", `"`+randomRequestID()[:16]+`"`)
	w.Header().Set("Last-Modified", now.UTC().Format(http.TimeFormat))
	w.Header().Set("x-ms-request-id", randomRequestID())
	w.Header().Set("x-ms-version", "2024-11-04")
	w.Header().Set("x-ms-request-server-encrypted", "true")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// streamToFileLimit is streamToFile that fails with errOverCap, keeping
// nothing, once r yields more than limit bytes.
func streamToFileLimit(dst string, r io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, errOverCap
	}
	lr := &io.LimitedReader{R: r, N: limit + 1}
	n, err := streamToFile(dst, lr)
	if err != nil {
		return n, err
	}
	if n > limit {
		_ = os.Remove(dst)
		return n, errOverCap
	}
	return n, nil
}

// streamToPart streams r into a unique temporary file beside dst and returns
// its path for the caller to publish. Over-cap writes leave nothing behind.
func streamToPart(dst string, r io.Reader, limit int64) (string, error) {
	if limit <= 0 {
		return "", errOverCap
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("cubby: mkdir: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.part")
	if err != nil {
		return "", fmt.Errorf("cubby: create temp: %w", err)
	}
	tmp := f.Name()
	n, copyErr := io.Copy(f, &io.LimitedReader{R: r, N: limit + 1})
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("cubby: close temp: %w", closeErr)
	}
	if n > limit {
		_ = os.Remove(tmp)
		return "", errOverCap
	}
	return tmp, nil
}

func streamToFile(dst string, r io.Reader) (int64, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("cubby: mkdir: %w", err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(dst)+".*.part")
	if err != nil {
		return 0, fmt.Errorf("cubby: create temp: %w", err)
	}
	tmp := f.Name()
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("cubby: close temp: %w", closeErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("cubby: rename blob: %w", err)
	}
	return n, nil
}

func validCacheKey(key string) bool {
	if key == "" || len(key) > maxKeyLen {
		return false
	}
	return !strings.Contains(key, ",")
}

type twirpError struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func writeTwirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(twirpError{Code: code, Msg: msg})
}

func parseJSONInt64(v json.RawMessage) (int64, error) {
	if len(v) == 0 {
		return 0, fmt.Errorf("cubby: empty int64")
	}
	var n int64
	if err := json.Unmarshal(v, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return 0, fmt.Errorf("cubby: int64: %w", err)
	}
	return strconv.ParseInt(s, 10, 64)
}

func hostURL(r *http.Request, path string) string {
	return "http://" + r.Host + path
}
