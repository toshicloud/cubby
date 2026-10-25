// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

// Package nodeoptions builds the NODE_OPTIONS entry that points
// @actions/cache at a cubby server. The entry is one data: URL module that
// rewrites ACTIONS_RESULTS_URL inside a node action's process, because the
// runner sets that variable after the job's environment and it cannot be
// changed from outside.
// Source: actions/runner v2.338.1, NodeScriptActionHandler.cs; see PROTOCOL.md.
package nodeoptions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// guard keeps the redirect's condition: @actions/cache reads
// ACTIONS_RESULTS_URL only for the v2 cache service, so a job that is not
// using v2, or has no URL yet, sees no change.
const guard = "if(process.env.ACTIONS_CACHE_SERVICE_V2&&process.env.ACTIONS_RESULTS_URL)"

// entryPrefix is the module import that carries the injected JavaScript.
const entryPrefix = "--import=data:text/javascript;base64,"

// ForURL returns one NODE_OPTIONS argument that rewrites ACTIONS_RESULTS_URL
// to the cubby server at raw. It accepts only an absolute http or https URL
// with a host, no credentials, no query and no fragment, with an empty path
// or "/"; the path is normalized to "/". The URL reaches JavaScript through
// encoding/json, and the module is base64-encoded so NODE_OPTIONS cannot
// split it on whitespace or quotes.
func ForURL(raw string) (string, error) {
	normalized, err := normalize(raw)
	if err != nil {
		return "", err
	}
	quoted, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode url: %w", err)
	}
	js := guard + "process.env.ACTIONS_RESULTS_URL=" + string(quoted)
	return entryPrefix + base64.StdEncoding.EncodeToString([]byte(js)), nil
}

// normalize validates raw and returns it with a trailing slash.
func normalize(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid url %q: scheme must be http or https", raw)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("invalid url %q: must be absolute", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid url %q: host is required", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid url %q: credentials are not allowed", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("invalid url %q: query is not allowed", raw)
	}
	// A trailing "#" parses to an empty fragment, so check the raw text.
	if strings.Contains(raw, "#") {
		return "", fmt.Errorf("invalid url %q: fragment is not allowed", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("invalid url %q: path must be empty or /", raw)
	}
	u.Path = "/"
	u.RawPath = ""
	return u.String(), nil
}
