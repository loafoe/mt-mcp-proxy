// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/auth"
	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/loafoe/mt-mcp-proxy/internal/observability"
	"github.com/loafoe/mt-mcp-proxy/internal/registry"
	"github.com/loafoe/mt-mcp-proxy/internal/session"
	"go.opentelemetry.io/otel/attribute"
)

// listInstancesTool is the synthetic tool name for tenant discovery.
const listInstancesTool = "list_instances"

// tenantArg is the synthetic argument injected for multi-tenant callers.
const tenantArg = "tenant"

// Handler is the proxy's MCP endpoint. It is an MCP server toward the client and
// an MCP client toward backends. It answers initialize itself, serves a cached
// tools/list with a per-caller transform, and routes tools/call to the tenant's
// backend over a lazily-opened backend session.
type Handler struct {
	Verifier auth.Verifier
	Registry *registry.Registry
	Sessions *session.Store
	Catalog  *catalog
	Logger   *slog.Logger
	// Obs records metrics and traces. Nil-safe: all recording is skipped when nil.
	Obs *observability.Observability

	ServerCfg config.ServerConfig
	AuthCfg   config.AuthConfig

	// clientFor builds a backend client; overridable in tests.
	clientFor func(*registry.Backend) *backendClient
}

// NewHandler wires a Handler with sane defaults.
func NewHandler(v auth.Verifier, reg *registry.Registry, store *session.Store, cat *catalog, logger *slog.Logger, obs *observability.Observability, serverCfg config.ServerConfig, authCfg config.AuthConfig) *Handler {
	return &Handler{
		Verifier:  v,
		Registry:  reg,
		Sessions:  store,
		Catalog:   cat,
		Logger:    logger,
		Obs:       obs,
		ServerCfg: serverCfg,
		AuthCfg:   authCfg,
		clientFor: newBackendClient,
	}
}

