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

func TestServeProtectedResourceMetadata(t *testing.T) {
	serverCfg := config.ServerConfig{
		Path:        "/mcp-endpoint",
		ExternalURL: "https://my-mcp-proxy.philips.com/mcp-endpoint",
	}
	authCfg := config.AuthConfig{
		Issuer:          "https://issuer.obs-us-east-ct.hsp.philips.com",
		ScopesSupported: []string{"mcp", "grafana"},
	}

	h := &Handler{
		ServerCfg: serverCfg,
		AuthCfg:   authCfg,
	}

	r := httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil)
	w := httptest.NewRecorder()

	h.ServeProtectedResourceMetadata(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", contentType)
	}

	var metadata ProtectedResourceMetadata
	if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if metadata.Resource != "https://my-mcp-proxy.philips.com/mcp-endpoint" {
		t.Errorf("expected resource %q, got %q", "https://my-mcp-proxy.philips.com/mcp-endpoint", metadata.Resource)
	}

	if len(metadata.AuthorizationServers) != 1 || metadata.AuthorizationServers[0] != "https://issuer.obs-us-east-ct.hsp.philips.com" {
		t.Errorf("expected auth servers %v, got %v", []string{"https://issuer.obs-us-east-ct.hsp.philips.com"}, metadata.AuthorizationServers)
	}

	if len(metadata.ScopesSupported) != 2 || metadata.ScopesSupported[0] != "mcp" || metadata.ScopesSupported[1] != "grafana" {
		t.Errorf("expected scopes %v, got %v", []string{"mcp", "grafana"}, metadata.ScopesSupported)
	}

	if len(metadata.BearerMethodsSupported) != 1 || metadata.BearerMethodsSupported[0] != "header" {
		t.Errorf("expected bearer methods [header], got %v", metadata.BearerMethodsSupported)
	}
}

func TestServeProtectedResourceMetadataFallbackURL(t *testing.T) {
	serverCfg := config.ServerConfig{
		Path: "/mcp-endpoint",
	}
	authCfg := config.AuthConfig{
		Issuer: "https://issuer.obs-us-east-ct.hsp.philips.com",
	}

	h := &Handler{
		ServerCfg: serverCfg,
		AuthCfg:   authCfg,
	}

	r := httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil)
	r.Host = "test-host.philips.com"
	w := httptest.NewRecorder()

	h.ServeProtectedResourceMetadata(w, r)

	var metadata ProtectedResourceMetadata
	if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if metadata.Resource != "http://test-host.philips.com/mcp-endpoint" {
		t.Errorf("expected resource %q, got %q", "http://test-host.philips.com/mcp-endpoint", metadata.Resource)
	}
}

func TestToolsCallUnauthorizedOAuth2(t *testing.T) {
	serverCfg := config.ServerConfig{
		Path:        "/mcp",
		ExternalURL: "https://proxy.example.com/mcp",
	}
	authCfg := config.AuthConfig{
		Mode:            config.AuthModeStatic,
		GroupsClaim:     "groups",
		HMACSecret:      secret,
		Issuer:          "https://issuer.obs-us-east-ct.hsp.philips.com",
		ScopesSupported: []string{"mcp", "grafana"},
	}

	verifier, err := auth.New(context.Background(), authCfg)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New([]config.BackendConfig{
		{Name: "grafana", URL: "http://localhost:8000/mcp", Credential: "sa", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(
		verifier,
		reg,
		session.NewStore(0),
		NewCatalog(reg.ReferenceBackend(), time.Minute),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		serverCfg,
		authCfg,
	)

	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "query_prometheus",
			"arguments": map[string]any{},
		},
	}
	b, _ := json.Marshal(body)
	r, _ := http.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}

	authHeader := w.Header().Get("WWW-Authenticate")
	expectedAuthHeader := `Bearer resource_metadata="https://proxy.example.com/.well-known/oauth-protected-resource", scope="mcp grafana"`
	if authHeader != expectedAuthHeader {
		t.Errorf("expected WWW-Authenticate %q, got %q", expectedAuthHeader, authHeader)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] == nil {
		t.Fatal("expected error object in JSON-RPC response")
	}
	msg := resp["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "unauthorized") {
		t.Errorf("expected unauthorized message, got %q", msg)
	}
}

