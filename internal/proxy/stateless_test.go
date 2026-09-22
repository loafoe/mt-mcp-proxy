// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/loafoe/mt-mcp-proxy/internal/registry"
)

// --- client-facing statelessness (only when every backend is stateless) ---

// All backends stateless: the proxy itself must go stateless toward its own
// callers too — no Mcp-Session-Id, protocolVersion 2026-07-28.
func TestInitializeClientFacingStatelessWhenAllBackendsStateless(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	h := harness(t, []config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})

	w := rpc(h, "initialize", map[string]any{"protocolVersion": "2026-07-28"}, "", "")
	if sid := w.Header().Get(mcpSessionHeader); sid != "" {
		t.Errorf("all-stateless deployment must not mint Mcp-Session-Id, got %q", sid)
	}
	if got := w.Header().Get(mcpProtocolVersionHeader); got != config.ProtocolVersionStateless {
		t.Errorf("MCP-Protocol-Version response header = %q, want %q", got, config.ProtocolVersionStateless)
	}
	result := decodeResult(t, w)["result"].(map[string]any)
	if got := result["protocolVersion"]; got != config.ProtocolVersionStateless {
		t.Errorf("initialize protocolVersion = %v, want %q", got, config.ProtocolVersionStateless)
	}
}

// Even one stateful backend must keep the client-facing session: a stateful
// backend's lazily-opened session is keyed off the client-facing proxySID.
func TestInitializeStaysStatefulWithMixedBackends(t *testing.T) {
	statelessFB := newStatelessFakeBackend(t, "grafana-stateless")
	statefulFB := newFakeBackend(t, "grafana-stateful")
	h := harness(t, []config.BackendConfig{
		statelessBackendCfg(statelessFB, "grafana-stateless"),
		{Name: "grafana-stateful", URL: statefulFB.url(), Tenants: []config.TenantConfig{
			{ID: "grafana-stateful-team", Groups: []string{"grafana-stateful-team"}},
		}},
	})

	w := rpc(h, "initialize", map[string]any{"protocolVersion": "2025-03-26"}, "", "")
	if sid := w.Header().Get(mcpSessionHeader); sid == "" {
		t.Error("a mixed deployment (any stateful backend) must still mint Mcp-Session-Id")
	}
	if got := w.Header().Get(mcpProtocolVersionHeader); got != "" {
		t.Errorf("stateful/mixed deployment must not stamp MCP-Protocol-Version, got %q", got)
	}
	result := decodeResult(t, w)["result"].(map[string]any)
	if got := result["protocolVersion"]; got != config.ProtocolVersionStateful {
		t.Errorf("initialize protocolVersion = %v, want %q", got, config.ProtocolVersionStateful)
	}
}

// A stateless-aware client should be able to skip initialize entirely on an
// all-stateless deployment and go straight to tools/call.
func TestToolsCallSkipsInitializeOnAllStatelessBackends(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	h := harness(t, []config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})

	// No prior "initialize" call, no session id at all.
	w := rpc(h, "tools/call", map[string]any{
		"name": "query_prometheus", "arguments": map[string]any{"query": "up"},
	}, token(t, "grafana-stateless-team"), "")

	resp := decodeResult(t, w)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected error calling tools/call without initialize: %s", w.Body.String())
	}
	if fb.initializeEverSeen() {
		t.Error("backend must not see initialize either")
	}
}

// DELETE is a no-op on an all-stateless deployment: there is no client-facing
// session to tear down.
func TestDeleteNoOpOnAllStatelessBackends(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	h := harness(t, []config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})

	req, _ := http.NewRequest(http.MethodDelete, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE on all-stateless deployment = %d, want 204", w.Code)
	}
}

