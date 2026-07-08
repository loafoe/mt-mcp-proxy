// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

const hmacSecret = "test-secret-please-ignore"

func signHMAC(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(hmacSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reqWith(token string) *http.Request {
	r, _ := http.NewRequest("POST", "/mcp", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func staticHMACVerifier(t *testing.T) Verifier {
	t.Helper()
	v, err := New(context.Background(), config.AuthConfig{
		Mode:        config.AuthModeStatic,
		GroupsClaim: "groups",
		HMACSecret:  hmacSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestStaticHMACHappyPath(t *testing.T) {
	v := staticHMACVerifier(t)
	tok := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"team-a", "team-b"},
	})
	groups, err := v.Groups(reqWith(tok))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(groups) != 2 || groups[0] != "team-a" {
		t.Errorf("got groups %v", groups)
	}
}

func TestStaticHMACSingleStringClaim(t *testing.T) {
	v := staticHMACVerifier(t)
	tok := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": "solo",
	})
	groups, err := v.Groups(reqWith(tok))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != "solo" {
		t.Errorf("got %v", groups)
	}
}

func TestStaticHMACExpired(t *testing.T) {
	v := staticHMACVerifier(t)
	tok := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(-time.Hour).Unix(),
		"groups": []string{"team-a"},
	})
	_, err := v.Groups(reqWith(tok))
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestStaticHMACBadSignature(t *testing.T) {
	v := staticHMACVerifier(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"x"},
	})
	bad, _ := tok.SignedString([]byte("wrong-secret"))
	_, err := v.Groups(reqWith(bad))
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestMissingAuthHeader(t *testing.T) {
	v := staticHMACVerifier(t)
	_, err := v.Groups(reqWith(""))
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestAudienceAndIssuerEnforced(t *testing.T) {
	v, err := New(context.Background(), config.AuthConfig{
		Mode:        config.AuthModeStatic,
		GroupsClaim: "groups",
		HMACSecret:  hmacSecret,
		Issuer:      "https://issuer",
		Audience:    "mcp-grafana",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wrong audience -> rejected.
	tok := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer",
		"aud":    "someone-else",
		"groups": []string{"x"},
	})
	if _, err := v.Groups(reqWith(tok)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong aud should fail, got %v", err)
	}
	// Correct claims -> accepted.
	ok := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer",
		"aud":    "mcp-grafana",
		"groups": []string{"x"},
	})
	if _, err := v.Groups(reqWith(ok)); err != nil {
		t.Errorf("valid token should pass, got %v", err)
	}
}

func TestAdditionalAudiencesAccepted(t *testing.T) {
	v, err := New(context.Background(), config.AuthConfig{
		Mode:                config.AuthModeStatic,
		GroupsClaim:         "groups",
		HMACSecret:          hmacSecret,
		Issuer:              "https://issuer",
		Audience:            "pico-mcp-ui",
		AdditionalAudiences: []string{"actions"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Token with the additional audience ("actions", as service-identity tokens
	// carry) must be accepted even though it isn't the primary audience.
	tok := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer",
		"aud":    "actions",
		"groups": []string{"x"},
	})
	if _, err := v.Groups(reqWith(tok)); err != nil {
		t.Errorf("additional audience should pass, got %v", err)
	}
	// The primary audience must still work.
	primary := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer",
		"aud":    "pico-mcp-ui",
		"groups": []string{"x"},
	})
	if _, err := v.Groups(reqWith(primary)); err != nil {
		t.Errorf("primary audience should pass, got %v", err)
	}
	// An audience in neither set is rejected.
	bad := signHMAC(t, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    "https://issuer",
		"aud":    "someone-else",
		"groups": []string{"x"},
	})
	if _, err := v.Groups(reqWith(bad)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unlisted audience should fail, got %v", err)
	}
}

func TestStaticRSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := New(context.Background(), config.AuthConfig{
		Mode:             config.AuthModeStatic,
		GroupsClaim:      "groups",
		PublicKeyPEMFile: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"team-rsa"},
	})
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := v.Groups(reqWith(signed))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-rsa" {
		t.Errorf("got %v", groups)
	}
}

func TestInsecureMode(t *testing.T) {
	v, err := New(context.Background(), config.AuthConfig{
		Mode:        config.AuthModeInsecure,
		GroupsClaim: "groups",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Token signed with an arbitrary secret; insecure mode ignores signature.
	tok := signHMAC(t, jwt.MapClaims{"groups": []string{"team-x"}})
	groups, err := v.Groups(reqWith(tok))
	if err != nil {
		t.Fatalf("insecure verify: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-x" {
		t.Errorf("got %v", groups)
	}
}

func TestMissingGroupsClaim(t *testing.T) {
	v := staticHMACVerifier(t)
	tok := signHMAC(t, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	if _, err := v.Groups(reqWith(tok)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("missing groups claim should fail, got %v", err)
	}
}

func TestDiscoverJWKSURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"x","jwks_uri":"https://issuer.example/keys"}`))
	}))
	t.Cleanup(srv.Close)

	got, err := discoverJWKSURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if gotPath != "/.well-known/openid-configuration" {
		t.Errorf("discovery hit %q, want /.well-known/openid-configuration", gotPath)
	}
	if got != "https://issuer.example/keys" {
		t.Errorf("got jwks_uri %q", got)
	}
}

func TestDiscoverJWKSURLPreservesIssuerPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"jwks_uri":"https://issuer.example/realms/x/keys"}`))
	}))
	t.Cleanup(srv.Close)

	// Issuer carries a path component (e.g. Keycloak realms).
	if _, err := discoverJWKSURL(context.Background(), srv.URL+"/realms/x"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if gotPath != "/realms/x/.well-known/openid-configuration" {
		t.Errorf("discovery hit %q, want the well-known path appended after the issuer path", gotPath)
	}
}

func TestDiscoverJWKSURLNoIssuer(t *testing.T) {
	if _, err := discoverJWKSURL(context.Background(), ""); err == nil {
		t.Fatal("expected error when issuer is empty")
	}
}

func TestDiscoverJWKSURLMissingURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"x"}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := discoverJWKSURL(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error when discovery doc has no jwks_uri")
	}
}

// TestJWKSModeWithDiscovery is an end-to-end check: an issuer that serves OIDC
// metadata pointing at a JWKS, and a token verified through New() with no
// jwks_url configured.
func TestJWKSModeWithDiscovery(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-key-1"

	mux := http.NewServeMux()
	var issuerURL string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"` + issuerURL + `","jwks_uri":"` + issuerURL + `/jwks"}`))
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rsaJWKS(key, kid)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuerURL = srv.URL

	v, err := New(context.Background(), config.AuthConfig{
		Mode:        config.AuthModeJWKS,
		GroupsClaim: "groups",
		Issuer:      issuerURL, // no JWKSURL -> must be discovered
	})
	if err != nil {
		t.Fatalf("New with discovery: %v", err)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iss":    issuerURL,
		"groups": []string{"team-disco"},
	})
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	groups, err := v.Groups(reqWith(signed))
	if err != nil {
		t.Fatalf("verify discovered-jwks token: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-disco" {
		t.Errorf("got %v", groups)
	}
}

// rsaJWKS renders a minimal single-key JWKS document for an RSA public key.
func rsaJWKS(key *rsa.PrivateKey, kid string) string {
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	eb := make([]byte, 4)
	binary.BigEndian.PutUint32(eb, uint32(key.PublicKey.E))
	// Trim leading zero bytes from the exponent.
	i := 0
	for i < len(eb)-1 && eb[i] == 0 {
		i++
	}
	e := base64.RawURLEncoding.EncodeToString(eb[i:])
	return `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`
}
