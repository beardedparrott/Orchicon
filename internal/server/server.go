// Package server boots the Orchicon control plane: opens the database pool, runs migrations (if
// enabled), seeds the dev tenant, starts the outbox relay, mounts the API.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/api"
	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/backup"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/ephemeral"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/logging"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/outbox"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/runtimeimage"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/webhook"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// Server owns the running control plane process and its dependencies.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	pool    *db.Pool
	relay   *outbox.Relay
	rcmgr   *reconciler.Manager
	otel    *telemetry.Shutdowner
	httpSrv *http.Server
	// Phase 9
	blobs    blobstore.Store
	authH    *auth.Handler
	webhookD *webhook.Dispatcher
	// Per-workflow runtime container lifecycle (nil when the daemon
	// socket is absent — headless serve).
	runtime *runtime.Lifecycle
	// Execution liveness reaper (fails executions orphaned by a plane
	// restart / lost runtime container).
	reaper *scheduler.ExecutionReaper
	// Rotating on-disk log writer (nil when not detached). Settings →
	// Defaults log management values are live-applied to it.
	logWriter *logging.RotatingWriter
	// hostServe is the DEMAND-KEYED host opencode serve (lazy: started on the first opencode demand).
	hostServe *opencode.HostServe
	// askHostServe is the Ask Orchicon serve (interactive permission profile).
	askHostServe *opencode.HostServe
	// hostServePool / askServePool are the RESOLVED-SET-KEYED pools of host serves for the in-process
	// population.
	hostServePool *opencode.HostServePool
	askServePool  *opencode.HostServePool
	// claudeBridge is the claude adapter, held so plane shutdown can retire its Ask sessions.
	claudeBridge *claude.Bridge
	// askService is the Ask Orchicon service, held so plane shutdown can close its per-conversation MCP
	// clients.
	askService *askorchicon.Service
}

