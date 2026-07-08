// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"errors"
	"testing"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
)

func testBackends() []config.BackendConfig {
	return []config.BackendConfig{
		{Name: "grafana-ab", URL: "http://ab:8000/mcp", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}},
			{ID: "team-b", Groups: []string{"team-b"}},
		}},
		{Name: "grafana-c", URL: "http://c:8000/mcp", Tenants: []config.TenantConfig{
			{ID: "team-c", Groups: []string{"team-c"}},
		}},
	}
}

func TestResolveTenant(t *testing.T) {
	r, err := New(testBackends())
	if err != nil {
		t.Fatal(err)
	}
	tn, err := r.ResolveTenant("team-b")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tn.Backend.Name != "grafana-ab" {
		t.Errorf("team-b should map to grafana-ab, got %q", tn.Backend.Name)
	}
}

func TestResolveTenantUnknown(t *testing.T) {
	r, _ := New(testBackends())
	if _, err := r.ResolveTenant("nope"); !errors.Is(err, ErrUnknownTenant) {
		t.Errorf("got %v, want ErrUnknownTenant", err)
	}
}

func TestAuthorizedTenants(t *testing.T) {
	r, _ := New(testBackends())
	// A caller in team-a and team-c is authorized for two tenants on two backends.
	got := r.AuthorizedTenants([]string{"team-a", "team-c"})
	if len(got) != 2 {
		t.Fatalf("expected 2 authorized tenants, got %d", len(got))
	}
	// Sorted by id: team-a, team-c.
	if got[0].ID != "team-a" || got[1].ID != "team-c" {
		t.Errorf("unexpected tenants: %q, %q", got[0].ID, got[1].ID)
	}
}

func TestAuthorizedTenantsNone(t *testing.T) {
	r, _ := New(testBackends())
	if got := r.AuthorizedTenants([]string{"stranger"}); len(got) != 0 {
		t.Errorf("expected no tenants, got %d", len(got))
	}
}

func TestIsAuthorized(t *testing.T) {
	r, _ := New(testBackends())
	tn, _ := r.ResolveTenant("team-c")
	if !IsAuthorized(tn, []string{"team-c"}) {
		t.Error("team-c group should authorize team-c tenant")
	}
	if IsAuthorized(tn, []string{"team-a"}) {
		t.Error("team-a group must not authorize team-c tenant")
	}
}

func TestReferenceBackendIsFirst(t *testing.T) {
	r, _ := New(testBackends())
	if r.ReferenceBackend().Name != "grafana-ab" {
		t.Errorf("reference backend should be the first, got %q", r.ReferenceBackend().Name)
	}
}

func TestNewRejectsDuplicateTenant(t *testing.T) {
	_, err := New([]config.BackendConfig{
		{Name: "a", URL: "http://a/mcp", Tenants: []config.TenantConfig{{ID: "x", Groups: []string{"g"}}}},
		{Name: "b", URL: "http://b/mcp", Tenants: []config.TenantConfig{{ID: "x", Groups: []string{"g"}}}},
	})
	if err == nil {
		t.Fatal("expected duplicate tenant id error")
	}
}

func TestNewRejectsRelativeURL(t *testing.T) {
	_, err := New([]config.BackendConfig{
		{Name: "a", URL: "/mcp", Tenants: []config.TenantConfig{{ID: "x", Groups: []string{"g"}}}},
	})
	if err == nil {
		t.Fatal("expected error for relative url")
	}
}

func TestTwoTenantsSameBackend(t *testing.T) {
	r, _ := New(testBackends())
	a, _ := r.ResolveTenant("team-a")
	b, _ := r.ResolveTenant("team-b")
	if a.Backend != b.Backend {
		t.Error("team-a and team-b should resolve to the same backend instance")
	}
}
