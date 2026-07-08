// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates the proxy configuration from YAML.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration tree.
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Auth          AuthConfig          `yaml:"auth"`
	Backends      []BackendConfig     `yaml:"backends"`
	Observability ObservabilityConfig `yaml:"observability"`
}

// ServerConfig controls the HTTP listener and exposed MCP endpoint.
type ServerConfig struct {
	Listen      string `yaml:"listen"`
	Path        string `yaml:"path"`
	ExternalURL string `yaml:"external_url"`
}

// ObservabilityConfig controls the metrics scrape listener. Tracing is not
// configured here: it is driven by the standard OTEL_* environment variables
// (OTEL_EXPORTER_OTLP_ENDPOINT etc.), matching mcp-grafana's convention, and is
// only enabled when an OTLP endpoint is set.
type ObservabilityConfig struct {
	// MetricsListen is the address for the separate Prometheus scrape listener,
	// isolated from the data-plane endpoint. An empty value disables it.
	MetricsListen string `yaml:"metrics_listen"`
	// MetricsPath is the path the scrape listener serves metrics on.
	MetricsPath string `yaml:"metrics_path"`
}

// AuthMode selects how incoming JWTs are trusted.
type AuthMode string

const (
	// AuthModeJWKS verifies signatures against a remote JWKS endpoint.
	AuthModeJWKS AuthMode = "jwks"
	// AuthModeStatic verifies signatures with a configured HMAC secret or PEM key.
	AuthModeStatic AuthMode = "static"
	// AuthModeInsecure parses claims without verifying (trusted-gateway only).
	AuthModeInsecure AuthMode = "insecure"
)

// AuthConfig describes how JWTs are verified and where groups are read from.
type AuthConfig struct {
	Mode        AuthMode `yaml:"mode"`
	GroupsClaim string   `yaml:"groups_claim"`
	Issuer      string   `yaml:"issuer"`
	Audience    string   `yaml:"audience"`
	// AdditionalAudiences are extra acceptable `aud` values beyond Audience.
	// Needed for service-identity tokens whose aud is "actions" while the
	// primary UI audience is "pico-mcp-ui" (parity with centcom).
	AdditionalAudiences []string `yaml:"additional_audiences"`
	ScopesSupported     []string `yaml:"scopes_supported"`

	// jwks mode. JWKSURL is optional: when empty it is discovered from the
	// issuer's OIDC metadata ({issuer}/.well-known/openid-configuration).
	JWKSURL string `yaml:"jwks_url"`

	// static mode (exactly one of these)
	HMACSecret       string `yaml:"hmac_secret"`
	PublicKeyPEMFile string `yaml:"public_key_pem_file"`
}

// BackendConfig is a single downstream MCP server and the tenants it serves.
// The backend must speak the MCP streamable-HTTP transport (e.g. an unmodified
// mcp-grafana instance, or `github-mcp-server http`).
type BackendConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Headers are static headers injected toward this backend on every request.
	Headers map[string]string `yaml:"headers"`
	// Credential is a bearer token sent to the backend so the unmodified MCP
	// server authenticates to its upstream with its own identity. It is injected
	// as "<CredentialHeader>: <CredentialScheme> <Credential>". This is a
	// backend-level default; a tenant may override it with its own Credential
	// (the common case for github-mcp-server, where each tenant has its own PAT).
	// Use a ${ENV_VARIABLE} reference to keep the secret out of the config file.
	Credential string `yaml:"credential"`
	// CredentialHeader is the header the credential is injected into. Defaults to
	// "Authorization". Set e.g. "X-Api-Key" for backends that expect an API key.
	CredentialHeader string `yaml:"credential_header"`
	// CredentialScheme is the scheme prefix prepended to the credential value in
	// the header. When omitted it defaults to "Bearer". Set it to an empty string
	// in YAML (credential_scheme: "") for a bare token, e.g. with an X-Api-Key
	// header. A pointer distinguishes "unset" (→ Bearer) from "explicitly empty".
	CredentialScheme *string `yaml:"credential_scheme"`
	// Tenants are the selectable units this backend serves. A backend declares
	// one or more.
	Tenants []TenantConfig `yaml:"tenants"`
}

