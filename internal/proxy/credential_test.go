// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
)

// A tenant's own credential overrides the backend default and is injected as the
// downstream Authorization: Bearer header — the core multi-tenant github case,
// where each team maps to its own GitHub PAT behind one github-mcp-server.
func TestPerTenantCredentialOverridesBackendDefault(t *testing.T) {
	be := newFakeBackend(t, "github")
	cfgs := []config.BackendConfig{
		{Name: "github", URL: be.url(), Credential: "backend-default-pat", Tenants: []config.TenantConfig{
			{ID: "team-platform", Groups: []string{"platform-eng"}, Credential: "ghp_platform"},
			{ID: "team-fallback", Groups: []string{"data-eng"}}, // no tenant credential → backend default
		}},
	}
	h := harness(t, cfgs)
	sid := initSession(h)

	// Tenant with its own credential.
	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{"tenant": "team-platform"}}, token(t, "platform-eng"), sid)
	if got := be.header("Authorization"); got != "Bearer ghp_platform" {
		t.Errorf("team-platform should inject its own PAT, got %q", got)
	}

	// Tenant without a credential falls back to the backend default.
	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{"tenant": "team-fallback"}}, token(t, "data-eng"), sid)
	if got := be.header("Authorization"); got != "Bearer backend-default-pat" {
		t.Errorf("team-fallback should inject the backend default, got %q", got)
	}
}

// A backend may inject the credential into a custom header with no scheme prefix
// (API-key style), e.g. X-Api-Key: <token>.
func TestCustomCredentialHeaderAndEmptyScheme(t *testing.T) {
	be := newFakeBackend(t, "apikey-backend")
	emptyScheme := ""
	cfgs := []config.BackendConfig{
		{
			Name:             "apikey-backend",
			URL:              be.url(),
			CredentialHeader: "X-Api-Key",
			CredentialScheme: &emptyScheme,
			Tenants: []config.TenantConfig{
				{ID: "team-a", Groups: []string{"team-a"}, Credential: "raw-key-123"},
			},
		},
	}
	h := harness(t, cfgs)
	sid := initSession(h)

	rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{}}, token(t, "team-a"), sid)
	if got := be.header("X-Api-Key"); got != "raw-key-123" {
		t.Errorf("expected bare token in X-Api-Key, got %q", got)
	}
	if got := be.header("Authorization"); got != "" {
		t.Errorf("Authorization should be unset for a custom credential header, got %q", got)
	}
}

// A caller may never reach a tenant their groups don't authorize, even by naming
// it explicitly — no credential is injected and the call is refused.
func TestCrossTenantAccessDenied(t *testing.T) {
	be := newFakeBackend(t, "github")
	cfgs := []config.BackendConfig{
		{Name: "github", URL: be.url(), Tenants: []config.TenantConfig{
			{ID: "team-platform", Groups: []string{"platform-eng"}, Credential: "ghp_platform"},
			{ID: "team-secret", Groups: []string{"secret-eng"}, Credential: "ghp_secret"},
		}},
	}
	h := harness(t, cfgs)
	sid := initSession(h)

	// Caller is only in platform-eng but names team-secret.
	w := rpc(h, "tools/call", map[string]any{"name": "query_prometheus", "arguments": map[string]any{"tenant": "team-secret"}}, token(t, "platform-eng"), sid)
	resp := decodeResult(t, w)
	result, _ := resp["result"].(map[string]any)
	if result == nil || result["isError"] != true {
		t.Fatalf("expected an isError tool result for unauthorized tenant, got %v", resp)
	}
	// The secret tenant's credential must never have been sent downstream.
	if got := be.header("Authorization"); got == "Bearer ghp_secret" {
		t.Error("secret tenant credential leaked downstream on an unauthorized call")
	}
}
