# Multi-Backend Helm Chart & mt-mcp-grafana Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generalize the `mt-mcp-proxy` Helm chart from a single `backend:` object to a `backends: []` list (managed-or-external, full observability parity with `mt-mcp-grafana`'s chart), then migrate the live `dip-ce-k3s-eu` Grafana deployment from `mt-mcp-grafana` onto it — side-by-side first, then cut over and decommission.

**Architecture:** No Go code changes. `internal/config.Config.Backends []BackendConfig` and `TenantConfig` already support everything needed (multiple backends, per-tenant credentials, backend-level credential fallback via `EffectiveCredential`/`ReferenceCredential`). This plan only rewrites Helm chart templates/values in `deploy/helm/mt-mcp-proxy` to iterate a list instead of a single object, then deploys the upgraded chart as a new release for real-world verification against the 3 backends `mt-mcp-grafana` currently serves.

**Tech Stack:** Helm 3/4 (chart templates), Go 1.x (existing `mt-mcp-proxy` binary, unmodified), Kubernetes (`dip-ce-k3s-eu` cluster), Prometheus Operator `ServiceMonitor` CRDs.

## Global Constraints

- No changes to any Go package (`internal/config`, `internal/proxy`, `internal/registry`, `internal/auth`, `internal/session`, `internal/observability`, `cmd/mt-mcp-proxy`) — chart-only work, per spec decision 1.
- New chart values field: `backends: []` replaces the singular `backend:` object entirely (not additive/back-compat — this repo has no other consumers of the old shape yet, confirmed via `grep` across `/Users/andy/DEV/Philips/innovation-day` finding only the one github-mcp deployment, which this plan updates in Task 6).
- Each backend entry supports exactly one of `managed.enabled: true` (chart deploys Deployment+Service) or `externalUrl` (routes to existing endpoint) — mirrors `mt-mcp-grafana`'s existing `templates/mcp-backends.yaml` `{{- if not .externalUrl }}` pattern.
- Backend-level `credentialSecret` (new) is distinct from existing tenant-level `credentialSecret` — both must be supported since Grafana backends use backend-level SA tokens while github used tenant-level PATs.
- Full observability parity: per-backend metrics + `ServiceMonitor` (managed backends only) and OTel tracing-on-backends, ported from `mt-mcp-grafana`'s `backendMetrics` block and `tracing.enableOnBackends` flag.
- Migration target values mirror today's live `mt-mcp-grafana` deployment on `dip-ce-k3s-eu`: 3 backends (`ri-obs-use1-ct` managed, `ops-sandbox` external, `src-co-sb` managed), each with an existing Kubernetes Secret already in the cluster (`ri-obs-use1-ct-grafana-token`, `ops-sandbox-grafana-token`, `src-co-sb-grafana-token` — confirmed present via `kubectl get secrets -n mt-mcp-grafana`).
- Side-by-side deployment: new Helm release in a **new namespace `mt-mcp-proxy-grafana`** (confirmed not already in use on the cluster) — `mt-mcp-grafana` keeps running untouched throughout verification.
- New picoclaw MCP server key: `mt-mcp-proxy-grafana` (distinct from existing `mt-mcp-grafana` key) — both active simultaneously until cutover.
- Discovery tool name changes from `list_grafana_instances` (old) to `list_instances` (new, already the proxy's built-in name — no chart change needed for this, it's inherent to using `mt-mcp-proxy` instead of `mt-mcp-grafana`).
- Kubeconfig for all cluster operations: `/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml`.

---

### Task 1: Convert `values.yaml` from singular `backend` to `backends` list

**Files:**
- Modify: `deploy/helm/mt-mcp-proxy/values.yaml`

