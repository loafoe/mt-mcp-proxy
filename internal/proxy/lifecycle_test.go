// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// initialize is answered locally, mints a proxySID, requires no JWT, and never
// touches a backend.
func TestInitializeIsLocalNoJWT(t *testing.T) {
	ab, c, cfgs := twoBackends(t)
	h := harness(t, cfgs)

	sid := initSession(h)
	if sid == "" {
		t.Fatal("initialize did not mint an Mcp-Session-Id")
	}
	if ab.sessionCount() != 0 || c.sessionCount() != 0 {
		t.Error("initialize must not open any backend session")
	}
}

// tools/list without a JWT must not 401; it returns the catalog plus the local
// tool, with an optional free-form tenant arg.
func TestToolsListNoJWTBaseline(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/list", nil, "", sid)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("tools/list must not require a JWT")
	}
	names := toolNames(t, w)
	if !contains(names, listInstancesTool) {
		t.Errorf("list_instances missing: %v", names)
	}
	qp := findTool(t, w, "query_prometheus")
	props := qp["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props[tenantArg]; !ok {
		t.Error("baseline tools/list should advertise an optional tenant arg")
	}
}

// Single-tenant caller: clean passthrough, no tenant arg injected.
func TestToolsListSingleTenantNoArg(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/list", nil, token(t, "team-c"), sid)
	qp := findTool(t, w, "query_prometheus")
	props := qp["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props[tenantArg]; ok {
		t.Error("single-tenant caller must NOT get a tenant arg")
	}
}

// Multi-tenant caller: required tenant enum of exactly the caller's tenants.
func TestToolsListMultiTenantRequiredEnum(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/list", nil, token(t, "team-a", "team-b"), sid)
	qp := findTool(t, w, "query_prometheus")
	schema := qp["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	tenantProp, ok := props[tenantArg].(map[string]any)
	if !ok {
		t.Fatal("multi-tenant caller must get a tenant arg")
	}
	enum, _ := tenantProp["enum"].([]any)
	if len(enum) != 2 {
		t.Fatalf("enum should list the caller's 2 tenants, got %v", enum)
	}
	req, _ := schema["required"].([]any)
	found := false
	for _, r := range req {
		if r == tenantArg {
			found = true
		}
	}
	if !found {
		t.Error("tenant arg must be required for multi-tenant callers")
	}
}

// Single-tenant caller routes implicitly to the right backend, with the SA
// token and tenant headers applied, and no tenant arg leaking downstream.
func TestToolsCallSingleTenantRoutes(t *testing.T) {
	ab, c, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/call", map[string]any{
		"name": "query_prometheus", "arguments": map[string]any{"query": "up"},
	}, token(t, "team-c"), sid)

	resp := decodeResult(t, w)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected error: %s", w.Body.String())
	}
	if c.sessionCount() != 1 {
		t.Errorf("call should open one session on grafana-c, got %d", c.sessionCount())
	}
	if ab.sessionCount() != 0 {
		t.Error("the non-target backend grafana-ab must not be touched")
	}
	if c.lastAuth != "Bearer c-sa" {
		t.Errorf("backend should receive its SA token, got %q", c.lastAuth)
	}
}

// Multi-tenant caller must select a tenant; omitting it yields a guiding error.
func TestToolsCallMultiTenantRequiresSelection(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/call", map[string]any{
		"name": "query_prometheus", "arguments": map[string]any{"query": "up"},
	}, token(t, "team-a", "team-b"), sid)

	resp := decodeResult(t, w)
	result, _ := resp["result"].(map[string]any)
	if result == nil || result["isError"] != true {
		t.Fatalf("expected isError result guiding tenant selection, got %s", w.Body.String())
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "team-a") || !strings.Contains(text, "team-b") {
		t.Errorf("error should list valid tenants, got %q", text)
	}
}

// Explicit tenant routing strips the tenant arg and applies per-tenant headers.
func TestToolsCallExplicitTenantStripAndHeaders(t *testing.T) {
	ab, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/call", map[string]any{
		"name":      "query_prometheus",
		"arguments": map[string]any{"query": "up", "tenant": "team-b"},
	}, token(t, "team-a", "team-b"), sid)

	if resp := decodeResult(t, w); resp["error"] != nil {
		t.Fatalf("unexpected error: %s", w.Body.String())
	}
	// tenant stripped from forwarded args
	if strings.Contains(string(ab.lastCallArgs), "tenant") {
		t.Errorf("tenant arg must be stripped before forwarding, got %s", ab.lastCallArgs)
	}
	if !strings.Contains(string(ab.lastCallArgs), "up") {
		t.Errorf("sibling args must survive, got %s", ab.lastCallArgs)
	}
	// per-tenant header for team-b is org 2
	if got := ab.header("X-Grafana-Org-Id"); got != "2" {
		t.Errorf("team-b tenant header X-Grafana-Org-Id=2 expected, got %q", got)
	}
}

// Unauthorized / unknown tenant yields a structured error listing valid choices.
func TestToolsCallUnauthorizedTenant(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	// team-c caller tries to target team-a (not authorized).
	w := rpc(h, "tools/call", map[string]any{
		"name":      "query_prometheus",
		"arguments": map[string]any{"tenant": "team-a"},
	}, token(t, "team-c"), sid)

	result := decodeResult(t, w)["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError, got %s", w.Body.String())
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "team-c") {
		t.Errorf("error should list the caller's valid tenant team-c, got %q", text)
	}
}

// tools/call without a JWT is rejected (auth required on execution).
func TestToolsCallRequiresJWT(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/call", map[string]any{"name": "query_prometheus"}, "", sid)
	if decodeResult(t, w)["error"] == nil {
		t.Error("tools/call without a JWT must error")
	}
}

// list_instances reflects only the caller's authorized tenants.
func TestListInstancesReflectsCaller(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	sid := initSession(h)

	w := rpc(h, "tools/call", map[string]any{"name": listInstancesTool}, token(t, "team-a"), sid)
	result := decodeResult(t, w)["result"].(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "team-a") {
		t.Errorf("should list team-a, got %q", text)
	}
	if strings.Contains(text, "team-c") {
		t.Errorf("must not list unauthorized team-c, got %q", text)
	}
}
