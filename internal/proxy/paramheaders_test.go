// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/base64"
	"testing"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
)

// githubOwnerRepoSchema mirrors github-mcp-server's real annotated inputSchema
// for a tool like list_issues: owner/repo carry SEP-2243's "x-mcp-header".
const githubOwnerRepoSchema = `{
  "type": "object",
  "properties": {
    "owner": {"type": "string", "x-mcp-header": "owner"},
    "repo": {"type": "string", "x-mcp-header": "repo"},
    "state": {"type": "string"}
  },
  "required": ["owner", "repo"]
}`

// warmCatalog issues a tools/list so the handler's catalog cache is populated
// before a test drives tools/call directly — mirroring how a real MCP client
// always discovers tools before calling one. paramHeadersForCall deliberately
// reads only the warm cache (see catalog.cached) rather than forcing a fetch,
// so a test that skips this and goes straight to tools/call is exercising the
// documented cold-cache fallback (no headers, call still proceeds), not this
// SEP-2243 projection.
func warmCatalog(h *Handler, t *testing.T) {
	t.Helper()
	rpc(h, "tools/list", nil, "", "")
}

func githubBackendCfg(fb *fakeBackend, name string) config.BackendConfig {
	return config.BackendConfig{
		Name: name, URL: fb.url(), Credential: name + "-sa",
		ProtocolVersion: config.ProtocolVersionStateless,
		Tenants: []config.TenantConfig{
			{ID: name + "-team", Groups: []string{name + "-team"}},
		},
	}
}

// This is the regression test for the 2026-09-23 GitHub MCP incident: a
// stateless backend (github-mcp-server >= v1.12.2) rejects any tools/call
// whose annotated owner/repo arguments aren't also projected onto
// Mcp-Param-owner/Mcp-Param-repo headers (SEP-2243), once it sees
// MCP-Protocol-Version: 2026-07-28 — which mt-mcp-proxy always sends to a
// backend configured stateless. mt-mcp-proxy never generated these headers,
// so every call to an owner/repo-scoped tool failed deterministically.
func TestToolsCallProjectsSEP2243ParamHeaders(t *testing.T) {
	fb := newStatelessFakeBackend(t, "github", "list_issues")
	fb.toolSchemas = map[string]string{"list_issues": githubOwnerRepoSchema}
	fb.enforceParamHeaders = true
	h := harness(t, []config.BackendConfig{githubBackendCfg(fb, "github")})
	warmCatalog(h, t)

	w := rpc(h, "tools/call", map[string]any{
		"name": "list_issues",
		"arguments": map[string]any{
			"owner": "philips-internal",
			"repo":  "dip-ai",
			"state": "open",
		},
	}, token(t, "github-team"), "")

	resp := decodeResult(t, w)
	if errObj, isErr := resp["error"]; isErr {
		t.Fatalf("tools/call rejected by backend header enforcement: %v (body=%s)", errObj, w.Body.String())
	}
	result, _ := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call returned an isError result: %s", w.Body.String())
	}

	if got := fb.header("Mcp-Param-owner"); got != "philips-internal" {
		t.Errorf("Mcp-Param-owner header = %q, want %q", got, "philips-internal")
	}
	if got := fb.header("Mcp-Param-repo"); got != "dip-ai" {
		t.Errorf("Mcp-Param-repo header = %q, want %q", got, "dip-ai")
	}
	// "state" has no x-mcp-header annotation: must not get a stray header.
	if got := fb.header("Mcp-Param-state"); got != "" {
		t.Errorf("Mcp-Param-state header = %q, want none (state is not annotated)", got)
	}
}

// A null/absent annotated argument must not produce a header at all — sending
// an empty Mcp-Param-owner would itself be a mismatch against the real SDK's
// validateParamHeaders ("unexpected header for absent parameter").
func TestToolsCallOmitsParamHeaderForAbsentArg(t *testing.T) {
	fb := newStatelessFakeBackend(t, "github", "list_issues")
	fb.toolSchemas = map[string]string{"list_issues": githubOwnerRepoSchema}
	fb.enforceParamHeaders = true
	h := harness(t, []config.BackendConfig{githubBackendCfg(fb, "github")})
	warmCatalog(h, t)

	// Deliberately omit "repo" too, so the schema's "required" is violated,
	// but that's the backend tool handler's concern, not header projection —
	// this test only exercises the header layer, which must not fabricate a
	// header for an argument that was never provided.
	w := rpc(h, "tools/call", map[string]any{
		"name":      "list_issues",
		"arguments": map[string]any{"owner": "philips-internal"},
	}, token(t, "github-team"), "")

	resp := decodeResult(t, w)
	if errObj, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected error: %v (body=%s)", errObj, w.Body.String())
	}
	if got := fb.header("Mcp-Param-repo"); got != "" {
		t.Errorf("Mcp-Param-repo header = %q, want none (repo argument absent)", got)
	}
}

