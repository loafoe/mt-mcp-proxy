// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/loafoe/mt-mcp-proxy/internal/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// backendClient speaks MCP streamable-HTTP to a single backend.
type backendClient struct {
	backend *registry.Backend
	http    *http.Client
	tracer  trace.Tracer
	// stateless mirrors backend.Stateless(): the 2026-07-28 revision, where
	// every request is self-contained (no initialize handshake, no
	// Mcp-Session-Id — protocol version and routing travel as headers instead).
	stateless bool
}

func newBackendClient(b *registry.Backend) *backendClient {
	return &backendClient{
		backend: b,
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
		tracer:    noop.NewTracerProvider().Tracer("mt-mcp-proxy"),
		stateless: b.Stateless(),
	}
}

// applyAuth injects the downstream credential (per the backend's configured
// header/scheme) and the given per-request headers (tenant headers merged over
// backend headers). The client's JWT is never forwarded — callers build requests
// fresh, so nothing to strip. credential is the tenant's effective credential;
// when empty, no credential header is set.
func (c *backendClient) applyAuth(req *http.Request, credential string, extra map[string]string) {
	if credential != "" {
		value := credential
		if c.backend.CredentialScheme != "" {
			value = c.backend.CredentialScheme + " " + credential
		}
		req.Header.Set(c.backend.CredentialHeader, value)
	}
	for k, v := range c.backend.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
}

// post sends a JSON-RPC payload and returns the decoded first message plus the
// backend session id from the response (if any). credential is injected via
// applyAuth on every request. mcpMethod/mcpName are only used in stateless mode
// (see backendClient.stateless): they carry the routing headers that replace
// the session id, so a gateway can route/authorize without parsing the body.
func (c *backendClient) post(ctx context.Context, payload []byte, sessionID, credential string, extra map[string]string, mcpMethod, mcpName string) (body []byte, respSessionID string, err error) {
	// Span around the downstream call. When tracing is disabled this is a no-op
	// span, but the propagator below still injects an empty (valid) context.
	ctx, span := c.tracer.Start(ctx, "mcp.backend.call")
	span.SetAttributes(attribute.String("mcp.backend.name", c.backend.Name))
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.backend.URL.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Accept both response framings; backends may stream via SSE.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.stateless {
		req.Header.Set(mcpProtocolVersionHeader, config.ProtocolVersionStateless)
		if mcpMethod != "" {
			req.Header.Set(mcpMethodHeader, mcpMethod)
		}
		if mcpName != "" {
			req.Header.Set(mcpNameHeader, mcpName)
		}
	} else if sessionID != "" {
		req.Header.Set(mcpSessionHeader, sessionID)
	}
	c.applyAuth(req, credential, extra)
	// Propagate W3C trace context so the downstream mcp-grafana instance joins
	// this trace when it has OpenTelemetry enabled.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("backend %q returned %d: %s", c.backend.Name, resp.StatusCode, string(b))
	}

	decoded, err := decodeBody(resp.Header.Get("Content-Type"), resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("backend %q: decode response: %w", c.backend.Name, err)
	}
	return decoded, resp.Header.Get(mcpSessionHeader), nil
}

// initialize performs the MCP initialize handshake and returns the backend's
// session id. clientInfo/protocol are passed through from the client's request.
func (c *backendClient) initialize(ctx context.Context, initParams json.RawMessage, credential string) (sessionID string, result json.RawMessage, err error) {
	payload, _ := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params:  initParams,
		ID:      json.RawMessage(`1`),
	})
	body, sid, err := c.post(ctx, payload, "", credential, nil, "initialize", "")
	if err != nil {
		return "", nil, err
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return "", nil, fmt.Errorf("backend %q: bad initialize response: %w", c.backend.Name, err)
	}
	if len(rpc.Error) > 0 {
		return "", nil, fmt.Errorf("backend %q initialize error: %s", c.backend.Name, string(rpc.Error))
	}
	return sid, rpc.Result, nil
}

// notifyInitialized sends notifications/initialized to complete the handshake.
func (c *backendClient) notifyInitialized(ctx context.Context, sessionID, credential string) {
	payload, _ := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	// Best-effort; ignore errors/notification responses.
	_, _, _ = c.post(ctx, payload, sessionID, credential, nil, "notifications/initialized", "")
}

// listTools fetches the backend's tool catalog. In stateless mode there is no
// prior initialize/session, so the request carries client identity in "_meta".
func (c *backendClient) listTools(ctx context.Context, sessionID, credential string) ([]rawTool, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage(`2`),
	}
	if c.stateless {
		req.Params = injectMeta(nil)
	}
	payload, _ := json.Marshal(req)
	body, _, err := c.post(ctx, payload, sessionID, credential, nil, "tools/list", "")
	if err != nil {
		return nil, err
	}
	var rpc struct {
		Result toolsListResult `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("backend %q: bad tools/list response: %w", c.backend.Name, err)
	}
	if len(rpc.Error) > 0 {
		return nil, fmt.Errorf("backend %q tools/list error: %s", c.backend.Name, string(rpc.Error))
	}
	return rpc.Result.Tools, nil
}

// callTool forwards a tools/call to the backend on the given session, injecting
// per-request tenant headers. toolName drives the stateless Mcp-Name routing
// header (ignored in stateful mode). It returns the raw JSON-RPC response bytes.
func (c *backendClient) callTool(ctx context.Context, sessionID, credential string, reqBytes []byte, tenantHeaders map[string]string, toolName string) ([]byte, error) {
	if c.stateless {
		var req jsonRPCRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil, fmt.Errorf("backend %q: bad tools/call request: %w", c.backend.Name, err)
		}
		req.Params = injectMeta(req.Params)
		var err error
		reqBytes, err = json.Marshal(req)
		if err != nil {
			return nil, err
		}
	}
	body, _, err := c.post(ctx, reqBytes, sessionID, credential, tenantHeaders, "tools/call", toolName)
	if err != nil {
		return nil, err
	}
	return body, nil
}
