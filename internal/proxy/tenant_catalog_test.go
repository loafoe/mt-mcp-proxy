// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/loafoe/mt-mcp-proxy/internal/registry"
)

// One github-mcp-server-like backend serving two tenants whose headers narrow
// the tool set differently: "internal" is read-only (no write_tool),
// "cfil" lacks the actions toolset (no actions_tool).
func readonlySplitBackend(t *testing.T) (*fakeBackend, []config.BackendConfig) {
	t.Helper()
	fb := newStatelessFakeBackend(t, "github", "read_tool", "write_tool", "actions_tool")
	fb.hiddenWhen = map[string][]string{
		"X-Mcp-Readonly: true":    {"write_tool"},
		"X-Mcp-Toolsets: default": {"actions_tool"},
	}
	cfgs := []config.BackendConfig{{
		Name: "github", URL: fb.url(), ProtocolVersion: config.ProtocolVersionStateless,
		Tenants: []config.TenantConfig{
			{ID: "internal", Groups: []string{"internal"}, Credential: "pat-internal",
				Headers: map[string]string{"X-MCP-Readonly": "true"}},
			{ID: "cfil", Groups: []string{"cfil"}, Credential: "pat-cfil",
				Headers: map[string]string{"X-MCP-Toolsets": "default"}},
		},
	}}
	return fb, cfgs
}

func toolTenantEnum(t *testing.T, tool map[string]any) ([]string, bool) {
	t.Helper()
	sch, _ := tool["inputSchema"].(map[string]any)
	props, _ := sch["properties"].(map[string]any)
	ta, ok := props[tenantArg].(map[string]any)
	if !ok {
		return nil, false
	}
	var ids []string
	for _, v := range ta["enum"].([]any) {
		ids = append(ids, v.(string))
	}
	required := false
	reqList, _ := sch["required"].([]any)
	for _, r := range reqList {
		if r == tenantArg {
			required = true
		}
	}
	return ids, required
}

func callResult(t *testing.T, h *Handler, tok, tool string, args map[string]any) map[string]any {
	t.Helper()
	w := rpc(h, "tools/call", map[string]any{"name": tool, "arguments": args}, tok, "")
	resp := decodeResult(t, w)
	if e, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected JSON-RPC error: %v", e)
	}
	return resp["result"].(map[string]any)
}

