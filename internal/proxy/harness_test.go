// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/loafoe/mt-mcp-proxy/internal/auth"
	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/loafoe/mt-mcp-proxy/internal/registry"
	"github.com/loafoe/mt-mcp-proxy/internal/session"
)

const secret = "proxy-test-secret"

func token(t *testing.T, groups ...string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": groups,
	})
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func harness(t *testing.T, backends []config.BackendConfig) *Handler {
	t.Helper()
	verifier, err := auth.New(context.Background(), config.AuthConfig{
		Mode: config.AuthModeStatic, GroupsClaim: "groups", HMACSecret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(backends)
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(0)
	cat := NewCatalog(reg.ReferenceBackend(), time.Minute)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(verifier, reg, store, cat, logger, nil, config.ServerConfig{Path: "/mcp"}, config.AuthConfig{})
}

func rpc(h *Handler, method string, params any, tok, sid string) *httptest.ResponseRecorder {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, _ := json.Marshal(body)
	r, _ := http.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	if sid != "" {
		r.Header.Set(mcpSessionHeader, sid)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func initSession(h *Handler) string {
	w := rpc(h, "initialize", map[string]any{"protocolVersion": "2025-03-26"}, "", "")
	return w.Header().Get(mcpSessionHeader)
}

func decodeResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return resp
}

func toolNames(t *testing.T, w *httptest.ResponseRecorder) []string {
	resp := decodeResult(t, w)
	result, _ := resp["result"].(map[string]any)
	toolsRaw, _ := result["tools"].([]any)
	var names []string
	for _, tr := range toolsRaw {
		m := tr.(map[string]any)
		names = append(names, m["name"].(string))
	}
	return names
}

func findTool(t *testing.T, w *httptest.ResponseRecorder, name string) map[string]any {
	resp := decodeResult(t, w)
	result, _ := resp["result"].(map[string]any)
	toolsRaw, _ := result["tools"].([]any)
	for _, tr := range toolsRaw {
		m := tr.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	return nil
}

func twoBackends(t *testing.T) (*fakeBackend, *fakeBackend, []config.BackendConfig) {
	ab := newFakeBackend(t, "grafana-ab")
	c := newFakeBackend(t, "grafana-c")
	cfgs := []config.BackendConfig{
		{Name: "grafana-ab", URL: ab.url(), Credential: "ab-sa", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}, Headers: map[string]string{"X-Grafana-Org-Id": "1"}},
			{ID: "team-b", Groups: []string{"team-b"}, Headers: map[string]string{"X-Grafana-Org-Id": "2"}},
		}},
		{Name: "grafana-c", URL: c.url(), Credential: "c-sa", Tenants: []config.TenantConfig{
			{ID: "team-c", Groups: []string{"team-c"}},
		}},
	}
	return ab, c, cfgs
}