**Interfaces:**
- Consumes: nothing (first task)
- Produces: the `values.yaml` shape every later template task reads —
  `.Values.backends` (list, replacing `.Values.backend`), each entry with
  fields `name`, `managed{enabled, image{repository,tag,pullPolicy}, command,
  args, port, env, credentialEnvVar, resources}`, `externalUrl`,
  `credentialHeader`, `credentialScheme`, `headers`,
  `credentialSecret{name,key,create,value}` (backend-level, new),
  `tenants[]{id, groups, headers, credentialSecret{name,key,create,value}}`
  (tenant-level credentialSecret is optional per-tenant override, unchanged
  shape from today). `managed.credentialEnvVar`, when non-empty, is the env
  var name the managed binary itself expects for its upstream credential
  (e.g. `GRAFANA_SERVICE_ACCOUNT_TOKEN`) — distinct from the
  `credentialHeader`/`credentialScheme` fields, which control how the PROXY
  injects a credential toward a backend that takes it as a request header
  (github-mcp-server's model). Also produces top-level
  `backendMetrics{enabled,port,path,serviceMonitor{enabled,
  namespace,labels,interval,scrapeTimeout,relabelings,metricRelabelings}}` and
  `tracing.enableOnBackends` (bool).

- [ ] **Step 1: Replace the `backend:` block with `backends:` list**

Open `deploy/helm/mt-mcp-proxy/values.yaml`. Replace the entire `# -- The
single downstream MCP backend this deployment fronts.` section (currently
`backend:` through the end of its `externalUrl` comment, i.e. everything from
the `backend:` key down to just before `# -- Tenants this deployment serves.`)
with:

```yaml
# -- The downstream MCP backends this deployment fronts. Each entry is either
# MANAGED (the chart deploys a Deployment+Service for it) or EXTERNAL (you
# point at an already-running streamable-HTTP MCP server). One deployment can
# front multiple backends of the same product (e.g. several Grafana
# instances) or, by convention, a single product.
backends:
  - # -- Internal/ops name (logging, DNS of the managed backend service).
    # Must be unique across all entries in this list.
    name: github
    # -- How the credential is injected downstream. Defaults suit
    # github-mcp-server (Authorization: Bearer <token>). For an API-key
    # backend set e.g. credentialHeader: X-Api-Key and credentialScheme: "".
    credentialHeader: Authorization
    credentialScheme: Bearer
    # -- Optional static headers sent toward this backend on every request
    headers: {}

    # -- Backend-level credential (optional). Used as the downstream token
    # when a tenant of this backend does not supply its own credentialSecret
    # (e.g. github: each tenant has its own PAT, so this is usually empty; a
    # Grafana backend: one SA token shared by that backend's tenant(s), so
    # this is set and per-tenant credentialSecret is omitted). Provided via a
    # Secret so it never lands in the rendered ConfigMap.
    credentialSecret:
      # -- Secret name holding this backend's credential (empty = none)
      name: ""
      # -- Key in the Secret
      key: token
      # -- Have the chart create the Secret inline from `value` (dev/testing)
      create: false
      # -- Raw credential (only used when create is true)
      value: ""

    # -- Managed backend: when enabled, the chart deploys the downstream MCP
    # server (a Deployment + Service) and wires the proxy to it via in-cluster
    # DNS. When disabled, set `externalUrl` to a pre-existing streamable-HTTP
    # MCP endpoint (the proxy routes directly to it; nothing is deployed).
    # SECURITY: an external/managed backend has NO inbound auth of its own —
    # it must be reachable only from the proxy (NetworkPolicy / cluster-
    # internal), since anyone who can reach it bypasses the proxy's JWT
    # verification.
    managed:
      # -- Deploy the downstream MCP server as part of this release
      enabled: true
      image:
        # -- Downstream MCP server image (default: github-mcp-server)
        repository: ghcr.io/github/github-mcp-server
        tag: "latest"
        pullPolicy: IfNotPresent
      # -- Command/args to launch the backend in streamable-HTTP mode.
      # github-mcp-server: `http` serves MCP at / on :8082.
      command: []
      args: ["http"]
      # -- Container port the backend's MCP endpoint listens on
      port: 8082
      # -- Env vars for the backend container (e.g. GITHUB_HOST for GHES). Do
      # NOT put per-tenant tokens here — those are injected per request by the
      # proxy.
      env: {}
      # -- If this backend needs its OWN upstream credential injected as an
      # env var (as opposed to the proxy injecting it as a header toward
      # github-mcp-server), set the env var name the binary expects here,
      # e.g. "GRAFANA_SERVICE_ACCOUNT_TOKEN" for grafana/mcp-grafana. The
      # value comes from this backend's credentialSecret (below). Leave
      # empty for backends like github-mcp-server that take no credential of
      # their own (the proxy is the one authenticating to them).
      credentialEnvVar: ""
      resources: {}

    # -- External backend URL (used only when managed.enabled is false), e.g.
    # "http://github-mcp-server.example.svc:8082/".
    externalUrl: ""

    # -- Tenants this backend serves. Model: groups authorize, tenant
    # selects, backend executes. Each tenant's id is advertised via
    # list_instances and is the `tenant` tool-argument value; its groups gate
    # access; its credential (if set) overrides the backend-level credential
    # above for calls routed to this tenant.
    tenants:
      - # -- Globally unique tenant id (routing key) across ALL backends
        id: team-platform
        # -- JWT groups authorized to select this tenant
        groups: ["platform-eng"]
        # -- Optional per-tenant headers (merge over backend headers)
        headers:
          X-MCP-Toolsets: "repos,issues,pull_requests"
        # -- Per-tenant credential override (optional). Omit name to fall
        # back to the backend-level credentialSecret above.
        credentialSecret:
          name: github-pat-platform
          key: token
          create: false
          value: ""
      - id: team-data
        groups: ["data-eng"]
        headers:
          X-MCP-Readonly: "true"
        credentialSecret:
          name: github-pat-data
          key: token
          create: false
          value: ""

# -- Metrics for MANAGED backend pods (external backends are scraped by
# whoever runs them). Applies uniformly to every managed backend in the
# `backends` list — matches mt-mcp-grafana's single global block; no evidence
# different backends need different metrics ports/paths.
backendMetrics:
  # -- Enable a metrics container port + env on managed backends
  enabled: false
  # -- Port managed backends serve Prometheus metrics on
  port: 9090
  # -- Path managed backends serve metrics on
  path: /metrics
  serviceMonitor:
    # -- Enable ServiceMonitor creation for managed-backend metrics
    enabled: false
    # -- Namespace for the ServiceMonitor (defaults to release namespace)
    namespace: ""
    # -- Additional labels for ServiceMonitor discovery
    labels: {}
    # -- Scrape interval
    interval: "30s"
    # -- Scrape timeout
    scrapeTimeout: "10s"
    # -- Relabeling rules applied before scraping
    relabelings: []
    # -- Metric relabeling rules applied after scraping
    metricRelabelings: []
```

Also delete the old top-level `tenants:` block entirely (the one that
currently follows `backend:` in the file, with `team-platform` /
`team-data` entries) — it has been folded into `backends[].tenants` above, so
a top-level `tenants:` key must no longer exist in the file. Confirm by
running `grep -n "^tenants:" deploy/helm/mt-mcp-proxy/values.yaml` after this
step: it must print nothing.

- [ ] **Step 2: Add `tracing.enableOnBackends`**

In the existing `tracing:` block (near the top of the file, already has
`enabled`, `endpoint`, `serviceName`, `extraEnv`), add one field:

```yaml
  # -- Also set OTEL_* env on managed backend pods so they join the proxy's
  # trace. OTEL_SERVICE_NAME is derived per backend (<backend-name>-backend).
  enableOnBackends: false
```

- [ ] **Step 3: Verify the YAML parses**

Run: `cd /Users/andy/DEV/Go/mt-mcp-proxy && python3 -c "import yaml; yaml.safe_load(open('deploy/helm/mt-mcp-proxy/values.yaml'))" && echo OK`
Expected: `OK` (no exception). If it raises, fix indentation before continuing
— every subsequent task's templates assume this file parses cleanly.

- [ ] **Step 4: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/values.yaml
git commit -m "feat(chart): convert backend to backends list in values.yaml"
```

---

### Task 2: Add a backend-credential env-name helper to `_helpers.tpl`

**Files:**
- Modify: `deploy/helm/mt-mcp-proxy/templates/_helpers.tpl`

**Interfaces:**
- Consumes: nothing new (pure template helper)
- Produces: `mt-mcp-proxy.backendCredEnv` — call with a backend name string,
  returns an env var name like `BACKEND_GITHUB_CREDENTIAL`. Used by Task 3
  (configmap), Task 4 (backend deployment→proxy env wiring), and Task 5
  (secrets). Mirrors the existing `mt-mcp-proxy.credEnv` helper (tenant-keyed)
  already in this file — this one is backend-keyed.

- [ ] **Step 1: Add the helper**

Open `deploy/helm/mt-mcp-proxy/templates/_helpers.tpl`. After the existing
`mt-mcp-proxy.credEnv` definition (the last block in the file), append:

```
{{/*
The env var name a backend's own credential is mapped into
(BACKEND_<NAME>_CREDENTIAL, uppercased, non-alphanumerics → underscore). Used
to keep secrets out of the rendered ConfigMap: the config references ${VAR},
resolved from this env var. Call with the backend name string, e.g.
{{ include "mt-mcp-proxy.backendCredEnv" .name }}.
*/}}
{{- define "mt-mcp-proxy.backendCredEnv" -}}
{{- $raw := printf "BACKEND_%s_CREDENTIAL" . | upper -}}
{{- regexReplaceAll "[^A-Z0-9_]" $raw "_" -}}
{{- end }}
```

- [ ] **Step 2: Verify the template renders in isolation**

Run:
```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
helm template test deploy/helm/mt-mcp-proxy \
  --show-only templates/proxy-service.yaml >/dev/null && echo OK
```
Expected: `OK`. This exercises `_helpers.tpl` parsing (via any template that
imports it) without yet depending on the not-done `backends` iteration in
other templates — `proxy-service.yaml` doesn't reference `.Values.backends`,
so it renders fine even mid-migration. If this fails with a `_helpers.tpl`
parse error, fix the `define` block syntax before continuing.

- [ ] **Step 3: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/templates/_helpers.tpl
git commit -m "feat(chart): add backendCredEnv helper for per-backend credentials"
```

---

### Task 3: Rewrite `proxy-configmap.yaml` to render a `backends:` list

**Files:**
- Modify: `deploy/helm/mt-mcp-proxy/templates/proxy-configmap.yaml`

**Interfaces:**
- Consumes: `.Values.backends[]` (Task 1 shape); `mt-mcp-proxy.credEnv`
  (existing, tenant-keyed) and `mt-mcp-proxy.backendCredEnv` (Task 2,
  backend-keyed).
- Produces: rendered `config.yaml` with a `backends:` list matching
  `internal/config.Config.Backends []BackendConfig` exactly — each entry has
  `name`, `url`, `credential` (backend-level, `${ENV}` ref, only emitted when
  `credentialSecret.name` is set), `credential_header`, `credential_scheme`,
  `headers`, `tenants[]{id, groups, credential (${ENV} ref, only emitted when
  that tenant has its own `credentialSecret.name`), headers}`. This is what
  Task 6 (values file) and Task 7 (E2E verification) depend on being correct.

- [ ] **Step 1: Replace the `backends:` section of the template**

Open `deploy/helm/mt-mcp-proxy/templates/proxy-configmap.yaml`. Everything
above the `backends:` key (the `server:`, `observability:`, `auth:` blocks)
is unchanged — leave it as-is. Replace everything from `backends:` to the end
of the file with:

```yaml
    backends:
      {{- range .Values.backends }}
      - name: "{{ .name }}"
        {{- if .managed.enabled }}
        url: "http://{{ include "mt-mcp-proxy.fullname" $ }}-{{ .name }}-backend:{{ .managed.port | default 8082 }}/"
        {{- else }}
        url: "{{ required (printf "backends[%s].externalUrl is required when managed.enabled is false" .name) .externalUrl }}"
        {{- end }}
        {{- if .credentialSecret.name }}
        credential: {{ printf "${%s}" (include "mt-mcp-proxy.backendCredEnv" .name) | quote }}
        {{- end }}
        credential_header: "{{ .credentialHeader | default "Authorization" }}"
        credential_scheme: "{{ .credentialScheme | default "Bearer" }}"
        {{- with .headers }}
        headers:
          {{- range $k, $v := . }}
          {{ $k }}: {{ $v | quote }}
          {{- end }}
        {{- end }}
        tenants:
          {{- range .tenants }}
          - id: "{{ .id }}"
            groups:
              {{- range .groups }}
              - "{{ . }}"
              {{- end }}
            {{- if .credentialSecret.name }}
            credential: {{ printf "${%s}" (include "mt-mcp-proxy.credEnv" .id) | quote }}
            {{- end }}
            {{- with .headers }}
            headers:
              {{- range $k, $v := . }}
              {{ $k }}: {{ $v | quote }}
              {{- end }}
            {{- end }}
          {{- end }}
      {{- end }}
```

Note the `$` (root context) used inside `include "mt-mcp-proxy.fullname" $` —
required because we're inside a `range .Values.backends` block, where `.` is
now the backend entry, not the root. The existing single-backend template
didn't need this since it wasn't inside a range.

- [ ] **Step 2: Render against a minimal 1-backend values override and inspect output**

Run:
```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
cat > /tmp/mt-mcp-proxy-test-values.yaml <<'EOF'
auth:
  issuer: "https://issuer.example.com"
  audience: "test"
backends:
  - name: github
    managed:
      enabled: true
      port: 8082
    tenants:
      - id: team-platform
        groups: ["platform-eng"]
        credentialSecret:
          name: github-pat-platform
EOF
helm template test deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values.yaml \
  --show-only templates/proxy-configmap.yaml
```
Expected output includes:
```
    backends:
      - name: "github"
        url: "http://test-mt-mcp-proxy-github-backend:8082/"
        credential_header: "Authorization"
        credential_scheme: "Bearer"
        tenants:
          - id: "team-platform"
            groups:
              - "platform-eng"
            credential: "${TENANT_TEAM_PLATFORM_CREDENTIAL}"
```
(exact release-name prefix may differ; the important checks are: `backends:`
is a list with one `- name:` entry, the `url` points at
`<fullname>-github-backend:8082`, and the tenant's `credential` line resolves
to the `credEnv` helper's env var name.) If the output doesn't match this
shape, fix the template before continuing — Task 6 depends on this rendering
correctly for 3 Grafana backends.

- [ ] **Step 3: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/templates/proxy-configmap.yaml
git commit -m "feat(chart): render backends list in proxy configmap"
```

---

### Task 4: Rewrite `backend.yaml` to deploy one Deployment+Service per managed backend

**Files:**
- Modify: `deploy/helm/mt-mcp-proxy/templates/backend.yaml`

**Interfaces:**
- Consumes: `.Values.backends[]` (Task 1); `.Values.backendMetrics` (Task 1);
  `.Values.tracing.enabled` / `.Values.tracing.enableOnBackends` (existing +
  Task 1); `mt-mcp-proxy.fullname`, `mt-mcp-proxy.labels`,
  `mt-mcp-proxy.selectorLabels` (existing helpers).
- Produces: for each backend with `managed.enabled: true`, a Deployment named
  `{{ fullname }}-{{ backend.name }}-backend` and Service of the same name
  exposing port `managed.port` (named `mcp`) and, when `backendMetrics.enabled`,
  a second port (named `metrics`) at `backendMetrics.port`. This Service name
  is exactly what Task 3's configmap template already assumes
  (`{{ fullname }}-{{ .name }}-backend`) — the names MUST match verbatim.

- [ ] **Step 1: Replace the whole file**

Open `deploy/helm/mt-mcp-proxy/templates/backend.yaml`. Replace its entire
contents (currently a single `{{- if .Values.backend.managed.enabled }}`
block) with:

```yaml
{{- range .Values.backends }}
{{- if .managed.enabled }}
{{- $port := .managed.port | default 8082 }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "mt-mcp-proxy.fullname" $ }}-{{ .name }}-backend
  labels:
    {{- include "mt-mcp-proxy.labels" $ | nindent 4 }}
    app.kubernetes.io/component: backend
    mcp-backend: "{{ .name }}"
spec:
  replicas: 1
  selector:
    matchLabels:
      {{- include "mt-mcp-proxy.selectorLabels" $ | nindent 6 }}
      app.kubernetes.io/component: backend
      mcp-backend: "{{ .name }}"
  template:
    metadata:
      labels:
        {{- include "mt-mcp-proxy.selectorLabels" $ | nindent 8 }}
        app.kubernetes.io/component: backend
        mcp-backend: "{{ .name }}"
    spec:
      containers:
        - name: backend
          image: "{{ .managed.image.repository }}:{{ .managed.image.tag }}"
          imagePullPolicy: {{ .managed.image.pullPolicy | default "IfNotPresent" }}
          {{- with .managed.command }}
          command:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with .managed.args }}
          args:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          ports:
            - name: mcp
              containerPort: {{ $port }}
              protocol: TCP
            {{- if $.Values.backendMetrics.enabled }}
            - name: metrics
              containerPort: {{ $.Values.backendMetrics.port | default 9090 }}
              protocol: TCP
            {{- end }}
          env:
            {{- range $k, $v := .managed.env }}
            - name: "{{ $k }}"
              value: "{{ $v }}"
            {{- end }}
            {{- if and .credentialSecret.name .managed.credentialEnvVar }}
            - name: "{{ .managed.credentialEnvVar }}"
              valueFrom:
                secretKeyRef:
                  name: "{{ .credentialSecret.name }}"
                  key: "{{ .credentialSecret.key | default "token" }}"
            {{- end }}
            {{- if and $.Values.tracing.enabled $.Values.tracing.enableOnBackends }}
            - name: OTEL_EXPORTER_OTLP_ENDPOINT
              value: "{{ $.Values.tracing.endpoint }}"
            - name: OTEL_SERVICE_NAME
              value: "{{ .name }}-backend"
            {{- range $k, $v := $.Values.tracing.extraEnv }}
            - name: "{{ $k }}"
              value: "{{ $v }}"
            {{- end }}
            {{- end }}
          resources:
            {{- toYaml .managed.resources | nindent 12 }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ include "mt-mcp-proxy.fullname" $ }}-{{ .name }}-backend
  labels:
    {{- include "mt-mcp-proxy.labels" $ | nindent 4 }}
    app.kubernetes.io/component: backend
    mcp-backend: "{{ .name }}"
