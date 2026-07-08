// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

// Package auth verifies incoming JWTs and extracts the group claim used for
// routing.
package auth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

// ErrUnauthorized indicates the request carried no valid token.
var ErrUnauthorized = errors.New("unauthorized")

// ErrInsufficientScope indicates the token is valid but lacks the required scopes.
var ErrInsufficientScope = errors.New("insufficient_scope")

// Verifier validates an HTTP request and returns the verified groups.
type Verifier interface {
	// Groups verifies the request's bearer token and returns its groups.
	// It returns ErrUnauthorized (wrapped) for any auth failure.
	Groups(r *http.Request) ([]string, error)
}

// New builds a Verifier from auth config.
func New(ctx context.Context, c config.AuthConfig) (Verifier, error) {
	base := claimReader{groupsClaim: c.GroupsClaim}
	switch c.Mode {
	case config.AuthModeInsecure:
		return &insecureVerifier{claimReader: base, scopesSupported: c.ScopesSupported, audiences: acceptedAudiences(c)}, nil
	case config.AuthModeStatic:
		key, alg, err := loadStaticKey(c)
		if err != nil {
			return nil, err
		}
		return &jwtVerifier{
			claimReader:     base,
			keyfunc:         func(*jwt.Token) (any, error) { return key, nil },
			parserOpts:      parserOpts(c),
			validAlgs:       alg,
			scopesSupported: c.ScopesSupported,
			audiences:       acceptedAudiences(c),
		}, nil
	case config.AuthModeJWKS:
		jwksURL := c.JWKSURL
		if jwksURL == "" {
			// Discover the JWKS endpoint from the issuer's OIDC metadata.
			discovered, err := discoverJWKSURL(ctx, c.Issuer)
			if err != nil {
				return nil, err
			}
			jwksURL = discovered
		}
		jwks, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
		if err != nil {
			return nil, fmt.Errorf("init jwks: %w", err)
		}
		return &jwtVerifier{
			claimReader:     base,
			keyfunc:         jwks.Keyfunc,
			parserOpts:      parserOpts(c),
			scopesSupported: c.ScopesSupported,
			audiences:       acceptedAudiences(c),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", c.Mode)
	}
}

// discoverJWKSURL fetches the issuer's OpenID Connect discovery document
// ({issuer}/.well-known/openid-configuration) and returns its jwks_uri.
func discoverJWKSURL(ctx context.Context, issuer string) (string, error) {
	if issuer == "" {
		return "", fmt.Errorf("auth: jwks_url is empty and no issuer is set for OIDC discovery")
	}
	base, err := url.Parse(issuer)
	if err != nil {
		return "", fmt.Errorf("auth: invalid issuer %q: %w", issuer, err)
	}
	// Per OIDC Discovery the well-known path is appended to the issuer,
	// preserving any path component the issuer already carries.
	base.Path = strings.TrimRight(base.Path, "/") + "/.well-known/openid-configuration"
	discoveryURL := base.String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("auth: build discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: fetch OIDC discovery %q: %w", discoveryURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: OIDC discovery %q returned status %d", discoveryURL, resp.StatusCode)
	}

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("auth: decode OIDC discovery %q: %w", discoveryURL, err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("auth: OIDC discovery %q has no jwks_uri", discoveryURL)
	}
	return doc.JWKSURI, nil
}

func parserOpts(c config.AuthConfig) []jwt.ParserOption {
	opts := []jwt.ParserOption{jwt.WithExpirationRequired()}
	if c.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(c.Issuer))
	}
	// Audience is validated manually (see checkAudience) rather than via
	// jwt.WithAudience, because we accept ANY of Audience+AdditionalAudiences
	// and jwt.WithAudience can only require a single value.
	return opts
}

// acceptedAudiences returns the full set of acceptable `aud` values (primary +
// additional). Empty means "do not check audience".
func acceptedAudiences(c config.AuthConfig) []string {
	var auds []string
	if c.Audience != "" {
		auds = append(auds, c.Audience)
	}
	auds = append(auds, c.AdditionalAudiences...)
	return auds
}

