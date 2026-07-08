// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ProtectedResourceMetadata represents the OAuth 2.0 Protected Resource Metadata document (RFC 9728).
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// ServeProtectedResourceMetadata serves the RFC 9728 protected resource metadata document.
func (h *Handler) ServeProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	metadata := ProtectedResourceMetadata{
		Resource:               h.resourceURL(r),
		BearerMethodsSupported: []string{"header"},
	}
	if h.AuthCfg.Issuer != "" {
		metadata.AuthorizationServers = []string{h.AuthCfg.Issuer}
	}
	if len(h.AuthCfg.ScopesSupported) > 0 {
		metadata.ScopesSupported = h.AuthCfg.ScopesSupported
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(metadata)
}

func (h *Handler) resourceURL(r *http.Request) string {
	path := h.ServerCfg.Path
	if path == "" {
		path = "/mcp"
	}
	if h.ServerCfg.ExternalURL != "" {
		ext := strings.TrimSuffix(h.ServerCfg.ExternalURL, "/")
		if !strings.HasSuffix(ext, path) {
			return ext + path
		}
		return ext
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func (h *Handler) metadataURL(r *http.Request) string {
	if h.ServerCfg.ExternalURL != "" {
		ext := strings.TrimSuffix(h.ServerCfg.ExternalURL, "/")
		path := h.ServerCfg.Path
		if path == "" {
			path = "/mcp"
		}
		if strings.HasSuffix(ext, path) {
			ext = strings.TrimSuffix(ext, path)
		}
		ext = strings.TrimSuffix(ext, "/")
		return ext + "/.well-known/oauth-protected-resource"
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/.well-known/oauth-protected-resource"
}

func writeHTTPError(w http.ResponseWriter, statusCode int, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(jsonRPCErrorBytes(id, code, msg))
}