spec:
  type: ClusterIP
  ports:
    - port: {{ $port }}
      targetPort: mcp
      protocol: TCP
      name: mcp
    {{- if $.Values.backendMetrics.enabled }}
    - port: {{ $.Values.backendMetrics.port | default 9090 }}
      targetPort: metrics
      protocol: TCP
      name: metrics
    {{- end }}
  selector:
    {{- include "mt-mcp-proxy.selectorLabels" $ | nindent 4 }}
    app.kubernetes.io/component: backend
    mcp-backend: "{{ .name }}"
{{- end }}
{{- end }}
```

Note: when `managed.credentialEnvVar` is set (e.g. `GRAFANA_SERVICE_ACCOUNT_TOKEN`
for a Grafana backend, per Task 6), this template injects that named env var
directly into the **backend** pod from `credentialSecret`, so the backend
binary itself has its own upstream credential — mirroring exactly what
`mt-mcp-grafana`'s `mcp-backends.yaml` does today for `mcp-grafana` pods. This
is a SEPARATE mechanism from the `BACKEND_<NAME>_CREDENTIAL` env var Task
3/Task 5 wire into the **proxy** pod: the proxy also sends that same token as
an `Authorization` header on its own requests toward the backend (this is
what `mt-mcp-grafana`'s existing `client.go` `applyAuth` does, and
`mt-mcp-proxy`'s equivalent code path — `EffectiveCredential` +
`CredentialHeader`/`CredentialScheme` — is unchanged and already does this).
Both env vars can read from the same Secret; they serve two different
consumers (the backend binary's own upstream auth vs. the proxy's outbound
request header) and both are required for Grafana backends to match today's
working `mt-mcp-grafana` behavior. Backends like github-mcp-server need only
the proxy-side mechanism (no `credentialEnvVar`), since they read their
credential exclusively from the incoming `Authorization` header the proxy
sets.

- [ ] **Step 2: Render with a managed Grafana-shaped backend and inspect**

Run:
```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
cat > /tmp/mt-mcp-proxy-test-values2.yaml <<'EOF'
auth:
  issuer: "https://issuer.example.com"
  audience: "test"