// TenantConfig is a selectable tenant served by a backend.
//
// Groups authorize, tenant selects, backend executes: the tenant ID is what the
// agent targets (the `tenant` tool argument) and what list_instances advertises;
// Groups lists the JWT groups allowed to select it.
type TenantConfig struct {
	ID     string   `yaml:"id"`
	Groups []string `yaml:"groups"`
	// Credential, when set, is the downstream bearer token used for calls routed
	// to this tenant, overriding the backend-level Credential. This is how each
	// tenant maps to its own upstream identity (e.g. a per-team GitHub PAT). Use
	// a ${ENV_VARIABLE} reference to keep the secret out of the config file.
	Credential string `yaml:"credential"`
	// Headers are injected toward the backend for requests routed to this tenant,
	// merged over (and overriding) the backend Headers. Applied per request,
	// which lets one backend serve distinct tenants via headers (e.g.
	// X-Grafana-Org-Id, or X-MCP-Toolsets for github-mcp-server).
	Headers map[string]string `yaml:"headers"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.expandEnv(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// envRef matches ${VAR} references, where VAR is a typical environment variable
// name (letters, digits, underscores, not starting with a digit).
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references in secret-bearing fields (service account
// tokens and header values) with the corresponding environment variable. A
// reference to an unset variable is an error, so misconfiguration fails fast
// rather than silently sending an empty credential upstream.
func (c *Config) expandEnv() error {
	for i := range c.Backends {
		b := &c.Backends[i]
		expanded, err := expandRefs(b.Credential)
		if err != nil {
			return fmt.Errorf("backend %q: credential: %w", b.Name, err)
		}
		b.Credential = expanded

		for k, v := range b.Headers {
			expanded, err := expandRefs(v)
			if err != nil {
				return fmt.Errorf("backend %q: header %q: %w", b.Name, k, err)
			}
			b.Headers[k] = expanded
		}

		for ti := range b.Tenants {
			t := &b.Tenants[ti]
			expanded, err := expandRefs(t.Credential)
			if err != nil {
				return fmt.Errorf("backend %q tenant %q: credential: %w", b.Name, t.ID, err)
			}
			t.Credential = expanded
			for k, v := range t.Headers {
				expanded, err := expandRefs(v)
				if err != nil {
					return fmt.Errorf("backend %q tenant %q: header %q: %w", b.Name, t.ID, k, err)
				}
				t.Headers[k] = expanded
			}
		}
	}
	return nil
}

// expandRefs substitutes every ${VAR} in s, returning an error if any
// referenced variable is unset.
func expandRefs(s string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var missing string
	out := envRef.ReplaceAllStringFunc(s, func(match string) string {
		name := envRef.FindStringSubmatch(match)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = name
			return ""
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %q is not set", missing)
	}
	return out, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.Path == "" {
		c.Server.Path = "/mcp"
	}
	if c.Auth.GroupsClaim == "" {
		c.Auth.GroupsClaim = "groups"
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.CredentialHeader == "" {
			b.CredentialHeader = "Authorization"
		}
		if b.CredentialScheme == nil {
			def := "Bearer"
			b.CredentialScheme = &def
		}
	}
	if c.Observability.MetricsListen == "" {
		c.Observability.MetricsListen = ":9090"
	}
	if c.Observability.MetricsPath == "" {
		c.Observability.MetricsPath = "/metrics"
	}
}

// Validate checks for a usable configuration and fails fast on conflicts.
func (c *Config) Validate() error {
	switch c.Auth.Mode {
	case AuthModeJWKS:
		// jwks_url may be omitted and discovered from the issuer's OIDC
		// metadata, so require at least one of the two.
		if c.Auth.JWKSURL == "" && c.Auth.Issuer == "" {
			return fmt.Errorf("auth mode %q requires jwks_url, or issuer for OIDC discovery", c.Auth.Mode)
		}
	case AuthModeStatic:
		if (c.Auth.HMACSecret == "") == (c.Auth.PublicKeyPEMFile == "") {
			return fmt.Errorf("auth mode %q requires exactly one of hmac_secret or public_key_pem_file", c.Auth.Mode)
		}
	case AuthModeInsecure:
		// no key material required
	default:
		return fmt.Errorf("auth.mode must be one of jwks|static|insecure, got %q", c.Auth.Mode)
	}

	if len(c.Backends) == 0 {
		return fmt.Errorf("at least one backend is required")
	}

	names := map[string]bool{}
	tenantOwner := map[string]string{} // tenant id -> backend name
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.Name == "" {
			return fmt.Errorf("backend[%d]: name is required", i)
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate backend name %q", b.Name)
		}
		names[b.Name] = true
		if b.URL == "" {
			return fmt.Errorf("backend %q: url is required", b.Name)
		}
		if len(b.Tenants) == 0 {
			return fmt.Errorf("backend %q: at least one tenant is required", b.Name)
		}
		for ti := range b.Tenants {
			t := &b.Tenants[ti]
			if t.ID == "" {
				return fmt.Errorf("backend %q: tenant[%d]: id is required", b.Name, ti)
			}
			if owner, ok := tenantOwner[t.ID]; ok {
				return fmt.Errorf("tenant id %q is declared by both %q and %q", t.ID, owner, b.Name)
			}
			tenantOwner[t.ID] = b.Name
			if len(t.Groups) == 0 {
				return fmt.Errorf("backend %q tenant %q: at least one group is required", b.Name, t.ID)
			}
		}
	}
	return nil
}
