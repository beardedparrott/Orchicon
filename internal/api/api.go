// Package api wires Connect handlers for the public API surface
// (docs/07_API_Specification.md). The generated connect-go service
// handlers are mounted here onto a single mux, wrapped by the
// auth-resolution middleware. Phase 9 adds AuthService + WebhookService
// + the RBAC Connect interceptor.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"connectrpc.com/connect"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/approval"
	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/execution"
	"github.com/beardedparrott/orchicon/internal/middleware"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/runtimeimage"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/settings"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/webhook"
	"github.com/beardedparrott/orchicon/internal/worker"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// 	Dependencies bundles the resources the API layer needs. Constructed
// once by the server and passed to Mount.
type Dependencies struct {
	Pool           *db.Pool
	Log            *slog.Logger
	Subscriber     eventbus.Subscriber
	PolicyEngine   *policy.Engine
	RecoveryEngine *recovery.Engine
	TelemetryQuery *telemetry.QueryClient
	// GrafanaURL is the base URL of the Grafana UI (default
	// http://localhost:3000). Used by the /grafana reverse proxy so the
	// embedded iframe works same-origin (docs/10 §11). Grafana runs with
	// serve_from_sub_path, so it generates every URL under /grafana.
	GrafanaURL string
	// Phase 9: auth + webhooks + blobstore.
	AuthHandler          *auth.Handler
	WebhookDispatcher    *webhook.Dispatcher
	Mode                 config.DeploymentMode
	// ModelDiscoverer enumerates models from opencode CLI.
	ModelDiscoverer   *aigateway.ModelDiscoverer
	MCPDiscoverer     *aigateway.MCPDiscoverer
	// BlobStore is the object storage abstraction (local filesystem + S3).
	BlobStore blobstore.Store
	// PostgresDSN is the Postgres connection string for backup/restore.
	PostgresDSN string
	// RuntimeClient talks to the host-side runtime daemon over its unix
	// socket (build/remove runtime images). Nil when the daemon is not
	// configured (headless serve).
	RuntimeClient *runtime.Client
	// SendExecutionMessage routes a mid-run human message into a live
	// session execution (Stage 3). Nil when the session transport is
	// unavailable.
	SendExecutionMessage func(ctx context.Context, execID, message string) error
	// ContinueSession runs a one-shot follow-up question against a worker's
	// session in place (no new execution/work item). Nil when the session
	// transport is unavailable.
	ContinueSession func(ctx context.Context, opts opencode.ContinueSessionOpts) (string, error)
	// HostServe is the always-on host opencode serve. Ask Orchicon
	// conversation turns run as persistent sessions on it (first message
	// CreateSession, follow-ups prompt_async on the same session). Nil when
	// the transport is disabled or the serve could not start — the chat
	// degrades to the legacy per-message subprocess path.
	HostServe *opencode.HostServe
}

