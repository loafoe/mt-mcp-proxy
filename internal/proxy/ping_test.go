// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"
)

// A locally-answered ping must advertise Content-Type: application/json —
// without it, net/http sniffs the JSON body and sends text/plain, which
// strict MCP clients (e.g. the Python SDK's streamable-http transport)
// reject outright, breaking keepalive.
func TestPingContentType(t *testing.T) {
	_, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	w := rpc(h, "ping", nil, "", "")

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	resp := decodeResult(t, w)
	result, ok := resp["result"].(map[string]any)
	if !ok || len(result) != 0 {
		t.Errorf("result = %v, want empty object", resp["result"])
	}
}
