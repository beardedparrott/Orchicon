// Package api wires Connect handlers for the public API surface (docs/07_API_Specification.md).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

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
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/execution"
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
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
	// SecretsKEK is the resolved 32-byte KEK for the tenant secrets store (ORCHICON_SECRETS_KEK
	// override, or the per-instance data-dir key).
	SecretsKEK []byte
	// UsageRecorder is the shared AI Gateway usage recorder (Postgres + OTel dual-write) worker
	// executions use.
	UsageRecorder *aigateway.UsageRecorder
	// FileEditService is the shared diff-pipeline ledger service (server constructed over the PG
	// store).
	FileEditService *fileedit.Service
	// Phase 9: auth + webhooks + blobstore.
	AuthHandler       *auth.Handler
	WebhookDispatcher *webhook.Dispatcher
	Mode              config.DeploymentMode
	// ModelDiscoverer enumerates models from opencode CLI.
	ModelDiscoverer *aigateway.ModelDiscoverer
	MCPDiscoverer   *aigateway.MCPDiscoverer
	// DiscovererFactory re-resolves the pair above when boot could not.
	DiscovererFactory aigateway.DiscovererFactory
	// ModelRefRegistry is the per-adapter provider catalog (built-in ∪ tenant custom) for
	// adapter-scoped model listing and legacy 2-segment inference.
	ModelRefRegistry adapter.ProviderRegistry
	// AdapterKinds returns the adapter kinds registered with the Dispatcher — the source
	// of the model picker's adapter bubble tier.
	AdapterKinds func() []string
	// AdapterChatKinds returns the adapter kinds whose bridge implements the ChatTurnClient (Ask chat)
	// capability.
	AdapterChatKinds func() []string
	// Dispatcher is the shared adapter routing substrate.
	Dispatcher *scheduler.Dispatcher
	// BlobStore is the object storage abstraction (local filesystem + S3).
	BlobStore blobstore.Store
	// ProvidersService is the providers settings core.
	ProvidersService *providers.Service
	// PostgresDSN is the Postgres connection string for backup/restore.
	PostgresDSN string

	// PermissionPolicyPath is the operator's persistent permission policy file
	// (ORCHICON_PERMISSION_POLICY).
	PermissionPolicyPath string
	// RuntimeClient talks to the host-side runtime daemon over its unix socket (build/remove runtime
	// images).
	RuntimeClient *runtime.Client
	// SendExecutionMessage routes a mid-run human message into a live execution's adapter session
	// (type-asserts the MessageInjector capability; a non-supporting bridge yields an actionable error.
	SendExecutionMessage func(ctx context.Context, execID, message string) error
	// ContinueSession runs a one-shot follow-up question against a worker's session in place (no new
	// execution/work item).
	ContinueSession func(ctx context.Context, opts scheduler.ContinueSessionOpts) (string, error)
	// AbortExecution stops a live execution's opencode session when a human cancels it, so the model
	// stops generating immediately (prevents the "terminated but still active" token burn).
	AbortExecution func(ctx context.Context, execID, reason string) error
	// ReleaseStuckExecutions runs the execution-liveness reaper NOW, scoped to one project, and returns
	// the ids it released along with how many slot-holding executions it examined.
	ReleaseStuckExecutions func(ctx context.Context, projectID string) (releasedIDs []string, examined int, err error)
	// HostServe is the always-on host opencode serve.
	HostServe *opencode.HostServe
	// AskService is the AskOrchiconService handler Mount constructs.
	AskService *askorchicon.Service
}

