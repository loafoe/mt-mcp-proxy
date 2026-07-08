# mt-mcp-proxy — Design

**Date:** 2026-07-08
**Status:** Implemented (v1)
**Author:** Andy Lo-A-Foe (with Claude)

## Summary

`mt-mcp-proxy` is a generic multi-tenant JWT/OIDC auth gateway that fronts any
**unmodified** MCP server speaking the **streamable-HTTP** transport. It
generalizes [`mt-mcp-grafana`](https://github.com/loafoe/mt-mcp-grafana) — which
did the same specifically for `mcp-grafana` — so the same engine can protect
[`github-mcp-server`](https://github.com/github/github-mcp-server) (`http` mode)
or any other streamable-HTTP MCP server.

The proxy verifies a caller's JWT, maps the JWT's `groups` to the tenants the
caller may use, and routes each `tools/call` to the backend that serves the
selected tenant — injecting that tenant's own downstream credential (e.g. a
per-team GitHub PAT). The client's JWT is never forwarded downstream.

## Motivation

`github-mcp-server`'s `http` mode is stateless and reads the GitHub token per
request from the `Authorization` header, so one process can serve many tokens —
but it has no notion of *who may use which token*. mt-mcp-proxy adds exactly
that authorization layer without modifying the backend. Research confirmed
`mt-mcp-grafana`'s routing/auth/credential-injection engine is already
domain-neutral; only a handful of Grafana-specific names needed generalizing.

## Decisions (from brainstorming)

1. **New generic repo** (`github.com/loafoe/mt-mcp-proxy`, in `~/DEV/Go`) that
   supersedes `mt-mcp-grafana`; built by lifting its engine and generalizing.
2. **HTTP-first.** Only streamable-HTTP backends are supported in v1. Both
   `github-mcp-server http` and `mcp-grafana` speak it. stdio backends (which
   would require a subprocess per credential set) are explicitly deferred.
3. **Static per-tenant credential.** Each tenant maps to a fixed downstream
   token from config; the caller's JWT authorizes but does not become the
   downstream identity (no token exchange).
4. **One product per deployment** (convention). The engine still supports
   multiple heterogeneous backends structurally, but a deployment fronts one.
5. **Generic `list_instances`** tool replaces `list_grafana_instances`.
6. **Helm chart** modeled on the `mt-mcp-grafana` chart is in scope.

## Architecture

The proxy is an MCP **server** toward clients and an MCP **client** toward
backends, both over streamable-HTTP. It hand-rolls JSON-RPC over `net/http` (no
MCP library), carried over from `mt-mcp-grafana`.

```
client --(HTTPS, Bearer JWT)--> [mt-mcp-proxy] --(cred header)--> backend MCP server
   initialize / ping / tools/list : answered locally (JWT optional on discovery)
   tools/call : verify JWT → groups → authorized tenants → select tenant →
                strip synthetic `tenant` arg → forward, injecting tenant credential
```

**Model: groups authorize, tenant selects, backend executes.**
- **group** — from the JWT; authorization only.
- **tenant** — the selectable unit (`list_instances` advertises it, the `tenant`
  tool argument targets it); maps to one backend; carries the downstream credential.
- **backend** — the downstream MCP server (URL, credential header/scheme, headers).

### Components (packages)

- `internal/config` — load & validate YAML; `${ENV}` interpolation of secrets;
  fail-fast validation (unique tenant ids, ≥1 group/tenant, auth-mode key rules).
- `internal/auth` — JWT verification (`jwks` | `static` | `insecure`), OIDC JWKS
  discovery, `exp`/`iss`/`aud`/scope checks, groups extraction.
- `internal/registry` — immutable index: `tenant id → {backend, groups,
  credential}`; `AuthorizedTenants(groups)` = set-intersection; per-tenant
  credential with backend-level fallback (`EffectiveCredential`).
- `internal/session` — client `Mcp-Session-Id` → per-**tenant** backend session id.
- `internal/proxy` — the MCP server/client: `initialize`, cached `tools/list`
  transform (injects the `tenant` selector), `tools/call` routing + credential
  injection, RFC 9728 `WWW-Authenticate` challenges.
- `internal/observability` — OTel metrics (Prometheus exporter) + OTLP traces,
  W3C traceparent propagation downstream.
- `cmd/mt-mcp-proxy` — wiring, HTTP server, `/healthz`, `/metrics` listener.

## Credential injection

Per tenant, the proxy injects `<credential_header>: <credential_scheme> <cred>`
toward the backend. Defaults `Authorization` / `Bearer` match github-mcp-server;
`credential_scheme: ""` supports bare-token headers like `X-Api-Key`. The tenant
credential overrides a backend-level default. All values support `${ENV}`.

**Session isolation:** because each tenant carries its own credential, the proxy
keys backend sessions by **tenant id** (not backend name) — two tenants sharing a
backend get separate downstream sessions, so one tenant's credential can never be
reused on another tenant's session. (This is a deliberate change from
`mt-mcp-grafana`, where the shared backend credential allowed one session.)

## Generalization from mt-mcp-grafana

| Touchpoint | Before | After |
|---|---|---|
| discovery tool | `list_grafana_instances` | `list_instances` |
| config credential | backend `service_account_token` | tenant/backend `credential` (+ `credential_header`, `credential_scheme`) |
| UI copy | "Grafana instance" | "instance" |
| service name | `mt-mcp-grafana` | `mt-mcp-proxy` |
| module/image/ko/CI | `…/mt-mcp-grafana` | `…/mt-mcp-proxy` |

The routing/auth engine is otherwise unchanged.

## Deployment

- **Image:** ko build → `ghcr.io/loafoe/mt-mcp-proxy` (chainguard static base),
  plus a plain `deploy/Dockerfile` for local builds.
- **docker-compose** (`deploy/`): runs `github-mcp-server http` + the proxy, with
  per-tenant PATs from the environment and an insecure-auth local config.
- **Helm chart** (`deploy/helm/mt-mcp-proxy`): proxy Deployment + Service, config
  ConfigMap, per-tenant credential Secrets mapped to `${ENV}`, optional **managed
  backend** (default image `ghcr.io/github/github-mcp-server`, `args: [http]`) or
  an **external** backend URL, and a ServiceMonitor.

## Testing

Carried over the full `mt-mcp-grafana` suite (auth, config, registry, session,
proxy lifecycle/observability/oauth) plus new `credential_test.go`:
- per-tenant credential overrides backend default (the core github case),
- custom credential header + empty scheme (API-key style),
- cross-tenant access denied with no credential leak.
`TestSameBackendTwoTenantsIsolatedSessions` verifies the tenant-keyed session
change. All packages pass `go vet` and `go test`.

## Out of scope (v1)

- stdio backends / subprocess-per-credential lifecycle.
- Per-user identity passthrough / OIDC token exchange.
- Multiple heterogeneous products advertised as distinct tool namespaces in one
  deployment (structurally possible; not a supported convention).
