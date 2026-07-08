// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
)

// Two tool calls on the same proxySID reuse one backend session.
func TestBackendSessionReused(t *testing.T) {
	_, c, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)
	tok := token(t, "team-c")

	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{}}, tok, sid)
	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{}}, tok, sid)

	if c.sessionCount() != 1 {
		t.Errorf("expected one reused backend session, got %d", c.sessionCount())
	}
	if got := c.callsOnSession("grafana-c-sess-1"); got != 2 {
		t.Errorf("expected 2 calls on the reused session, got %d", got)
	}
}

// Two tenants on the SAME backend each get their OWN backend session, so a
// per-tenant downstream credential is never shared across tenants. Each still
// carries its own per-tenant headers (per-request header application).
func TestSameBackendTwoTenantsIsolatedSessions(t *testing.T) {
	ab, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)
	tok := token(t, "team-a", "team-b")

	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{"tenant": "team-a"}}, tok, sid)
	if got := ab.header("X-Grafana-Org-Id"); got != "1" {
		t.Errorf("team-a should set org 1, got %q", got)
	}
	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{"tenant": "team-b"}}, tok, sid)
	if got := ab.header("X-Grafana-Org-Id"); got != "2" {
		t.Errorf("team-b should set org 2, got %q", got)
	}
	// One session per tenant: the two tenants must not share a downstream session
	// (each may carry a distinct per-tenant credential).
	if ab.sessionCount() != 2 {
		t.Errorf("each tenant should get its own backend session, got %d", ab.sessionCount())
	}
}

// An unknown/expired proxySID on tools/call yields "session not found".
func TestUnknownSessionRejected(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)

	w := rpc(h, "tools/call", map[string]any{"name": "query_prometheus"}, token(t, "team-c"), "bogus-sid")
	resp := decodeResult(t, w)
	if resp["error"] == nil {
		t.Fatal("expected an error for unknown session")
	}
	msg := resp["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "session not found") {
		t.Errorf("expected session-not-found, got %q", msg)
	}
}

// DELETE tears down the proxy session and the mapped backend session.
func TestDeleteTearsDownBackendSession(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)
	tok := token(t, "team-c")
	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{}}, tok, sid)

	req, _ := http.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(mcpSessionHeader, sid)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE should return 204, got %d", w.Code)
	}

	// A subsequent call on the same sid is now unknown.
	w2 := rpc(h, "tools/call", map[string]any{"name": "query_prometheus"}, tok, sid)
	if decodeResult(t, w2)["error"] == nil {
		t.Error("session should be gone after DELETE")
	}
}

// SSE-framed tools/list responses from the backend are still parsed and
// transformed (the catalog fetch is SSE-aware).
func TestCatalogHandlesSSEBackend(t *testing.T) {
	ab := newFakeBackend(t, "grafana-ab")
	ab.sse = true // backend answers tools/list as text/event-stream
	cfgs := []config.BackendConfig{
		{Name: "grafana-ab", URL: ab.url(), Credential: "ab-sa", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}},
		}},
	}
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/list", nil, token(t, "team-a"), sid)
	names := toolNames(t, w)
	if !contains(names, "query_prometheus") || !contains(names, listInstancesTool) {
		t.Errorf("SSE-framed catalog not parsed/transformed: %v", names)
	}
}
