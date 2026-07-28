# mt-mcp-proxy — Multi-Backend Helm Chart & mt-mcp-grafana Migration

**Date:** 2026-07-28
**Status:** Proposed
**Author:** Andy Lo-A-Foe (with Claude)
**Supersedes-in-part:** [2026-07-08-mt-mcp-proxy-design.md](2026-07-08-mt-mcp-proxy-design.md)
decision 4 ("one product per deployment" convention) — amended below for the
same-product / multiple-instances case.

## Summary

`mt-mcp-proxy` generalized `mt-mcp-grafana`'s engine so it could front any
streamable-HTTP MCP server (proven against `github-mcp-server`). The Go binary
already supports multiple backends structurally (`Config.Backends
[]BackendConfig`), but the Helm chart only exposes a single `backend:` object.
`mt-mcp-grafana`'s chart, by contrast, already supports a `backends: []` list
(each managed-or-external, with per-backend metrics/tracing) because it needs
to front N independent Grafana instances behind one proxy.

This spec generalizes the `mt-mcp-proxy` chart to the same `backends: []` shape,
then migrates the live `dip-ce-k3s-eu` Grafana deployment from `mt-mcp-grafana`
onto the upgraded `mt-mcp-proxy`, run side-by-side in a new namespace until
verified, then decommissions `mt-mcp-grafana`.

No changes to the Go binary are required — `internal/config`, `internal/proxy`,
`internal/registry`, etc. already handle N backends correctly. This is a chart
template + values-schema change, plus an operational migration.

## Motivation

- `mt-mcp-grafana` is a one-off binary duplicating the proxy engine that now
  lives generically in `mt-mcp-proxy`. Keeping it deployed means maintaining
  two codebases with the same auth/routing logic.
- The only thing blocking retirement is that `mt-mcp-proxy`'s chart can't yet
  express "3 Grafana instances, 2 managed + 1 external, each with its own SA
  token and tenants" the way `mt-mcp-grafana`'s chart can.
- Amending decision 4: "one product per deployment" was a convention, not an
  engine limitation. The Grafana use case is multiple *instances of the same
  product* behind one proxy (one `list_instances` call surfaces all of them) —
  this was always the design intent for `Config.Backends`, and `mt-mcp-grafana`
  proved the chart shape works. This spec makes that convention a supported
  chart pattern, not just a config-file capability.

## Decisions

1. **Chart-only change.** No Go code changes. `internal/config.BackendConfig`
   and `TenantConfig` already have every field needed (`URL`, `Headers`,
   `Credential`, `CredentialHeader`, `CredentialScheme`, per-tenant `Credential`
   overrides). Verified against `config.go` (dip-ce-k3s-eu review, 2026-07-28).
2. **`backend:` (singular object) → `backends:` (list)** in `values.yaml`,
   mirroring `mt-mcp-grafana`'s shape. Each entry is either:
   - **managed** (`managed.enabled: true`): chart deploys a Deployment+Service
     for that backend, same fields as today's singular `backend.managed.*`
     (image, command, args, port, env, resources) — just per-list-entry now.
   - **external** (`externalUrl` set): no pod deployed; proxy routes directly
     to the given URL. (Already supported in principle via `backend.externalUrl`;
     now expressible per-entry within a list.)
3. **Credentials stay tenant-scoped**, per the existing `mt-mcp-proxy` model
   (`tenants[].credentialSecret`), *not* a backend-level SA token like
   `mt-mcp-grafana`'s `service_account_token`. For the Grafana migration, each
   backend will have exactly one tenant, so this is equivalent in practice —
   no new backend-level-credential concept is introduced. (`config.go`'s
   backend-level `Credential` field already exists as a fallback and is
   reused for this, unchanged.)
4. **Full observability parity** (per your direction): bring `mt-mcp-grafana`'s
   per-backend metrics + ServiceMonitor and OTel-tracing-on-backends into the
   generalized chart, scoped to managed backends only (external backends are
   scraped/traced by whoever runs them — unchanged from `mt-mcp-grafana`'s
   existing behavior).
