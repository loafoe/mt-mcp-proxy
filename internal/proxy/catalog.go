// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/registry"
)

// defaultInitParams is the initialize params the proxy uses when opening backend
// sessions on its own behalf (catalog fetch). Mirrors a generic MCP client.
var defaultInitParams = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mt-mcp-proxy","version":"0"}}`)

// catalog caches the reference backend's tool list. All backends are assumed to
// expose the identical catalog, so one fetch serves every caller.
type catalog struct {
	ref  *registry.Backend
	cred string // credential used to authenticate the catalog fetch
	ttl  time.Duration
	now  func() time.Time

	mu        sync.Mutex
	tools     []rawTool
	fetchedAt time.Time
}

// NewCatalog creates a tool-catalog cache backed by the reference backend,
// authenticating the fetch with cred (the backend default credential, or the
// reference tenant's credential when the backend has none).
func NewCatalog(ref *registry.Backend, cred string, ttl time.Duration) *catalog {
	return &catalog{ref: ref, cred: cred, ttl: ttl, now: time.Now}
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

// fetch lists tools from the reference backend. It authenticates with the
// backend's default credential so backends that gate every request on a
// credential (e.g. github-mcp-server) still serve the catalog.
//
// A stateful backend needs a short-lived MCP session opened first; a stateless
// backend (config.ProtocolVersionStateless) has no initialize handshake at all —
// tools/list is a self-contained request.
func (c *catalog) fetch(ctx context.Context) ([]rawTool, error) {
	client := newBackendClient(c.ref)
	cred := c.cred
	if c.ref.Stateless() {
		return client.listTools(ctx, "", cred)
	}
	sid, _, err := client.initialize(ctx, defaultInitParams, cred)
	if err != nil {
		return nil, err
	}
	if sid != "" {
		client.notifyInitialized(ctx, sid, cred)
	}
	return client.listTools(ctx, sid, cred)
}