// New constructs a Server from configuration.
func New(cfg config.Config, log *slog.Logger, logWriter *logging.RotatingWriter) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Secrets KEK: an explicit ORCHICON_SECRETS_KEK override wins.
	secretsKEK, kekErr := secretcrypto.ResolveKEK(cfg.SecretsKEK, cfg.DataDir)
	if kekErr != nil {
		log.Warn("secrets KEK unavailable (secrets store disabled)", "error", kekErr)
	}

	// OTel telemetry pipeline (tracer + meter + OTLP exporter → the telemetry backends).
	var otelShutdown *telemetry.Shutdowner
	var err error
	if cfg.Telemetry != "none" {
		otelShutdown, err = telemetry.Setup(context.Background(), cfg, log)
		if err != nil {
			log.Warn("otel setup failed (telemetry disabled)", "error", err)
		}
	}

	pool, err := db.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("server: open db: %w", err)
	}

	// Seed the deployment tenant so the control plane has a tenant context before auth (Phase 9) lands.
	if err := db.SeedDevTenant(context.Background(), pool, cfg.DeploymentTenantID); err != nil {
		log.Warn("seed dev tenant failed (continuing)", "error", err)
	}

	// Seed canned workers for the deployment tenant so they're available for workflow templates and
	// manual dispatch.
	if err := db.SeedDevWorkers(context.Background(), pool, cfg.DeploymentTenantID); err != nil {
		log.Warn("seed dev workers failed (continuing)", "error", err)
	}

	if err := db.SeedDevWorkflows(context.Background(), pool); err != nil {
		log.Warn("seed dev workflows failed (continuing)", "error", err)
	}

	// Connect to NATS and start the outbox relay.
	pub, err := eventbus.NewNATSPublisher(context.Background(), cfg.NATSURL)
	if err != nil {
		log.Warn("nats publisher unavailable at boot (relay will retry via reconnect)", "error", err)
	} else {
		log.Info("nats publisher connected", "url", cfg.NATSURL)
	}

	// NATS subscriber for streaming RPCs (StreamProjectEvents etc.).
	// Created lazily by the eventbus when a stream RPC first connects.
	var sub eventbus.Subscriber
	if pub != nil {
		sub, err = eventbus.NewNATSSubscriber(context.Background(), cfg.NATSURL)
		if err != nil {
			log.Warn("nats subscriber unavailable at boot (streaming disabled)", "error", err)
		}
	}

	mux := http.NewServeMux()
	// Phase 7: Policy Engine (Rego) + Recovery Workflow Engine.
	policyEngine := policy.New(pool, log)
	recoveryEngine := recovery.New(pool, log)
	// Phase 8: Telemetry query client (docs/08 §5).
	telemetryQuery := telemetry.NewQueryClient(cfg.TempoURL, cfg.LokiURL, cfg.VMURL)
	log.Info("telemetry query client configured",
		"tempo", cfg.TempoURL, "loki", cfg.LokiURL, "vm", cfg.VMURL)

	// Phase 9: BlobStore abstraction (docs/01 §2). The local filesystem
	// store is production-viable; S3 is the cloud backend.
	blobs, err := blobstore.New(context.Background(), cfg.BlobStore)
	if err != nil {
		log.Warn("blob store init failed (object storage disabled)", "error", err)
	} else {
		log.Info("blob store ready", "kind", cfg.BlobStore.Kind)
	}

	// Phase 9: Auth handler (OIDC code-flow + dev IdP + token issuer).
	authHandler := auth.NewHandler(cfg, pool, log)
	log.Info("auth configured", "issuer", cfg.Auth.Issuer, "mode", cfg.Mode)

	// BootstrapLocalAdmin mints a credential ONLY when the operator pins BOTH
	// ORCHICON_LOCAL_ADMIN_USERNAME and ORCHICON_LOCAL_ADMIN_PASSWORD.
	if err := auth.BootstrapLocalAdmin(context.Background(), pool, log, cfg); err != nil {
		log.Warn("local admin bootstrap failed (continuing)", "error", err)
	}

	// Phase 9: Webhook dispatcher (NATS consumer → HTTP POST + retries + dead-letter — docs/07 §3.11).
	var webhookDisp *webhook.Dispatcher
	if sub != nil {
		webhookDisp = webhook.NewDispatcher(pool, sub, log)
	}

	// Adapter demand set at boot (AC 1/AC 6): whether this plane is "opencode-free" is a COMPUTED
	// consequence of the model refs the tenant actually references (tenant Ask default ∪ dispatchable.
	opencodeInDemand := true
	if dtx, derr := pool.BeginTenantTx(context.Background(), cfg.DeploymentTenantID); derr != nil {
		log.Warn("adapter demand set unavailable at boot — probing for opencode as before", "error", derr)
	} else {
		demand, dserr := adapter.TenantDemandSet(context.Background(), dtx.Tx, cfg.DeploymentTenantID)
		dtx.Rollback(context.Background())
		if dserr != nil {
			log.Warn("adapter demand set unavailable at boot — probing for opencode as before", "error", dserr)
		} else {
			opencodeInDemand = demand.Has(adapter.DefaultAdapterKind)
			if !opencodeInDemand {
				log.Info("plane is opencode-free — no opencode binary probed at boot",
					"tenant", cfg.DeploymentTenantID, "demanded_kinds", strings.Join(demand.Kinds(), ","))
			}
		}
	}

	// Model discoverer: shells out to opencode CLI to list models (the legacy opencode/claude adapter
	// picker path).
	var modelDiscoverer *aigateway.ModelDiscoverer
	var mcpDiscoverer *aigateway.MCPDiscoverer

	// ONE CONSTRUCTION PATH, CALLED TWICE.
	resolveDiscoverers := func() (*aigateway.ModelDiscoverer, *aigateway.MCPDiscoverer, error) {
		// ONE resolver, shared with the adapter's spawn path and the host serve
		// (internal/opencode/binary.go).
		opencodeBin, binErr := opencode.ResolveBinaryPath()
		if binErr != nil {
			return nil, nil, binErr
		}
		model, mcp := aigateway.BuildDiscoverers(context.Background(), log, opencodeBin)
		// The plane runs on the host and spawns opencode with the host PATH, but a workflow execution runs
		// in a runtime container that sees ONLY the adapter paths the runtime daemon bind-mounts.
		if !opencode.ContainerVisible(opencodeBin) {
			log.Warn("opencode resolves to a path the runtime containers cannot see — workflow-run executions will not find it",
				"path", opencodeBin,
				"fix", "install opencode under $HOME as your user, never with sudo: curl -fsSL https://opencode.ai/install | bash")
		}
		return model, mcp, nil
	}

	if opencodeInDemand {
		if m, mc, binErr := resolveDiscoverers(); binErr != nil {
			// THE MESSAGE NO LONGER PROMISES A RESTART IS NEEDED, because it is not: the service re-resolves on
			// the next request for models, so installing opencode and re-selecting the adapter is enough.
			log.Warn("opencode binary not found at boot — discovery stays unconfigured until one is resolvable (install opencode on PATH, set ORCHICON_OPENCODE_BIN, or just retry the model picker after installing it)", "error", binErr)
		} else {
			modelDiscoverer, mcpDiscoverer = m, mc
		}
	} else {
		// NOT A DEAD END ANY MORE.
		log.Info("no opencode demand at boot — discovery will resolve on the first request for opencode models")
	}

	// Reconciler framework (docs/03 §2).
	adapterBridge := opencode.New(log)
	// Concrete adapter types appear ONLY here, at construction/registration; every other path goes
	// through the scheduler-defined contract interfaces.
	dispatcher := scheduler.NewDispatcher()

	// Demand-keyed host opencode serve for the in-process execution population (Stage 3 session
	// transport): standalone dispatches, follow-ups.
	var hostServe *opencode.HostServe
	var askServe *opencode.HostServe
	// hostServePool backs the in-process population with a serve per RESOLVED MCP set: a shared serve
	// builds its config ONCE and lives for the plane.
	var hostServePool *opencode.HostServePool
	var askServePool *opencode.HostServePool
	if os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT") != "0" {
		dataDir := ""
		if home, herr := os.UserHomeDir(); herr == nil {
			dataDir = filepath.Join(home, ".local", "share", "orchicon", "opencode")
		}
		if dataDir != "" {
			hostServe = opencode.NewHostServe(log, dataDir, "")
			adapterBridge.SetHostServe(hostServe)
			hostServePool = opencode.NewHostServePool(log, dataDir, "", opencode.ProfileWorker)
			hostServePool.SetDefaultServe(hostServe)
			adapterBridge.SetHostServePool(hostServePool)
			adapterBridge.SetHostScopeResolver(mcpsettings.NewResolver(pool))
			// Ask Orchicon gets its OWN serve: opencode's permission config is per-process, so the interactive
			// profile cannot ride the worker serve without leaking into dispatched executions.
			askServe = opencode.NewAskHostServe(log, dataDir+"-ask", "")
			adapterBridge.SetAskHostServe(askServe)
			askServePool = opencode.NewHostServePool(log, dataDir+"-ask", "", opencode.ProfileInteractive)
			askServePool.SetDefaultServe(askServe)
			// AND THE ADAPTER GETS IT, so an Ask turn can land on a serve whose baked config carries the
			// conversation's own MCP servers.
			adapterBridge.SetAskHostServePool(askServePool)
		} else {
			log.Warn("host opencode serve data dir unavailable — sessions disabled")
		}
	}

	// Phase 8: wire the AI Gateway usage recorder into the adapter so step_finish token/cost telemetry
	// is dual-written to Postgres (source of truth) + OTel metrics (VictoriaMetrics) — docs/08 §5.2.
	usageRecorder := aigateway.NewUsageRecorder(pool, log)
	// Cost authority lives in the gateway: pricingResolver(modelDiscoverer)
	// (internal/server/pricing_resolver.go) resolves per provider/model so the recorded CostUSD.
	usageRecorder.SetPricingResolver(pricingResolver(modelDiscoverer))
	usageRecorderFn := func(ctx context.Context, in scheduler.UsageRecord) error {
		// Adapter parity: attribute the sample to the adapter that ACTUALLY drove the model call.
		adapterKind := in.AdapterKind
		if adapterKind == "" {
			adapterKind = "opencode"
		}
		_, err := usageRecorder.Record(ctx, aigateway.UsageInput{
			TenantID:         in.TenantID,
			ProjectID:        in.ProjectID,
			TaskID:           in.TaskID,
			ExecutionID:      in.ExecutionID,
			WorkerID:         in.WorkerID,
			Provider:         in.Provider,
			Model:            in.Model,
			PromptTokens:     in.PromptTokens,
			CacheReadTokens:  in.CacheReadTokens,
			CacheWriteTokens: in.CacheWriteTokens,
			CompletionTokens: in.CompletionTokens,
			ReasoningTokens:  in.ReasoningTokens,
			CostUSD:          in.CostUSD,
			AdapterKind:      adapterKind,
			CorrelationID:    in.CorrelationID,
			TraceID:          in.TraceID,
			WorkflowRunID:    in.WorkflowRunID,
		})
		return err
	}
	adapterBridge.SetUsageRecorder(usageRecorderFn)
	// Durable session transcript (Stage 3): the adapter's session path records every side of the worker
	// conversation into execution_session_parts via this writer (best-effort.
	sessionStoreFn := func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error {
		ttx, err := pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return err
		}
		defer ttx.Rollback(ctx)
		if err := db.AppendExecutionSessionParts(ctx, ttx.Tx, tenantID, parts); err != nil {
			return err
		}
		return ttx.Commit(ctx)
	}
	adapterBridge.SetSessionStore(sessionStoreFn)

	// Diff pipeline (file-edit ledger): ground-truth, server-computed diffs for every file the session
	// touches.
	feStore := fileedit.NewPGStore(pool)
	feSvc := fileedit.NewService(feStore, log)
	// THE DIFF LEDGER IS INSTALLED ON THE DISPATCHER, ONCE, FOR EVERY BRIDGE.
	dispatcher.SetDiffLedger(
		scheduler.DiffLedgerFunc(newFileEditHook(feSvc, log, db.FileEditOwnerExecution)),
		scheduler.DiffLedgerFunc(newFileEditHook(feSvc, log, db.FileEditOwnerAskConversation)),
	)
	if pub != nil {
		// PUBLISHED FOR EVERY OWNER KIND, under its OWN subject and event type.
		feSvc.Publisher = func(ownerKind, ownerID string, row *db.FileEditLedgerRow) {
			subject, payload, err := fileedit.EventPayload(ownerKind, ownerID, row)
			if err != nil {
				return
			}
			_ = pub.Publish(context.Background(), subject, row.ID, payload)
		}
	}
	// File-edit ledger hook (diff pipeline): the execution population's ingestion point — engine
	// file_edits output tried first for write/edit.

	// This is the ONLY place the concrete adapter appears in a dispatch-capable position.
	dispatcher.Register("opencode", adapterBridge)

	taskRec := scheduler.NewTaskReconciler(pool, log, dispatcher)
	// Diff pipeline (AC 4): at every terminal transition the file-edit
	// ledger reconciles against the run worktree's git state (best-effort).
	taskRec.SetFileEditReconciler(newFileEditReconciler(pool, log))
	// Bounded in-pass fan-out for the scan pass: independent ready tasks
	// dispatch concurrently (ORCHICON_DISPATCH_CONCURRENCY, default 4).
	taskRec.SetDispatchConcurrency(cfg.DispatchConcurrency)
	// Per-project/tenant max-concurrent-runs admission gate (concurrency guards): reconcileOne holds a
	// dispatch when the project is at its effective cap, leaving the item ready until a slot frees.
	taskRec.SetDispatchLimiter(scheduler.DBDispatchLimiter())
	// Per-workflow runtime containers: the control plane talks to the host-side runtime daemon over a
	// unix socket.
	var rtClient *runtime.Client
	if cfg.RuntimeSocket != "" {
		rtClient = runtime.NewClient(cfg.RuntimeSocket, cfg.Instance)
	}

	// Seed canned "stock" runtime image rows (one per daemon-reported stock variant) so they appear as
	// normal, editable rows in the Runtime Images page — the canned-image equivalent of SeedDevWorkers.
	if err := runtimeimage.SeedCannedImages(context.Background(), pool, log, rtClient); err != nil {
		log.Warn("seed canned runtime images failed (continuing)", "error", err)
	}
	// adapterKind resolves the execution's adapter kind for mid-run RPCs.
	adapterKind := func(ctx context.Context, execID string) (string, error) {
		return resolveAdapterKind(ctx, pool, cfg.DeploymentTenantID, execID)
	}
	// execReaper is captured by the ReleaseStuckExecutions closure below and assigned once the reaper
	// exists: it is built AFTER this literal.
	var execReaper *scheduler.ExecutionReaper
	deps := api.Dependencies{
		Pool:              pool,
		Log:               log,
		Subscriber:        sub,
		PolicyEngine:      policyEngine,
		RecoveryEngine:    recoveryEngine,
		TelemetryQuery:    telemetryQuery,
		AuthHandler:       authHandler,
		WebhookDispatcher: webhookDisp,
		Mode:              cfg.Mode,
		ModelDiscoverer:   modelDiscoverer,
		MCPDiscoverer:     mcpDiscoverer,
		// The SAME closure boot used, so a late resolution cannot diverge from the early one.
		DiscovererFactory: resolveDiscoverers,
		AdapterKinds:      dispatcher.Kinds,
		AdapterChatKinds:  dispatcher.ChatKinds,
		Dispatcher:        dispatcher,
		BlobStore:         blobs,
		PostgresDSN:       cfg.PostgresDSN,
		// The permission policy file the settings service reads and writes (and that Boot already checked)
		// — one path, resolved once from config, so the API and the enforcement seams cannot disagree.
		PermissionPolicyPath: cfg.PermissionPolicyPath,
		RuntimeClient:        rtClient,
		// adapterKind resolves the execution's adapter kind for mid-run RPCs (dispatching adapter_id
		// preferred, worker model_ref fallback) via the shared resolver and dispatcher.
		SendExecutionMessage: func(ctx context.Context, execID, message string) error {
			kind, err := adapterKind(ctx, execID)
			if err != nil {
				return fmt.Errorf("resolve adapter for execution %s: %w", execID, err)
			}
			bridge, err := dispatcher.Resolve(kind)
			if err != nil {
				return err
			}
			inj, ok := bridge.(scheduler.MessageInjector)
			if !ok {
				return fmt.Errorf("adapter kind %q does not support mid-run message injection", kind)
			}
			return inj.SendExecutionMessage(ctx, execID, message)
		},
		ContinueSession: func(ctx context.Context, opts scheduler.ContinueSessionOpts) (string, error) {
			kind, err := adapterKind(ctx, opts.ExecutionID)
			if err != nil {
				return "", fmt.Errorf("resolve adapter for execution %s: %w", opts.ExecutionID, err)
			}
			bridge, err := dispatcher.Resolve(kind)
			if err != nil {
				return "", err
			}
			cont, ok := bridge.(scheduler.SessionContinuer)
			if !ok {
				return "", fmt.Errorf("adapter kind %q does not support follow-up sessions", kind)
			}
			return cont.ContinueSession(ctx, opts)
		},
		AbortExecution: func(ctx context.Context, execID, reason string) error {
			kind, err := adapterKind(ctx, execID)
			if err != nil {
				return fmt.Errorf("resolve adapter for execution %s: %w", execID, err)
			}
			bridge, err := dispatcher.Resolve(kind)
			if err != nil {
				return err
			}
			aborter, ok := bridge.(scheduler.Aborter)
			if !ok {
				return fmt.Errorf("adapter kind %q does not support live-session abort", kind)
			}
			return aborter.AbortExecution(ctx, execID, reason)
		},
		// ReleaseStuckExecutions backs the run view's "release stuck executions": it runs the liveness
		// reaper NOW, scoped to one project.
		ReleaseStuckExecutions: func(ctx context.Context, projectID string) ([]string, int, error) {
			if execReaper == nil {
				return nil, 0, errors.New("execution reaper is not wired on this plane")
			}
			return execReaper.ReapProject(ctx, projectID)
		},
		HostServe:  askServe,
		SecretsKEK: secretsKEK,
		// UsageRecorder wires the shared AI Gateway recorder into Ask
		// Orchicon so Ask sessions capture live usage per adapter.
		UsageRecorder: usageRecorder,
		// FileEditService wires the diff-pipeline ledger into Ask Orchicon: Ask conversations ledger file
		// edits from real file-state snapshots (owner_kind ask_conversation).
		FileEditService: feSvc,
	}
	handler := api.Mount(mux, &deps)

	// Native in-process session engine (adapter kind "orchicon").
	nativeBridge := orchicon.NewBridge(
		orchiconRegistryResolver(&deps, pool, secretsKEK, log),
		"",
		log,
	)
	// MCP definitions are scope-addressed (ADR-0012): ResolveScope returns the project-owned ∪
	// scope-owned union (SetScopeResolver).
	nativeBridge.SetScopeResolver(mcpsettings.NewResolver(pool))
	nativeBridge.SetMCPSecretResolver(func(ctx context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error) {
		return mcpsettings.ResolveSecretRefs(ctx, pool, secretsKEK, tenantID, env, headers)
	})
	// The NATIVE Ask path resolves and starts its own MCP client per conversation (ask_mcp.go): the
	// SAME resolver and the SAME KEK the worker bridge uses.
	if deps.AskService != nil {
		deps.AskService.SetScopeResolver(mcpsettings.NewResolver(pool))
		deps.AskService.SetSecretKEK(secretsKEK)
	}
	nativeBridge.SetUsageRecorder(func(ctx context.Context, in scheduler.UsageRecord) error {
		_, err := usageRecorder.Record(ctx, aigateway.UsageInput{
			TenantID:         in.TenantID,
			ProjectID:        in.ProjectID,
			TaskID:           in.TaskID,
			ExecutionID:      in.ExecutionID,
			WorkerID:         in.WorkerID,
			Provider:         in.Provider,
			Model:            in.Model,
			PromptTokens:     in.PromptTokens,
			CacheReadTokens:  in.CacheReadTokens,
			CacheWriteTokens: in.CacheWriteTokens,
			CompletionTokens: in.CompletionTokens,
			ReasoningTokens:  in.ReasoningTokens,
			CostUSD:          in.CostUSD,
			// AdapterKind/SessionID are carried through so ONE callback serves both shapes: a worker execution
			// self-describes as "orchicon" with an empty SessionID.
			AdapterKind:   in.AdapterKind,
			SessionID:     in.SessionID,
			CorrelationID: in.CorrelationID,
			TraceID:       in.TraceID,
			WorkflowRunID: in.WorkflowRunID,
		})
		return err
	})
	// Session-terminal prefix-cache telemetry (D3, ADR-0009 D6): the native bridge drains the
	// per-session cache rollup to the OTel metrics recorder (hit/miss counters + hit-rate gauge).
	nativeBridge.SetCacheSink(func(ctx context.Context, exec db.ExecutionRow, st orchicon.CacheStats) {
		_ = usageRecorder.RecordPrefixCache(ctx, exec.TenantID, exec.ProjectID, exec.WorkerID, exec.ID, st.Turns, st.Hits, st.MissWrites, st.CacheReadTokens, st.CacheWriteTokens)
	})
	nativeBridge.SetSessionStore(func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error {
		ttx, err := pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return err
		}
		defer ttx.Rollback(ctx)
		if err := db.AppendExecutionSessionParts(ctx, ttx.Tx, tenantID, parts); err != nil {
			return err
		}
		return ttx.Commit(ctx)
	})
	// Ask-time product tools (askorchicon registry) for native Ask turns: without this the model
	// answers from the system prompt with no tool calls while the host-serve path acts.
	if deps.AskService != nil {
		nativeBridge.SetAskTools(deps.AskService.NativeAskTools())
		// Durable compaction notices: the proactive pressure gate collapses a conversation's context INSIDE
		// the bridge, which has no database handle.
		nativeBridge.SetAskCompactNotice(deps.AskService.RecordCompactionNotice)
		// The Ask file/shell suite's in-process bash carries the execution guard's destructive-command shim
		// (worker-path parity).
		askorchicon.SetAskGuardLogger(log)
	} else {
		log.Warn("native Ask turns have no tool provider (Ask service unavailable) — orchicon-adapter conversations are text-only")
	}
	// Ask session history persistence: per-session JSON under the instance data dir so orchicon-adapter
	// conversations survive a server restart (in-memory history alone used to reset every follow-up.
	if cfg.DataDir != "" {
		nativeBridge.SetAskHistoryDir(filepath.Join(cfg.DataDir, "ask-history"))
	} else {
		log.Warn("native Ask history persistence disabled (no data dir) — orchicon-adapter conversations lose context on restart")
	}
	// File-edit ledger hook (diff pipeline) for the native-loop family: the same constructor as the
	// opencode adapter above — the session fires it once per completed registry result in executeTools.
	dispatcher.Register("orchicon", nativeBridge)

	// Claude Code bridge (kind "claude" — NEVER "anthropic": anthropic is a PROVIDER segment, and a
	// 2-segment "anthropic/<model>" ref must keep inferring kind opencode.
	claudeBridge := claude.New(log)
	claudeBridge.SetUsageRecorder(usageRecorderFn)
	claudeBridge.SetSessionStore(sessionStoreFn)
	// MCP: the SAME two wirings the native bridge receives above, because the resolution is shared
	// rather than per-adapter.
	claudeBridge.SetScopeResolver(mcpsettings.NewResolver(pool))
	claudeBridge.SetMCPSecretResolver(func(ctx context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error) {
		return mcpsettings.ResolveSecretRefs(ctx, pool, secretsKEK, tenantID, env, headers)
	})
	dispatcher.Register(adapter.KindClaude, claudeBridge)

	// Per-adapter enable/disable (AC 3): every kind named in ORCHICON_DISABLED_ADAPTER_KINDS is
	// switched OFF for this plane.
	for _, kind := range strings.Split(os.Getenv("ORCHICON_DISABLED_ADAPTER_KINDS"), ",") {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		dispatcher.Disable(kind)
		log.Warn("adapter kind disabled by ORCHICON_DISABLED_ADAPTER_KINDS — dispatches of this kind fail loudly", "kind", kind)
	}

	// Wrap with OTel tracing interceptor (spans on every API call).
	handler = telemetry.Middleware(handler)

	httpSrv := &http.Server{
		// Addr is informational only: Run serves explicit listeners (bindPrimary + the optional bridge bind
		// in listen.go), so this value never itself decides the bind set.
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	s := &Server{cfg: cfg, log: log, pool: pool, httpSrv: httpSrv, otel: otelShutdown,
		blobs: blobs, authH: authHandler, webhookD: webhookDisp, logWriter: logWriter,
		hostServe: hostServe, askHostServe: askServe,
		hostServePool: hostServePool, askServePool: askServePool,
		claudeBridge: claudeBridge}
	if deps.AskService != nil {
		// The Ask service holds the per-conversation MCP clients, so it is the
		// object plane shutdown must reach; keep the reference.
		s.askService = deps.AskService
	}
	if pub != nil {
		// Outbox retention: published rows older than the configured window are pruned on a schedule in
		// bounded batches.
		s.relay = outbox.NewRelay(pool, pub, log,
			outbox.WithRetention(time.Duration(cfg.OutboxRetentionDays)*24*time.Hour),
			outbox.WithPruneBatch(cfg.OutboxPruneBatch),
			outbox.WithPruneInterval(cfg.OutboxPruneInterval),
		)
	}

	// Wire the direct NATS publisher for low-latency execution event streaming.
	if pub != nil {
		taskRec.SetEventPublisher(pub)
	}
	// Per-workflow runtime containers (Slice 2): the control plane talks to the host-side runtime
	// daemon over a unix socket.
	var runtimeLifecycle scheduler.RuntimeLifecycle
	if rtClient != nil {
		if rtClient.Ready(context.Background()) {
			runtimeLifecycle = runtime.NewLifecycle(rtClient, pool, log, opencode.RuntimeServeConfig, secretsKEK)
			// The run-level MCP/skills union comes from the SAME resolver the native/claude bridges use, so a
			// run's union and a worker's own set can never disagree on a scope's meaning.
			if lc, ok := runtimeLifecycle.(*runtime.Lifecycle); ok {
				lc.SetScopeResolver(mcpsettings.NewResolver(pool))
				// The container-locality provider view: a runtime container's 127.0.0.1 is the container itself, so
				// a local model published on this machine is unreachable from a worker at the URL the operator.
				lc.SetContainerProviders(func(ctx context.Context, tenantID string) []runtime.ProviderConfig {
					svc := deps.ProvidersService
					if svc == nil {
						return nil
					}
					return svc.ContainerProviders(ctx, tenantID)
				})
				adapterBridge.SetRunServeConfigProvider(lc)
			}
			// Route executions that belong to a workflow run into that
			// workflow's runtime container instead of a local subprocess.
			adapterBridge.SetRuntimeClient(rtClient)
			// Always-container native: the native bridge routes `bash`
			// into the run's container (same lease the gate ensured).
			nativeBridge.SetRuntimeClient(rtClient)
			// The claude adapter routes its streaming session into the run's
			// container over the daemon's duplex stdio transport.
			claudeBridge.SetRuntimeClient(rtClient)
			// Execution liveness: fail executions orphaned by a plane restart or a lost runtime container so
			// recovery re-dispatches.
			s.reaper = scheduler.NewExecutionReaper(pool, activeProbe(dispatcher, pool, log), taskRec.FailLostExecution, log)
			execReaper = s.reaper
			log.Info("workflow runtime daemon connected", "socket", cfg.RuntimeSocket)
		} else {
			log.Warn("workflow runtime daemon not reachable — in-process execution", "socket", cfg.RuntimeSocket)
		}
	}
	// Headless: still reap in-process executions orphaned by a restart.
	if s.reaper == nil {
		s.reaper = scheduler.NewExecutionReaper(pool, activeProbe(dispatcher, pool, log), taskRec.FailLostExecution, log)
	}
	execReaper = s.reaper
	workflowRec := scheduler.NewWorkflowReconciler(pool, log, policyEngine, taskRec, recoveryEngine, runtimeLifecycle)
	// Bounded in-pass fan-out for the post-commit inline dispatch: parallel branch step runs dispatch
	// concurrently (ORCHICON_DISPATCH_CONCURRENCY, default 4).
	workflowRec.SetDispatchConcurrency(cfg.DispatchConcurrency)
	// The Server keeps a concrete *runtime.Lifecycle for the adopt sweep; the interface may be nil (no
	// daemon) while the concrete value is set — type-assert to extract it.
	if lc, ok := runtimeLifecycle.(*runtime.Lifecycle); ok {
		s.runtime = lc
	}
	// Wire the workflow notifier: when a work item completes.
	taskRec.SetWorkflowNotifier(func(ctx context.Context, runID string) {
		if s.rcmgr != nil {
			s.rcmgr.Enqueue("workflow", runID)
		}
	})
	recoveryRec := recovery.NewReconciler(recoveryEngine)
	scheduledRunRec := scheduler.NewScheduledRunReconciler(pool, log,
		func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
			return workflow.StartWorkflowDirect(ctx, pool, log, tenantID, workflowID, projectID, workItemID)
		})
	// Sequence engine: a scheduled parent with children and no workflow is fired through StartSequence
	// (flip running + reset descendants + arm first child).
	startWorkflowFn := func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return workflow.StartWorkflowDirect(ctx, pool, log, tenantID, workflowID, projectID, workItemID)
	}
	sequenceRec := scheduler.NewSequenceReconciler(pool, log, startWorkflowFn)
	scheduledRunRec.SetSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		return scheduler.StartSequence(ctx, pool, log, tenantID, parentID, startWorkflowFn)
	})
	recurringFireRec := scheduler.NewRecurringFireReconciler(pool, log, startWorkflowFn)
	recurringFireRec.SetRunContextStarter(func(ctx context.Context, tenantID, workflowID, projectID, workItemID string, rc map[string]any) error {
		return workflow.StartWorkflowDirectWithContext(ctx, pool, log, tenantID, workflowID, projectID, workItemID, rc)
	})
	recurringFireRec.SetSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		return scheduler.StartSequence(ctx, pool, log, tenantID, parentID, startWorkflowFn)
	})
	s.rcmgr = reconciler.NewManager(pool, log)
	s.rcmgr.Register(taskRec)
	s.rcmgr.Register(workflowRec)
	s.rcmgr.Register(recoveryRec)
	s.rcmgr.Register(scheduledRunRec)
	s.rcmgr.Register(sequenceRec)
	s.rcmgr.Register(recurringFireRec)
	// Worktree provisioning (per-run isolated working tree at arm time).
	worktreeRec := scheduler.NewWorktreeReconciler(pool, log)
	// Non-repo in-place serialization gate (concurrency guards D3): non-repo runs are atomically
	// admitted before being marked 'skipped', so two runs never share the mutable project_dir.
	worktreeRec.SetDispatchLimiter(scheduler.DBDispatchLimiter())
	s.rcmgr.Register(worktreeRec)
	// Wire the sequence notifier: when a bound child work item reaches a terminal state, advance its
	// parent's chain immediately (the scan pass every 200ms is the safety net).
	workflowRec.SetSequenceNotifier(func(ctx context.Context, parentID string) {
		if s.rcmgr != nil {
			s.rcmgr.Enqueue("sequence", parentID)
		}
	})
	// Wire the worktree notifier: when a run is armed (pending→running), provision its isolated working
	// tree immediately (the scan pass every 200ms is the safety net).
	workflowRec.SetWorktreeNotifier(func(ctx context.Context, runID string) {
		if s.rcmgr != nil {
			s.rcmgr.Enqueue("worktree", runID)
		}
	})
	// Wire the integration notifier: a run that reached COMPLETED integrates its project NOW, so the
	// next run's branch is cut from a tip that already contains it.
	workflowRec.SetIntegrationEnqueuer(func(ctx context.Context, tenantID, projectID, runID string) {
		worktreeRec.EnqueueRunForIntegration(ctx, tenantID, projectID, runID)
	})
	workflowRec.SetIntegrationNotifier(func(ctx context.Context, tenantID, projectID string) {
		if projectID == "" {
			return
		}
		go worktreeRec.IntegrateProjectNow(ctx, tenantID, projectID)
	})
	// Wire the scan-pass failure sink: a run the scan cannot progress must back off like any other
	// failed key, not retry — and log — on every 200ms heartbeat.
	workflowRec.SetRunFailedHook(func(runID string, err error) {
		if s.rcmgr != nil {
			s.rcmgr.FailRun("workflow", runID, err)
		}
	})

	// Seed in-process adapter registrations (opencode + the native orchicon session engine) so the
	// TaskReconciler can find a ready adapter for dispatch (docs/04 §6.3.
	seedAllDevAdapters(context.Background(), pool, log)

	return s, nil
}

