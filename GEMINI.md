# GEMINI.md — Project guide

**`mt-mcp-proxy`** is a generic multi-tenant JWT/OIDC auth gateway that fronts any
unmodified MCP server speaking the **streamable-HTTP** transport (e.g.
`github-mcp-server http`, `mcp-grafana`). It verifies a caller's JWT, maps the
JWT's `groups` to authorized tenants, and routes each `tools/call` to the backend
that serves the selected tenant — injecting that tenant's own downstream
credential. The client's JWT is never forwarded downstream.

It generalizes `mt-mcp-grafana` (which did this for `mcp-grafana` specifically);
see `docs/superpowers/specs/2026-07-08-mt-mcp-proxy-design.md` for the full design
and `README.md` for usage.

## Model

**groups authorize, tenant selects, backend executes.** A group (from the JWT)
grants access; a tenant is the selectable unit (`list_instances` / the `tenant`
tool argument) that maps to one backend and carries its downstream credential; a
backend is the downstream MCP server.

## Layout

- `cmd/mt-mcp-proxy` — entrypoint, HTTP server, `/healthz`, `/metrics` listener.
- `internal/config` — YAML load/validate, `${ENV}` secret interpolation, fail-fast.
- `internal/auth` — JWT verify (jwks/static/insecure), OIDC discovery, groups.
- `internal/registry` — tenant/backend index, group→tenants, per-tenant credential.
- `internal/session` — client session → per-tenant backend session id.
- `internal/proxy` — MCP server+client: initialize, cached tools/list, tools/call
  routing and credential injection.
- `internal/observability` — OTel metrics + traces, traceparent propagation.
- `deploy/` — Dockerfile, docker-compose example. The Helm chart lives in the
  separate `loafoe/helm-charts` repo (`charts/mt-mcp-proxy`), not here.

## Conventions

- HTTP-first: only streamable-HTTP backends (stdio is out of scope in v1).
- Static per-tenant credentials injected as `<credential_header>:
  <credential_scheme> <cred>` (defaults `Authorization` / `Bearer`).
- One downstream product per deployment (the engine still supports several
  structurally).
- Secrets come from the environment via `${ENV}` references; never commit tokens.

## Build & test

```sh
go build ./cmd/mt-mcp-proxy
go test ./...
helm lint /Users/andy/DEV/Personal/helm-charts/charts/mt-mcp-proxy
```