backendMetrics:
  enabled: true
backends:
  - name: grafana-a
    managed:
      enabled: true
      image:
        repository: grafana/mcp-grafana
        tag: "0.14.0"
      args: ["--transport", "streamable-http", "--address", "0.0.0.0:8000"]
      port: 8000
      env:
        GRAFANA_URL: "https://grafana.example.com"
      credentialEnvVar: "GRAFANA_SERVICE_ACCOUNT_TOKEN"
    credentialSecret:
      name: grafana-a-token
    tenants:
      - id: team-a
        groups: ["team-a"]
EOF
helm template test deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values2.yaml \
  --show-only templates/backend.yaml
```
Expected: a `Deployment` named `test-mt-mcp-proxy-grafana-a-backend` with two
container ports (`mcp` 8000, `metrics` 9090), a `GRAFANA_SERVICE_ACCOUNT_TOKEN`
env var sourced from `secretKeyRef.name: grafana-a-token`, and a matching
`Service` with two ports. If `helm template` errors instead of rendering, fix
the template — common mistake is forgetting `$.` vs `.` inside the nested
`range`.

- [ ] **Step 3: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/templates/backend.yaml
git commit -m "feat(chart): deploy one Deployment+Service per managed backend"
```

---

### Task 5: Update `secrets.yaml` and `proxy-deployment.yaml` for nested backend→tenant credentials

**Files:**
- Modify: `deploy/helm/mt-mcp-proxy/templates/secrets.yaml`
- Modify: `deploy/helm/mt-mcp-proxy/templates/proxy-deployment.yaml`

**Interfaces:**
- Consumes: `.Values.backends[].credentialSecret` (backend-level, Task 1),
  `.Values.backends[].tenants[].credentialSecret` (tenant-level, Task 1),
  `mt-mcp-proxy.credEnv` (existing), `mt-mcp-proxy.backendCredEnv` (Task 2).
- Produces: inline dev/test Secrets for both backend-level and tenant-level
  credentials when `credentialSecret.create: true`; proxy container env vars
  `${TENANT_<ID>_CREDENTIAL}` (unchanged name, now sourced from inside the
  nested loop) and `${BACKEND_<NAME>_CREDENTIAL}` (new) that Task 3's
  rendered config references.

- [ ] **Step 1: Rewrite `secrets.yaml` to iterate backends, then tenants, plus backend-level secrets**

Replace the entire contents of `deploy/helm/mt-mcp-proxy/templates/secrets.yaml`
with:

```yaml
{{- range .Values.backends }}
{{- if and .credentialSecret.create .credentialSecret.value }}
---
apiVersion: v1
kind: Secret
metadata:
  name: "{{ .credentialSecret.name }}"
  labels:
    {{- include "mt-mcp-proxy.labels" $ | nindent 4 }}
type: Opaque
stringData:
  {{ .credentialSecret.key | default "token" }}: "{{ .credentialSecret.value }}"
{{- end }}
{{- range .tenants }}
{{- if and .credentialSecret.create .credentialSecret.value }}
---
apiVersion: v1
kind: Secret
metadata:
  name: "{{ .credentialSecret.name }}"
  labels:
    {{- include "mt-mcp-proxy.labels" $ | nindent 4 }}
type: Opaque
stringData:
  {{ .credentialSecret.key | default "token" }}: "{{ .credentialSecret.value }}"
{{- end }}
{{- end }}
{{- end }}
```

- [ ] **Step 2: Update `proxy-deployment.yaml`'s env block**

Open `deploy/helm/mt-mcp-proxy/templates/proxy-deployment.yaml`. Find the
comment block:
```yaml
            # Map each tenant's credential Secret into the ${ENV} the rendered
            # config references, so secrets never appear in the ConfigMap.
            {{- range .Values.tenants }}
            - name: "{{ include "mt-mcp-proxy.credEnv" .id }}"
              valueFrom:
                secretKeyRef:
                  name: "{{ .credentialSecret.name }}"
                  key: "{{ .credentialSecret.key | default "token" }}"
            {{- end }}
```
Replace it with:
```yaml
            # Map each backend's own credential Secret (if set) and each of
            # its tenants' credential Secrets into the ${ENV} vars the
            # rendered config references, so secrets never appear in the
            # ConfigMap.
            {{- range .Values.backends }}
            {{- if .credentialSecret.name }}
            - name: "{{ include "mt-mcp-proxy.backendCredEnv" .name }}"
              valueFrom:
                secretKeyRef:
                  name: "{{ .credentialSecret.name }}"
                  key: "{{ .credentialSecret.key | default "token" }}"
            {{- end }}
            {{- range .tenants }}
            {{- if .credentialSecret.name }}
            - name: "{{ include "mt-mcp-proxy.credEnv" .id }}"
              valueFrom:
                secretKeyRef:
                  name: "{{ .credentialSecret.name }}"
                  key: "{{ .credentialSecret.key | default "token" }}"
            {{- end }}
            {{- end }}
            {{- end }}
```

- [ ] **Step 3: Render the full chart against the two earlier test-values files and check for errors**

Run:
```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
helm template test deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values.yaml >/dev/null && echo OK1
helm template test deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values2.yaml >/dev/null && echo OK2
helm lint deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values.yaml
helm lint deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values2.yaml
```
Expected: `OK1`, `OK2`, and both `helm lint` runs report `0 chart(s) failed`.
The full chart render (not just `--show-only` on one template) is the real
check here — it exercises every template file against both a github-shaped
and a Grafana-shaped values file simultaneously, catching any cross-template
naming mismatch (e.g. Task 3's configmap URL vs Task 4's Service name).

