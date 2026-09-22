// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// mcpSessionHeader is the streamable-HTTP session header (stateful revisions).
const mcpSessionHeader = "Mcp-Session-Id"

// Headers used by the 2026-07-28 stateless revision: every request is
// self-contained, so the protocol version and routing info travel on the
// request itself rather than being pinned to a session.
const (
	// mcpProtocolVersionHeader carries the MCP revision on every request.
	mcpProtocolVersionHeader = "MCP-Protocol-Version"
	// mcpMethodHeader mirrors the JSON-RPC method so gateways can route/authorize
	// on headers without parsing the body.
	mcpMethodHeader = "Mcp-Method"
	// mcpNameHeader carries the tool name for tools/call requests.
	mcpNameHeader = "Mcp-Name"
)

// jsonRPCRequest is an incoming JSON-RPC request. ID is kept as RawMessage to
// preserve numeric/string fidelity when echoing it back.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

// isNotification reports whether the request has no id (a JSON-RPC notification).
func (r *jsonRPCRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// toolCallParams is the params object of a tools/call request. Arguments is kept
// raw so we can strip a key without re-encoding sibling arguments.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// rawTool keeps a tool's full JSON so unknown fields survive round-tripping. We
// only need to read/modify name + inputSchema, so model it as a raw object.
type rawTool map[string]json.RawMessage

// toolsListResult is the result payload of tools/list.
type toolsListResult struct {
	Tools []rawTool `json:"tools"`
}

// decodeBody reads an MCP response body that may be either a single JSON object
// (application/json) or SSE-framed (text/event-stream). It returns the first
// JSON-RPC message payload found.
//
// SSE framing per the MCP streamable-HTTP transport looks like:
//
//	event: message
//	data: {"jsonrpc":"2.0",...}
//
// Multiple data: lines in one event are concatenated with newlines.
func decodeBody(contentType string, body io.Reader) ([]byte, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return decodeSSE(body)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// decodeSSE extracts the first event's data payload from an SSE stream.
func decodeSSE(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	// Allow large JSON-RPC payloads (default token is 64KB).
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var data []string
	flush := func() ([]byte, bool) {
		if len(data) > 0 {
			return []byte(strings.Join(data, "\n")), true
		}
		return nil, false
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			// End of an event; return the first non-empty data block.
			if payload, ok := flush(); ok {
				return payload, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// Ignore event:, id:, retry:, comments.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if payload, ok := flush(); ok {
		return payload, nil
	}
	return nil, fmt.Errorf("no data payload in SSE stream")
}

// writeJSONRPCResult writes a successful JSON-RPC response with the given result
// object and echoed id. When w is an http.ResponseWriter, the caller MUST set
// Content-Type before calling this — otherwise net/http sniffs the JSON body
// and sends text/plain, which breaks strict MCP clients.
func writeJSONRPCResult(w io.Writer, id json.RawMessage, result any) error {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"result":  result,
	}
	if len(id) > 0 {
		resp["id"] = id
	}
	return json.NewEncoder(w).Encode(resp)
}

// toolResultText builds a tools/call result object carrying a single text block.
func toolResultText(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// jsonRPCErrorBytes renders a JSON-RPC error response.
func jsonRPCErrorBytes(id json.RawMessage, code int, message string) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": message},
	}
	if len(id) > 0 {
		resp["id"] = id
	}
	b, _ := json.Marshal(resp)
	return b
}

// compactJSON returns a compact form of v, for embedding raw objects.
func compactJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}