func TestToolsCallInsufficientScopeOAuth2(t *testing.T) {
	serverCfg := config.ServerConfig{
		Path:        "/mcp",
		ExternalURL: "https://proxy.example.com/mcp",
	}
	authCfg := config.AuthConfig{
		Mode:            config.AuthModeStatic,
		GroupsClaim:     "groups",
		HMACSecret:      secret,
		Issuer:          "https://issuer.obs-us-east-ct.hsp.philips.com",
		ScopesSupported: []string{"mcp", "grafana"},
	}

	verifier, err := auth.New(context.Background(), authCfg)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New([]config.BackendConfig{
		{Name: "grafana", URL: "http://localhost:8000/mcp", Credential: "sa", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(
		verifier,
		reg,
		session.NewStore(0),
		NewCatalog(reg.ReferenceBackend(), time.Minute),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		serverCfg,
		authCfg,
	)

	// Sign a token with a claim that has the wrong scope
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer.obs-us-east-ct.hsp.philips.com",
		"groups": []string{"team-a"},
		"scope":  "other-scope",
	})
	tokenStr, _ := tok.SignedString([]byte(secret))

	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "query_prometheus",
			"arguments": map[string]any{},
		},
	}
	b, _ := json.Marshal(body)
	r, _ := http.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status %d, got %d", http.StatusForbidden, w.Code)
	}

	authHeader := w.Header().Get("WWW-Authenticate")
	expectedAuthHeader := `Bearer error="insufficient_scope", scope="mcp grafana", resource_metadata="https://proxy.example.com/.well-known/oauth-protected-resource", error_description="insufficient scope"`
	if authHeader != expectedAuthHeader {
		t.Errorf("expected WWW-Authenticate %q, got %q", expectedAuthHeader, authHeader)
	}
}

func TestToolsCallSufficientScopeOAuth2(t *testing.T) {
	serverCfg := config.ServerConfig{
		Path:        "/mcp",
		ExternalURL: "https://proxy.example.com/mcp",
	}
	authCfg := config.AuthConfig{
		Mode:            config.AuthModeStatic,
		GroupsClaim:     "groups",
		HMACSecret:      secret,
		Issuer:          "https://issuer.obs-us-east-ct.hsp.philips.com",
		ScopesSupported: []string{"mcp", "grafana"},
	}

	verifier, err := auth.New(context.Background(), authCfg)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New([]config.BackendConfig{
		{Name: "grafana", URL: "http://localhost:8000/mcp", Credential: "sa", Tenants: []config.TenantConfig{
			{ID: "team-a", Groups: []string{"team-a"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(
		verifier,
		reg,
		session.NewStore(0),
		NewCatalog(reg.ReferenceBackend(), time.Minute),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		serverCfg,
		authCfg,
	)

	// Sign a token with a claim that has the correct scope
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer.obs-us-east-ct.hsp.philips.com",
		"groups": []string{"team-a"},
		"scope":  "grafana", // matches scopes_supported
	})
	tokenStr, _ := tok.SignedString([]byte(secret))

	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "query_prometheus",
			"arguments": map[string]any{},
		},
	}
	b, _ := json.Marshal(body)
	r, _ := http.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()

	// Wait, we need to initialize a session first to call a tool, or we'll get "session not found" error.
	// But "session not found" error happens AFTER authentication! So if we get "session not found" (code 200/500/whatever, but not 401/403),
	// it means auth succeeded!
	h.ServeHTTP(w, r)

	// Check that we got a session-not-found error, meaning authentication passed!
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("unexpected auth rejection status %d", w.Code)
	}
}