// Mount returns an http.Handler serving the Orchicon API. Generated
// connect-go handlers are registered with the RBAC interceptor applied
// per-RPC (docs/07 §6.3). The whole surface is wrapped by the
// auth-resolution middleware so every tenant-scoped RPC carries
// identity + tenant context into the data-access layer.
func Mount(mux *http.ServeMux, deps Dependencies) http.Handler {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version.Current().Tag + `"}`))
	})

	// Phase 9: out-of-band auth HTTP endpoints (OIDC code-flow + dev
	// login + refresh + session — docs/07 §6.1).
	if deps.AuthHandler != nil {
		deps.AuthHandler.Register(mux)
	}

	// The RBAC interceptor applies the per-RPC entitlement check on
	// top of the identity resolved by the auth middleware (docs/07 §6.2).
	rbacInterceptor := middleware.NewRBACInterceptor()
	interceptorOpt := connect.WithInterceptors(rbacInterceptor)

	// ProjectService (docs/07 §3.1).
	projSvc := project.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewProjectServiceHandler(projSvc, interceptorOpt))

	// WorkerService (docs/07 §3.3).
	workerSvc := worker.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewWorkerServiceHandler(workerSvc, interceptorOpt))

	// WorkflowService (docs/07 §3.4). Constructed before WorkItemService
	// so the WorkItemService can wire the StartWorkflowStarter.
	workflowSvc := workflow.New(deps.Pool, deps.Log, deps.Subscriber)
	mux.Handle(apiv1connect.NewWorkflowServiceHandler(workflowSvc, interceptorOpt))

	// WorkItemService (docs/07 §3.2).
	workItemSvc := workitem.New(deps.Pool, deps.Log)
	workItemSvc.SetStartWorkflowStarter(func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return workflow.StartWorkflowDirect(ctx, deps.Pool, deps.Log, tenantID, workflowID, projectID, workItemID)
	})
	// Sequence auto-start (run-instant on a parent with children): fire
	// the chain through the sequence engine. Validation of the subtree
	// runs in the handler before this is invoked.
	workItemSvc.SetStartSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		return scheduler.StartSequence(ctx, deps.Pool, deps.Log, tenantID, parentID,
			func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
				return workflow.StartWorkflowDirect(ctx, deps.Pool, deps.Log, tenantID, workflowID, projectID, workItemID)
			})
	})
	// Sequence control (ControlSequence RPC): resume parks/continues a
	// sequence parent manually. Both go through the sequence engine.
	workItemSvc.SetResumeSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		return scheduler.ResumeSequence(ctx, deps.Pool, deps.Log, tenantID, parentID,
			func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
				return workflow.StartWorkflowDirect(ctx, deps.Pool, deps.Log, tenantID, workflowID, projectID, workItemID)
			})
	})
	workItemSvc.SetStopSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		return scheduler.StopSequence(ctx, deps.Pool, deps.Log, tenantID, parentID)
	})
	if deps.RuntimeClient != nil {
		workItemSvc.SetRuntimeImageResolver(func(ctx context.Context) string {
			imgs, err := deps.RuntimeClient.Images(ctx)
			if err != nil || imgs == nil {
				return ""
			}
			return imgs.Default
		})
	}
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(workItemSvc, interceptorOpt))

	// RuntimeAdapterService (docs/07 §3.7).
	adapterSvc := adapter.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewRuntimeAdapterServiceHandler(adapterSvc, interceptorOpt))

	// ExecutionService (docs/07 §3.8).
	execSvc := execution.New(deps.Pool, deps.Log, deps.Subscriber)
	if deps.SendExecutionMessage != nil {
		execSvc.SetSendExecutionMessage(deps.SendExecutionMessage)
	}
	if deps.ContinueSession != nil {
		execSvc.SetContinueSession(deps.ContinueSession)
	}
	mux.Handle(apiv1connect.NewExecutionServiceHandler(execSvc, interceptorOpt))

	// PolicyService (docs/07 §3.5).
	policySvc := policy.NewService(deps.Pool, deps.Log, deps.PolicyEngine, deps.Subscriber)
	mux.Handle(apiv1connect.NewPolicyServiceHandler(policySvc, interceptorOpt))

	// RecoveryService (docs/07 §3.6, docs/06).
	recoverySvc := recovery.NewService(deps.Pool, deps.Log, deps.RecoveryEngine, deps.Subscriber)
	mux.Handle(apiv1connect.NewRecoveryServiceHandler(recoverySvc, interceptorOpt))

	// TelemetryService (docs/07 §3.9, docs/08 §5).
	telemetrySvc := telemetry.NewService(deps.Pool, deps.TelemetryQuery, deps.Subscriber)
	mux.Handle(apiv1connect.NewTelemetryServiceHandler(telemetrySvc, interceptorOpt))

	// AIGatewayService (docs/07 §3.10).
	aiGatewaySvc := aigateway.NewService(deps.Pool, deps.Log, deps.Subscriber, deps.ModelDiscoverer, deps.MCPDiscoverer)
	mux.Handle(apiv1connect.NewAIGatewayServiceHandler(aiGatewaySvc, interceptorOpt))

	// Phase 9: AuthService (docs/07 §3.12) — API keys, identities, RBAC
	// roles + bindings, tenants, audit.
	authSvc := auth.NewService(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewAuthServiceHandler(authSvc, interceptorOpt))

	// Phase 9: WebhookService (docs/07 §3.11) — subscriptions + deliveries.
	webhookSvc := webhook.NewService(deps.Pool, deps.Log, deps.WebhookDispatcher, deps.Subscriber)
	mux.Handle(apiv1connect.NewWebhookServiceHandler(webhookSvc, interceptorOpt))

	// ApprovalService — human-in-the-loop approval gates for workflow steps.
	// A higher read cap so approval decisions can carry attached files and
	// screenshots (up to 20 attachments, each up to 2-3 MiB).
	approvalSvc := approval.NewService(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewApprovalServiceHandler(
		approvalSvc,
		interceptorOpt,
		connect.WithReadMaxBytes(32<<20),
	))

	// SettingsService — tenant-level configuration defaults.
	settingsSvc := settings.New(deps.Pool, deps.Log, deps.PostgresDSN)
	mux.Handle(apiv1connect.NewSettingsServiceHandler(settingsSvc, interceptorOpt))

	// RuntimeImageService — tenant runtime container image specs + build.
	runtimeImageSvc := runtimeimage.New(deps.Pool, deps.Log, deps.RuntimeClient)
	mux.Handle(apiv1connect.NewRuntimeImageServiceHandler(runtimeImageSvc, interceptorOpt))

	// AskOrchiconService — conversational agent.
	askSvc := askorchicon.New(deps.Pool, deps.Log, deps.BlobStore, deps.ModelDiscoverer)
	if deps.SendExecutionMessage != nil {
		askSvc.SetSendExecutionMessage(deps.SendExecutionMessage)
	}
	askSvc.SetHostServe(deps.HostServe)
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(askSvc, interceptorOpt))

	// Grafana UI reverse proxy (docs/10 §11): serves Grafana same-origin
	// under /grafana so the embedded iframe in the Telemetry page works in
	// all deployment modes (not just Vite dev proxy). Grafana runs with
	// serve_from_sub_path=true and root_url=<control-plane>/grafana, so it
	// expects to be reached AT the /grafana path and generates every asset
	// and API URL with that prefix itself. The proxy therefore forwards the
	// full /grafana/... path unchanged — no StripPrefix (stripping makes
	// Grafana see "/" and 301-redirect back to the subpath, a loop).
	if deps.GrafanaURL != "" {
		grafanaTarget, err := url.Parse(deps.GrafanaURL)
		if err == nil {
			grafanaProxy := httputil.NewSingleHostReverseProxy(grafanaTarget)
			grafanaProxy.ErrorLog = nil
			mux.Handle("/grafana", grafanaProxy)
			mux.Handle("/grafana/", grafanaProxy)
		}
	}

	// Phase 9: wrap with the auth-resolution middleware. It resolves the
	// caller's identity from the bearer token (OIDC access token or API
	// key) and stores identity + tenant in the context (docs/07 §6.3).
	var h http.Handler = mux
	if deps.AuthHandler != nil {
		h = middleware.ResolveAuth(h, deps.AuthHandler.Issuer(), deps.AuthHandler.Resolver(), deps.Log)
	} else {
		// Dev fallback when auth is not configured: resolve tenant only.
		h = middleware.ResolveTenant(mux)
	}
	_ = blobstore.ErrNotFound
	return h
}
