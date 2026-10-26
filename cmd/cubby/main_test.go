// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.toshicloud.io/cubby/internal/nodeoptions"
)

const testURL = "http://127.0.0.1:8095"

func TestNodeOptionsKeepsExistingOptions(t *testing.T) {
	entry, err := nodeoptions.ForURL(testURL)
	if err != nil {
		t.Fatalf("ForURL: %v", err)
	}
	var stdout, stderr strings.Builder
	err = run(context.Background(), []string{
		"node-options", "--url", testURL, "--existing", "--max-old-space-size=512",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v (stderr %s)", err, stderr.String())
	}
	if want := entry + " --max-old-space-size=512\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestNodeOptionsWithoutExisting(t *testing.T) {
	entry, err := nodeoptions.ForURL(testURL)
	if err != nil {
		t.Fatalf("ForURL: %v", err)
	}
	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"node-options", "--url", testURL}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v (stderr %s)", err, stderr.String())
	}
	if want := entry + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"frobnicate"}},
		{"help with arguments", []string{"help", "serve"}},
		{"serve without dir", []string{"serve", "--tenant", "t", "--repo", "acme/app", "--write-ref", "refs/heads/main", "--cap", "1024"}},
		{"serve without tenant", []string{"serve", "--dir", filepath.Join(t.TempDir(), "cache"), "--repo", "acme/app", "--write-ref", "refs/heads/main", "--cap", "1024"}},
		{"serve without cap", []string{"serve", "--dir", filepath.Join(t.TempDir(), "cache"), "--tenant", "t", "--repo", "acme/app", "--write-ref", "refs/heads/main"}},
		{"serve without write ref", []string{"serve", "--dir", filepath.Join(t.TempDir(), "cache"), "--tenant", "t", "--repo", "acme/app", "--cap", "1024"}},
		{"serve unknown flag", []string{"serve", "--bogus"}},
		{"serve extra argument", []string{"serve", "--dir", "d", "--tenant", "t", "--repo", "acme/app", "--write-ref", "refs/heads/main", "--cap", "1024", "extra"}},
		{"node-options without url", []string{"node-options"}},
		{"node-options unknown flag", []string{"node-options", "--url", testURL, "--bogus"}},
		{"node-options extra argument", []string{"node-options", "--url", testURL, "extra"}},
		{"node-options bad url", []string{"node-options", "--url", "ftp://127.0.0.1:8095/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(context.Background(), tc.args, io.Discard, io.Discard); err == nil {
				t.Fatalf("run(%q) = nil, want an error", tc.args)
			}
		})
	}
}

func TestServeRejectsExposedListenerBeforeCreatingFiles(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8095", "localhost:8095", "[::]:8095", "192.0.2.1:8095"} {
		t.Run(listen, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			err := run(context.Background(), []string{
				"serve", "--dir", dir, "--tenant", "tenant", "--repo", "acme/app",
				"--write-ref", "refs/heads/main", "--cap", "1073741824", "--listen", listen,
			}, io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("serve --listen %s = nil, want an error", listen)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("serve --listen %s created the storage directory", listen)
			}
		})
	}
}

func TestServeRejectsExistingUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cubby.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"serve", "--dir", filepath.Join(t.TempDir(), "cache"), "--tenant", "tenant",
		"--repo", "acme/app", "--write-ref", "refs/heads/main", "--cap", "1073741824",
		"--listen", "unix:" + path,
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("serve on an existing path = nil, want an error")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("an existing file at the socket path was removed: %v", statErr)
	}
}

func TestCloseKeepsReplacementSocketPath(t *testing.T) {
	target, err := parseListen("unix:" + filepath.Join(t.TempDir(), "cubby.sock"))
	if err != nil {
		t.Fatalf("parseListen: %v", err)
	}
	ln, err := target.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created, err := os.Lstat(target.unix)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	// Another process replaces the path after our socket is gone.
	if err := os.Remove(target.unix); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	if err := os.WriteFile(target.unix, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	_ = ln.Close()
	removeSocket(target.unix, created)
	if _, err := os.Stat(target.unix); err != nil {
		t.Fatalf("closing the listener removed a replaced path: %v", err)
	}
}

func TestServeStopsOnCancelledContext(t *testing.T) {
	// An explicit zero or negative cap is the cache-off case, but the flag is
	// still supplied, so each of these must start and stop cleanly.
	for _, capValue := range []string{"1073741824", "0", "-1"} {
		t.Run("cap "+capValue, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			socket := filepath.Join(t.TempDir(), "cubby.sock")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := run(ctx, []string{
				"serve", "--dir", dir, "--tenant", "tenant", "--repo", "acme/app",
				"--write-ref", "refs/heads/main", "--cap", capValue, "--listen", "unix:" + socket,
			}, io.Discard, io.Discard)
			if err != nil {
				t.Fatalf("serve with a cancelled context and --cap %s: %v", capValue, err)
			}
			if _, statErr := os.Stat(socket); !os.IsNotExist(statErr) {
				t.Fatalf("unix socket was not removed: %v", statErr)
			}
		})
	}
}
