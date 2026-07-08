// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package observability

// mcpHistogramBuckets are the explicit bucket boundaries (seconds) for MCP
// operation-duration histograms. They mirror mcp-grafana's choice, which
// follows the OpenTelemetry GenAI MCP semantic conventions:
// https://opentelemetry.io/docs/specs/semconv/gen-ai/mcp/
var mcpHistogramBuckets = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// Attribute keys recorded on MCP operation metrics and spans. The mcp.* and
// error.type keys follow the OTel semantic conventions; tenant/backend are
// proxy-specific dimensions.
const (
	attrMethodName = "mcp.method.name"
	attrToolName   = "mcp.tool.name"
	attrTenantID   = "mcp.tenant.id"
	attrBackend    = "mcp.backend.name"
	attrErrorType  = "error.type"
)