// The optional 2026-07-28 "server/discover" RPC answers with a DiscoverResult
// (SEP-2575) — a different required shape than initialize's InitializeResult
// (no protocolVersion/serverInfo; cacheScope/resultType/supportedVersions/
// ttlMs instead) — without any session side effect, in either mode.
func TestServerDiscover(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	h := harness(t, []config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})

	w := rpc(h, "server/discover", nil, "", "")
	if sid := w.Header().Get(mcpSessionHeader); sid != "" {
		t.Error("server/discover must never mint a session")
	}
	result := decodeResult(t, w)["result"].(map[string]any)
	if got := result["cacheScope"]; got != "public" {
		t.Errorf("server/discover cacheScope = %v, want %q", got, "public")
	}
	if got := result["resultType"]; got != "complete" {
		t.Errorf("server/discover resultType = %v, want %q", got, "complete")
	}
	versions, ok := result["supportedVersions"].([]any)
	if !ok || len(versions) != 1 || versions[0] != config.ProtocolVersionStateless {
		t.Errorf("server/discover supportedVersions = %v, want [%q]", result["supportedVersions"], config.ProtocolVersionStateless)
	}
	if _, ok := result["ttlMs"]; !ok {
		t.Error("server/discover result missing required ttlMs field")
	}
	if _, present := result["protocolVersion"]; present {
		t.Error("server/discover result must not carry protocolVersion — that's an initialize-only field")
	}
}

// A backend configured with protocol_version: 2026-07-28 never sees an
// initialize handshake or a session id: every request is self-contained.
func statelessBackendCfg(fb *fakeBackend, name string) config.BackendConfig {
	return config.BackendConfig{
		Name: name, URL: fb.url(), Credential: name + "-sa",
		ProtocolVersion: config.ProtocolVersionStateless,
		Tenants: []config.TenantConfig{
			{ID: name + "-team", Groups: []string{name + "-team"}},
		},
	}
}

func TestCatalogFetchStatelessBackendSkipsInitialize(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	reg, err := registry.New([]config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})
	if err != nil {
		t.Fatal(err)
	}
	cat := NewCatalog(reg.ReferenceBackend(), reg.ReferenceCredential(), time.Minute)

	tools, err := cat.get(context.Background())
	if err != nil {
		t.Fatalf("catalog fetch: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected tools from stateless backend")
	}
	if fb.initializeEverSeen() {
		t.Error("stateless backend must never receive an initialize request")
	}
	if fb.sessionIDEverSeen() {
		t.Error("stateless backend must never receive Mcp-Session-Id")
	}
	if got := fb.header(mcpProtocolVersionHeader); got != config.ProtocolVersionStateless {
		t.Errorf("MCP-Protocol-Version header = %q, want %q", got, config.ProtocolVersionStateless)
	}
	if got := fb.header(mcpMethodHeader); got != "tools/list" {
		t.Errorf("Mcp-Method header = %q, want tools/list", got)
	}
}

func TestToolsCallStatelessBackendSkipsSession(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	cfg := statelessBackendCfg(fb, "grafana-stateless")
	h := harness(t, []config.BackendConfig{cfg})
	sid := initSession(h)

	w := rpc(h, "tools/call", map[string]any{
		"name": "query_prometheus", "arguments": map[string]any{"query": "up"},
	}, token(t, "grafana-stateless-team"), sid)

	resp := decodeResult(t, w)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected error: %s", w.Body.String())
	}
	if fb.initializeEverSeen() {
		t.Error("stateless backend must never receive an initialize request")
	}
	if fb.sessionIDEverSeen() {
		t.Error("stateless backend must never receive Mcp-Session-Id")
	}
	if fb.lastAuth != "Bearer grafana-stateless-sa" {
		t.Errorf("backend should receive its SA token, got %q", fb.lastAuth)
	}
	if got := fb.header(mcpProtocolVersionHeader); got != config.ProtocolVersionStateless {
		t.Errorf("MCP-Protocol-Version header = %q, want %q", got, config.ProtocolVersionStateless)
	}
	if got := fb.header(mcpMethodHeader); got != "tools/call" {
		t.Errorf("Mcp-Method header = %q, want tools/call", got)
	}
	if got := fb.header(mcpNameHeader); got != "query_prometheus" {
		t.Errorf("Mcp-Name header = %q, want query_prometheus", got)
	}
	// Must match go-sdk's namespaced MetaKey* constants exactly (SEP-2575) —
	// github-mcp-server reads these bare, unprefixed keys back out of
	// params["_meta"] and silently treats an unnamespaced key as absent.
	if !strings.Contains(string(fb.params()), `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`) {
		t.Errorf("stateless request _meta should carry the namespaced protocolVersion key, got %s", fb.params())
	}
	if !strings.Contains(string(fb.params()), `"io.modelcontextprotocol/clientInfo"`) {
		t.Errorf("stateless request _meta should carry the namespaced clientInfo key, got %s", fb.params())
	}
	if !strings.Contains(string(fb.params()), `"io.modelcontextprotocol/clientCapabilities"`) {
		t.Errorf("stateless request _meta should carry the namespaced clientCapabilities key, got %s", fb.params())
	}
}

