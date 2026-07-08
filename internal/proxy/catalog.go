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
	ref *registry.Backend
	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	tools     []rawTool
	fetchedAt time.Time
}

// NewCatalog creates a tool-catalog cache backed by the reference backend.
func NewCatalog(ref *registry.Backend, ttl time.Duration) *catalog {
	return &catalog{ref: ref, ttl: ttl, now: time.Now}
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

// fetch opens a short-lived MCP session to the reference backend and lists tools.
// It authenticates with the backend's default credential so backends that gate
// every request on a credential (e.g. github-mcp-server) still serve the catalog.
func (c *catalog) fetch(ctx context.Context) ([]rawTool, error) {
	client := newBackendClient(c.ref)
	cred := c.ref.Credential
	sid, _, err := client.initialize(ctx, defaultInitParams, cred)
	if err != nil {
		return nil, err
	}
	if sid != "" {
		client.notifyInitialized(ctx, sid, cred)
	}
	return client.listTools(ctx, sid, cred)
}
