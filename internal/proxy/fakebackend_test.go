// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeBackend is a minimal mcp-grafana stand-in implementing streamable-HTTP:
// initialize (issues a session id), tools/list, tools/call, and DELETE. Set
// stateless to true to emulate a 2026-07-28 backend instead: initialize is
// never expected, Mcp-Session-Id is never issued/read, and every request must
// carry the stateless routing headers.
type fakeBackend struct {
	name      string
	server    *httptest.Server
	sse       bool // answer tools/list as text/event-stream when true
	stateless bool // emulate the 2026-07-28 stateless revision
	toolNames []string
	// toolSchemas overrides the default {"query": string} inputSchema for a
	// given tool name — used to emulate a tool with SEP-2243 "x-mcp-header"
	// annotations (e.g. github-mcp-server's owner/repo routing params).
	toolSchemas map[string]string
	// enforceParamHeaders, when true, makes tools/call mimic
	// modelcontextprotocol/go-sdk's real validateParamHeaders check (as
	// exercised by github-mcp-server >= v1.12.2 once MCP-Protocol-Version >=
	// 2026-07-28 is present): any tool argument whose schema property is
	// annotated "x-mcp-header" must have a matching Mcp-Param-* header, or the
	// call is rejected with a JSON-RPC error instead of "ok".
	enforceParamHeaders bool

	mu             sync.Mutex
	sessionsSeen   map[string]int    // backend session id -> request count
	lastAuth       string            // Authorization header on last tools/call
	lastHeaders    map[string]string // all headers on last request
	lastCallArgs   json.RawMessage   // arguments on last tools/call
	lastParams     json.RawMessage   // raw params on last request (any method)
	deletedSID     string            // session id seen on DELETE
	initCount      int
	statelessCalls int // requests seen while in stateless mode
	sawSessionID   bool
	sawInitialize  bool
}

func newFakeBackend(t *testing.T, name string, toolNames ...string) *fakeBackend {
	if len(toolNames) == 0 {
		toolNames = []string{"query_prometheus", "search_dashboards"}
	}
	fb := &fakeBackend{
		name:         name,
		toolNames:    toolNames,
		sessionsSeen: map[string]int{},
		lastHeaders:  map[string]string{},
	}
	fb.server = httptest.NewServer(http.HandlerFunc(fb.handle))
	t.Cleanup(fb.server.Close)
	return fb
}

// newStatelessFakeBackend is newFakeBackend for a 2026-07-28 stateless backend.
func newStatelessFakeBackend(t *testing.T, name string, toolNames ...string) *fakeBackend {
	fb := newFakeBackend(t, name, toolNames...)
	fb.stateless = true
	return fb
}

func (fb *fakeBackend) url() string { return fb.server.URL + "/mcp" }