// Handler returns the current HTTP handler (API + middleware). Used by
// the dev subcommand to wrap the handler with frontend serving.
func (s *Server) Handler() http.Handler {
	return s.httpSrv.Handler
}

// SetHandler replaces the HTTP handler. Used by the dev subcommand to
// inject the embedded frontend SPA serving alongside the API.
func (s *Server) SetHandler(h http.Handler) {
	s.httpSrv.Handler = h
}

// Run blocks until ctx is cancelled, serving traffic, running the outbox relay and reconciler
// framework, and shutting down gracefully within ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting orchicon control plane",
		"version", version.Current().String(), "http", s.cfg.HTTPAddr,
		"extra_bind", s.cfg.ExtraBind)

	// The demand-keyed host opencode serve lives as long as the plane once it has been started; stop it
	// (and its supervision) on shutdown.
	defer func() {
		if s.hostServe != nil {
			s.hostServe.Stop()
		}
		if s.askHostServe != nil {
			s.askHostServe.Stop()
		}
		// Pooled serves are ADDITIONAL processes with their own data dirs; the pool's Stop also stops the
		// default serve it adopted.
		if s.hostServePool != nil {
			s.hostServePool.Stop()
		}
		if s.askServePool != nil {
			s.askServePool.Stop()
		}
	}()

	// Clear stale edit locks from a prior server session.
	if _, err := s.pool.Exec(ctx, "DELETE FROM edit_locks"); err != nil {
		s.log.Warn("startup: clear edit locks", "error", err)
	}

	// Two listeners, not one (listen.go): the primary bind is FATAL if it fails — host clients (orch,
	// the GUI) reach the plane nowhere else.
	primary, err := s.startListeners(ctx)
	if err != nil {
		s.authH.CloseEmbeddedOP()
		s.pool.Close()
		s.shutdownOTel()
		return err
	}
	errCh := make(chan error, 4)
	go func() { errCh <- s.httpSrv.Serve(primary) }()
	s.log.Info("http listener serving", "addr", primary.Addr().String())

	if s.relay != nil {
		go func() { errCh <- s.relay.Run(ctx) }()
	}

	if s.rcmgr != nil {
		go func() { errCh <- s.rcmgr.Run(ctx) }()
	}

	// Adopt workflow runtime containers at boot and on a 30s sweep: reap orphans (containers whose run
	// is no longer active — covers runs terminalized while the plane was down, aborted runs.
	if s.runtime != nil {
		go runOnStartupThenEvery(ctx, 1*time.Second, 30*time.Second, func() {
			if err := s.runtime.Adopt(ctx); err != nil {
				s.log.Warn("workflow runtime adopt failed", "error", err)
			}
			if s.reaper != nil {
				if err := s.reaper.Reap(ctx); err != nil {
					s.log.Warn("execution liveness reap failed", "error", err)
				}
			}
		})
	} else if s.reaper != nil {
		go runOnStartupThenEvery(ctx, 1*time.Second, 30*time.Second, func() {
			if err := s.reaper.Reap(ctx); err != nil {
				s.log.Warn("execution liveness reap failed", "error", err)
			}
		})
	}

	// EPHEMERAL ABANDONMENT SWEEP (Ask Orchicon Quick Work).
	go func() {
		sweeper := ephemeral.NewSweeper(s.pool, s.log)
		runOnStartupThenEvery(ctx, 1*time.Minute, ephemeral.SweepInterval, func() {
			if _, err := sweeper.Sweep(ctx); err != nil {
				s.log.Warn("ephemeral sweep failed", "error", err)
			}
		})
	}()

	// MCP stdio stale-child sweep (ADR-0008): MCP server subprocesses spawned for sessions carry
	// ORCHICON_MCP_STDIO=1.
	go runOnStartupThenEvery(ctx, 1*time.Second, 30*time.Second, func() {
		mcpclient.SweepStaleChildren(ctx, s.log)
	})

	// Phase 9: webhook dispatcher (NATS consumer → HTTP POST + retries + dead-letter — docs/07 §3.11).
	if s.webhookD != nil {
		go func() { errCh <- s.webhookD.Run(ctx) }()
	}

	// Periodically heartbeat the in-process dev adapter so the TaskReconciler can dispatch tasks beyond
	// the initial 60s heartbeat TTL (docs/03 §5).
	go s.heartbeatDevAdapter(ctx)

	// Index-integrity sweep: at boot and every IndexCheckInterval, verify every user btree index with
	// amcheck and rebuild any corrupted ones.
	go s.indexHealthLoop(ctx)

	// Container mode: keep the project-mounts manifest (on the data volume) in sync with the projects
	// table so scripts/container.sh can mount the project dirs/files the user selected.
	if dataDir := os.Getenv("ORCHICON_DATA_DIR"); dataDir != "" {
		go runProjectMountsWriter(ctx, s.pool, s.log, dataDir)
		project.SetOnProjectChanged(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := writeProjectMountsManifest(ctx, s.pool, dataDir); err != nil {
				s.log.Warn("project mounts manifest refresh failed", "error", err)
			}
		})
	}

	// Start the scheduled backup loop.
	{
		bDir, _ := backup.DefaultDir()
		bSched := backup.NewScheduler(s.cfg.PostgresDSN, bDir, s.log)
		bSched.Start(ctx, func() (string, int) {
			qCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stx, err := s.pool.BeginTenantTx(qCtx, "tnt_dev")
			if err != nil {
				return "", 0
			}
			defer stx.Rollback(qCtx)
			row, err := db.GetTenantSettings(qCtx, stx.Tx, "tnt_dev")
			if err != nil {
				return "", 0
			}
			return row.BackupSchedule, int(row.BackupRetentionDays)
		})
		defer bSched.Stop()
	}

	// Live-apply Settings → Defaults log management values to the rotating on-disk log writer every few
	// seconds (no restart needed when the operator changes log size/roll/retention in the UI).
	if s.logWriter != nil {
		go s.applyLogSettingsLoop(ctx)
	}

	select {
	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
		if s.webhookD != nil {
			s.webhookD.Stop()
		}
		s.authH.CloseEmbeddedOP()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.pool.Close()
			s.shutdownOTel()
			return fmt.Errorf("server: shutdown: %w", err)
		}
		// Ask sessions are host children, not processes the plane's exit reaps: without this a plane
		// restart leaves every claude Ask child running, holding its transcript and its stdin.
		if s.claudeBridge != nil {
			s.claudeBridge.CloseAsk()
		}
		// Ask Orchicon's per-conversation MCP clients: their stdio children are host children too, so a
		// plane restart must close them here beside the claude Ask sessions.
		if s.askService != nil {
			s.askService.CloseAskMCP()
		}
		// AND THE ASK GUARD BELONGS BESIDE THEM, for the same reason.
		askorchicon.CloseAskGuard()
		s.pool.Close()
		s.shutdownOTel()
		return nil
	case err := <-errCh:
		s.authH.CloseEmbeddedOP()
		if s.claudeBridge != nil {
			s.claudeBridge.CloseAsk()
		}
		// Ask Orchicon's per-conversation MCP clients: their stdio children are host children too, so a
		// plane restart must close them here beside the claude Ask sessions.
		if s.askService != nil {
			s.askService.CloseAskMCP()
		}
		// AND THE ASK GUARD BELONGS BESIDE THEM, for the same reason.
		askorchicon.CloseAskGuard()
		s.pool.Close()
		s.shutdownOTel()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: serve: %w", err)
	}
}

