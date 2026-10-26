// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Command cubby serves a cache for self-hosted runners on a local listener
// and prints the NODE_OPTIONS entry that points @actions/cache at it.
// cmd/cubby only parses flags, builds cubby.Options and serves; the library
// is the product.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.toshicloud.io/cubby"
	"go.toshicloud.io/cubby/fixedscope"
	"go.toshicloud.io/cubby/internal/nodeoptions"
)

const (
	defaultListen     = "127.0.0.1:8095"
	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "cubby: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches one subcommand. It returns nil on success (including a
// flag package help request) and an error for main to report.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("missing command")
	}
	switch args[0] {
	case "serve":
		return runServe(ctx, args[1:], stderr)
	case "node-options":
		return runNodeOptions(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		if len(args) > 1 {
			return fmt.Errorf("unexpected arguments after %q: %s", args[0], strings.Join(args[1:], " "))
		}
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: cubby <command> [flags]

commands:
  serve         run the cache server on a loopback address or unix socket
  node-options  print the NODE_OPTIONS entry for a cubby URL

run "cubby <command> --help" for a command's flags
`)
}

// runNodeOptions prints the NODE_OPTIONS value: cubby's import argument, then
// the caller's existing options verbatim. The output is the environment
// variable's value, not a shell-quoted command line.
func runNodeOptions(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("node-options", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "cubby server base URL, absolute http or https (required)")
	existing := fs.String("existing", "", "existing NODE_OPTIONS to keep after cubby's argument")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: cubby node-options --url <url> [--existing <options>]\n\nprints one NODE_OPTIONS value on stdout\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("node-options: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("node-options: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *rawURL == "" {
		return errors.New("node-options: --url is required")
	}
	entry, err := nodeoptions.ForURL(*rawURL)
	if err != nil {
		return fmt.Errorf("node-options: %w", err)
	}
	if *existing != "" {
		entry += " " + *existing
	}
	if _, err := io.WriteString(stdout, entry+"\n"); err != nil {
		return fmt.Errorf("node-options: write: %w", err)
	}
	return nil
}

// stringsFlag collects a repeatable string flag in the order given.
type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, ",") }

func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runServe validates every argument, builds the scope and the server, opens
// the listener and serves until ctx is cancelled.
func runServe(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dir      = fs.String("dir", "", "storage directory, created if missing (required)")
		listen   = fs.String("listen", defaultListen, "loopback IP:port, or unix:/absolute/path")
		tenant   = fs.String("tenant", "", "tenant that owns the bytes and the cap (required)")
		repo     = fs.String("repo", "", "repository, owner/name (required)")
		writeRef = fs.String("write-ref", "", "ref saves write to (required)")
		readRefs stringsFlag
		capBytes = fs.Int64("cap", 0, "tenant byte cap in bytes (required; zero or less disables the cache)")
		upstream = fs.String("upstream", "", "absolute https URL for other twirp services; empty forwards nothing")
	)
	fs.Var(&readRefs, "read-ref", "extra ref restores search, in order (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: cubby serve --dir <path> --tenant <tenant> --repo <owner/name> --write-ref <ref> --cap <bytes> [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("serve: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *dir == "" {
		return errors.New("serve: --dir is required")
	}
	if *tenant == "" {
		return errors.New("serve: --tenant is required")
	}
	if *repo == "" {
		return errors.New("serve: --repo is required")
	}
	if *writeRef == "" {
		return errors.New("serve: --write-ref is required")
	}
	// --cap is required, even when the value is zero or negative, which the
	// library reads as the cache being off. Visit reports only set flags, so
	// an explicit 0 or -1 counts while an omitted flag does not.
	capSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cap" {
			capSet = true
		}
	})
	if !capSet {
		return errors.New("serve: --cap is required; pass 0 or a negative value to disable the cache")
	}

	up, err := parseUpstream(*upstream)
	if err != nil {
		return err
	}
	target, err := parseListen(*listen)
	if err != nil {
		return err
	}
	resolve, err := fixedscope.New(cubby.Scope{
		Tenant:   *tenant,
		Repo:     *repo,
		WriteRef: *writeRef,
		ReadRefs: readRefs,
		CapBytes: *capBytes,
	})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	srv, err := cubby.New(cubby.Options{
		Dir:      *dir,
		Resolve:  resolve,
		Upstream: up,
		Log:      log,
	})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	ln, err := target.open()
	if err != nil {
		return err
	}
	// Remove a unix socket this process created, and only that file.
	var socketFile os.FileInfo
	if target.unix != "" {
		socketFile, _ = os.Lstat(target.unix)
	}
	defer func() {
		_ = ln.Close()
		removeSocket(target.unix, socketFile)
	}()

	log.Info("listening", "addr", ln.Addr().String())

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return serverError(err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown incomplete; closing connections", "err", err)
		_ = httpSrv.Close()
	}
	return serverError(<-serveErr)
}

// serverError maps the normal end of a serve loop to nil.
func serverError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve: %w", err)
}

// removeSocket deletes a unix socket path only when it is still the file
// created for this listener; a path that another process has since replaced
// is left alone.
func removeSocket(path string, created os.FileInfo) {
	if path == "" || created == nil {
		return
	}
	cur, err := os.Lstat(path)
	if err != nil || !os.SameFile(created, cur) {
		return
	}
	_ = os.Remove(path)
}

// parseUpstream validates --upstream: an absolute https URL without
// credentials or a fragment, or empty for no forwarding.
func parseUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("serve: --upstream: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("serve: --upstream %q: scheme must be https", raw)
	}
	if u.Opaque != "" || u.Host == "" {
		return nil, fmt.Errorf("serve: --upstream %q: must be an absolute URL with a host", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("serve: --upstream %q: credentials are not allowed", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("serve: --upstream %q: fragment is not allowed", raw)
	}
	return u, nil
}

// listenTarget is a validated --listen value.
type listenTarget struct {
	unix string         // socket path, when the listener is a unix socket
	tcp  netip.AddrPort // loopback address, when the listener is TCP
}

// parseListen validates --listen without touching the filesystem: either
// unix:/absolute/path or a literal loopback IP and port. Hostnames and
// wildcard or non-loopback addresses are refused because the download and
// upload URLs are not signed.
func parseListen(spec string) (listenTarget, error) {
	if path, ok := strings.CutPrefix(spec, "unix:"); ok {
		if path == "" {
			return listenTarget{}, errors.New("serve: --listen: unix socket path is empty")
		}
		if !filepath.IsAbs(path) {
			return listenTarget{}, fmt.Errorf("serve: --listen %q: unix socket path must be absolute", spec)
		}
		return listenTarget{unix: path}, nil
	}
	addr, err := netip.ParseAddrPort(spec)
	if err != nil {
		return listenTarget{}, fmt.Errorf("serve: --listen %q: want a loopback IP:port (for example %s) or unix:/absolute/path", spec, defaultListen)
	}
	if !addr.Addr().IsLoopback() {
		return listenTarget{}, fmt.Errorf("serve: --listen %q: %s is not a loopback address", spec, addr.Addr())
	}
	return listenTarget{tcp: addr}, nil
}

// open creates the listener. A unix socket is created new with mode 0600:
// an existing path is never unlinked, so a second cubby on the same path
// fails instead of taking over the first one's socket.
func (t listenTarget) open() (net.Listener, error) {
	if t.unix != "" {
		ln, err := net.Listen("unix", t.unix)
		if err != nil {
			return nil, fmt.Errorf("serve: listen unix:%s: %w", t.unix, err)
		}
		ul, ok := ln.(*net.UnixListener)
		if !ok {
			_ = ln.Close()
			return nil, fmt.Errorf("serve: listen unix:%s: unexpected listener type %T", t.unix, ln)
		}
		// This process removes the socket itself, after checking the path is
		// still the file it created. Closing must not unlink a path that
		// another process has since bound.
		ul.SetUnlinkOnClose(false)
		if err := os.Chmod(t.unix, 0o600); err != nil {
			_ = ul.Close()
			_ = os.Remove(t.unix)
			return nil, fmt.Errorf("serve: listen unix:%s: chmod: %w", t.unix, err)
		}
		return ul, nil
	}
	ln, err := net.Listen("tcp", t.tcp.String())
	if err != nil {
		return nil, fmt.Errorf("serve: listen %s: %w", t.tcp, err)
	}
	return ln, nil
}