func (fb *fakeBackend) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		fb.mu.Lock()
		fb.deletedSID = r.Header.Get(mcpSessionHeader)
		fb.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req jsonRPCRequest
	_ = json.Unmarshal(body, &req)

	fb.mu.Lock()
	if r.Header.Get(mcpSessionHeader) != "" {
		fb.sawSessionID = true
	}
	if req.Method == "initialize" {
		fb.sawInitialize = true
	}
	if fb.stateless {
		fb.statelessCalls++
	}
	fb.lastParams = req.Params
	fb.lastHeaders = map[string]string{}
	for k := range r.Header {
		fb.lastHeaders[k] = r.Header.Get(k)
	}
	fb.mu.Unlock()

	switch req.Method {
	case "initialize":
		fb.mu.Lock()
		fb.initCount++
		sid := fmt.Sprintf("%s-sess-%d", fb.name, fb.initCount)
		fb.sessionsSeen[sid] = 0
		fb.mu.Unlock()
		w.Header().Set(mcpSessionHeader, sid)
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"serverInfo":      map[string]any{"name": fb.name},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		tools := make([]rawTool, 0, len(fb.toolNames))
		for _, n := range fb.toolNames {
			schema := `{"type":"object","properties":{"query":{"type":"string"}}}`
			if s, ok := fb.toolSchemas[n]; ok {
				schema = s
			}
			tools = append(tools, rawTool{
				"name":        json.RawMessage(`"` + n + `"`),
				"description": json.RawMessage(`"stock tool"`),
				"inputSchema": json.RawMessage(schema),
			})
		}
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  toolsListResult{Tools: tools},
		})
		if fb.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
		}
	case "tools/call":
		sid := r.Header.Get(mcpSessionHeader)
		fb.mu.Lock()
		if sid != "" {
			fb.sessionsSeen[sid]++
		}
		fb.lastAuth = r.Header.Get("Authorization")
		fb.lastHeaders = map[string]string{}
		for k := range r.Header {
			fb.lastHeaders[k] = r.Header.Get(k)
		}
		var p toolCallParams
		_ = json.Unmarshal(req.Params, &p)
		fb.lastCallArgs = p.Arguments
		var schema string
		if fb.toolSchemas != nil {
			schema = fb.toolSchemas[p.Name]
		}
		headers := map[string]string{}
		for k := range r.Header {
			headers[k] = r.Header.Get(k)
		}
		fb.mu.Unlock()
		if fb.enforceParamHeaders {
			if err := checkParamHeaders(headers, schema, p.Arguments); err != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]any{"code": -32020, "message": err},
				})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSONRPCResult(w, req.ID, toolResultText("ok from "+fb.name, false))
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// checkParamHeaders mirrors modelcontextprotocol/go-sdk's real
// validateParamHeaders (as exercised by github-mcp-server >= v1.12.2) closely
// enough for tests: for every top-level inputSchema property annotated
// "x-mcp-header" with a non-null argument, the corresponding Mcp-Param-*
// header must be present and match. Returns a non-empty error message on any
// mismatch/absence, "" when the request satisfies every annotation (or the
// tool has none).
func checkParamHeaders(headers map[string]string, schemaJSON string, argsJSON json.RawMessage) string {
	if schemaJSON == "" {
		return ""
	}
	var schema toolInputSchema
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return ""
	}
	var args map[string]json.RawMessage
	if len(argsJSON) > 0 {
		_ = json.Unmarshal(argsJSON, &args)
	}
	for prop, ps := range schema.Properties {
		if len(ps.XMCPHeader) == 0 {
			continue
		}
		var headerName string
		if err := json.Unmarshal(ps.XMCPHeader, &headerName); err != nil || headerName == "" {
			continue
		}
		fullHeader := http.CanonicalHeaderKey(paramHeaderPrefix + headerName)
		argRaw, ok := args[prop]
		headerVal := headers[fullHeader]
		if !ok || string(argRaw) == "null" {
			if headerVal != "" {
				return fmt.Sprintf("header mismatch: unexpected %s header for absent parameter %q", fullHeader, prop)
			}
			continue
		}
		if headerVal == "" {
			return fmt.Sprintf("header mismatch: missing %s header for parameter %q", fullHeader, prop)
		}
		encoded, ok := encodeParamHeaderValue(argRaw)
		if !ok || encoded != headerVal {
			return fmt.Sprintf("header mismatch: %s header value %q does not match body value", fullHeader, headerVal)
		}
	}
	return ""
}

func (fb *fakeBackend) sessionCount() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return len(fb.sessionsSeen)
}

func (fb *fakeBackend) callsOnSession(sid string) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.sessionsSeen[sid]
}

func (fb *fakeBackend) header(name string) string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.lastHeaders[http.CanonicalHeaderKey(name)]
}

// sessionIDEverSeen reports whether any request carried Mcp-Session-Id.
func (fb *fakeBackend) sessionIDEverSeen() bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.sawSessionID
}

// initializeEverSeen reports whether an "initialize" request was ever received.
func (fb *fakeBackend) initializeEverSeen() bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.sawInitialize
}

// requestCount returns the number of requests handled (any method).
func (fb *fakeBackend) requestCount() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.statelessCalls
}

// params returns the raw params object of the last request received.
func (fb *fakeBackend) params() json.RawMessage {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.lastParams
}