- [ ] **Step 4: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/templates/secrets.yaml deploy/helm/mt-mcp-proxy/templates/proxy-deployment.yaml
git commit -m "feat(chart): wire backend-level and nested tenant credentials"
```

---

### Task 6: Add `servicemonitor-backends.yaml` for managed-backend metrics

**Files:**
- Create: `deploy/helm/mt-mcp-proxy/templates/servicemonitor-backends.yaml`

**Interfaces:**
- Consumes: `.Values.backendMetrics` (Task 1), `mt-mcp-proxy.fullname`,
  `mt-mcp-proxy.labels`, `mt-mcp-proxy.selectorLabels` (existing helpers), the
  `mcp-backend: "{{ .name }}"` Service label Task 4 already sets.
- Produces: one `ServiceMonitor` (when `backendMetrics.serviceMonitor.enabled`)
  scoped to `app.kubernetes.io/component: backend`, matching
  `mt-mcp-grafana`'s existing `servicemonitor-backends.yaml` pattern exactly
  (including its `targetLabels` promotion so each backend's series stay
  distinguishable).

- [ ] **Step 1: Create the file**

```yaml
{{- if .Values.backendMetrics.serviceMonitor.enabled }}
{{- if not .Values.backendMetrics.enabled }}
{{- fail "backendMetrics.serviceMonitor.enabled requires backendMetrics.enabled" }}
{{- end }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "mt-mcp-proxy.fullname" . }}-backends
  {{- with .Values.backendMetrics.serviceMonitor.namespace }}
  namespace: {{ . }}
  {{- end }}
  labels:
    {{- include "mt-mcp-proxy.labels" . | nindent 4 }}
    app.kubernetes.io/component: backend
    {{- with .Values.backendMetrics.serviceMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchLabels:
      {{- include "mt-mcp-proxy.selectorLabels" . | nindent 6 }}
      app.kubernetes.io/component: backend
  targetLabels:
    - mcp-backend
  endpoints:
    - port: metrics
      path: {{ .Values.backendMetrics.path | default "/metrics" }}
      interval: {{ .Values.backendMetrics.serviceMonitor.interval | default "30s" }}
      scrapeTimeout: {{ .Values.backendMetrics.serviceMonitor.scrapeTimeout | default "10s" }}
      {{- with .Values.backendMetrics.serviceMonitor.relabelings }}
      relabelings:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.backendMetrics.serviceMonitor.metricRelabelings }}
      metricRelabelings:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end }}
```

Note this targets label key `mcp-backend` (matching Task 4's Service label),
not `mcp-instance` (the key `mt-mcp-grafana`'s equivalent template uses) —
naming is intentionally updated to the generic chart's vocabulary
(backends, not instances).

- [ ] **Step 2: Render with backend metrics + ServiceMonitor enabled**

Run:
```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
cat >> /tmp/mt-mcp-proxy-test-values2.yaml <<'EOF'
backendMetrics:
  enabled: true
  serviceMonitor:
    enabled: true
EOF
helm template test deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values2.yaml \
  --show-only templates/servicemonitor-backends.yaml
```
Expected: a `ServiceMonitor` named `test-mt-mcp-proxy-backends` with
`matchLabels` including `app.kubernetes.io/component: backend` and
`targetLabels: [mcp-backend]`. If nothing prints, check that
`backendMetrics.enabled: true` also appears somewhere in the merged values
(the `{{- fail ... }}` guard silently produces no output only when the `if`
above it is false — but the `fail` call itself would abort the whole render
with a clear error if `serviceMonitor.enabled` were true and
`backendMetrics.enabled` false, so an empty result here means
`serviceMonitor.enabled` evaluated false — re-check the appended YAML actually
merged, since `cat >>` at Step 2 appends a second `backendMetrics:` key to a
file that may already define one from Task 4 Step 2, and YAML doesn't merge
duplicate top-level keys — the LAST one wins). To avoid that trap, instead
regenerate the whole file fresh:
```bash
cat > /tmp/mt-mcp-proxy-test-values2.yaml <<'EOF'
auth:
  issuer: "https://issuer.example.com"
  audience: "test"
backendMetrics:
  enabled: true
  serviceMonitor:
    enabled: true
backends:
  - name: grafana-a
    managed:
      enabled: true
      image:
        repository: grafana/mcp-grafana
        tag: "0.14.0"
      args: ["--transport", "streamable-http", "--address", "0.0.0.0:8000"]
      port: 8000
      env:
        GRAFANA_URL: "https://grafana.example.com"
      credentialEnvVar: "GRAFANA_SERVICE_ACCOUNT_TOKEN"
    credentialSecret:
      name: grafana-a-token
    tenants:
      - id: team-a
        groups: ["team-a"]
EOF
helm template test deploy/helm/mt-mcp-proxy -f /tmp/mt-mcp-proxy-test-values2.yaml \
  --show-only templates/servicemonitor-backends.yaml
```

- [ ] **Step 3: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/templates/servicemonitor-backends.yaml
git commit -m "feat(chart): add ServiceMonitor for managed-backend metrics"
```

---

### Task 7: Bump chart version and migrate the live github-mcp deployment's values file to the new schema

**Files:**
- Modify: `deploy/helm/mt-mcp-proxy/Chart.yaml`
- Modify: `/Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu.yaml`

**Interfaces:**
- Consumes: the `backends: []` schema from Task 1; the rendering behavior
  verified in Tasks 3–6.
- Produces: a `Chart.yaml` version bump signaling the breaking values change,
  and an updated github-mcp values file so the ALREADY-LIVE `github-mcp`
  Helm release on `dip-ce-k3s-eu` (namespace `github-mcp`) keeps working —
  this is the one existing consumer of the old singular `backend:` shape and
  must not break. This is the compatibility concern flagged mid-plan: the
  `backends: []` rename is breaking for chart consumers, but the migration
  here is a values-file-only change (no Go/binary changes), so the live
  github deployment adapts by re-running `helm upgrade` with an updated
  values file — nothing about its actual behavior (routing, auth, PAT
  injection) changes.

- [ ] **Step 1: Bump the chart version**

Open `deploy/helm/mt-mcp-proxy/Chart.yaml`. Change:
```yaml
version: 0.1.0
```
to:
```yaml
version: 0.2.0
```
(minor bump — the chart gains capability and changes its values schema in a
breaking way, but this is a pre-1.0 chart with a single known consumer being
updated in this same task, not a wide breaking release requiring a major bump).

- [ ] **Step 2: Convert the github-mcp values file to the new schema**

Read the current file first:
```bash
cat /Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu.yaml
```
It currently has a top-level `backend:` object (name `github`, managed
image `ghcr.io/github/github-mcp-server`, `args: ["http"]`, port 8082, env
`GITHUB_READ_ONLY`/`GITHUB_TOOLSETS`) and a top-level `tenants:` list (one
entry, `id: github-internal`, groups, headers, `credentialSecret`). Replace
BOTH of those top-level keys with a single `backends:` list that nests the
tenant inside it. The new file's `backends:` section should read:

```yaml
backends:
  - name: github
    credentialHeader: Authorization
    credentialScheme: Bearer
    managed:
      enabled: true
      image:
        repository: ghcr.io/github/github-mcp-server
        tag: "latest"
        pullPolicy: IfNotPresent
      args: ["http"]
      port: 8082
      env:
        GITHUB_READ_ONLY: "true"
        GITHUB_TOOLSETS: "default,actions"
    tenants:
      - id: github-internal
        groups:
          - "urn:iamr:67481a52-f105-498a-98b7-d74a5ddf026a:dip-centcom-admins"
          - "philips-internal:dip-centcom-users"
        headers:
          X-MCP-Readonly: "true"
        credentialSecret:
          name: github-pat-internal
          key: token
          create: false
```

