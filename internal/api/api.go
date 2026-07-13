// Package api wires Connect handlers for the public API surface
// (docs/07_API_Specification.md). The generated connect-go service
// handlers are mounted here onto a single mux, wrapped by the
// tenant-resolution middleware. v0.1 mounts ProjectService; later
// phases add the remaining services.
package api

import (
	"log/slog"
	"net/http"

	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/execution"
	"github.com/beardedparrott/orchicon/internal/middleware"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/worker"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// Dependencies bundles the resources the API layer needs. Constructed
// once by the server and passed to Mount.
type Dependencies struct {
	Pool          *db.Pool
	Log           *slog.Logger
	Subscriber    eventbus.Subscriber
	PolicyEngine  *policy.Engine
	RecoveryEngine *recovery.Engine
	SigNozClient  *telemetry.SigNozClient
}

// Mount returns an http.Handler serving the Orchicon API. Generated
// connect-go handlers are registered as they are added. The whole
// surface is wrapped by the tenant-resolution middleware so every
// tenant-scoped RPC carries tenant context into the data-access layer.
func Mount(mux *http.ServeMux, deps Dependencies) http.Handler {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version.Current().Tag + `"}`))
	})

	// ProjectService (docs/07 §3.1). The Vite dev-server proxy
	// (frontend/vite.config.ts) forwards /orchicon.api.v1 paths here,
	// so no CORS headers are needed in dev (docs/10 §9).
	projSvc := project.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewProjectServiceHandler(projSvc))

	// WorkerService (docs/07 §3.3). Worker CRUD + versioning lifecycle
	// (publish/deprecate/retire) + edit locks for the visual editor.
	workerSvc := worker.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkerServiceHandler(workerSvc))

	// WorkItemService (docs/07 §3.2). Work item CRUD + dependency DAG
	// (recursive CTE cycle detection — docs/09 §11) + worker assignment.
	workItemSvc := workitem.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(workItemSvc))

	// RuntimeAdapterService (docs/07 §3.7). Public adapter registry:
	// list registered adapters, inspect capabilities.
	adapterSvc := adapter.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewRuntimeAdapterServiceHandler(adapterSvc))

	// ExecutionService (docs/07 §3.8). Live streaming telemetry, manual
	// control (pause/resume/cancel/checkpoint), Tier 2 per-tool-call
	// approval (docs/05 §7.1).
	execSvc := execution.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewExecutionServiceHandler(execSvc))

	// WorkflowService (docs/07 §3.4). Workflow CRUD + versioning
	// lifecycle (publish/deprecate) + runs (start/abort) + streaming +
	// edit locks for the visual Workflow editor.
	workflowSvc := workflow.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewWorkflowServiceHandler(workflowSvc))

	// PolicyService (docs/07 §3.5). Policy CRUD + publish/supersede +
	// EvaluatePolicy (dry-run) + ExplainDecision (Rego trace) + decision
	// log. Tier 1 (decision-point) Rego-only baseline.
	policySvc := policy.NewService(deps.Pool, deps.Log, deps.PolicyEngine, deps.Subscriber)
	mux.Handle(apiv1connect.NewPolicyServiceHandler(policySvc))

	// RecoveryService (docs/07 §3.6, docs/06). Trigger/cancel, streaming
	// events, continuation-plan approval, MarkTaskSucceeded (Reviewer/
	// human task completion).
	recoverySvc := recovery.NewService(deps.Pool, deps.Log, deps.RecoveryEngine, deps.Subscriber)
	mux.Handle(apiv1connect.NewRecoveryServiceHandler(recoverySvc))

	// TelemetryService (docs/07 §3.9, docs/08 §5). Proxies tenant-scoped
	// queries to SigNoz/ClickHouse so users explore traces/metrics/logs
	// without leaving the Orchicon shell (docs/10 §11 seamless embedding).
	telemetrySvc := telemetry.NewService(deps.Pool, deps.SigNozClient, deps.Subscriber)
	mux.Handle(apiv1connect.NewTelemetryServiceHandler(telemetrySvc))

	// AIGatewayService (docs/07 §3.10, docs/01 §2: embedded in the control
	// plane binary). ListProviders, GetUsage, GetCost (drill-down roll-up:
	// Tenant → Project → Task → Execution), StreamUsageEvents.
	aiGatewaySvc := aigateway.NewService(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewAIGatewayServiceHandler(aiGatewaySvc))

	return middleware.ResolveTenant(mux)
}