// Mount returns an http.Handler serving the Orchicon API.
func Mount(mux *http.ServeMux, deps *Dependencies) http.Handler {
	if deps == nil {
		panic("api.Mount: nil *Dependencies — programming error")
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version.Current().Tag + `"}`))
	})

	// /enginez reports whether RUNTIME CONTAINERS are available on this host, and why not when they are
	// not (AC5).
	mux.Handle("/enginez", newEngineStatusCache(30*time.Second, containerengine.DefaultProbe()))

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

	// ProviderService — tenant-facing Providers management surface behind Settings →
	// Adapters.
	providerSvc := providers.NewHandler(deps.Pool, deps.SecretsKEK, deps.Log)
	// Provider-landscape mutations invalidate the model discoverer's cache immediately (custom provider
	// CRUD, token saves): new models become usable right away instead of after the discovery TTL.
	if deps.ModelDiscoverer != nil {
		providerSvc.OnProviderChange = deps.ModelDiscoverer.Invalidate
	}
	providerSvc.Service().RegisterSubstrateLoader()
	deps.ProvidersService = providerSvc.Service()
	mux.Handle(apiv1connect.NewProviderServiceHandler(providerSvc, interceptorOpt))

	// MCPService (adapter-settings MCP management) — tenant-facing MCP server surface behind Settings →
	// Adapters → MCP: CRUD over server definitions (project XOR Ask conversation).
	mcpSvc := mcpsettings.NewHandler(deps.Pool, deps.SecretsKEK, deps.Log)
	mux.Handle(apiv1connect.NewMCPServiceHandler(mcpSvc, interceptorOpt))

	workerSvc := worker.New(deps.Pool, deps.Log)
	// Explicit adapter selections validate against the Dispatcher's registered kinds —
	// the injected func avoids the api → scheduler import cycle.
	workerSvc.SetAdapterKinds(deps.AdapterKinds)
	// Tenant custom providers join model-ref validation: the global validation registry
	// stays built-in-only; the worker service merges the requesting tenant's enabled custom ids.
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
	// Sequence auto-start (run-instant on a parent with children): fire the chain through the sequence
	// engine.
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
	if deps.ReleaseStuckExecutions != nil {
		execSvc.SetReleaseStuckExecutions(deps.ReleaseStuckExecutions)
	}

	mux.Handle(apiv1connect.NewExecutionServiceHandler(execSvc, interceptorOpt))
	// FileEditService (docs/07 §3.8 diff pipeline): the standalone fetch +
	// stream RPCs for the GUI sidebar / TUI pane over the pool-backed store.
	if deps.Pool != nil {
		feSvc := fileedit.NewRPCService(fileedit.NewPGStore(deps.Pool), deps.Log, deps.Subscriber)
		mux.Handle(apiv1connect.NewFileEditServiceHandler(feSvc, interceptorOpt))
	}

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
	cliRegistry := aigateway.NewCLIProviderRegistry(deps.ModelRefRegistry, deps.ModelDiscoverer)
	if deps.ModelRefRegistry == nil {
		deps.ModelRefRegistry = cliRegistry
	}
	// Worker model_ref validation shares the same composition as the picker and settings (builtin ∪
	// tenant customs ∪ live CLI provider ids): validation must agree with what the picker offers.
	worker.SetModelRefRegistry(cliRegistry)
	aiGatewaySvc := aigateway.NewService(deps.Pool, deps.Log, deps.Subscriber, deps.ModelDiscoverer, deps.MCPDiscoverer, deps.ModelRefRegistry, deps.AdapterKinds, deps.AdapterChatKinds)
	if deps.DiscovererFactory != nil {
		aiGatewaySvc.SetDiscovererFactory(deps.DiscovererFactory)
	}
	mux.Handle(apiv1connect.NewAIGatewayServiceHandler(aiGatewaySvc, interceptorOpt))

	// Phase 9: AuthService (docs/07 §3.12) — API keys, identities, RBAC
	// roles + bindings, tenants, audit.
	authSvc := auth.NewService(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewAuthServiceHandler(authSvc, interceptorOpt))

	// Phase 9: WebhookService (docs/07 §3.11) — subscriptions + deliveries.
	webhookSvc := webhook.NewService(deps.Pool, deps.Log, deps.WebhookDispatcher, deps.Subscriber)
	mux.Handle(apiv1connect.NewWebhookServiceHandler(webhookSvc, interceptorOpt))

	// ApprovalService — human-in-the-loop approval gates for workflow steps.
	approvalSvc := approval.NewService(deps.Pool, deps.Log)
	mux.Handle(apiv1connect.NewApprovalServiceHandler(
		approvalSvc,
		interceptorOpt,
		connect.WithReadMaxBytes(32<<20),
	))

	// SettingsService — tenant-level configuration defaults.
	settingsSvc := settings.New(deps.Pool, deps.Log, deps.PostgresDSN)
	settingsSvc.SetPermissionPolicyPath(deps.PermissionPolicyPath)
	// The settings validator shares the CLI-aware registry (composed above): the validator must agree
	// with the picker or every CLI-namespace ref the picker offered fails at save with "provider not.
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
	secretsSvc := secrets.NewHandler(deps.Pool, deps.SecretsKEK, deps.Log)
	// A ROTATED PROVIDER KEY MUST REACH THE NEXT REQUEST.
	if deps.ProvidersService != nil {
		secretsSvc.SetOnChanged(deps.ProvidersService.InvalidateSecret)
	}
	mux.Handle(apiv1connect.NewSecretsServiceHandler(secretsSvc, interceptorOpt))

	// AskOrchiconService — conversational agent.
	askSvc := askorchicon.New(deps.Pool, deps.Log, deps.BlobStore, deps.ModelDiscoverer, deps.SecretsKEK)
	askSvc.SetAdapterKinds(deps.AdapterKinds)
	askSvc.SetChatKinds(deps.AdapterChatKinds)
	askSvc.SetDispatcher(deps.Dispatcher)
	// The Ask update_settings tool write path shares the same CLI-aware model ref registry as the
	// settings validator/picker.
	askSvc.SetValidationRegistry(cliRegistry)
	if deps.SendExecutionMessage != nil {
		askSvc.SetSendExecutionMessage(deps.SendExecutionMessage)
	}
	askSvc.SetHostServe(deps.HostServe)
	if deps.RuntimeClient != nil {
		askSvc.SetRuntimeClient(deps.RuntimeClient)
	}
	if deps.UsageRecorder != nil {
		askSvc.SetUsageRecorder(deps.UsageRecorder)
	}
	// Diff pipeline (plan step 8): the Ask-side ledger hook + terminal-turn git reconciliation.
	if deps.FileEditService != nil && deps.Pool != nil {
		feAsk := deps.FileEditService
		// Per-conversation observers: cache last-seen content per path
		// (git-HEAD-seeded) for the built-in write/edit after-reads.
		feAskObs := map[string]*fileedit.Observer{}
		var feAskMu sync.Mutex
		feObserver := func(convID, baseDir string) *fileedit.Observer {
			if baseDir == "" {
				return nil
			}
			feAskMu.Lock()
			defer feAskMu.Unlock()
			o, ok := feAskObs[convID]
			if !ok {
				o = fileedit.NewObserver(baseDir)
				feAskObs[convID] = o
			}
			return o
		}

		askSvc.SetFileEditHook(func(ctx context.Context, tenantID, convID, toolName string, input map[string]any, output string) {
			switch toolName {
			case "write", "edit":
				// The worktree engine's single-op write/edit wrappers emit the structured file_edits payload (exact
				// ground truth) under these same tool names.
				if parsed, _ := feAsk.RecordEngineOutput(ctx, tenantID, db.FileEditOwnerAskConversation, convID, toolName, output); parsed > 0 {
					break
				}
				dir := askBaseDir(ctx, deps)
				o := feObserver(convID, dir)
				if o == nil {
					return
				}
				p, _ := input["filePath"].(string)
				if p == "" {
					p, _ = input["path"].(string)
				}
				if p == "" {
					return
				}
				tool := fileedit.ToolOpenCodeWrite
				if toolName == "edit" {
					tool = fileedit.ToolOpenCodeEdit
				}
				if e := o.ObserveAfter(p, tool); e.Path != "" {
					feAsk.Record(ctx, tenantID, db.FileEditOwnerAskConversation, convID, []fileedit.Entry{e})
				}
			case "batch_write":
				// Worktree engine tool: the engine already computed the
				// ground-truth diffs in its structured output.
				feAsk.RecordEngineOutput(ctx, tenantID, db.FileEditOwnerAskConversation, convID, toolName, output)
			}
		})
		askSvc.SetFileEditReconciler(func(ctx context.Context, tenantID, convID string) {
			dir := askBaseDir(ctx, deps)
			if dir == "" {
				return
			}
			if err := fileedit.ReconcileGit(ctx, fileedit.NewPGStore(deps.Pool), dir, tenantID, db.FileEditOwnerAskConversation, convID, "", deps.Log); err != nil {
				deps.Log.Warn("ask file edit ledger git reconciliation failed", "conversation", convID, "dir", dir, "error", err)
			}
		})
	}
	deps.AskService = askSvc
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(askSvc, interceptorOpt))

	// ANY future UI surface that finds itself unable to send a credential must satisfy that constraint
	// the same way — read through the authenticated client.

	// There is no tenant-only fallback and no anonymous path into tenant-scoped data, and it is the
	// ONLY handler chain this package returns — there is no outer mux (see the historical note above).
	h := middleware.ResolveAuth(mux, deps.AuthHandler.Issuer(), deps.AuthHandler.Resolver(), deps.Log)
	_ = blobstore.ErrNotFound
	return h
}

// askBaseDir resolves the directory an Ask conversation's file edits resolve against.
func askBaseDir(ctx context.Context, deps *Dependencies) string {
	if deps == nil || deps.Pool == nil {
		return ""
	}
	scope, err := askorchicon.AskFileScopeFor(ctx, deps.Pool)
	if err != nil {
		// A conversation with no project, or a project whose dir is unset.
		return ""
	}
	return strings.TrimSpace(scope.Dir)
}