Every value above is copied verbatim from the current file's `backend:` and
`tenants:` blocks — this is a pure restructuring (flatten a separate
`tenants:` list into the one backend's `tenants:`), not a values change. The
`image.tag` field is added explicitly as `"latest"` (matching what the old
chart's default already was) since the new schema has no chart-level default
image repo/tag baked in the same way. Also bump the `image.tag` at the top of
the file's `image:` block from `v0.1.2` to whatever the Task 8 release tag
ends up being (leave as `v0.1.2` for now — Task 8 will need a fresh
mt-mcp-proxy binary release only if the binary changed, which it has not in
this plan, so `v0.1.2` remains correct and no image rebuild is needed).

Update the comment on line 1 of the file (currently references the old
`backend:`/`tenants:` shape implicitly) — no change needed there since it
doesn't name the removed keys directly; leave the header comment as-is.

- [ ] **Step 3: Dry-run against the live cluster**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm upgrade --install github-mcp \
  /Users/andy/DEV/Go/mt-mcp-proxy/deploy/helm/mt-mcp-proxy \
  -n github-mcp \
  -f /Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu.yaml \
  --dry-run
```
Expected: renders successfully (exit 0, full manifest printed, no error). If
it errors, compare the rendered Deployment/Service/ConfigMap names against
what's currently live:
```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl get all -n github-mcp
```
and adjust the values file until the dry-run output's resource names match
the currently-running resource names (so the upgrade doesn't orphan the old
backend Deployment/Service under a different name).

- [ ] **Step 4: Apply the real upgrade**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm upgrade --install github-mcp \
  /Users/andy/DEV/Go/mt-mcp-proxy/deploy/helm/mt-mcp-proxy \
  -n github-mcp \
  -f /Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu.yaml
```
Expected: `Release "github-mcp" has been upgraded.`

- [ ] **Step 5: Verify github-mcp still works end-to-end**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl get pods -n github-mcp
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl -n github-mcp port-forward svc/github-mcp-mt-mcp-proxy 18080:8080 &
sleep 2
curl -s -X POST http://localhost:18080/mcp \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"plan-verify","version":"0"}}}' \
  -D -
kill %1
```
Expected: HTTP 200 with an `Mcp-Session-Id` response header and a JSON body
containing `"serverInfo"` — proves the upgraded chart's rendered config still
produces a working proxy for the pre-existing github backend. This does not
need a JWT since `initialize` is unauthenticated (per the existing
`E2E-TEST.md` pattern).

- [ ] **Step 6: Commit**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
git add deploy/helm/mt-mcp-proxy/Chart.yaml
git commit -m "chore(chart): bump version to 0.2.0 for backends list schema"
cd /Users/andy/DEV/Philips/innovation-day
git add mt-mcp-proxy/values-dip-ce-k3s-eu.yaml
git commit -m "chore(github-mcp): migrate values to backends list schema"
```
(Run the second `git add`/`git commit` only if
`/Users/andy/DEV/Philips/innovation-day` is itself a git repository — check
first with `git -C /Users/andy/DEV/Philips/innovation-day status`; if it is
not a repo, skip the second commit and note in your task report that the
file was updated but not committed.)

**Note:** `git -C /Users/andy/DEV/Philips/innovation-day status` was checked
during planning and returned `fatal: not a git repository` — so in practice
Step 6's second commit will be skipped. This is expected; just save the file.

---

### Task 8: Write the Grafana migration values file for `dip-ce-k3s-eu`

**Files:**
- Create: `/Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu-grafana.yaml`

**Interfaces:**
- Consumes: the `backends: []` schema (Task 1), the live secrets already in
  the `mt-mcp-grafana` namespace (`ri-obs-use1-ct-grafana-token`,
  `ops-sandbox-grafana-token`, `src-co-sb-grafana-token` — confirmed present
  via `kubectl get secrets -n mt-mcp-grafana` during planning).
- Produces: the values file Task 9 deploys with, reproducing exactly the 3
  backends currently served by `mt-mcp-grafana` (per
  `/Users/andy/DEV/Philips/innovation-day/mt-mcp-grafana/values.yaml`, read
  during planning): `ri-obs-use1-ct` (managed), `ops-sandbox` (external,
  points at the existing `grafana-mcp` namespace service), `src-co-sb`
  (managed).

- [ ] **Step 1: Write the file**

```yaml
# mt-mcp-proxy values for dip-ce-k3s-eu (namespace: mt-mcp-proxy-grafana).
#
# Side-by-side migration target: reproduces the 3 Grafana backends currently
# served by mt-mcp-grafana (namespace mt-mcp-grafana), using the SAME
# pre-existing Kubernetes Secrets (created out-of-band, not by this chart) so
# no new Grafana service-account tokens are needed. See
# docs/superpowers/plans/2026-07-28-multi-backend-chart-and-grafana-migration.md
# in the mt-mcp-proxy repo for the full migration plan.

replicaCount: 1

image:
  repository: ghcr.io/loafoe/mt-mcp-proxy
  tag: "v0.1.2"

auth:
  mode: jwks
  groupsClaim: groups
  issuer: "https://issuer.ri-obs-use1.hsp.philips.com"
  audience: "pico-mcp-ui"
  additionalAudiences:
    - "actions"

backendMetrics:
  enabled: true
  port: 9090
  path: /metrics
  serviceMonitor:
    enabled: false

backends:
  - name: ri-obs-use1-ct
    credentialHeader: Authorization
    credentialScheme: Bearer
    managed:
      enabled: true
      image:
        repository: grafana/mcp-grafana
        tag: "0.14.0"
      args:
        - "--transport"
        - "streamable-http"
        - "--address"
        - "0.0.0.0:8000"
        - "--metrics"
        - "--metrics-address"
        - "0.0.0.0:9090"
        - "--disable-sift"
        - "--disable-oncall"
        - "--disable-incident"
      port: 8000
      env:
        GRAFANA_URL: "https://gf.ri-obs-use1-ct.hsp.philips.com"
      credentialEnvVar: "GRAFANA_SERVICE_ACCOUNT_TOKEN"
    credentialSecret:
      name: ri-obs-use1-ct-grafana-token
      key: token
      create: false
    tenants:
      - id: ri-obs-use1-ct
        groups:
          - "philips-internal:ri-obs-editors"
          - "urn:iamr:67481a52-f105-498a-98b7-d74a5ddf026a:dip-centcom-admins"

  - name: ops-sandbox
    credentialHeader: Authorization
    credentialScheme: Bearer
    externalUrl: "http://grafana-mcp-ops-sandbox.grafana-mcp.svc.cluster.local:8000/mcp"
    credentialSecret:
      name: ops-sandbox-grafana-token
      key: token
      create: false
    tenants:
      - id: ops-sandbox
        groups:
          - "philips-internal:dip-oaas-admins"
          - "urn:iamr:67481a52-f105-498a-98b7-d74a5ddf026a:dip-centcom-admins"

  - name: src-co-sb
    credentialHeader: Authorization
    credentialScheme: Bearer
    managed:
      enabled: true
      image:
        repository: grafana/mcp-grafana
        tag: "0.14.0"
      args:
        - "--transport"
        - "streamable-http"
        - "--address"
        - "0.0.0.0:8000"
        - "--metrics"
        - "--metrics-address"
        - "0.0.0.0:9090"
        - "--disable-sift"
        - "--disable-oncall"
        - "--disable-incident"
      port: 8000
      env:
        GRAFANA_URL: "https://grafana.aa7c-use1.5dcacc9.hsp.philips.com"
      credentialEnvVar: "GRAFANA_SERVICE_ACCOUNT_TOKEN"
    credentialSecret:
      name: src-co-sb-grafana-token
      key: token
      create: false
    tenants:
      - id: src-co-sb
        groups:
          - "philips-internal:src-co-sb-centcom"
          - "urn:iamr:67481a52-f105-498a-98b7-d74a5ddf026a:dip-centcom-admins"

resources: {}
```

Every `grafanaUrl`, group, and secret name above is copied verbatim from
`/Users/andy/DEV/Philips/innovation-day/mt-mcp-grafana/values.yaml` (read
during planning) — this file must reproduce that deployment's authorization
behavior exactly, differing only in being served by `mt-mcp-proxy` instead of
`mt-mcp-grafana`.

**Important:** the 3 `credentialSecret.name` values above
(`ri-obs-use1-ct-grafana-token`, `ops-sandbox-grafana-token`,
`src-co-sb-grafana-token`) reference Secrets that live in the
**`mt-mcp-grafana` namespace today**, not in the new `mt-mcp-proxy-grafana`
namespace this chart will deploy into. Kubernetes Secrets are
namespace-scoped — a `secretKeyRef` can only reference a Secret in the same
namespace as the pod. Task 9 must copy these 3 Secrets into the new namespace
before deploying (covered there); this values file assumes that has already
happened.

- [ ] **Step 2: Validate the file parses and matches the chart schema**

```bash
cd /Users/andy/DEV/Go/mt-mcp-proxy
helm lint deploy/helm/mt-mcp-proxy \
  -f /Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu-grafana.yaml
helm template migration-test deploy/helm/mt-mcp-proxy \
  -f /Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu-grafana.yaml \
  > /tmp/mt-mcp-proxy-grafana-rendered.yaml
grep -c "^kind: Deployment$" /tmp/mt-mcp-proxy-grafana-rendered.yaml
```
Expected: `helm lint` reports `0 chart(s) failed`; the `grep -c` count is `3`
(1 proxy Deployment + 2 managed-backend Deployments — `ops-sandbox` is
external so it contributes no Deployment).

- [ ] **Step 3: Commit** (only if the innovation-day directory is a git repo;
confirmed during Task 7 that it is not, so instead just leave the file saved
on disk — no commit step here.)

---

### Task 9: Deploy the side-by-side release to `dip-ce-k3s-eu`

**Files:** none (cluster operations only; no repo files change in this task)

**Interfaces:**
- Consumes: the values file from Task 8; the upgraded chart from Tasks 1–6.
- Produces: a running Helm release `mt-mcp-proxy-grafana` in a new namespace
  `mt-mcp-proxy-grafana` on `dip-ce-k3s-eu`, with the 3 Grafana backend
  Secrets copied in from the `mt-mcp-grafana` namespace. `mt-mcp-grafana`
  itself is untouched throughout this task.

- [ ] **Step 1: Create the namespace**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl create namespace mt-mcp-proxy-grafana
```
Expected: `namespace/mt-mcp-proxy-grafana created`.

- [ ] **Step 2: Copy the 3 Grafana SA token Secrets into the new namespace**

Kubernetes has no built-in "copy secret across namespaces" command; extract
and re-apply each one:

```bash
export KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml
for s in ri-obs-use1-ct-grafana-token ops-sandbox-grafana-token src-co-sb-grafana-token; do
  kubectl get secret "$s" -n mt-mcp-grafana -o json \
    | jq 'del(.metadata.namespace, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.ownerReferences, .metadata.annotations."meta.helm.sh/release-name", .metadata.annotations."meta.helm.sh/release-namespace", .metadata.labels."app.kubernetes.io/managed-by")' \
    | kubectl apply -n mt-mcp-proxy-grafana -f -
done
kubectl get secrets -n mt-mcp-proxy-grafana
```
Expected: all 3 secrets listed under `mt-mcp-proxy-grafana`. The `jq del(...)`
strips namespace/Helm-ownership metadata so the copies aren't mistaken for
resources managed by the `mt-mcp-grafana` Helm release, and so `kubectl
apply` doesn't fail on an immutable-namespace-mismatch error.

- [ ] **Step 3: Deploy the release**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm upgrade --install mt-mcp-proxy-grafana \
  /Users/andy/DEV/Go/mt-mcp-proxy/deploy/helm/mt-mcp-proxy \
  -n mt-mcp-proxy-grafana \
  -f /Users/andy/DEV/Philips/innovation-day/mt-mcp-proxy/values-dip-ce-k3s-eu-grafana.yaml
```
Expected: `Release "mt-mcp-proxy-grafana" has been installed.`

- [ ] **Step 4: Confirm all pods come up healthy**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl get pods -n mt-mcp-proxy-grafana -w
```
Expected: 3 pods (1 proxy + 2 managed Grafana backends — `ops-sandbox` is
external, no pod), each reaching `1/1 Running`. Press Ctrl-C once all 3 are
Running. If any backend pod crash-loops, check its logs:
```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl logs -n mt-mcp-proxy-grafana deploy/mt-mcp-proxy-grafana-ri-obs-use1-ct-backend
```
A common failure here is a wrong `GRAFANA_SERVICE_ACCOUNT_TOKEN` env var name
mismatch between Task 4's template and what `grafana/mcp-grafana` actually
reads — cross-check against
`/Users/andy/DEV/Personal/helm-charts/charts/mt-mcp-grafana/templates/mcp-backends.yaml`
line with `GRAFANA_SERVICE_ACCOUNT_TOKEN` if this fails.

- [ ] **Step 5: Confirm the proxy container logs show 3 backends loaded**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl logs -n mt-mcp-proxy-grafana deploy/mt-mcp-proxy-grafana --tail=50
```
Expected: no error-level log lines about failing to load the tool catalog or
backend connection failures for any of the 3 backends. (No commit — this is
a pure cluster-verification task; nothing in a repo changes.)

---

### Task 10: Wire picoclaw to the new server and verify tool/tenant parity

**Files:**
- Modify: `/Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml`

**Interfaces:**
- Consumes: the running `mt-mcp-proxy-grafana` Service from Task 9.
- Produces: a new `mt-mcp-proxy-grafana` entry under `mcp.servers` in the
  picoclaw values file (distinct key from the existing `mt-mcp-grafana`
  entry — both active).

- [ ] **Step 1: Add the new server entry**

Open `/Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml`.
In the `mcp.servers` block, immediately after the existing `mt-mcp-grafana:`
entry (currently lines 126-132: `enabled: true`, `type: http`, the
`mt-mcp-grafana.mt-mcp-grafana.svc.cluster.local:8080/mcp` URL, and
`dynamic_headers.allowed: ["Authorization"]`), add:

```yaml
        mt-mcp-proxy-grafana:
          enabled: true
          type: http
          url: "http://mt-mcp-proxy-grafana.mt-mcp-proxy-grafana.svc.cluster.local:8080/mcp"
          dynamic_headers:
            allowed:
              - "Authorization"
```

Do not modify or remove the existing `mt-mcp-grafana:` entry — both run
side-by-side per this task's goal.

- [ ] **Step 2: Deploy picoclaw with both servers active**

```bash
cd /Users/andy/DEV/Personal/helm-charts
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm upgrade --install picoclaw ./charts/picoclaw -n picoclaw \
  -f /Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml
```
Expected: `Release "picoclaw" has been upgraded.`

- [ ] **Step 3: Mint a test JWT carrying all 3 tenant groups**

Follow the existing documented flow (per
`/Users/andy/DEV/Philips/innovation-day/MINTING-TEST-JWT.md`, read during
planning):
```bash
export KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml
U=$(kubectl -n centcom get secret centcom-service-identity -o jsonpath='{.data.username}' | base64 -d)
P=$(kubectl -n centcom get secret centcom-service-identity -o jsonpath='{.data.password}' | base64 -d)
S=$(kubectl -n centcom get secret centcom-service-identity -o jsonpath='{.data.oauth2-secret}' | base64 -d)
env USERNAME="$U" PASSWORD="$P" OAUTH2_SECRET="$S" /tmp/minttoken > /tmp/jwt.txt
```
This mints a service-identity token carrying the `dip-centcom-admins` group,
which is present in ALL 3 migrated tenants' `groups` lists (per Task 8's
values file) — so this one JWT should see all 3 tenants via `list_instances`.

- [ ] **Step 4: Compare `tools/list` and `list_instances` between old and new**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl -n mt-mcp-grafana port-forward svc/mt-mcp-grafana 18080:8080 &
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl -n mt-mcp-proxy-grafana port-forward svc/mt-mcp-proxy-grafana 18081:8080 &
sleep 2
JWT=$(cat /tmp/jwt.txt)

for PORT in 18080 18081; do
  echo "=== port $PORT ==="
  curl -s -X POST "http://localhost:$PORT/mcp" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Authorization: Bearer $JWT" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"parity-check","version":"0"}}}' \
    -D /tmp/headers-$PORT.txt -o /dev/null
  SID=$(grep -i mcp-session-id /tmp/headers-$PORT.txt | awk '{print $2}' | tr -d '\r')
  curl -s -X POST "http://localhost:$PORT/mcp" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Authorization: Bearer $JWT" \
    -H "Mcp-Session-Id: $SID" \
    -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
    | jq -r '.result.tools[].name' | sort > /tmp/tools-$PORT.txt
done
diff <(sed 's/list_grafana_instances/list_instances/' /tmp/tools-18080.txt) /tmp/tools-18081.txt
kill %1 %2
```
Expected: `diff` produces no output (empty) — meaning the two tool sets are
identical once accounting for the one intentional rename
(`list_grafana_instances` → `list_instances`). Any other diff line is a real
parity gap that must be investigated before proceeding to cutover.

- [ ] **Step 5: Verify `list_instances` shows all 3 tenants**

```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl -n mt-mcp-proxy-grafana port-forward svc/mt-mcp-proxy-grafana 18081:8080 &
sleep 2
JWT=$(cat /tmp/jwt.txt)
curl -s -X POST "http://localhost:18081/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer $JWT" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"tenant-check","version":"0"}}}' \
  -D /tmp/headers-tenant.txt -o /dev/null