func (s *Server) shutdownOTel() {
	if s.otel != nil {
		s.otel.Shutdown(context.Background())
	}
}

// applyLogSettingsLoop polls the default tenant's Settings → Defaults log management values and
// live-applies them to the rotating on-disk log writer.
func (s *Server) applyLogSettingsLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	s.applyLogSettingsOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			if s.logWriter != nil {
				_ = s.logWriter.Close()
			}
			return
		case <-ticker.C:
			s.applyLogSettingsOnce(ctx)
		}
	}
}

func (s *Server) applyLogSettingsOnce(ctx context.Context) {
	qCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stx, err := s.pool.BeginTenantTx(qCtx, "tnt_dev")
	if err != nil {
		return
	}
	defer stx.Rollback(qCtx)
	row, err := db.GetTenantSettings(qCtx, stx.Tx, "tnt_dev")
	if err != nil {
		return
	}
	cfg := s.logSettingsFromRow(row)
	if cfg != nil {
		s.logWriter.Apply(*cfg)
	}
}

// logSettingsFromRow maps tenant settings to the rotating writer config, layered on the writer's
// CURRENT config (env/config defaults from boot).
func (s *Server) logSettingsFromRow(row db.TenantSettingsRow) *logging.Config {
	if s.logWriter == nil {
		return nil
	}
	base := s.logWriter.Config()
	if row.LogDirectory != "" {
		base.Dir = row.LogDirectory
	}
	if row.LogMaxSizeMB > 0 {
		base.MaxSizeBytes = row.LogMaxSizeMB << 20
	}
	if row.LogRollIntervalHours > 0 {
		base.RollInterval = time.Duration(row.LogRollIntervalHours) * time.Hour
	}
	if row.LogRetentionDays > 0 {
		base.RetentionDays = int(row.LogRetentionDays)
	}
	if row.LogMaxFiles > 0 {
		base.MaxFiles = int(row.LogMaxFiles)
	}
	return &base
}