// A second tools/call to the same tenant on a stateless backend must not reuse
// any backend session bookkeeping — there is none to reuse.
func TestToolsCallStatelessBackendNoSessionReuse(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	cfg := statelessBackendCfg(fb, "grafana-stateless")
	h := harness(t, []config.BackendConfig{cfg})
	sid := initSession(h)
	tok := token(t, "grafana-stateless-team")

	rpc(h, "tools/call", map[string]any{"name": "query_prometheus"}, tok, sid)
	rpc(h, "tools/call", map[string]any{"name": "query_prometheus"}, tok, sid)

	if fb.requestCount() != 2 {
		t.Errorf("expected 2 independent stateless requests, got %d", fb.requestCount())
	}
	if fb.sessionCount() != 0 {
		t.Errorf("stateless backend must never issue/track a session, got %d sessions", fb.sessionCount())
	}
}

// --- SEP-2575 "resultType" (and friends) on every locally-synthesized Result ---
//
// A 2026-07-28-negotiating client (e.g. hermes-agent's mcp==2.0.0 SDK, once it
// falls back from initialize to server/discover) validates every subsequent
// response against the strict v2026_07_28 Result models, which require
// "resultType" on every result and additionally "cacheScope"/"ttlMs" on
// ListToolsResult — verified directly against modelcontextprotocol's real
// mcp_types package, not guessed. A response missing these is a client-side
// pydantic ValidationError, not a graceful ignore.

func TestToolsListStatelessIncludesRequiredFields(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	h := harness(t, []config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})

	w := rpc(h, "tools/list", nil, token(t, "grafana-stateless-team"), "")
	result := decodeResult(t, w)["result"].(map[string]any)
	if got := result["resultType"]; got != "complete" {
		t.Errorf("tools/list resultType = %v, want %q", got, "complete")
	}
	if got := result["cacheScope"]; got != "private" {
		t.Errorf("tools/list cacheScope = %v, want %q", got, "private")
	}
	if _, ok := result["ttlMs"]; !ok {
		t.Error("tools/list result missing required ttlMs field")
	}
}

func TestToolsListStatefulOmitsSEP2575Fields(t *testing.T) {
	fb := newFakeBackend(t, "grafana-stateful")
	h := harness(t, []config.BackendConfig{{
		Name: "grafana-stateful", URL: fb.url(),
		Tenants: []config.TenantConfig{{ID: "team-a", Groups: []string{"team-a"}}},
	}})
	sid := initSession(h)

	w := rpc(h, "tools/list", nil, token(t, "team-a"), sid)
	result := decodeResult(t, w)["result"].(map[string]any)
	if _, present := result["resultType"]; present {
		t.Error("a legacy (2025-03-26) deployment must not stamp resultType on tools/list")
	}
}

func TestListInstancesStatelessIncludesResultType(t *testing.T) {
	fb := newStatelessFakeBackend(t, "grafana-stateless")
	h := harness(t, []config.BackendConfig{statelessBackendCfg(fb, "grafana-stateless")})

	w := rpc(h, "tools/call", map[string]any{"name": listInstancesTool, "arguments": map[string]any{}},
		token(t, "grafana-stateless-team"), "")
	result := decodeResult(t, w)["result"].(map[string]any)
	if got := result["resultType"]; got != "complete" {
		t.Errorf("list_instances resultType = %v, want %q", got, "complete")
	}
}