SID=$(grep -i mcp-session-id /tmp/headers-tenant.txt | awk '{print $2}' | tr -d '\r')
curl -s -X POST "http://localhost:18081/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer $JWT" \
  -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_instances","arguments":{}}}'
kill %1
```
Expected: the response text lists all 3 tenant ids (`ri-obs-use1-ct`,
`ops-sandbox`, `src-co-sb`), each showing authorization via the
`dip-centcom-admins` group — mirroring the `list_grafana_instances` output
format documented in `MINTING-TEST-JWT.md`.

- [ ] **Step 6: One real read query per tenant**

For each of the 3 tenant ids, call a real read tool with that `tenant`
argument (mirroring the github E2E-TEST.md pattern) — e.g.:
```bash
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl -n mt-mcp-proxy-grafana port-forward svc/mt-mcp-proxy-grafana 18081:8080 &
sleep 2
JWT=$(cat /tmp/jwt.txt)
curl -s -X POST "http://localhost:18081/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer $JWT" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"read-check","version":"0"}}}' \
  -D /tmp/headers-read.txt -o /dev/null
SID=$(grep -i mcp-session-id /tmp/headers-read.txt | awk '{print $2}' | tr -d '\r')
for TENANT in ri-obs-use1-ct ops-sandbox src-co-sb; do
  echo "=== tenant $TENANT ==="
  curl -s -X POST "http://localhost:18081/mcp" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Authorization: Bearer $JWT" \
    -H "Mcp-Session-Id: $SID" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"list_datasources\",\"arguments\":{\"tenant\":\"$TENANT\"}}}"
