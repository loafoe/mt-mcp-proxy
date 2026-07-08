# mt-mcp-proxy

A generic **multi-tenant auth gateway** that sits in front of **any unmodified**
MCP server speaking the MCP **streamable-HTTP** transport — for example
[`github-mcp-server`](https://github.com/github/github-mcp-server) (`http` mode)
or [`mcp-grafana`](https://github.com/grafana/mcp-grafana).

Clients authenticate to the proxy with a **JWT**. The proxy is itself an MCP
server: it terminates the MCP session toward the client and acts as an MCP
client toward each backend. It verifies the token, maps the JWT's **groups** to
the **tenants** the caller may use, and routes each tool call to the backend that
serves the selected tenant — injecting that tenant's own **downstream
credential** (e.g. a per-team GitHub PAT).

```
client --(HTTPS, Bearer JWT)--> [mt-mcp-proxy] --(Authorization: Bearer <tenant cred>)--> github-mcp-server http
                                       |
        terminates client MCP session · serves tools/list from cache ·
        per tool call: groups authorize → tenant selects → backend executes
```

The proxy never forwards the client's JWT downstream. The downstream server only
ever sees the tenant's configured credential.

## Why

`github-mcp-server`'s `http` mode is stateless and reads the GitHub token per
request from the `Authorization` header — so one process can serve many tokens,
but it has **no notion of who may use which token**. mt-mcp-proxy adds exactly
that: verified JWT → group-based authorization → per-tenant credential, without
modifying the backend.

## Model: groups, tenants, backends

Three distinct concepts — **groups authorize, tenant selects, backend executes**:

- **group** — comes from the JWT. Authorization only.
- **tenant** — the selectable unit. What `list_instances` advertises and what the
  `tenant` tool argument targets. Maps to exactly one backend and carries the
  downstream credential used for its calls.
- **backend** — the downstream MCP server (URL, credential injection settings,
  headers).

A tenant lists the JWT groups allowed to select it; multiple tenants may point at
the same backend (e.g. one `github-mcp-server`, several teams each with their own
PAT).

## How routing works

- On a tool call, the proxy verifies the JWT and computes the caller's
  **authorized tenants** (tenants whose `groups` intersect the JWT's groups).
- If the caller is authorized for **one** tenant, the call routes there
  implicitly (no `tenant` argument needed).
- If authorized for **several**, the agent picks one via the `tenant` tool
  argument (advertised as a required enum); the proxy validates it against the
  JWT, strips it, and routes. Calling `list_instances` returns the caller's
  authorized tenant ids.
- A `tenant` the caller isn't authorized for, or no matching tenant at all,
  yields a structured error listing the valid choices — and no credential is
  ever injected for an unauthorized tenant.

## Configuring backends and tenants

Each entry under `backends` is one **unmodified** downstream MCP server that
registers a `tenants:` list. Fronting `github-mcp-server`:

```yaml
backends:
  - name: github
    url: http://github-mcp-server:8082/     # `github-mcp-server http`, MCP at /
    # credential_header/credential_scheme default to Authorization / Bearer,
    # which is exactly what github-mcp-server expects — so no need to set them.
    tenants:
      - id: team-platform                    # selectable tenant id
        groups: [platform-eng]               # JWT groups authorized for it
        credential: "${GITHUB_PAT_PLATFORM}" # this tenant's GitHub token
        headers: {X-MCP-Toolsets: "repos,issues,pull_requests"}
      - id: team-data
        groups: [data-eng]
        credential: "${GITHUB_PAT_DATA}"
        headers: {X-MCP-Readonly: "true"}
```

Rules enforced at startup (fail-fast):

- Tenant `id`s are globally unique (they are the routing key).
- Each backend has ≥1 tenant; each tenant has ≥1 group; `name`s unique, `url`
  absolute.
- A group may authorize several tenants (authorization is set-intersection;
  selection is the explicit tenant id).

To add a tenant, append it under a backend and restart.

## Downstream credentials

The proxy injects a per-tenant **credential** toward the backend so the
downstream server authenticates to its upstream with the right identity. The
client's JWT is used only for routing and is never forwarded.

- **`credential`** (tenant-level) — the token for calls routed to that tenant,
  e.g. a team's GitHub PAT. A **backend-level** `credential` acts as the default
  for tenants that don't set their own.
- **`credential_header`** (backend-level, default `Authorization`) — the header
  the credential is injected into. Set e.g. `X-Api-Key` for API-key backends.
- **`credential_scheme`** (backend-level, default `Bearer`) — the scheme prefix.
  Set to `""` for a bare token (e.g. with `X-Api-Key`).

All credential and header values support `${ENV_VARIABLE}` interpolation, read
from the environment at startup (recommended — keeps secrets out of the config
file). A referenced variable that is unset makes startup fail.

Because each tenant carries its own credential, the proxy opens a **separate
downstream session per tenant**: two tenants sharing a backend never share a
session initialized with one tenant's credential.

## Sessions

The proxy owns the MCP session. It answers `initialize` itself and mints its own
`Mcp-Session-Id`; it serves `tools/list` from a cached reference catalog; and per
tool call it lazily opens/reuses a backend MCP session for the selected tenant,
mapping the client session to backend sessions internally.

Because the session map is in-memory:

- **Multi-replica:** the load balancer must use **sticky routing** on
  `Mcp-Session-Id`.
- **Restart:** sessions are dropped; the client re-runs `initialize`.
- `DELETE` on the endpoint tears down the client session and its backend sessions.

## Transport

This proxy targets MCP **streamable HTTP** (spec 2025-03-26). A single endpoint
handles `POST` (JSON-RPC, answered as `application/json` *or* a streamed
`text/event-stream`), optional `GET` (server→client stream), and `DELETE`
(session end). The proxy answers `initialize`/`ping`/`tools/list` locally and
forwards `tools/call` to the selected tenant's backend.

> **stdio backends** (a downstream MCP server that only speaks stdio, requiring a
> subprocess per credential) are **not** supported in this version — this is
> planned future work, tracked in [`docs/stdio-backends.md`](docs/stdio-backends.md).
> Both `github-mcp-server http` and `mcp-grafana` speak streamable-HTTP, which
> covers the multi-tenant case cleanly today.

## Auth modes

| mode       | verification                                              |
|------------|-----------------------------------------------------------|
| `jwks`     | signature verified against a JWKS endpoint                |
| `static`   | HMAC secret **or** a PEM public key from config           |
| `insecure` | claims parsed without verification (trusted-gateway only) |

In `jwks` mode, `jwks_url` is **optional**: when omitted, the JWKS endpoint is
discovered from the issuer's OIDC metadata
(`{issuer}/.well-known/openid-configuration` → `jwks_uri`), so configuring
`issuer` alone is enough. In all signed modes, `exp` is required and `iss`/`aud`
are enforced when set. Unauthenticated/insufficient-scope calls get RFC 9728
`WWW-Authenticate` challenges pointing at `/.well-known/oauth-protected-resource`.

## Observability

The proxy emits **metrics** and **traces** via OpenTelemetry.

**Metrics (Prometheus).** A *separate* listener (default `:9090`, configurable
under `observability:`) serves `/metrics` and `/healthz`, isolated from the
`/mcp` data plane. Set `metrics_listen: ""` to disable it. Recorded with OTel
GenAI MCP semantic conventions:

- `mcp_server_operation_duration` — histogram (seconds) of `tools/call` handling,
  labeled `mcp_method_name`, `mcp_tool_name`, `mcp_tenant_id`, `mcp_backend_name`,
  and `error_type` (on failures).
- `mcp_server_operation_count` — counter, same labels.
- `mcp_server_session_count` — counter of MCP sessions minted at `initialize`.

**Traces (OTLP).** Driven by the standard `OTEL_*` environment variables. Tracing
is enabled only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. A `tools/call` opens an
`mcp.tools/call` span; each downstream call opens an `mcp.backend.call` span. The
proxy always injects the W3C `traceparent` header downstream, so setting the same
`OTEL_*` vars on the backend correlates a single trace end-to-end.

## Run

```sh
go build ./cmd/mt-mcp-proxy
./mt-mcp-proxy -config config.yaml
```

Or with Docker Compose (proxy + `github-mcp-server http` together):

```sh
cd deploy && docker compose up
```

See [`config.example.yaml`](config.example.yaml) for the full config shape, and
[`deploy/`](deploy/) for a runnable example and the Helm chart pointer.

## Test

```sh
go test ./...
```

## Layout

- `internal/config`  — load & validate YAML (backends + `tenants:`); fail fast.
- `internal/auth`    — JWT verification (jwks/static/insecure) + group extraction.
- `internal/registry`— `tenant id → {backend, tenant, credential}` and
  `group → tenants` index.
- `internal/session` — client `Mcp-Session-Id` → per-tenant backend session map.
- `internal/proxy`   — MCP server (toward client) + MCP client (toward backends):
  `initialize`, cached `tools/list` transform, `tools/call` tenant routing and
  credential injection.
- `internal/observability` — OTel metrics (Prometheus exporter) + traces (OTLP),
  W3C trace-context propagation to backends.
- `cmd/mt-mcp-proxy`— wiring, HTTP server, `/healthz`, separate `/metrics`
  listener.

The design is documented in
`docs/superpowers/specs/2026-07-08-mt-mcp-proxy-design.md`.

## License

Apache License 2.0 — Copyright 2026 Andy Lo-A-Foe. See [`LICENSE`](LICENSE).
