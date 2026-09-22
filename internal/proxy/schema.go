// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
)

// Per-request "_meta" field names for the >= 2026-07-28 protocol revision
// (SEP-2575). These must match github.com/modelcontextprotocol/go-sdk's
// MetaKeyProtocolVersion/MetaKeyClientInfo/MetaKeyClientCapabilities exactly —
// a stateless backend built on that SDK (e.g. github-mcp-server) reads these
// bare, unnamespaced keys back out of params["_meta"] and silently treats a
// mismatch as "unknown", not an error.
const (
	metaKeyProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaKeyClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// cloneTool deep-copies a rawTool so per-caller schema mutation never corrupts
// the shared cached catalog.
func cloneTool(t rawTool) rawTool {
	out := make(rawTool, len(t))
	for k, v := range t {
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// tenantSchema builds the JSON schema for the synthetic tenant argument. When
// ids is non-empty it is rendered as an enum; required is advisory metadata for
// the description.
func tenantSchema(ids []string, required bool) json.RawMessage {
	desc := "Optional. Target instance (tenant). Call list_instances to discover valid values."
	if required {
		desc = "Required. Target instance (tenant) for this call."
	}
	m := map[string]any{"type": "string", "description": desc}
	if len(ids) > 0 {
		m["enum"] = ids
	}
	return compactJSON(m)
}

// injectTenantArg adds the tenant property to a tool's inputSchema, and marks it
// required when required is true. Operates on the rawTool's inputSchema in place.
func injectTenantArg(t rawTool, schema json.RawMessage, required bool) {
	var sch map[string]json.RawMessage
	if raw, ok := t["inputSchema"]; ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, &sch)
	}
	if sch == nil {
		sch = map[string]json.RawMessage{"type": json.RawMessage(`"object"`)}
	}

	// properties
	var props map[string]json.RawMessage
	if raw, ok := sch["properties"]; ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, &props)
	}
	if props == nil {
		props = map[string]json.RawMessage{}
	}
	props[tenantArg] = schema
	sch["properties"] = compactJSON(props)

	if required {
		var reqList []string
		if raw, ok := sch["required"]; ok && len(raw) > 0 {
			_ = json.Unmarshal(raw, &reqList)
		}
		if !contains(reqList, tenantArg) {
			reqList = append(reqList, tenantArg)
		}
		sch["required"] = compactJSON(reqList)
	}

	t["inputSchema"] = compactJSON(sch)
}

// listInstancesToolDef returns the synthetic discovery tool definition.
func listInstancesToolDef() rawTool {
	return rawTool{
		"name":        json.RawMessage(`"` + listInstancesTool + `"`),
		"description": json.RawMessage(`"Lists the instances (tenants) you are authorized to access. Use the returned ids as the 'tenant' argument."`),
		"inputSchema": json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

// extractTenantArg reads the tenant argument value from a tools/call arguments
// object, returning "" when absent or not a string.
func extractTenantArg(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	raw, ok := m[tenantArg]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// stripTenantArg removes the tenant key from a tools/call params object without
// re-encoding sibling arguments (preserves number/string fidelity and avoids
// reordering the rest). Returns the new params bytes.
func stripTenantArg(params json.RawMessage) (json.RawMessage, error) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	rawArgs, ok := p["arguments"]
	if !ok || len(rawArgs) == 0 {
		return params, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, err
	}
	if _, present := args[tenantArg]; !present {
		return params, nil
	}
	delete(args, tenantArg)
	p["arguments"] = compactJSON(args)
	return compactJSON(p), nil
}

// statelessClientMeta is the client identity/capabilities the 2026-07-28
// stateless revision expects in every request's "_meta", replacing the
// protocolVersion/clientInfo/capabilities that used to be declared once via
// initialize.
var statelessClientMeta = map[string]any{
	metaKeyProtocolVersion:    config.ProtocolVersionStateless,
	metaKeyClientInfo:         map[string]any{"name": "mt-mcp-proxy", "version": "0"},
	metaKeyClientCapabilities: map[string]any{},
}

// injectMeta merges the stateless client identity into a params object's
// "_meta" key, preserving any sibling fields (e.g. tools/call's name/arguments).
// A nil/empty params becomes a fresh object carrying only "_meta".
func injectMeta(params json.RawMessage) json.RawMessage {
	p := map[string]json.RawMessage{}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	p["_meta"] = compactJSON(statelessClientMeta)
	return compactJSON(p)
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
