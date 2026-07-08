# stdio backends — future work

**Status:** not implemented in v1. mt-mcp-proxy currently supports **streamable-HTTP**
backends only. This document records the gap and the intended design so it can be
picked up later.

## The original goal

The project goal called for fronting **both stdio and streamable-HTTP** MCP
servers. v1 ships streamable-HTTP because it cleanly covers the primary targets:
`github-mcp-server http` and `mcp-grafana` both speak streamable-HTTP, and in
that mode the credential travels as a per-request header — which maps directly
onto the proxy's per-tenant credential injection. stdio is deliberately deferred,
not abandoned.

## Why stdio is fundamentally different

A stdio MCP server communicates over a single `stdin`/`stdout` JSON-RPC stream
and takes its credential **at process start** (an env var or flag), not per
request. Consequences:

- **One subprocess per credential.** Because the credential is baked in at spawn
  time, a single stdio process cannot serve two tenants with different
  credentials. The proxy must run (and manage) **one child process per tenant**
  (keyed by tenant id, mirroring how HTTP backend sessions are already
  tenant-keyed today).
- **Process lifecycle.** Lazily spawn on a tenant's first `tools/call`; keep it
  warm; reap on idle TTL and on `DELETE`; restart on crash. This is real
  supervision work with no analogue in the current stateless HTTP path.
- **Framing.** stdio uses newline-delimited (or `Content-Length`-framed) JSON-RPC
  over the pipe rather than HTTP request/response. A new transport layer parallel
  to `internal/proxy/client.go` is needed.
- **Concurrency.** A stdio pipe is a single ordered stream, so concurrent
  in-flight requests to one child must be multiplexed by JSON-RPC `id` and
  demultiplexed on the way back, or serialized per child.

## Intended design (when built)

- Add a `transport: http | stdio` field to `BackendConfig` (default `http`,
  preserving current behavior).
- For `stdio`, config gains `command`, `args`, and a `credential_env` naming the
  env var the child reads (e.g. `GITHUB_PERSONAL_ACCESS_TOKEN`). The proxy spawns
  `command args...` with `credential_env=<tenant credential>` in the child env.
- Introduce a `backendTransport` interface with two implementations:
  `httpBackend` (today's `backendClient`) and `stdioBackend` (a supervised child
  + a JSON-RPC-over-pipe codec + an id-multiplexer). `internal/registry` resolves
  a backend to the right transport; `internal/proxy` calls it through the
  interface, unchanged above that seam.
- A per-tenant process pool with idle-TTL reaping and crash-restart, sharing the
  existing session store's tenant keying.
- Tests: a fake stdio MCP server binary (or an in-process pipe pair) proving
  spawn-per-tenant, credential-env injection, reuse, idle reap, and crash restart.

## Scope note

This is a meaningful addition (process supervision, a second transport, pipe
framing) rather than a config tweak, which is why it is tracked separately. The
streamable-HTTP engine and the tenant/auth model above the transport seam are
already transport-agnostic, so adding stdio should not disturb them.