// A value requiring Base64 encoding per SEP-2243 (here, a repo name with
// leading whitespace — a plain space *inside* a value is safe ASCII and must
// NOT be encoded, matching go-sdk's requiresBase64Encoding exactly) must be
// wrapped, not sent raw — a raw unsafe header value round-trips incorrectly
// and would itself be a mismatch.
func TestToolsCallBase64EncodesUnsafeParamHeaderValue(t *testing.T) {
	fb := newStatelessFakeBackend(t, "github", "list_issues")
	fb.toolSchemas = map[string]string{"list_issues": githubOwnerRepoSchema}
	fb.enforceParamHeaders = true
	h := harness(t, []config.BackendConfig{githubBackendCfg(fb, "github")})
	warmCatalog(h, t)

	const unsafeRepo = " dip-ai"
	w := rpc(h, "tools/call", map[string]any{
		"name":      "list_issues",
		"arguments": map[string]any{"owner": "philips-internal", "repo": unsafeRepo},
	}, token(t, "github-team"), "")

	resp := decodeResult(t, w)
	if errObj, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected error: %v (body=%s)", errObj, w.Body.String())
	}
	got := fb.header("Mcp-Param-repo")
	if got == unsafeRepo {
		t.Errorf("Mcp-Param-repo header sent raw with an unsafe value %q, want Base64-wrapped", got)
	}
	decoded, ok := decodeParamHeaderValueForTest(got)
	if !ok || decoded != unsafeRepo {
		t.Errorf("Mcp-Param-repo header %q does not decode back to %q", got, unsafeRepo)
	}
}

// paramHeadersForCall must never force a catalog fetch as a side effect of
// tools/call — that would turn every call into a hidden network request
// against the reference backend (see catalog.cached's doc comment, and the
// pre-existing tests it was added to stop breaking:
// TestToolsCallSingleTenantRoutes, TestSameBackendTwoTenantsIsolatedSessions,
// TestToolsCallStatelessBackendNoSessionReuse). A client that skips straight
// to tools/call on a genuinely cold proxy just doesn't get the headers.
func TestToolsCallSkipsParamHeadersOnColdCatalog(t *testing.T) {
	fb := newStatelessFakeBackend(t, "github", "list_issues")
	fb.toolSchemas = map[string]string{"list_issues": githubOwnerRepoSchema}
	h := harness(t, []config.BackendConfig{githubBackendCfg(fb, "github")})

	w := rpc(h, "tools/call", map[string]any{
		"name":      "list_issues",
		"arguments": map[string]any{"owner": "philips-internal", "repo": "dip-ai"},
	}, token(t, "github-team"), "")

	resp := decodeResult(t, w)
	if errObj, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected error: %v (body=%s)", errObj, w.Body.String())
	}
	if fb.requestCount() != 1 {
		t.Errorf("expected exactly 1 backend request (the tools/call itself), got %d — paramHeadersForCall must not have triggered a catalog fetch", fb.requestCount())
	}
	if got := fb.header("Mcp-Param-owner"); got != "" {
		t.Errorf("Mcp-Param-owner header = %q, want none (catalog was never warmed)", got)
	}
}

// decodeParamHeaderValueForTest reverses encodeParamHeaderValue's Base64
// wrapping, mirroring go-sdk's decodeHeaderValue closely enough to assert
// round-trip fidelity.
func decodeParamHeaderValueForTest(v string) (string, bool) {
	const prefix, suffix = paramHeaderBase64Prefix, paramHeaderBase64Suffix
	if len(v) < len(prefix)+len(suffix) || v[:len(prefix)] != prefix || v[len(v)-len(suffix):] != suffix {
		return v, true
	}
	encoded := v[len(prefix) : len(v)-len(suffix)]
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}
