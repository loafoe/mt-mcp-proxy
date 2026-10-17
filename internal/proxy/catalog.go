// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/registry"
)

// defaultInitParams is the initialize params the proxy uses when opening backend
// sessions on its own behalf (catalog fetch). Mirrors a generic MCP client.
var defaultInitParams = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mt-mcp-proxy","version":"0"}}`)

// catalog caches one tenant view's tool list: what the backend returns to
// tools/list when called with that tenant's credential and headers. Backends
// such as github-mcp-server narrow the tool set per request (X-MCP-Readonly,
// X-MCP-Toolsets, PAT scopes), so tenants sharing a backend can see different
// tools.
type catalog struct {
	backend *registry.Backend
	cred    string            // credential used to authenticate the fetch
	headers map[string]string // tenant headers sent with the fetch
	ttl     time.Duration
	now     func() time.Time

	mu        sync.Mutex
	tools     []rawTool
	fetchedAt time.Time
}

func newCatalog(b *registry.Backend, cred string, headers map[string]string, ttl time.Duration) *catalog {
	return &catalog{backend: b, cred: cred, headers: headers, ttl: ttl, now: time.Now}
}

// Catalogs holds the tool catalog of every tenant. Tenants that see their
// backend identically (same backend, credential and headers, e.g. several
// Grafana tenants on one instance) share a single catalog and fetch.
type Catalogs struct {
	byTenant map[string]*catalog
	unique   []*catalog
	logger   *slog.Logger
}

// NewCatalogs builds one catalog per distinct tenant view in reg.
func NewCatalogs(reg *registry.Registry, ttl time.Duration, logger *slog.Logger) *Catalogs {
	cs := &Catalogs{byTenant: map[string]*catalog{}, logger: logger}
	byKey := map[string]*catalog{}
	tenants := reg.Tenants()
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
	for _, t := range tenants {
		key := viewKey(t)
		c, ok := byKey[key]
		if !ok {
			c = newCatalog(t.Backend, t.EffectiveCredential(), t.Headers, ttl)
			byKey[key] = c
			cs.unique = append(cs.unique, c)
		}
		cs.byTenant[t.ID] = c
	}
	return cs
}

// viewKey identifies what a tenant's tools/list depends on.
func viewKey(t *registry.Tenant) string {
	keys := make([]string, 0, len(t.Headers))
	for k := range t.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(t.Backend.Name + "\x00" + t.EffectiveCredential())
	for _, k := range keys {
		sb.WriteString("\x00" + http.CanonicalHeaderKey(k) + "=" + t.Headers[k])
	}
	return sb.String()
}

func (cs *Catalogs) forTenant(id string) *catalog {
	return cs.byTenant[id]
}

// warm fetches every catalog once, returning the joined errors of those that
// failed (already-fresh catalogs are not refetched, so retrying is cheap).
func (cs *Catalogs) warm(ctx context.Context) error {
	var errs []error
	for _, c := range cs.unique {
		if _, err := c.get(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// tenantTools is one tenant's tool list, as resolved for a tools/list call.
type tenantTools struct {
	tenant *registry.Tenant
	tools  []rawTool
}

// toolsFor returns the tool lists of the given tenants, in order, skipping
// (and logging) any whose catalog cannot be fetched. It fails only when none
// of them is available.
func (cs *Catalogs) toolsFor(ctx context.Context, tenants []*registry.Tenant) ([]tenantTools, error) {
	var out []tenantTools
	var errs []error
	for _, t := range tenants {
		c := cs.byTenant[t.ID]
		if c == nil {
			continue
		}
		tools, err := c.get(ctx)
		if err != nil {
			cs.logger.Warn("tenant catalog unavailable, omitting its tools", "tenant", t.ID, "backend", t.Backend.Name, "err", err)
			errs = append(errs, err)
			continue
		}
		out = append(out, tenantTools{tenant: t, tools: tools})
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// cachedTool returns the named tool from a tenant's cached catalog without
// fetching. known is false when that catalog has not been fetched yet, so the
// caller cannot tell whether the tool exists.
func (cs *Catalogs) cachedTool(tenantID, name string) (tool rawTool, found, known bool) {
	c := cs.byTenant[tenantID]
	if c == nil {
		return nil, false, false
	}
	tools, ok := c.cached()
	if !ok {
		return nil, false, false
	}
	for _, t := range tools {
		if toolName(t) == name {
			return t, true, true
		}
	}
	return nil, false, true
}

func toolName(t rawTool) string {
	var name string
	if raw, ok := t["name"]; ok {
		_ = json.Unmarshal(raw, &name)
	}
	return name
}

// tools returns the cached catalog, refreshing it from the reference backend if
// empty or stale.
func (c *catalog) get(ctx context.Context) ([]rawTool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tools != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.tools, nil
	}
	tools, err := c.fetch(ctx)
	if err != nil {
		// On refresh failure, serve a stale copy if we have one.
		if c.tools != nil {
			return c.tools, nil
		}
		return nil, err
	}
	c.tools = tools
	c.fetchedAt = c.now()
	return tools, nil
}

// cached returns the catalog's cached tools without triggering a fetch, and
// whether anything has been cached yet. Used where a fetch would be an
// unwanted side effect (e.g. computing SEP-2243 param headers for a
// tools/call that itself never touched tools/list) — the cache is populated
// by any prior tools/list call, real MCP clients always issue one before a
// tools/call, so this only misses on a client that skips straight to
// tools/call on a genuinely cold proxy.
func (c *catalog) cached() ([]rawTool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tools, c.tools != nil
}

// fetch lists tools as the tenant view sees them: authenticated with the
// tenant's effective credential (backends such as github-mcp-server gate
// every request on one) and carrying the tenant's headers.
//
// A stateful backend needs a short-lived MCP session opened first; a stateless
// backend (config.ProtocolVersionStateless) has no initialize handshake at all —
// tools/list is a self-contained request.
func (c *catalog) fetch(ctx context.Context) ([]rawTool, error) {
	client := newBackendClient(c.backend)
	cred := c.cred
	if c.backend.Stateless() {
		return client.listTools(ctx, "", cred, c.headers)
	}
	sid, _, err := client.initialize(ctx, defaultInitParams, cred)
	if err != nil {
		return nil, err
	}
	if sid != "" {
		client.notifyInitialized(ctx, sid, cred)
	}
	return client.listTools(ctx, sid, cred, c.headers)
}
