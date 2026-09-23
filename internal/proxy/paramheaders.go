// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// paramHeaderPrefix is the HTTP header prefix SEP-2243 projects annotated
// tool-input properties onto (e.g. "owner" -> "Mcp-Param-owner").
const paramHeaderPrefix = "Mcp-Param-"

// Sentinel wrapper SEP-2243 uses to Base64-encode a header value that isn't
// safe plain ASCII.
const (
	paramHeaderBase64Prefix = "=?base64?"
	paramHeaderBase64Suffix = "?="
)

// toolInputSchema is the subset of a tool's inputSchema this package reads to
// find SEP-2243 "x-mcp-header" annotations. github-mcp-server only annotates
// top-level properties (owner/repo), so nested schemas are not walked.
type toolInputSchema struct {
	Properties map[string]struct {
		XMCPHeader json.RawMessage `json:"x-mcp-header,omitempty"`
	} `json:"properties"`
}

// paramHeadersForTool computes the Mcp-Param-* headers a SEP-2243-conformant
// MCP client sends alongside a tools/call: one per top-level input property
// the tool's schema annotates with "x-mcp-header", carrying that property's
// value from the call arguments. mt-mcp-proxy hand-rolls its backend requests
// instead of using an MCP client SDK, so it never got this projection for
// free — a backend that enforces the annotation once it sees
// MCP-Protocol-Version: 2026-07-28 (github-mcp-server >= v1.12.2) rejects any
// call missing it with a "header mismatch" error.
func paramHeadersForTool(tool rawTool, argsJSON json.RawMessage) map[string]string {
	rawSchema, ok := tool["inputSchema"]
	if !ok {
		return nil
	}
	var schema toolInputSchema
	if err := json.Unmarshal(rawSchema, &schema); err != nil || len(schema.Properties) == 0 {
		return nil
	}

	var args map[string]json.RawMessage
	if len(argsJSON) > 0 {
		_ = json.Unmarshal(argsJSON, &args)
	}

	var headers map[string]string
	for prop, ps := range schema.Properties {
		if len(ps.XMCPHeader) == 0 {
			continue
		}
		var headerName string
		if err := json.Unmarshal(ps.XMCPHeader, &headerName); err != nil || headerName == "" {
			continue
		}
		argRaw, ok := args[prop]
		if !ok || string(argRaw) == "null" {
			continue
		}
		encoded, ok := encodeParamHeaderValue(argRaw)
		if !ok {
			continue
		}
		if headers == nil {
			headers = make(map[string]string)
		}
		headers[paramHeaderPrefix+headerName] = encoded
	}
	return headers
}

// encodeParamHeaderValue renders a JSON primitive as an HTTP header value per
// SEP-2243: strings are used as-is when they're safe ASCII, else Base64-
// wrapped; booleans become "true"/"false"; integral numbers their decimal
// form. Reports false for any non-primitive (object/array/non-integer number).
func encodeParamHeaderValue(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	var s string
	switch val := v.(type) {
	case string:
		s = val
	case bool:
		s = strconv.FormatBool(val)
	case float64:
		if val != float64(int64(val)) {
			return "", false
		}
		s = strconv.FormatInt(int64(val), 10)
	default:
		return "", false
	}
	if requiresParamHeaderBase64(s) {
		return paramHeaderBase64Prefix + base64.StdEncoding.EncodeToString([]byte(s)) + paramHeaderBase64Suffix, true
	}
	return s, true
}

func requiresParamHeaderBase64(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\t") || strings.HasSuffix(s, " ") || strings.HasSuffix(s, "\t") {
		return true
	}
	for _, c := range s {
		if c < 0x20 || c > 0x7E {
			return true
		}
	}
	// Plain-ASCII values that happen to look like the Base64 sentinel wrapper
	// must also be encoded, to avoid ambiguity with an already-encoded value.
	if strings.HasPrefix(s, paramHeaderBase64Prefix) && strings.HasSuffix(s, paramHeaderBase64Suffix) {
		return true
	}
	return false
}

// mergeHeaders combines header maps in order, later maps overriding earlier
// ones on key collision. Returns nil (not an empty map) when every input is
// empty, so callers can pass the result straight through as an "extra"
// argument that means "nothing to add".
func mergeHeaders(maps ...map[string]string) map[string]string {
	var out map[string]string
	for _, m := range maps {
		for k, v := range m {
			if out == nil {
				out = make(map[string]string)
			}
			out[k] = v
		}
	}
	return out
}