// checkAudience returns nil when accepted is empty, or when the token's `aud`
// claim (string or []string) contains at least one accepted value.
func checkAudience(claims jwt.MapClaims, accepted []string) error {
	if len(accepted) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(accepted))
	for _, a := range accepted {
		allowed[a] = struct{}{}
	}
	var tokenAuds []string
	switch aud := claims["aud"].(type) {
	case string:
		tokenAuds = []string{aud}
	case []interface{}:
		for _, a := range aud {
			if s, ok := a.(string); ok {
				tokenAuds = append(tokenAuds, s)
			}
		}
	case []string:
		tokenAuds = aud
	}
	for _, a := range tokenAuds {
		if _, ok := allowed[a]; ok {
			return nil
		}
	}
	return fmt.Errorf("%w: token audience %v not in accepted set %v", ErrUnauthorized, tokenAuds, accepted)
}

// loadStaticKey returns the verification key and the algorithm family allowed.
func loadStaticKey(c config.AuthConfig) (any, []string, error) {
	if c.HMACSecret != "" {
		return []byte(c.HMACSecret), []string{"HS256", "HS384", "HS512"}, nil
	}
	pemBytes, err := os.ReadFile(c.PublicKeyPEMFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read public key: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, nil, fmt.Errorf("public key file %q is not valid PEM", c.PublicKeyPEMFile)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}
	// Accept the common asymmetric families; jwt enforces the actual match.
	return key, []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}, nil
}

// jwtVerifier verifies signatures via a keyfunc (static key or JWKS).
type jwtVerifier struct {
	claimReader
	keyfunc         jwt.Keyfunc
	parserOpts      []jwt.ParserOption
	validAlgs       []string
	scopesSupported []string
	audiences       []string
}

func (v *jwtVerifier) Groups(r *http.Request) ([]string, error) {
	tokenStr, err := bearerToken(r)
	if err != nil {
		return nil, err
	}
	opts := v.parserOpts
	if len(v.validAlgs) > 0 {
		opts = append(opts, jwt.WithValidMethods(v.validAlgs))
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(tokenStr, claims, v.keyfunc, opts...); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if err := checkAudience(claims, v.audiences); err != nil {
		return nil, err
	}
	if len(v.scopesSupported) > 0 && !hasRequiredScope(claims, v.scopesSupported) {
		return nil, ErrInsufficientScope
	}
	return v.extractGroups(claims)
}

// insecureVerifier parses claims without verifying the signature. Only for
// deployments behind a trusted gateway that already validated the token.
type insecureVerifier struct {
	claimReader
	scopesSupported []string
	audiences       []string
}

func (v *insecureVerifier) Groups(r *http.Request) ([]string, error) {
	tokenStr, err := bearerToken(r)
	if err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{}
	parser := jwt.NewParser()
	if _, _, err := parser.ParseUnverified(tokenStr, claims); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if err := checkAudience(claims, v.audiences); err != nil {
		return nil, err
	}
	if len(v.scopesSupported) > 0 && !hasRequiredScope(claims, v.scopesSupported) {
		return nil, ErrInsufficientScope
	}
	return v.extractGroups(claims)
}

func hasRequiredScope(claims jwt.MapClaims, required []string) bool {
	if len(required) == 0 {
		return true
	}
	rawScope, ok := claims["scope"]
	if !ok {
		rawScope, ok = claims["scp"]
		if !ok {
			return false
		}
	}

	var tokenScopes []string
	switch v := rawScope.(type) {
	case string:
		tokenScopes = strings.Fields(v)
	case []string:
		tokenScopes = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				tokenScopes = append(tokenScopes, s)
			}
		}
	}

	for _, req := range required {
		for _, tok := range tokenScopes {
			if req == tok {
				return true
			}
		}
	}
	return false
}

// claimReader extracts the configured groups claim from parsed claims.
type claimReader struct {
	groupsClaim string
}

func (cr claimReader) extractGroups(claims jwt.MapClaims) ([]string, error) {
	raw, ok := claims[cr.groupsClaim]
	if !ok {
		return nil, fmt.Errorf("%w: token missing %q claim", ErrUnauthorized, cr.groupsClaim)
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	case []string:
		return v, nil
	case []any:
		groups := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				groups = append(groups, s)
			}
		}
		return groups, nil
	default:
		return nil, fmt.Errorf("%w: %q claim has unexpected type %T", ErrUnauthorized, cr.groupsClaim, raw)
	}
}

func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", fmt.Errorf("%w: missing Authorization header", ErrUnauthorized)
	}
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", fmt.Errorf("%w: Authorization header is not a Bearer token", ErrUnauthorized)
	}
	return strings.TrimSpace(h[len(prefix):]), nil
}
