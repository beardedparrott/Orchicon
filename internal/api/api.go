// Package api wires Connect handlers for the public API surface
// (docs/07_API_Specification.md). The generated connect-go service
// handlers are mounted here onto a single mux, wrapped by the
// auth-resolution middleware, with the RBAC Connect interceptor applied
// per-RPC. Auth is mandatory in every mode: every non-public request
// carries a resolved identity or is rejected 401.
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
	"github.com/beardedparrott/orchicon/internal/category"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/execution"
	"github.com/beardedparrott/orchicon/internal/middleware"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/providers"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/runtimeimage"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/secrets"
	"github.com/beardedparrott/orchicon/internal/settings"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/webhook"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/worker"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

//	Dependencies bundles the resources the API layer needs. Constructed
//
// once by the server and passed to Mount.
type Dependencies struct {
	Pool           *db.Pool
	Log            *slog.Logger
	Subscriber     eventbus.Subscriber
	PolicyEngine   *policy.Engine
	RecoveryEngine *recovery.Engine
	TelemetryQuery *telemetry.QueryClient
	// SecretsKEK is the resolved 32-byte KEK for the tenant secrets store
	// (ORCHICON_SECRETS_KEK override, or the per-instance data-dir key).
	// nil/len != 32 disables the store (fail-closed at the service layer).
	SecretsKEK []byte
	// GrafanaURL is the base URL of the Grafana UI (default
	// http://localhost:3000). Used by the /grafana reverse proxy so the
	// embedded iframe works same-origin (docs/10 §11). Grafana runs with
	// serve_from_sub_path, so it generates every URL under /grafana.
	GrafanaURL string
	// Phase 9: auth + webhooks + blobstore. AuthHandler is required in
	// every mode: api.Mount resolves identity through it, so a plane
	// without one is a programming error (config validation requires an
	// IdP in every mode and auth.NewHandler never returns nil).
	AuthHandler       *auth.Handler
	WebhookDispatcher *webhook.Dispatcher
	Mode              config.DeploymentMode
	// ModelDiscoverer enumerates models from opencode CLI.
	ModelDiscoverer *aigateway.ModelDiscoverer
	MCPDiscoverer   *aigateway.MCPDiscoverer
	// ModelRefRegistry is the per-adapter provider catalog (built-in ∪
	// tenant custom) for adapter-scoped model listing and legacy 2-segment
	// inference (ADR-0003). nil falls back to the built-in catalog.
	ModelRefRegistry adapter.ProviderRegistry
	// AdapterKinds returns the adapter kinds registered with the Dispatcher
	// (ADR-0004 D1) — the source of the model picker's adapter bubble tier.
	// Injected as a func to avoid an api → scheduler import cycle; nil falls
	// back to the default adapter kind.
	AdapterKinds func() []string
	// BlobStore is the object storage abstraction (local filesystem + S3).
	BlobStore blobstore.Store
	// ProvidersService is the providers settings core (ADR-0006). Mount
	// constructs it and stores it back here so the wiring order is a
	// single Mount call; nil until Mount runs (or in tests that mount
	// without the secrets KEK — token writes then fail closed).
	ProvidersService *providers.Service
	// PostgresDSN is the Postgres connection string for backup/restore.
	PostgresDSN string
	// RuntimeClient talks to the host-side runtime daemon over its unix
	// socket (build/remove runtime images). Nil when the daemon is not
	// configured (headless serve).
	RuntimeClient *runtime.Client
	// SendExecutionMessage routes a mid-run human message into a live
	// execution's adapter session (type-asserts the MessageInjector
	// capability; a non-supporting bridge yields an actionable error, never
	// a panic). Nil when the session transport is unavailable.
	SendExecutionMessage func(ctx context.Context, execID, message string) error
	// ContinueSession runs a one-shot follow-up question against a worker's
	// session in place (no new execution/work item). Nil when the session
	// transport is unavailable.
	ContinueSession func(ctx context.Context, opts scheduler.ContinueSessionOpts) (string, error)
	// AbortExecution stops a live execution's opencode session when a human
	// cancels it, so the model stops generating immediately (prevents the
	// "terminated but still active" token burn). Nil when the session
	// transport is unavailable.
	AbortExecution func(ctx context.Context, execID, reason string) error
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

	// CategoryService (first-class categories).
	catSvc := category.New(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewCategoryServiceHandler(catSvc, interceptorOpt))

	// ProviderService (ADR-0006) — tenant-facing Providers management
	// surface behind Settings → Adapters. Constructed here (before
	// WorkerService) so the worker validation path can consume the
	// tenant's enabled custom provider ids; the handler is registered on
	// the mux exactly once (the earlier/only Handle call). Note: the
	// comment above originally grouped this under WorkerService — the
	// service construction, substrate loader registration, and handler
	// mount all belong together.
	providerSvc := providers.NewHandler(deps.Pool, deps.SecretsKEK, deps.Log)
	providerSvc.Service().RegisterSubstrateLoader()
	deps.ProvidersService = providerSvc.Service()
	mux.Handle(apiv1connect.NewProviderServiceHandler(providerSvc, interceptorOpt))

	// MCPService (adapter-settings MCP management) — tenant-facing MCP
	// server surface behind Settings → Adapters → MCP: CRUD over server
	// entries (stdio + streamable HTTP), curated registry catalog with
	// one-click prefill, explicit-only auto-install (dry-run for CI), and
	// project/tenant-default selections (references, never copies). The
	// sibling MCP-client task consumes the stored entries at session time.
	mcpSvc := mcpsettings.NewHandler(deps.Pool, deps.SecretsKEK, deps.Log)
	mux.Handle(apiv1connect.NewMCPServiceHandler(mcpSvc, interceptorOpt))

	workerSvc := worker.New(deps.Pool, deps.Log)
	// Explicit adapter selections validate against the Dispatcher's
	// registered kinds (ADR-0005 D2) — the injected func avoids the
	// api → scheduler import cycle.
	workerSvc.SetAdapterKinds(deps.AdapterKinds)
	// Tenant custom providers join model-ref validation (ADR-0006 D6): the
	// global validation registry stays built-in-only; the worker service
	// merges the requesting tenant's enabled custom ids where tenant
	// context exists.
	if deps.ProvidersService != nil {
		workerSvc.SetCustomProviderIDs(deps.ProvidersService.EnabledCustomProviderIDs)
	}
	mux.Handle(apiv1connect.NewWorkerServiceHandler(workerSvc, interceptorOpt))

	// WorkflowService (docs/07 §3.4). Constructed before WorkItemService
	// so the WorkItemService can wire the StartWorkflowStarter.
	workflowSvc := workflow.New(deps.Pool, deps.Log, deps.Subscriber)
	if deps.AbortExecution != nil {
		workflowSvc.SetAbortExecution(deps.AbortExecution)
	}
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
	if deps.AbortExecution != nil {
		scheduler.SetStopAbortHook(deps.AbortExecution)
	}
	if deps.RuntimeClient != nil {
		// Reuse any existing runtime lifecycle for reap; scheduler will reap via hook if set
	}
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
	if deps.AbortExecution != nil {
		execSvc.SetAbortExecution(deps.AbortExecution)
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

	// AIGatewayService (docs/07 §3.10). The CLI-aware validation registry
	// composes BEFORE the gateway and settings services: the static
	// registry (builtin ∪ tenant customs) wrapped with live CLI provider
	// discovery, so CLI-namespace refs (e.g. opencode/deepseek/…) validate
	// at save time and the gateway's models RPC scopes through the same
	// composition. ModelRefRegistry nil → the composition becomes the
	// registry.
	cliRegistry := aigateway.NewCLIProviderRegistry(deps.ModelRefRegistry, deps.ModelDiscoverer)
	if deps.ModelRefRegistry == nil {
		deps.ModelRefRegistry = cliRegistry
	}
	aiGatewaySvc := aigateway.NewService(deps.Pool, deps.Log, deps.Subscriber, deps.ModelDiscoverer, deps.MCPDiscoverer, deps.ModelRefRegistry, deps.AdapterKinds)
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
	// The settings validator shares the CLI-aware registry (composed above):
	// the validator must agree with the picker or every CLI-namespace ref
	// the picker offered fails at save with "provider not found".
	settingsSvc.SetValidationRegistry(cliRegistry)
	mux.Handle(apiv1connect.NewSettingsServiceHandler(settingsSvc, interceptorOpt))

	// RuntimeImageService — tenant runtime container image specs + build.
	runtimeImageSvc := runtimeimage.New(deps.Pool, deps.Log, deps.RuntimeClient)
	mux.Handle(apiv1connect.NewRuntimeImageServiceHandler(runtimeImageSvc, interceptorOpt))
	// Heal ghost builds stuck in building after stream drops / daemon restarts (boot + periodic).
	go func() {
		ctx := context.Background()
		if _, err := runtimeImageSvc.ReconcileStuckBuilding(ctx, 0); err != nil {
			deps.Log.Warn("runtime image reconcile (boot) failed", "error", err)
		}
	}()
	go runtimeImageSvc.StartReconciler(context.Background())

	// SecretsService — tenant-scoped encrypted secrets (Tavily etc.).
	// The KEK is resolved once at server construction (env override or
	// first-boot generation in the instance data dir); a nil/short key
	// leaves the store disabled (fail-closed at the service layer).
	secretsSvc := secrets.NewHandler(deps.Pool, deps.SecretsKEK, deps.Log)
	mux.Handle(apiv1connect.NewSecretsServiceHandler(secretsSvc, interceptorOpt))

	// AskOrchiconService — conversational agent.
	askSvc := askorchicon.New(deps.Pool, deps.Log, deps.BlobStore, deps.ModelDiscoverer, deps.SecretsKEK)
	askSvc.SetAdapterKinds(deps.AdapterKinds)
	if deps.SendExecutionMessage != nil {
		askSvc.SetSendExecutionMessage(deps.SendExecutionMessage)
	}
	askSvc.SetHostServe(deps.HostServe)
	if deps.RuntimeClient != nil {
		askSvc.SetRuntimeClient(deps.RuntimeClient)
	}
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

	// Wrap the whole surface with the auth-resolution middleware
	// (docs/07 §6.3). It resolves the caller's identity from the bearer
	// token (OIDC access token or API key) and stores identity + tenant
	// in the context. Auth is mandatory in every mode — a request without
	// a valid credential is 401 and the tenant always comes from the
	// resolved identity, never a request header. There is no tenant-only
	// fallback and no anonymous path into tenant-scoped data.
	h := middleware.ResolveAuth(mux, deps.AuthHandler.Issuer(), deps.AuthHandler.Resolver(), deps.Log)
	_ = blobstore.ErrNotFound
	return h
}