done
kill %1
```
Expected: each call returns a real list of Grafana datasources (not an
error), proving the SA token for that backend is correctly injected and that
backend is reachable. A `401`/`403`/connection-refused response for any
tenant means that backend's Secret or `credentialEnvVar` wiring is broken —
stop and fix before proceeding to Task 11.

No commit in this task — the picoclaw values file change from Step 1 is the
only repo file touched, and `/Users/andy/DEV/Philips/innovation-day` is not a
git repo (confirmed in Task 7), so there is nothing to commit.

---

### Task 11: Cut over picoclaw and decommission mt-mcp-grafana

**Files:**
- Modify: `/Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml`
- Modify: `/Users/andy/DEV/Philips/innovation-day/CLAUDE.md` (if it exists at
  the repo root and references `mt-mcp-grafana` — check first)

**Interfaces:**
- Consumes: the verified-parity `mt-mcp-proxy-grafana` deployment from Tasks
  9–10.
- Produces: picoclaw pointing exclusively at the new server; the
  `mt-mcp-grafana` Helm release and namespace removed from `dip-ce-k3s-eu`.

**Do not start this task until every check in Task 10 passed.** This is the
one irreversible step in the whole plan (namespace deletion) — treat it as a
hard gate.

- [ ] **Step 1: Disable the old server in picoclaw**

In `/Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml`,
change the existing `mt-mcp-grafana:` entry's `enabled: true` to
`enabled: false` (do not delete the block yet — keep it as a quick rollback
path for one deployment cycle):

```yaml
        mt-mcp-grafana:
          enabled: false
          type: http
          url: "http://mt-mcp-grafana.mt-mcp-grafana.svc.cluster.local:8080/mcp"
          dynamic_headers:
            allowed:
              - "Authorization"
```

- [ ] **Step 2: Redeploy picoclaw and confirm it re-enumerates onto the new server only**

```bash
cd /Users/andy/DEV/Personal/helm-charts
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm upgrade --install picoclaw ./charts/picoclaw -n picoclaw \
  -f /Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl rollout restart deployment/picoclaw -n picoclaw
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl logs -n picoclaw deploy/picoclaw --tail=50 | grep -i "mcp\|tool"
```
Expected: log lines showing picoclaw connected to `mt-mcp-proxy-grafana` and
NOT to `mt-mcp-grafana` (no connection attempt logged for the disabled
server). Confirm via a real chat/tool-call through picoclaw/centcom that
Grafana tools still work (e.g. ask ClusterClaw a Grafana question) before
proceeding — this is the last chance to catch a regression with the old
deployment still available to fall back to.

- [ ] **Step 3: Check for other references to the old tool name / namespace**

```bash
grep -rn "list_grafana_instances\|mt-mcp-grafana" \
  /Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml \
  /Users/andy/DEV/Philips/innovation-day/CLAUDE.md 2>/dev/null
```
For any hit in a `soul:` or agent-instruction block (not the disabled
`mt-mcp-grafana:` server entry itself, which is expected to remain until Step
5), update the text to reference `list_instances` / the new server name
instead, so ClusterClaw's own self-description stays accurate.

- [ ] **Step 4: Wait one operational cycle, then confirm no regressions**

This step has no fixed command — it's a monitoring pause. Before deleting
anything, confirm via normal usage (or by checking centcom/picoclaw logs over
the following day) that no user-facing regression has surfaced with
`mt-mcp-grafana` disabled. Do not proceed to Step 5 the same day the cutover
happened unless the user explicitly says to skip this wait.

- [ ] **Step 5: Remove the old server entry and Helm release**

Delete the `mt-mcp-grafana:` block from
`/Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml`
entirely (the `enabled: false` entry added in Step 1), then:
```bash
cd /Users/andy/DEV/Personal/helm-charts
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm upgrade --install picoclaw ./charts/picoclaw -n picoclaw \
  -f /Users/andy/DEV/Philips/innovation-day/picoclaw/dip-ce-k3s-eu-values.yaml

KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  helm uninstall mt-mcp-grafana -n mt-mcp-grafana
KUBECONFIG=/Users/andy/DEV/Personal/pulumi/k3s-on-ec2/dip-ce-k3s-eu.yaml \
  kubectl delete namespace mt-mcp-grafana
```
Expected: `release "mt-mcp-grafana" uninstalled` and
`namespace "mt-mcp-grafana" deleted`.

- [ ] **Step 6: Update CLAUDE.md documentation**

Update the "Registered centcom-satellites" / relevant deployment sections in
`/Users/andy/DEV/Philips/innovation-day/CLAUDE.md` (if this project has one at
its root documenting deployments the way the current CLAUDE.md context
documents centcom/centcom-satellite) to reflect that Grafana access is now
served by `mt-mcp-proxy-grafana` (namespace `mt-mcp-proxy-grafana`) rather
than `mt-mcp-grafana`. Since this repo (`innovation-day`) is not a git repo
(confirmed Task 7), there is no commit step — just save the file.

**Follow-up not included in this plan (per spec Out of Scope):** archiving or
deleting the `mt-mcp-grafana` git repository at
`ssh://git@codeberg.org/loafoe/mt-mcp-grafana.git` once nothing else
references it. Flag this to the user as a manual follow-up once Step 6 is
done — do not delete the repo as part of this plan.
