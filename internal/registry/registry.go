// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

// Package registry indexes tenants and the backends that serve them.
//
// The model is: groups authorize, tenant selects, backend executes. A JWT's
// groups determine which tenants a caller may use; a tenant resolves to exactly
// one backend.
package registry

import (
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
)

// Backend is a resolved downstream MCP server (streamable-HTTP).
type Backend struct {
	Name string
	URL  *url.URL
	// Headers are static headers injected toward this backend.
	Headers map[string]string
	// Credential is the backend-level default bearer token, injected as
	// "<CredentialHeader>: <CredentialScheme> <Credential>". A tenant's own
	// Credential overrides it.
	Credential string
	// CredentialHeader is the header the credential is injected into (default
	// "Authorization").
	CredentialHeader string
	// CredentialScheme is the scheme prefix for the credential value (default
	// "Bearer"; may be empty for a bare token).
	CredentialScheme string
}

// Tenant is a selectable unit served by a backend.
type Tenant struct {
	ID      string
	Groups  []string
	Headers map[string]string
	// Credential, when set, overrides the backend Credential for calls routed to
	// this tenant (the per-tenant downstream identity, e.g. a team's GitHub PAT).
	Credential string
	Backend    *Backend
}

// EffectiveCredential returns the credential used for calls to this tenant: the
// tenant's own when set, else the backend default.
func (t *Tenant) EffectiveCredential() string {
	if t.Credential != "" {
		return t.Credential
	}
	return t.Backend.Credential
}

// ErrUnknownTenant means the tenant id is not registered.
var ErrUnknownTenant = errors.New("unknown tenant")

// Registry is an immutable tenant index built at startup.
type Registry struct {
	byTenant map[string]*Tenant // tenant id -> tenant
	backends []*Backend         // in config order
	ref      *Backend           // reference backend for the tool catalog
}

// New builds a Registry from backend configs. Validation (uniqueness, >=1
// tenant/backend, >=1 group/tenant) is expected from config.Validate; we guard
// the routing-critical invariants here too.
func New(backends []config.BackendConfig) (*Registry, error) {
	r := &Registry{byTenant: make(map[string]*Tenant)}
	for _, bc := range backends {
		u, err := url.Parse(bc.URL)
		if err != nil {
			return nil, fmt.Errorf("backend %q: invalid url: %w", bc.Name, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("backend %q: url must be absolute, got %q", bc.Name, bc.URL)
		}
		scheme := "Bearer"
		if bc.CredentialScheme != nil {
			scheme = *bc.CredentialScheme
		}
		header := bc.CredentialHeader
		if header == "" {
			header = "Authorization"
		}
		b := &Backend{
			Name:             bc.Name,
			URL:              u,
			Headers:          bc.Headers,
			Credential:       bc.Credential,
			CredentialHeader: header,
			CredentialScheme: scheme,
		}
		r.backends = append(r.backends, b)
		if r.ref == nil {
			r.ref = b
		}
		for _, tc := range bc.Tenants {
			if _, dup := r.byTenant[tc.ID]; dup {
				return nil, fmt.Errorf("duplicate tenant id %q", tc.ID)
			}
			r.byTenant[tc.ID] = &Tenant{
				ID:         tc.ID,
				Groups:     tc.Groups,
				Headers:    tc.Headers,
				Credential: tc.Credential,
				Backend:    b,
			}
		}
	}
	if len(r.byTenant) == 0 {
		return nil, fmt.Errorf("no tenants configured")
	}
	return r, nil
}

// ReferenceBackend returns the first configured backend, used to fetch the
// (assumed identical) tool catalog.
func (r *Registry) ReferenceBackend() *Backend {
	return r.ref
}

// Backends returns all backends in config order.
func (r *Registry) Backends() []*Backend {
	return r.backends
}

// Tenants returns all registered tenants (unordered).
func (r *Registry) Tenants() []*Tenant {
	out := make([]*Tenant, 0, len(r.byTenant))
	for _, t := range r.byTenant {
		out = append(out, t)
	}
	return out
}

// ResolveTenant returns the tenant for an id, or ErrUnknownTenant.
func (r *Registry) ResolveTenant(id string) (*Tenant, error) {
	t, ok := r.byTenant[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTenant, id)
	}
	return t, nil
}

// AuthorizedTenants returns the tenants the supplied groups may select, sorted
// by tenant id for deterministic output. A tenant is authorized when its groups
// intersect the caller's groups.
func (r *Registry) AuthorizedTenants(groups []string) []*Tenant {
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}
	var out []*Tenant
	for _, t := range r.byTenant {
		for _, g := range t.Groups {
			if _, ok := groupSet[g]; ok {
				out = append(out, t)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// IsAuthorized reports whether the caller's groups may select tenant t.
func IsAuthorized(t *Tenant, groups []string) bool {
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}
	for _, g := range t.Groups {
		if _, ok := groupSet[g]; ok {
			return true
		}
	}
	return false
}
