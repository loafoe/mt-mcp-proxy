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
// initialize (issues a session id), tools/list, tools/call, and DELETE.
type fakeBackend struct {
	name      string
	server    *httptest.Server
	sse       bool // answer tools/list as text/event-stream when true
	toolNames []string

	mu           sync.Mutex
	sessionsSeen map[string]int    // backend session id -> request count
	lastAuth     string            // Authorization header on last tools/call
	lastHeaders  map[string]string // all headers on last tools/call
	lastCallArgs json.RawMessage   // arguments on last tools/call
	deletedSID   string            // session id seen on DELETE
	initCount    int
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
			tools = append(tools, rawTool{
				"name":        json.RawMessage(`"` + n + `"`),
				"description": json.RawMessage(`"stock tool"`),
				"inputSchema": json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
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
		fb.sessionsSeen[sid]++
		fb.lastAuth = r.Header.Get("Authorization")
		fb.lastHeaders = map[string]string{}
		for k := range r.Header {
			fb.lastHeaders[k] = r.Header.Get(k)
		}
		var p toolCallParams
		_ = json.Unmarshal(req.Params, &p)
		fb.lastCallArgs = p.Arguments
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSONRPCResult(w, req.ID, toolResultText("ok from "+fb.name, false))
	default:
		w.WriteHeader(http.StatusAccepted)
	}
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
	return fb.lastHeaders[name]
}