// heartbeatDevAdapter renews the in-process dev adapter's heartbeat every 30s so the TaskReconciler
// can dispatch tasks beyond the initial heartbeat TTL (docs/03 §5, docs/04 §6.3).
func (s *Server) heartbeatDevAdapter(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ttx, err := s.pool.BeginTenantTx(ctx, "tnt_dev")
			if err != nil {
				continue
			}
			// Heartbeat EVERY in-process dev adapter, from the SAME table the boot seed uses, so a kind cannot
			// be seeded without also being kept ready past the TTL (or heartbeated without a row to heartbeat).
			for _, seed := range devAdapterSeeds() {
				if err := db.HeartbeatAdapter(ctx, ttx.Tx, "tnt_dev", seed.id, []byte(seed.caps())); err != nil {
					s.log.Warn("dev adapter heartbeat failed", "kind", seed.kind, "id", seed.id, "error", err)
				}
			}
			_ = ttx.Commit(ctx)
		}
	}
}

// indexHealthLoop runs the amcheck index-integrity sweep once at boot and then every
// cfg.IndexCheckInterval.
func (s *Server) indexHealthLoop(ctx context.Context) {
	runOnce := func() {
		if repaired := db.RepairCorruptIndexes(ctx, s.pool, s.log); len(repaired) > 0 {
			s.log.Warn("index integrity sweep repaired corrupt indexes", "indexes", repaired)
		}
	}
	runOnce()
	if s.cfg.IndexCheckInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.cfg.IndexCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// resolveAdapterKind resolves the adapter kind of an execution for mid-run RPC routing.
func resolveAdapterKind(ctx context.Context, pool *db.Pool, deploymentTenant, execID string) (string, error) {
	tid := tenant.FromContext(ctx)
	if tid == "" {
		tid = deploymentTenant
	}
	tx, err := pool.BeginTenantTx(ctx, tid)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	exec, err := db.GetExecution(ctx, tx.Tx, tid, execID)
	if err != nil {
		return "", err
	}
	// Prefer the dispatching adapter row.
	if exec.AdapterID != nil && *exec.AdapterID != "" {
		adp, aerr := db.GetAdapter(ctx, tx.Tx, tid, *exec.AdapterID)
		if aerr == nil && adp.Kind != "" {
			return adp.Kind, nil
		}
		// Adapter row missing/invalid — fall through to the worker-path
		// resolution below (never leave a mid-run RPC unresolved).
	}
	ver, err := db.GetLatestWorkerVersion(ctx, tx.Tx, tid, exec.WorkerID, true)
	if err != nil {
		return "", err
	}
	kind := adapter.AdapterKind(ver.ModelRef)
	if kind == "" {
		// Same fallback the reconciler applies at dispatch time: an empty/malformed model_ref dispatches
		// under the legacy default kind.
		kind = adapter.DefaultAdapterKind
	}
	return kind, nil
}

// devAdapterSeed is one in-process adapter the plane registers so the
// TaskReconciler's selectAdapter can find a READY row for its kind.
//
// ONE TABLE, READ BY BOTH THE BOOT SEED AND THE HEARTBEAT. That is the point:
// dispatch needs two things from every kind — a row, and a row that stays ready
// past the heartbeat TTL — and when those were two separate hardcoded lists
// (opencode seeded, orchicon seeded, claude NEITHER) a whole adapter kind became
// undispatchable. The same defect had already shipped twice; see
// TestEveryBuiltinAdapterKindHasASeededRow, which now fails when a kind is added
// without an entry here.
//
// A BRIDGE REGISTRATION AND A ROW ARE DIFFERENT REQUIREMENTS, and only the row
// decides whether anything dispatches:
//
//	dispatcher.Register(kind, bridge)   routes an execution that was CHOSEN
//	db.ListReadyAdaptersByKind(kind)    decides a task may be chosen AT ALL
//
// A kind missing here fails every dispatch with `no ready adapters of kind <k>`.
type devAdapterSeed struct {
	id   string
	kind string
	caps func() string
}

func devAdapterSeeds() []devAdapterSeed {
	return []devAdapterSeed{
		{"adp_opencode_dev", adapter.KindOpencode, opencode.BuildCapabilitiesJSON},
		{"adp_orchicon_dev", adapter.KindOrchicon, orchicon.BuildCapabilitiesJSON},
		{"adp_claude_dev", adapter.KindClaude, claude.BuildCapabilitiesJSON},
	}
}

// seedAllDevAdapters seeds every in-process adapter kind from ONE table. It is the ONLY seeder —
// the per-kind wrappers it replaced are gone, so there is no second list to forget to update.
func seedAllDevAdapters(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	for _, s := range devAdapterSeeds() {
		seedDevAdapterKind(ctx, pool, log, s.id, s.kind, s.caps)
	}
}

// seedDevAdapterKind is the shared body: upsert a ready in-process adapter row for the given
// id/kind, heartbeating when the row already exists.
func seedDevAdapterKind(ctx context.Context, pool *db.Pool, log *slog.Logger, adapterID, kind string, caps func() string) {
	tenantID := "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		log.Warn("seed dev adapter: begin tx failed", "error", err)
		return
	}
	defer ttx.Rollback(ctx)

	// Check if adapter already exists.
	_, err = db.GetAdapter(ctx, ttx.Tx, tenantID, adapterID)
	if err == nil {
		// Already registered — just heartbeat.
		if err := db.HeartbeatAdapter(ctx, ttx.Tx, tenantID, adapterID, []byte(caps())); err != nil {
			log.Warn("seed dev adapter: heartbeat failed", "error", err)
			return
		}
		if err := ttx.Commit(ctx); err != nil {
			log.Warn("seed dev adapter: commit failed", "error", err)
		}
		return
	}

	// Insert new adapter registration.
	row := db.AdapterRow{
		ID:                      adapterID,
		TenantID:                tenantID,
		Kind:                    kind,
		Version:                 "0.1.0",
		Endpoint:                "in-process",
		Capabilities:            []byte(caps()),
		Status:                  domain.AdapterReady,
		MaxConcurrentExecutions: 5,
	}
	if _, err := db.CreateAdapter(ctx, ttx.Tx, row); err != nil {
		log.Warn("seed dev adapter: create failed", "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		log.Warn("seed dev adapter: commit failed", "error", err)
		return
	}
	log.Info("seeded dev adapter", "id", adapterID, "kind", kind)
}

// activeProbe returns the execution-reaper liveness probe for the dispatcher.
func activeProbe(dispatcher *scheduler.Dispatcher, pool *db.Pool, log *slog.Logger) func(exec db.ExecutionRow) bool {
	return func(exec db.ExecutionRow) bool {
		ctx := context.Background()
		tx, err := pool.BeginTenantTx(ctx, "tnt_dev")
		if err != nil {
			log.Warn("liveness probe: begin tx failed", "execution", exec.ID, "error", err)
			return false
		}
		defer tx.Rollback(ctx)
		ver, err := db.GetLatestWorkerVersion(ctx, tx.Tx, "tnt_dev", exec.WorkerID, true)
		if err != nil {
			log.Warn("liveness probe: worker version lookup failed", "execution", exec.ID, "error", err)
			return false
		}
		kind := adapter.AdapterKind(ver.ModelRef)
		if kind == "" {
			// Same fallback the reconciler applies at dispatch time: an empty/malformed model_ref dispatches
			// under the legacy default kind.
			kind = adapter.DefaultAdapterKind
		}
		bridge, err := dispatcher.Resolve(kind)
		if err != nil {
			log.Warn("liveness probe: no bridge for kind", "execution", exec.ID, "kind", kind, "error", err)
			return false
		}
		rep, ok := bridge.(scheduler.LivenessReporter)
		if !ok {
			log.Warn("liveness probe: bridge does not support liveness", "execution", exec.ID, "kind", kind)
			return false
		}
		return rep.IsExecutionActive(exec.ID)
	}
}

// orchiconRegistryResolver returns a lazily-resolving ProviderResolver for the native session
// engine.
func orchiconRegistryResolver(deps *api.Dependencies, pool *db.Pool, kek []byte, log *slog.Logger) orchicon.ProviderResolver {
	return orchicon.ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (orchicon.Provider, error) {
		svc := deps.ProvidersService
		if svc == nil {
			return nil, fmt.Errorf("providers service not yet constructed (api.Mount) — orchicon adapter kind not ready")
		}
		reg := svc.Registry()
		return reg.Get(ctx, tenantID, providerID)
	})
}
