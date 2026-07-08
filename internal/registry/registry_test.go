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

// ReferenceCredential falls back to the reference backend's first tenant when
// the backend has no default credential (per-tenant-only creds, e.g. github
// PATs). Without this the catalog fetch would 401 on backends that gate every
// request on a credential.
func TestReferenceCredentialFallsBackToFirstTenant(t *testing.T) {
	r, err := New([]config.BackendConfig{
		{Name: "github", URL: "http://gh:8082/", Tenants: []config.TenantConfig{
			{ID: "team-platform", Groups: []string{"platform-eng"}, Credential: "ghp_platform"},
			{ID: "team-data", Groups: []string{"data-eng"}, Credential: "ghp_data"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.ReferenceCredential(); got != "ghp_platform" {
		t.Errorf("expected first tenant credential ghp_platform, got %q", got)
	}
}

// A backend-level default credential takes precedence over tenant credentials
// for the catalog fetch.
func TestReferenceCredentialPrefersBackendDefault(t *testing.T) {
	r, err := New([]config.BackendConfig{
		{Name: "grafana", URL: "http://g:8000/mcp", Credential: "backend-sa", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}, Credential: "tenant-a"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.ReferenceCredential(); got != "backend-sa" {
		t.Errorf("expected backend default backend-sa, got %q", got)
	}
}