5. **Migration is side-by-side, not in-place.** Deploy the upgraded chart as a
   *new* Helm release in a *new* namespace, verify parity, then cut over
   picoclaw and decommission `mt-mcp-grafana` (namespace, Helm release, and —
   separately, once nothing depends on it — the `mt-mcp-grafana` git repo).
6. **Tool name changes.** Callers currently see `list_grafana_instances` and
   `mcp_mt-mcp-grafana_*`-prefixed tools (via picoclaw's server key). After
   cutover: `list_instances` (per the generic proxy's existing naming, no
   Grafana-specific tool name) and a new picoclaw server key,
   `mt-mcp-proxy-grafana` — distinct from the existing `mt-mcp-grafana` key so
   both run side-by-side without collision. This is a user-visible tool-name
   change; call it out in the cutover step so downstream agent
   instructions/soul text referencing the old name are updated.

## Chart changes (detail)

### values.yaml shape (target)

```yaml
backends:
  - name: ri-obs-use1-ct          # managed
    managed:
      enabled: true
      image:
        repository: grafana/mcp-grafana
        tag: "0.14.0"
      args: ["--transport", "streamable-http", "--address", "0.0.0.0:8000",
             "--disable-sift", "--disable-oncall", "--disable-incident"]
      port: 8000
      env:
        GRAFANA_URL: "https://gf.ri-obs-use1-ct.hsp.philips.com"
      resources: {}
    credentialHeader: Authorization   # backend-level default cred (Grafana SA token)
    credentialScheme: Bearer
    credentialSecret:
      name: ri-obs-use1-ct-grafana-token
      key: token
      create: false
    headers: {}
    tenants:
      - id: ri-obs-use1-ct
        groups: ["philips-internal:ri-obs-editors", "urn:iamr:...:dip-centcom-admins"]
        headers: {}

  - name: ops-sandbox              # external
    externalUrl: "http://grafana-mcp-ops-sandbox.grafana-mcp.svc.cluster.local:8000/mcp"
    credentialSecret:
      name: ops-sandbox-grafana-token
      key: token
      create: false
    tenants:
      - id: ops-sandbox
        groups: ["philips-internal:dip-oaas-admins", "urn:iamr:...:dip-centcom-admins"]
```

This requires a **new backend-level `credentialSecret`** field (distinct from
today's tenant-level `credentialSecret`), since the Grafana case authenticates
the *backend* to Grafana with one SA token shared by that backend's tenant(s) —
mirroring `config.go`'s existing backend-level `Credential` fallback field,
which today the chart never populates. This is a chart-values addition only;
`internal/config` already supports it.

### Templates to change

- `templates/proxy-configmap.yaml`: range over `.Values.backends` (was a single
  `.Values.backend`), emit backend-level `credential` (from a new backend
  `${ENV}` ref, mirroring the existing tenant `credEnv` helper) alongside
  existing per-tenant credential refs.
- `templates/backend.yaml` → rename semantics to iterate: for each backend with
  `managed.enabled`, emit a Deployment+Service named
  `{{ fullname }}-{{ backend.name }}-backend`, matching `mt-mcp-grafana`'s
  `{{ .Release.Name }}-{{ .name }}-mcp` per-backend naming pattern. Add the
  metrics container port + `backendMetrics` values block (enable flag, port,
  path) and OTel env-on-backend wiring, ported from `mt-mcp-grafana`'s
  `mcp-backends.yaml`.
- `templates/secrets.yaml`: extend to also create backend-level credential
  Secrets when `backends[].credentialSecret.create` + `.value` are set (same
  inline-dev-secret pattern already used for tenant secrets).
- `templates/proxy-deployment.yaml`: add backend-level `${ENV}` mappings
  alongside the existing per-tenant ones; add a new `mt-mcp-proxy.backendCredEnv`
  helper in `_helpers.tpl` (mirrors `credEnv`, keyed by backend name not tenant
  id).
- New `templates/servicemonitor-backends.yaml`, ported from
  `mt-mcp-grafana`'s, scoped to managed backends with `backendMetrics.enabled`.
- `values.yaml`: add `backendMetrics: {enabled, port, path, serviceMonitor:
  {enabled, namespace, labels, interval, scrapeTimeout, relabelings,
  metricRelabelings}}` at the top level (applies to all managed backends,
  matching `mt-mcp-grafana`'s single global block — no evidence multiple
  backends need different metrics ports/paths).
- `tracing.enableOnBackends` (bool) added alongside the existing `tracing.*`
  block, same semantics as `mt-mcp-grafana`.

### Chart tests / validation

- `helm template` snapshot for a 3-backend values file (2 managed + 1 external)
  reproducing today's `mt-mcp-grafana` deployment; diff-review the rendered
  ConfigMap against the config schema `internal/config.Load` accepts. This
  repo has no chart CI today; adding one is out of scope here — `helm lint` +
  `helm template` are run manually during implementation and again before the
  live migration, matching how the chart has been validated so far.
- No new Go tests needed (binary unchanged); existing `internal/config`,
  `internal/registry` test suites already cover N-backend configs generically.

## Migration plan (dip-ce-k3s-eu)

1. **Deploy side-by-side.** New Helm release (chart: the upgraded
   `mt-mcp-proxy` chart) in a **new namespace, `mt-mcp-proxy-grafana`**, values
   mirroring the 3 current
   `mt-mcp-grafana` backends (`ri-obs-use1-ct` managed, `ops-sandbox` external,
   `src-co-sb` managed) with identical `grafanaUrl`/`args`/SA-token-secret
   references. `mt-mcp-grafana` keeps running untouched in its own namespace.
2. **Wire into picoclaw as a second server.** Add a new entry under
   `mcp.servers` in `picoclaw/dip-ce-k3s-eu-values.yaml` (distinct key from
   `mt-mcp-grafana`) pointing at the new service's in-cluster DNS name, with
   `dynamic_headers.allowed: ["Authorization"]` — same pattern as the existing
   `mt-mcp-grafana` and `github-mcp` entries. Both servers active simultaneously.
3. **Verify parity**, per backend/tenant:
   - `tools/list` returns the same tool set (allowing for the
     `list_grafana_instances` → `list_instances` rename) as `mt-mcp-grafana`.
   - `list_instances` shows the same 3 tenants for the same test JWT groups
     `list_grafana_instances` shows today.
   - One real read query per tenant (e.g. `list_datasources` or
     `query_prometheus`) succeeds against each of the 3 backends, proving the
     SA token / routing / headers are correctly wired — mirrors the assertion
     style already used in `mt-mcp-proxy/E2E-TEST.md` for github.
   - Metrics: confirm the new ServiceMonitor scrapes both the proxy and the two
     managed backends.
4. **Cutover.** Point picoclaw's Grafana tool usage at the new server (either
   flip `enabled: false` on the old `mt-mcp-grafana` entry or remove it
   outright), restart picoclaw so it re-enumerates tools. Update any
   soul/instruction text in picoclaw values or centcom that references
   `list_grafana_instances` or the `mt-mcp-grafana` tool prefix by name.
5. **Decommission.** Delete the `mt-mcp-grafana` Helm release and namespace on
   `dip-ce-k3s-eu`. Separately (not blocking this migration): archive or
   deprecate the `mt-mcp-grafana` git repo once confirmed nothing else
   references it — out of scope for this spec's implementation plan, called
   out here so it isn't forgotten.

## Testing

- Chart: `helm lint` + `helm template` against the migration values file;
  manual review of rendered ConfigMap/Deployments/Secrets for the 3-backend
  case.
- Runtime: the parity checklist in Migration step 3 above, run against the
  real `dip-ce-k3s-eu` cluster with a real test JWT (per the existing
  `MINTING-TEST-JWT` flow documented in the innovation-day repo).
- No unit-test changes — Go binary is unmodified.

## Out of scope

- Any change to `internal/config`, `internal/proxy`, `internal/registry`, or
  other Go packages — the engine already supports this.
- stdio backends (unchanged from the 2026-07-08 design's exclusion).
- Archiving/deleting the `mt-mcp-grafana` git repository itself (flagged as a
  follow-up in step 5, not part of this implementation).
- Adding a public HTTPRoute for `mt-mcp-proxy` (unchanged — still
  cluster-internal only, per the existing `mt-mcp-proxy` DEPLOYMENT.md note).