func (h *Handler) backendClient(b *registry.Backend) *backendClient {
	c := newBackendClient(b)
	if h.clientFor != nil {
		c = h.clientFor(b)
	}
	// Give the client the proxy tracer so downstream calls are traced and the
	// trace context is propagated to the backend. Only override when Obs is set
	// so a test-supplied clientFor can install its own tracer.
	if h.Obs != nil {
		c.tracer = h.Obs.Tracer()
	}
	return c
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		h.handleDelete(w, r)
		return
	case http.MethodGet:
		// A client may open a listen stream. v1 does not multiplex
		// backend-initiated streams; accept and hold without backend fan-in.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
		// handled below
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeRPCError(w, nil, -32700, "failed to read request body")
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	switch req.Method {
	case "initialize":
		h.handleInitialize(w, r, &req)
	case "notifications/initialized":
		// Client handshake completion; proxy-local, nothing downstream.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		h.handleToolsList(w, r, &req)
	case "tools/call":
		h.handleToolsCall(w, r, &req)
	case "ping":
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSONRPCResult(w, req.ID, map[string]any{})
	default:
		// Unknown method: the proxy is the MCP server, so respond rather than
		// blindly forwarding (we don't know which backend it belongs to).
		if req.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// handleInitialize answers as the proxy itself and mints a proxySID. No JWT is
// required (discovery is decoupled from auth).
func (h *Handler) handleInitialize(w http.ResponseWriter, r *http.Request, req *jsonRPCRequest) {
	sess := h.Sessions.Mint()
	h.Obs.RecordSession(r.Context())
	result := map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "mt-mcp-proxy", "version": "0"},
	}
	w.Header().Set(mcpSessionHeader, sess.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = writeJSONRPCResult(w, req.ID, result)
}

// handleToolsList serves the cached catalog with the identity-hybrid transform.
func (h *Handler) handleToolsList(w http.ResponseWriter, r *http.Request, req *jsonRPCRequest) {
	tools, err := h.Catalog.get(r.Context())
	if err != nil {
		h.Logger.Error("catalog fetch failed", "err", err)
		writeRPCError(w, req.ID, -32603, "failed to load tool catalog")
		return
	}

	// Copy the catalog so per-caller mutation does not corrupt the cache.
	out := make([]rawTool, 0, len(tools)+1)
	for _, t := range tools {
		out = append(out, cloneTool(t))
	}

	// Determine the caller's authorized tenants (JWT optional on discovery).
	var tenantIDs []string
	if groups, err := h.Verifier.Groups(r); err == nil {
		for _, t := range h.Registry.AuthorizedTenants(groups) {
			tenantIDs = append(tenantIDs, t.ID)
		}
	}

	switch {
	case len(tenantIDs) >= 2:
		// Multi-tenant caller: inject a required enum selector.
		schema := tenantSchema(tenantIDs, true)
		for _, t := range out {
			injectTenantArg(t, schema, true)
		}
	case len(tenantIDs) == 1:
		// Single authorized tenant: clean passthrough, no selector.
	default:
		// No/invalid JWT (discovery): optional free-form selector.
		schema := tenantSchema(nil, false)
		for _, t := range out {
			injectTenantArg(t, schema, false)
		}
	}

	out = append(out, listInstancesToolDef())

	w.Header().Set("Content-Type", "application/json")
	_ = writeJSONRPCResult(w, req.ID, toolsListResult{Tools: out})
}

// handleToolsCall handles the local list_instances tool and tenant
// routing for stock tools. It opens a span over the operation and records the
// operation-duration/count metrics with the resolved tenant/backend and an
// error.type derived from the outcome.
func (h *Handler) handleToolsCall(w http.ResponseWriter, r *http.Request, req *jsonRPCRequest) {
	ctx, span := h.Obs.Tracer().Start(r.Context(), "mcp.tools/call")
	defer span.End()
	r = r.WithContext(ctx)

	start := time.Now()
	out := observability.ToolCallAttrs{Method: "tools/call"}
	defer func() {
		h.Obs.RecordToolCall(ctx, out, time.Since(start).Seconds())
	}()

	groups, err := h.Verifier.Groups(r)
	if err != nil {
		if errors.Is(err, auth.ErrInsufficientScope) {
			out.ErrorType = "insufficient_scope"
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer error="insufficient_scope", scope="%s", resource_metadata="%s", error_description="insufficient scope"`,
				strings.Join(h.AuthCfg.ScopesSupported, " "),
				h.metadataURL(r),
			))
			writeHTTPError(w, http.StatusForbidden, req.ID, -32001, "forbidden: insufficient scope")
			return
		}
		if errors.Is(err, auth.ErrUnauthorized) {
			out.ErrorType = "unauthorized"
			authHeader := fmt.Sprintf(`Bearer resource_metadata="%s"`, h.metadataURL(r))
			if len(h.AuthCfg.ScopesSupported) > 0 {
				authHeader = fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, h.metadataURL(r), strings.Join(h.AuthCfg.ScopesSupported, " "))
			}
			w.Header().Set("WWW-Authenticate", authHeader)
			writeHTTPError(w, http.StatusUnauthorized, req.ID, -32001, "unauthorized: a valid token is required for tool calls")
			return
		}
		out.ErrorType = "auth_error"
		h.Logger.Error("auth error", "err", err)
		writeHTTPError(w, http.StatusInternalServerError, req.ID, -32603, "internal error")
		return
	}

	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		out.ErrorType = "invalid_params"
		writeRPCError(w, req.ID, -32602, "invalid tools/call params")
		return
	}
	out.Tool = params.Name
	span.SetAttributes(attribute.String("mcp.tool.name", params.Name))

	authorized := h.Registry.AuthorizedTenants(groups)

	// Local tool: list_instances.
	if params.Name == listInstancesTool {
		h.writeInstances(w, req.ID, authorized)
		return
	}

	// Resolve the target tenant.
	tenant, errResult := h.selectTenant(params.Arguments, groups, authorized)
	if errResult != "" {
		out.ErrorType = "tenant_selection"
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSONRPCResult(w, req.ID, toolResultText(errResult, true))
		return
	}
	out.TenantID = tenant.ID
	out.Backend = tenant.Backend.Name
	span.SetAttributes(
		attribute.String("mcp.tenant.id", tenant.ID),
		attribute.String("mcp.backend.name", tenant.Backend.Name),
	)

	// Strip the synthetic tenant argument before forwarding.
	cleanParams, err := stripTenantArg(req.Params)
	if err != nil {
		out.ErrorType = "invalid_params"
		writeRPCError(w, req.ID, -32602, "invalid arguments")
		return
	}
	forwardReq := jsonRPCRequest{JSONRPC: "2.0", Method: "tools/call", Params: cleanParams, ID: req.ID}
	forwardBytes, _ := json.Marshal(forwardReq)

	if !h.routeToBackend(w, r, req.ID, tenant, forwardBytes) {
		out.ErrorType = "backend_error"
	}
}

// selectTenant picks the tenant for a call. Returns an error message (to be sent
// as an isError tool result) or a tenant.
func (h *Handler) selectTenant(args json.RawMessage, groups []string, authorized []*registry.Tenant) (*registry.Tenant, string) {
	explicit := extractTenantArg(args)

	if explicit != "" {
		tenant, err := h.Registry.ResolveTenant(explicit)
		if err != nil {
			return nil, fmt.Sprintf("unknown tenant %q; choose one of: %s", explicit, tenantList(authorized))
		}
		if !registry.IsAuthorized(tenant, groups) {
			return nil, fmt.Sprintf("not authorized for tenant %q; choose one of: %s", explicit, tenantList(authorized))
		}
		return tenant, ""
	}

	switch len(authorized) {
	case 0:
		return nil, "no instances are authorized for your token"
	case 1:
		return authorized[0], ""
	default:
		return nil, fmt.Sprintf("multiple instances available; pass the %q argument, one of: %s", tenantArg, tenantList(authorized))
	}
}

// routeToBackend forwards a prepared tools/call to the tenant's backend over a
// lazily-opened backend session, applying per-request tenant headers. It reports
// whether the call succeeded (false on any backend/session failure).
func (h *Handler) routeToBackend(w http.ResponseWriter, r *http.Request, id json.RawMessage, tenant *registry.Tenant, forwardBytes []byte) bool {
	proxySID := r.Header.Get(mcpSessionHeader)
	sess, ok := h.Sessions.Get(proxySID)
	if !ok {
		writeRPCError(w, id, -32000, "session not found; call initialize first")
		return false
	}

	client := h.backendClient(tenant.Backend)
	cred := tenant.EffectiveCredential()
	// Key the backend session by tenant ID, not backend name: each tenant carries
	// its own downstream credential, so two tenants sharing a backend must not
	// share a session initialized with one tenant's credential.
	backendSID, have := sess.BackendSID(tenant.ID)
	if !have {
		// Lazily open a backend MCP session for this tenant, authenticated with
		// the tenant's effective credential.
		sid, _, err := client.initialize(r.Context(), defaultInitParams, cred)
		if err != nil {
			h.Logger.Error("backend initialize failed", "backend", tenant.Backend.Name, "err", err)
			writeRPCError(w, id, -32002, "failed to open backend session")
			return false
		}
		if sid != "" {
			client.notifyInitialized(r.Context(), sid, cred)
		}
		backendSID = sid
		h.Sessions.SetBackendSID(proxySID, tenant.ID, backendSID)
	}

	respBytes, err := client.callTool(r.Context(), backendSID, cred, forwardBytes, tenant.Headers)
	if err != nil {
		h.Logger.Error("backend tools/call failed", "backend", tenant.Backend.Name, "err", err)
		writeRPCError(w, id, -32003, "backend call failed")
		return false
	}

	h.Logger.Debug("routed tool call", "tenant", tenant.ID, "backend", tenant.Backend.Name)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBytes)
	return true
}

// handleDelete tears down the proxySID and its backend sessions.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	proxySID := r.Header.Get(mcpSessionHeader)
	if sess, ok := h.Sessions.Delete(proxySID); ok {
		// Backend sessions are keyed by tenant ID (see routeToBackend). Tear down
		// each tenant's downstream session with that tenant's credential.
		for _, t := range h.Registry.Tenants() {
			if bsid, have := sess.BackendSID(t.ID); have && bsid != "" {
				go h.teardownBackend(t, bsid)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) teardownBackend(t *registry.Tenant, backendSID string) {
	client := h.backendClient(t.Backend)
	req, err := http.NewRequest(http.MethodDelete, t.Backend.URL.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set(mcpSessionHeader, backendSID)
	client.applyAuth(req, t.EffectiveCredential(), nil)
	if resp, err := client.http.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func (h *Handler) writeInstances(w http.ResponseWriter, id json.RawMessage, tenants []*registry.Tenant) {
	var sb strings.Builder
	if len(tenants) == 0 {
		sb.WriteString("You are not authorized to access any instances.")
	} else {
		sb.WriteString("You are authorized to access the following instances (tenants):\n")
		for _, t := range tenants {
			fmt.Fprintf(&sb, "- %s (authorized via groups: %s)\n", t.ID, strings.Join(t.Groups, ", "))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = writeJSONRPCResult(w, id, toolResultText(sb.String(), false))
}

func tenantList(tenants []*registry.Tenant) string {
	ids := make([]string, 0, len(tenants))
	for _, t := range tenants {
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonRPCErrorBytes(id, code, msg))
}