func resultText(r map[string]any) string {
	content, _ := r["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	return content[0].(map[string]any)["text"].(string)
}

// Each caller sees only the tools their own tenants can run — fetched with
// that tenant's credential and headers, not one shared reference catalog.
func TestToolsListIsPerTenant(t *testing.T) {
	_, cfgs := readonlySplitBackend(t)
	h := harness(t, cfgs)

	cases := map[string]struct {
		groups []string
		want   []string
	}{
		"read-only tenant": {[]string{"internal"}, []string{"read_tool", "actions_tool", listInstancesTool}},
		"write tenant":     {[]string{"cfil"}, []string{"read_tool", "write_tool", listInstancesTool}},
		"both (union)":     {[]string{"internal", "cfil"}, []string{"read_tool", "write_tool", "actions_tool", listInstancesTool}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := toolNames(t, rpc(h, "tools/list", nil, token(t, tc.groups...), ""))
			if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(tc.want), ",") {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

// For a caller with several tenants, a tool's tenant selector only offers the
// tenants that actually have it: required when more than one does, optional
// (single value) when only one does.
func TestToolsListTenantSelectorIsPerTool(t *testing.T) {
	_, cfgs := readonlySplitBackend(t)
	h := harness(t, cfgs)
	w := rpc(h, "tools/list", nil, token(t, "internal", "cfil"), "")

	for tool, want := range map[string]struct {
		ids      string
		required bool
	}{
		"read_tool":    {"cfil,internal", true},
		"write_tool":   {"cfil", false},
		"actions_tool": {"internal", false},
	} {
		ids, required := toolTenantEnum(t, findTool(t, w, tool))
		if strings.Join(ids, ",") != want.ids || required != want.required {
			t.Errorf("%s: tenant enum %v required=%v, want %s required=%v", tool, ids, required, want.ids, want.required)
		}
	}
}

// A single-tenant caller keeps the clean passthrough: no tenant selector.
func TestToolsListSingleTenantNoSelector(t *testing.T) {
	_, cfgs := readonlySplitBackend(t)
	h := harness(t, cfgs)
	w := rpc(h, "tools/list", nil, token(t, "cfil"), "")
	if _, ok := toolTenantEnum(t, findTool(t, w, "write_tool")); ok {
		t.Error("single-tenant caller must not get a tenant selector")
	}
}

// Without a tenant argument, a tool only one of the caller's tenants has is
// routed to that tenant (with its own credential and headers).
func TestToolsCallAutoSelectsOnlyTenantWithTool(t *testing.T) {
	fb, cfgs := readonlySplitBackend(t)
	h := harness(t, cfgs)
	tok := token(t, "internal", "cfil")
	rpc(h, "tools/list", nil, tok, "")

	r := callResult(t, h, tok, "write_tool", map[string]any{"query": "x"})
	if r["isError"] == true {
		t.Fatalf("write_tool should auto-route to cfil, got error: %s", resultText(r))
	}
	if got := fb.lastToolCallAuth(); got != "Bearer pat-cfil" {
		t.Errorf("backend credential = %q, want the cfil PAT", got)
	}
	if got := fb.header("X-MCP-Toolsets"); got != "default" {
		t.Errorf("cfil tenant header not applied: X-MCP-Toolsets = %q", got)
	}

	// A tool both tenants have still needs an explicit choice.
	r = callResult(t, h, tok, "read_tool", map[string]any{"query": "x"})
	if r["isError"] != true || !strings.Contains(resultText(r), "cfil, internal") {
		t.Errorf("read_tool without tenant should ask to choose between cfil and internal, got %q", resultText(r))
	}
}

// Asking a tenant for a tool it does not have fails in the proxy with a clear
// message naming where the tool is available — the backend is never called.
func TestToolsCallRejectsToolMissingFromTenant(t *testing.T) {
	fb, cfgs := readonlySplitBackend(t)
	h := harness(t, cfgs)

	for name, tc := range map[string]struct {
		groups []string
		args   map[string]any
	}{
		"explicit tenant":      {[]string{"internal", "cfil"}, map[string]any{"query": "x", "tenant": "internal"}},
		"single-tenant caller": {[]string{"internal"}, map[string]any{"query": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			tok := token(t, tc.groups...)
			rpc(h, "tools/list", nil, tok, "")
			r := callResult(t, h, tok, "write_tool", tc.args)
			msg := resultText(r)
			if r["isError"] != true || !strings.Contains(msg, `not available for tenant "internal"`) {
				t.Errorf("want a not-available error for internal, got %q", msg)
			}
			if fb.lastToolCallAuth() != "" {
				t.Error("the backend must not be called for a tool the tenant does not have")
			}
		})
	}

	// The available-on hint only names tenants the caller may use.
	tok := token(t, "internal")
	r := callResult(t, h, tok, "write_tool", map[string]any{"query": "x"})
	if strings.Contains(resultText(r), "cfil") {
		t.Errorf("error must not reveal tenants the caller is not authorized for: %q", resultText(r))
	}
}

// Tenants that see the backend identically (same backend, credential and
// headers) share one catalog fetch; tenants whose headers differ get their own.
func TestCatalogsShareIdenticalTenantViews(t *testing.T) {
	ab, c, cfgs := twoBackends(t)
	cfgs[0].Tenants = append(cfgs[0].Tenants, config.TenantConfig{ID: "team-a2", Groups: []string{"team-a2"},
		Headers: map[string]string{"X-Grafana-Org-Id": "1"}})
	h := harness(t, cfgs)
	if err := h.WarmCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	// grafana-ab: team-a and team-a2 share org 1, team-b is org 2 -> 2 fetches.
	if got := ab.toolsListCount(); got != 2 {
		t.Errorf("grafana-ab tools/list fetches = %d, want 2 (team-a/team-a2 shared, team-b separate)", got)
	}
	if got := c.toolsListCount(); got != 1 {
		t.Errorf("grafana-c tools/list fetches = %d, want 1", got)
	}
}

// One unreachable backend must not take tools/list down for a caller who can
// also use a healthy one.
func TestToolsListSkipsUnavailableTenant(t *testing.T) {
	ab, c, cfgs := twoBackends(t)
	_ = ab
	c.server.Close()
	h := harness(t, cfgs)

	w := rpc(h, "tools/list", nil, token(t, "team-a", "team-c"), "")
	resp := decodeResult(t, w)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("tools/list must still succeed with one backend down: %s", w.Body.String())
	}
	if findTool(t, w, "query_prometheus") == nil {
		t.Error("tools from the healthy backend should still be listed")
	}

	// With every authorized tenant unavailable it is still an error.
	w = rpc(h, "tools/list", nil, token(t, "team-c"), "")
	if _, isErr := decodeResult(t, w)["error"]; !isErr {
		t.Errorf("tools/list with no reachable tenant should fail, got %s", w.Body.String())
	}
}

// Structured tool output (MCP outputSchema / structuredContent) passes
// through the proxy untouched: the tenant rewrite only edits inputSchema.
func TestStructuredOutputPassesThrough(t *testing.T) {
	fb := newStatelessFakeBackend(t, "github", "get_me")
	fb.outputSchemas = map[string]string{"get_me": `{"type":"object","properties":{"login":{"type":"string"}},"required":["login"]}`}
	fb.structured = map[string]string{"get_me": `{"login":"octocat"}`}
	cfgs := []config.BackendConfig{{
		Name: "github", URL: fb.url(), ProtocolVersion: config.ProtocolVersionStateless,
		Tenants: []config.TenantConfig{
			{ID: "a", Groups: []string{"a"}, Credential: "pat-a"},
			{ID: "b", Groups: []string{"b"}, Credential: "pat-b"},
		},
	}}
	h := harness(t, cfgs)
	tok := token(t, "a", "b")

	tool := findTool(t, rpc(h, "tools/list", nil, tok, ""), "get_me")
	if !sameJSON(t, tool["outputSchema"], fb.outputSchemas["get_me"]) {
		t.Errorf("outputSchema = %v, want %s", tool["outputSchema"], fb.outputSchemas["get_me"])
	}
	r := callResult(t, h, tok, "get_me", map[string]any{"tenant": "a"})
	if !sameJSON(t, r["structuredContent"], fb.structured["get_me"]) {
		t.Errorf("structuredContent = %v, want %s", r["structuredContent"], fb.structured["get_me"])
	}
}

// sameJSON reports whether a decoded value equals the JSON document want.
func sameJSON(t *testing.T, got any, want string) bool {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(got, w)
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// newTestCatalogs builds the per-tenant catalog set the handler uses.
func newTestCatalogs(t *testing.T, reg *registry.Registry) *Catalogs {
	t.Helper()
	return NewCatalogs(reg, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
