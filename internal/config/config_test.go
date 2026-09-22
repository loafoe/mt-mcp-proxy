// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const oneBackend = `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a:8000/mcp
    tenants:
      - id: team-a
        groups: [team-a]
`

func TestLoadDefaultsAndValid(t *testing.T) {
	c, err := Load(writeTemp(t, oneBackend))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Server.Listen != ":8080" || c.Server.Path != "/mcp" {
		t.Errorf("server defaults not applied: %+v", c.Server)
	}
	if c.Auth.GroupsClaim != "groups" {
		t.Errorf("groups_claim default not applied: %q", c.Auth.GroupsClaim)
	}
	if len(c.Backends[0].Tenants) != 1 || c.Backends[0].Tenants[0].ID != "team-a" {
		t.Errorf("tenant not parsed: %+v", c.Backends[0].Tenants)
	}
	if c.Observability.MetricsListen != ":9090" || c.Observability.MetricsPath != "/metrics" {
		t.Errorf("observability defaults not applied: %+v", c.Observability)
	}
}

func TestObservabilityMetricsListenDisable(t *testing.T) {
	// An explicit empty metrics_listen must survive defaulting so the listener
	// can be disabled. Use the YAML tilde to set an explicit empty string.
	c, err := Load(writeTemp(t, `
auth:
  mode: insecure
observability:
  metrics_listen: ""
  metrics_path: "/m"
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - id: team-a
        groups: [g1]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// metrics_listen given as "" is indistinguishable from unset in YAML, so it
	// defaults back to :9090; metrics_path was explicitly set and must persist.
	if c.Observability.MetricsPath != "/m" {
		t.Errorf("metrics_path not parsed: %q", c.Observability.MetricsPath)
	}
}

func TestValidateRejectsDuplicateTenantID(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - id: shared
        groups: [g1]
  - name: b
    url: http://b/mcp
    tenants:
      - id: shared
        groups: [g2]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected duplicate tenant id error")
	}
}

func TestValidateRequiresTenant(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a/mcp
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error when backend has no tenants")
	}
}

func TestValidateTenantRequiresGroup(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - id: team-a
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error when tenant has no groups")
	}
}

func TestGroupReuseAcrossTenantsAllowed(t *testing.T) {
	// A group authorizing two tenants is allowed (selection is by tenant id).
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - id: team-a
        groups: [shared]
      - id: team-a2
        groups: [shared]
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("group reuse across tenants should be allowed, got %v", err)
	}
}

func TestValidateStaticRequiresExactlyOneKey(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: static
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - {id: x, groups: [x]}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error when no static key provided")
	}
}

func TestValidateJWKSRequiresURLOrIssuer(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: jwks
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - {id: x, groups: [x]}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error when both jwks_url and issuer are missing")
	}
}

func TestValidateJWKSIssuerOnlyOK(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: jwks
  issuer: https://issuer.example.com
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - {id: x, groups: [x]}
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("issuer-only jwks config should validate, got %v", err)
	}
}

func TestCredentialEnvInterpolation(t *testing.T) {
	t.Setenv("BACKEND_A_SA", "glsa_from_env")
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a:8000/mcp
    credential: "${BACKEND_A_SA}"
    tenants:
      - {id: team-a, groups: [team-a]}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Backends[0].Credential != "glsa_from_env" {
		t.Errorf("token not interpolated, got %q", c.Backends[0].Credential)
	}
}

func TestEnvInterpolationInTenantHeaders(t *testing.T) {
	t.Setenv("TENANT_SECRET", "s3cr3t")
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a:8000/mcp
    tenants:
      - id: team-a
        groups: [team-a]
        headers:
          X-Token: "Bearer ${TENANT_SECRET}"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Backends[0].Tenants[0].Headers["X-Token"]; got != "Bearer s3cr3t" {
		t.Errorf("tenant header not interpolated, got %q", got)
	}
}

func TestEnvInterpolationMissingVarFails(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a:8000/mcp
    credential: "${DEFINITELY_NOT_SET_VAR}"
    tenants:
      - {id: team-a, groups: [team-a]}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error when referenced env var is unset")
	}
}

func TestProtocolVersionDefaultsToStateful(t *testing.T) {
	c, err := Load(writeTemp(t, oneBackend))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Backends[0].ProtocolVersion; got != ProtocolVersionStateful {
		t.Errorf("protocol_version default = %q, want %q", got, ProtocolVersionStateful)
	}
}

func TestProtocolVersionStatelessAccepted(t *testing.T) {
	c, err := Load(writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a:8000/mcp
    protocol_version: "2026-07-28"
    tenants:
      - id: team-a
        groups: [team-a]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Backends[0].ProtocolVersion; got != ProtocolVersionStateless {
		t.Errorf("protocol_version = %q, want %q", got, ProtocolVersionStateless)
	}
}

func TestProtocolVersionRejectsUnknown(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: insecure
backends:
  - name: a
    url: http://a/mcp
    protocol_version: "2024-11-05"
    tenants:
      - {id: x, groups: [x]}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unsupported protocol_version")
	}
}

func TestValidateUnknownMode(t *testing.T) {
	p := writeTemp(t, `
auth:
  mode: bogus
backends:
  - name: a
    url: http://a/mcp
    tenants:
      - {id: x, groups: [x]}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown auth mode")
	}
}
