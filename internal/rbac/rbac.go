// Package rbac implements role-based access control for Orchicon
// (docs/07 §6.2). Entitlements are resource:action pairs (e.g.
// "project:create") granted to an identity via roles. The middleware
// resolves the identity's entitlement set per request; per-RPC
// enforcement is a Connect interceptor that maps each RPC procedure to
// the required entitlement and checks the resolved set.
//
// Admins (identity with the "admin" role) bypass per-call checks.
// API keys carry their own scopes union the bound identity's
// entitlements (docs/07 §6.1). UI gating is a UX convenience; the
// server is the security boundary (docs/10 §10 invariant #5).
package rbac

import (
	"strings"
)

// Entitlement is a resource:action pair. The wildcard "*" matches any
// resource or action (admin role carries "*").
type Entitlement string

// EntitlementFor returns the entitlement required to invoke an RPC
// procedure. The procedure is the fully-qualified name
// "/orchicon.api.v1.<Service>/<Method>". Read RPCs (Get/List/Stream)
// require "<resource>:read"; mutating RPCs require "<resource>:write"
// or a specific action. Unknown RPCs require nothing (fail-open at the
// entitlement layer; the Policy Engine handles finer-grained
// governance — docs/02 §2.5).
//
// This is the resource-level RBAC check (docs/07 §6.2); it is distinct
// from the domain Policy evaluation at decision points.
func EntitlementFor(procedure string) Entitlement {
	// procedure = "/orchicon.api.v1.<Service>/<Method>"
	if !strings.HasPrefix(procedure, "/orchicon.api.v1.") {
		return ""
	}
	rest := strings.TrimPrefix(procedure, "/orchicon.api.v1.")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	service, method := parts[0], parts[1]
	resource := serviceToResource(service)
	if resource == "" {
		return ""
	}
	if isRead(method) {
		return Entitlement(resource + ":read")
	}
	if action := specificAction(resource, method); action != "" {
		return Entitlement(action)
	}
	return Entitlement(resource + ":write")
}

// serviceToResource maps a Connect service name to the RBAC resource.
func serviceToResource(service string) string {
	switch service {
	case "ProjectService":
		return "project"
	case "WorkItemService":
		return "workitem"
	case "WorkerService":
		return "worker"
	case "WorkflowService":
		return "workflow"
	case "PolicyService":
		return "policy"
	case "RecoveryService":
		return "recovery"
	case "ExecutionService":
		return "execution"
	case "RuntimeAdapterService":
		return "adapter"
	case "TelemetryService":
		return "telemetry"
	case "AIGatewayService":
		return "aigateway"
	case "AuthService":
		return "auth"
	case "WebhookService":
		return "webhook"
	}
	return ""
}

// isRead reports whether a method is a read-only RPC by convention
// (Get/List/Stream prefix).
func isRead(method string) bool {
	return strings.HasPrefix(method, "Get") ||
		strings.HasPrefix(method, "List") ||
		strings.HasPrefix(method, "Stream")
}

// specificAction maps a handful of methods to granular entitlements.
// Most mutating RPCs fall through to "<resource>:write".
func specificAction(resource, method string) string {
	switch {
	case resource == "policy" && method == "SupersedePolicy":
		return "policy:supersede"
	case resource == "worker" && method == "PublishWorkerVersion":
		return "worker:publish"
	case resource == "workflow" && method == "PublishWorkflow":
		return "workflow:publish"
	case resource == "policy" && method == "PublishPolicy":
		return "policy:publish"
	}
	return ""
}

// Has reports whether the entitlement set grants the required
// entitlement. Admins (granted "*") pass everything.
func Has(granted []string, required Entitlement) bool {
	if required == "" {
		return true
	}
	for _, g := range granted {
		if g == "*" || g == "*:*" {
			return true
		}
		if g == string(required) {
			return true
		}
		// Wildcard action: "resource:*" matches "resource:action".
		if strings.HasSuffix(g, ":*") {
			prefix := strings.TrimSuffix(g, "*")
			if strings.HasPrefix(string(required), prefix) {
				return true
			}
		}
		// Wildcard resource: "*:action"
		if strings.HasPrefix(g, "*:") {
			if strings.HasSuffix(string(required), strings.TrimPrefix(g, "*")) {
				return true
			}
		}
	}
	return false
}
