// Package server boots the Orchicon control plane: opens the database
// pool, runs migrations (if enabled), seeds the dev tenant, starts the
// outbox relay, mounts the API, and serves HTTP + gRPC until shutdown.
// It is the single composition root.
//
// Phase 3 adds: the OTel telemetry pipeline (tracer/meter/exporter),
// the reconciler framework (work queue + advisory-lock leadership), and
// the NATS subscriber for streaming RPCs.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/api"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/backup"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/logging"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/outbox"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/runtimeimage"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/telemetry"
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
	// serveCancel stops the host opencode serve on plane shutdown.
	serveCancel context.CancelFunc
}

// New constructs a Server from configuration. It opens the DB pool,
// connects to NATS, sets up OTel, starts the outbox relay, and mounts
// the API. logWriter is the optional rotating on-disk log sink (detached
// serve); when non-nil the server live-applies Settings → Defaults log
// management values to it and prunes on shutdown.
func New(cfg config.Config, log *slog.Logger, logWriter *logging.RotatingWriter) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// OTel telemetry pipeline (tracer + meter + OTLP exporter → Grafana stack).
	// If the collector is unreachable, telemetry is dropped with bounded
	// in-process buffering; control flow is not blocked (docs/08 §8). The
	// runtime-container sandbox plane sets ORCHICON_TELEMETRY=none (no
	// Grafana stack in the sandbox) — the pipeline is skipped entirely.
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

	// Seed the deployment tenant so the control plane has a tenant context
	// before auth (Phase 9) lands. Idempotent. The tenant id comes from
	// ORCHICON_DEPLOYMENT_TENANT_ID (default "tnt_dev").
	if err := db.SeedDevTenant(context.Background(), pool, cfg.DeploymentTenantID); err != nil {
		log.Warn("seed dev tenant failed (continuing)", "error", err)
	}

	// Seed canned workers for the dev tenant so they're available for
	// workflow templates and manual dispatch. Idempotent — workers that
	// already exist are skipped or updated with current data.
	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		log.Warn("seed dev workers failed (continuing)", "error", err)
	}

	if err := db.SeedDevWorkflows(context.Background(), pool); err != nil {
		log.Warn("seed dev workflows failed (continuing)", "error", err)
	}

	// Connect to NATS and start the outbox relay. If NATS is unavailable
	// at boot, the relay logs and retries; events stay safely in the
	// outbox table until NATS recovers (docs/09 §6).
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
	// Phase 7: Policy Engine (Rego) + Recovery Workflow Engine. The
	// PolicyEngine evaluates published Policies at decision points
	// (admission/dispatch/budget/approval/recovery/completion — docs/02
	// §2.5 Tier 1). The RecoveryEngine triggers + progresses recoveries
	// through the default 6-step workflow (docs/06).
	policyEngine := policy.New(pool, log)
	recoveryEngine := recovery.New(pool, log)
	// Phase 8: Telemetry query client (docs/08 §5). Queries Tempo/Loki/
	// VictoriaMetrics directly for tenant-scoped traces/metrics/logs.
	// Empty backend URLs disable the queries (degrade gracefully — docs/08 §8).
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
	// Constructs the TokenIssuer + identity Resolver shared with the
	// auth middleware (docs/07 §6).
	authHandler := auth.NewHandler(cfg, pool, log)
	log.Info("auth configured", "issuer", cfg.Auth.Issuer, "mode", cfg.Mode)

	// Local-mode first-admin bootstrap: seed the embedded-OP login
	// credential for a fresh plane (admin/admin with a forced password
	// change) so it is usable out of the box. Runs in every local-mode
	// plane with the embedded OP (dev AND prod volumes); no-op in
	// production, when the OP is disabled, or once an admin credential
	// already exists. Logs the generated password once on first boot.
	if err := auth.BootstrapLocalAdmin(context.Background(), pool, log, cfg); err != nil {
		log.Warn("local admin bootstrap failed (continuing)", "error", err)
	}

	// Phase 9: Webhook dispatcher (NATS consumer → HTTP POST + retries +
	// dead-letter — docs/07 §3.11). Starts in Run(); nil when NATS is
	// unavailable (webhooks degrade gracefully).
	var webhookDisp *webhook.Dispatcher
	if sub != nil {
		webhookDisp = webhook.NewDispatcher(pool, sub, log)
	}

	// Model discoverer: shells out to opencode CLI to list models.
	// Falls back to a static mock list in dev mode when opencode is
	// not on PATH (docs/04 §6).
	var modelDiscoverer *aigateway.ModelDiscoverer
	if _, err := exec.LookPath("opencode"); err == nil {
		modelDiscoverer = aigateway.NewModelDiscoverer(log, "opencode")
	} else {
		log.Warn("opencode binary not found on PATH, using mock model list", "error", err)
		modelDiscoverer = aigateway.MockModelDiscoverer(log)
	}

	// MCP discoverer: shells out to opencode CLI to list MCP servers.
	var mcpDiscoverer *aigateway.MCPDiscoverer
	if _, err := exec.LookPath("opencode"); err == nil {
		mcpDiscoverer = aigateway.NewMCPDiscoverer(log, "opencode")
	} else {
		log.Warn("opencode binary not found on PATH, using mock MCP server list", "error", err)
		mcpDiscoverer = aigateway.MockMCPDiscoverer(log)
	}

	// Reconciler framework (docs/03 §2). Phase 5 registers the
	// TaskReconciler — the control loop that dispatches ready tasks to
	// runtime adapters (docs/03 §4). The OpenCode adapter bridge is the
	// CLI subprocess wrapper that drives the `opencode` binary
	// (docs/04 §6). If the binary is absent, the bridge runs in
	// simulation mode for dev verification.
	adapterBridge := opencode.New(log)

	// Always-on host opencode serve for the in-process execution
	// population (Stage 3 session transport): standalone dispatches,
	// follow-ups, and any execution not bound to a workflow-run container
	// run as persistent sessions on it. The plane supervises the serve
	// (spawn on boot + health watchdog with restart), so the session host
	// is never down. With the one-shot subprocess path removed, a serve
	// failure now means those executions fail fast (failed_to_start)
	// rather than degrading to a second transport.
	var hostServe *opencode.HostServe
	var serveCancel context.CancelFunc
	if os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT") != "0" {
		dataDir := ""
		if home, herr := os.UserHomeDir(); herr == nil {
			dataDir = filepath.Join(home, ".local", "share", "orchicon", "opencode")
		}
		if dataDir != "" {
			hostServe = opencode.NewHostServe(log, dataDir, "")
			serveCtx, cancel := context.WithCancel(context.Background())
			serveCancel = cancel
			go func() {
				if err := hostServe.Start(serveCtx); err != nil {
					log.Warn("host opencode serve unavailable — session-dependent executions will fail fast", "error", err)
					return
				}
				hostServe.Watch(serveCtx)
			}()
			adapterBridge.SetHostServe(hostServe)
		} else {
			log.Warn("host opencode serve data dir unavailable — sessions disabled")
		}
	}

	// Phase 8: wire the AI Gateway usage recorder into the adapter so
	// step_finish token/cost telemetry is dual-written to Postgres
	// (source of truth) + OTel metrics (VictoriaMetrics) — docs/08 §5.2.
	// The adapter calls the recorder via a closure to stay decoupled
	// from the aigateway package (docs/04 §6.0: thin bridge).
	usageRecorder := aigateway.NewUsageRecorder(pool, log)
	adapterBridge.SetUsageRecorder(func(ctx context.Context, in opencode.UsageRecord) error {
		_, err := usageRecorder.Record(ctx, aigateway.UsageInput{
			TenantID:         in.TenantID,
			ProjectID:        in.ProjectID,
			TaskID:           in.TaskID,
			ExecutionID:      in.ExecutionID,
			WorkerID:         in.WorkerID,
			Provider:         in.Provider,
			Model:            in.Model,
			PromptTokens:     in.PromptTokens,
			CompletionTokens: in.CompletionTokens,
			CostUSD:          in.CostUSD,
			CorrelationID:    in.CorrelationID,
			TraceID:          in.TraceID,
			WorkflowRunID:    in.WorkflowRunID,
		})
		return err
	})
	// Durable session transcript (Stage 3): the adapter's session path
	// records every side of the worker conversation into
	// execution_session_parts via this writer (best-effort — a write
	// failure loses the trailing batch, never control flow).
	adapterBridge.SetSessionStore(func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error {
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

	taskRec := scheduler.NewTaskReconciler(pool, log, adapterBridge)
	// Bounded in-pass fan-out for the scan pass: independent ready tasks
	// dispatch concurrently (ORCHICON_DISPATCH_CONCURRENCY, default 4).
	taskRec.SetDispatchConcurrency(cfg.DispatchConcurrency)
	// Per-workflow runtime containers: the control plane talks to the
	// host-side runtime daemon over a unix socket. When the socket is
	// absent (headless `orchicon serve`), the lifecycle is disabled and
	// execution stays in-process.
	var rtClient *runtime.Client
	if cfg.RuntimeSocket != "" {
		rtClient = runtime.NewClient(cfg.RuntimeSocket, cfg.Instance)
	}

	// Seed canned "stock" runtime image rows (one per daemon-reported stock
	// variant) so they appear as normal, editable rows in the Runtime Images
	// page — the canned-image equivalent of SeedDevWorkers. Idempotent;
	// pristine rows auto-build when a new seed version appears. Skips
	// silently when the daemon is absent (headless serve).
	if err := runtimeimage.SeedCannedImages(context.Background(), pool, log, rtClient); err != nil {
		log.Warn("seed canned runtime images failed (continuing)", "error", err)
	}
	deps := api.Dependencies{
		Pool:              pool,
		Log:               log,
		Subscriber:        sub,
		PolicyEngine:      policyEngine,
		RecoveryEngine:    recoveryEngine,
		TelemetryQuery:    telemetryQuery,
		GrafanaURL:        cfg.GrafanaURL,
		AuthHandler:       authHandler,
		WebhookDispatcher: webhookDisp,
		Mode:              cfg.Mode,
		ModelDiscoverer:   modelDiscoverer,
		MCPDiscoverer:     mcpDiscoverer,
		BlobStore:         blobs,
		PostgresDSN:       cfg.PostgresDSN,
		RuntimeClient:     rtClient,
		SendExecutionMessage: func(ctx context.Context, execID, message string) error {
			return adapterBridge.SendExecutionMessage(ctx, execID, message)
		},
		ContinueSession: func(ctx context.Context, opts opencode.ContinueSessionOpts) (string, error) {
			return adapterBridge.ContinueSession(ctx, opts)
		},
		HostServe: hostServe,
	}
	handler := api.Mount(mux, deps)

	// Wrap with OTel tracing interceptor (spans on every API call).
	handler = telemetry.Middleware(handler)

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	s := &Server{cfg: cfg, log: log, pool: pool, httpSrv: httpSrv, otel: otelShutdown,
		blobs: blobs, authH: authHandler, webhookD: webhookDisp, logWriter: logWriter,
		serveCancel: serveCancel}
	if pub != nil {
		s.relay = outbox.NewRelay(pool, pub, log)
	}

	// Wire the direct NATS publisher for low-latency execution event
	// streaming. When set, the reconciler publishes streaming events
	// (text, tool_call, artifact, status changes) directly to NATS
	// after each callback commits, bypassing the outbox relay's 500ms
	// poll interval. The outbox is still written for durability.
	if pub != nil {
		taskRec.SetEventPublisher(pub)
	}
	// Per-workflow runtime containers (Slice 2): the control plane talks
	// to the host-side runtime daemon over a unix socket. When the socket
	// is absent (headless `orchicon serve`), the lifecycle is disabled and
	// execution stays in-process.
	//
	// Declared as the INTERFACE type (not *runtime.Lifecycle) so an absent
	// daemon leaves the interface genuinely nil — a typed-nil
	// *runtime.Lifecycle wrapped in the interface is non-nil to the
	// reconciler and its EnsureForRun/ReapForRun nil-dereference the
	// receiver, crashing the whole plane on the first workflow run.
	var runtimeLifecycle scheduler.RuntimeLifecycle
	if rtClient != nil {
		if rtClient.Ready(context.Background()) {
			runtimeLifecycle = runtime.NewLifecycle(rtClient, pool, log, opencode.RuntimeServeConfig)
			// Route executions that belong to a workflow run into that
			// workflow's runtime container instead of a local subprocess.
			adapterBridge.SetRuntimeClient(rtClient)
			// Execution liveness: fail executions orphaned by a plane
			// restart or a lost runtime container so recovery re-dispatches.
			s.reaper = scheduler.NewExecutionReaper(pool, adapterBridge.IsExecutionActive, taskRec.FailLostExecution, log)
			log.Info("workflow runtime daemon connected", "socket", cfg.RuntimeSocket)
		} else {
			log.Warn("workflow runtime daemon not reachable — in-process execution", "socket", cfg.RuntimeSocket)
		}
	}
	// Headless: still reap in-process executions orphaned by a restart.
	if s.reaper == nil {
		s.reaper = scheduler.NewExecutionReaper(pool, adapterBridge.IsExecutionActive, taskRec.FailLostExecution, log)
	}
	workflowRec := scheduler.NewWorkflowReconciler(pool, log, policyEngine, taskRec, recoveryEngine, runtimeLifecycle)
	// The Server keeps a concrete *runtime.Lifecycle for the adopt sweep;
	// the interface may be nil (no daemon) while the concrete value is
	// set — type-assert to extract it.
	if lc, ok := runtimeLifecycle.(*runtime.Lifecycle); ok {
		s.runtime = lc
	}
	// Wire the workflow notifier: when a work item completes, enqueue
	// the workflow run ID so the WorkflowReconciler progresses the DAG
	// immediately instead of waiting for its next scan pass (200ms).
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
	// Sequence engine: a scheduled parent with children and no workflow is
	// fired through StartSequence (flip running + reset descendants + arm
	// first child). The sequence reconciler advances the chain on child
	// terminal transitions.
	startWorkflowFn := func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return workflow.StartWorkflowDirect(ctx, pool, log, tenantID, workflowID, projectID, workItemID)
	}
	sequenceRec := scheduler.NewSequenceReconciler(pool, log, startWorkflowFn)
	scheduledRunRec.SetSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		return scheduler.StartSequence(ctx, pool, log, tenantID, parentID, startWorkflowFn)
	})
	recurringFireRec := scheduler.NewRecurringFireReconciler(pool, log, startWorkflowFn)
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
	// Worktree provisioning (per-run isolated working tree at arm time). The
	// notifier makes provisioning fire at run-arm; the reconciler's scan
	// pass is the safety net.
	worktreeRec := scheduler.NewWorktreeReconciler(pool, log)
	s.rcmgr.Register(worktreeRec)
	// Wire the sequence notifier: when a bound child work item reaches a
	// terminal state, advance its parent's chain immediately (the scan
	// pass every 200ms is the safety net).
	workflowRec.SetSequenceNotifier(func(ctx context.Context, parentID string) {
		if s.rcmgr != nil {
			s.rcmgr.Enqueue("sequence", parentID)
		}
	})
	// Wire the worktree notifier: when a run is armed (pending→running),
	// provision its isolated working tree immediately (the scan pass every
	// 200ms is the safety net).
	workflowRec.SetWorktreeNotifier(func(ctx context.Context, runID string) {
		if s.rcmgr != nil {
			s.rcmgr.Enqueue("worktree", runID)
		}
	})

	// Seed an in-process OpenCode adapter registration so the
	// TaskReconciler can find a ready adapter for dispatch (docs/04 §6.3:
	// in-process adapter for dev only). Idempotent.
	seedDevAdapter(context.Background(), pool, log)

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

// Run blocks until ctx is cancelled, serving traffic, running the outbox
// relay and reconciler framework, and shutting down gracefully within
// ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting orchicon control plane",
		"version", version.Current().String(), "http", s.cfg.HTTPAddr)

	// The host opencode serve lives as long as the plane (its watchdog
	// reaps it on shutdown via the cancel).
	defer func() {
		if s.serveCancel != nil {
			s.serveCancel()
		}
	}()

	// Clear stale edit locks from a prior server session. Locks have a
	// 5-minute TTL; on restart any lock still in the DB is orphaned and
	// would block the same user on a fresh page load.
	if _, err := s.pool.Exec(ctx, "DELETE FROM edit_locks"); err != nil {
		s.log.Warn("startup: clear edit locks", "error", err)
	}

	errCh := make(chan error, 4)
	go func() { errCh <- s.httpSrv.ListenAndServe() }()

	if s.relay != nil {
		go func() { errCh <- s.relay.Run(ctx) }()
	}

	if s.rcmgr != nil {
		go func() { errCh <- s.rcmgr.Run(ctx) }()
	}

	// Adopt workflow runtime containers at boot and on a 30s sweep:
	// reap orphans (containers whose run is no longer active — covers
	// runs terminalized while the plane was down, aborted runs, and any
	// terminal state the reconciler transition path missed) and ensure
	// containers for active runs. The same sweep fails executions whose
	// process is gone (plane restart / lost runtime container) so
	// recovery re-dispatches instead of leaving the workflow stuck.
	if s.runtime != nil {
		go func() {
			sweep := time.NewTicker(30 * time.Second)
			defer sweep.Stop()
			var once bool
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if !once {
					select {
					case <-ctx.Done():
						return
					case <-time.After(1 * time.Second):
					}
					once = true
				}
				if err := s.runtime.Adopt(ctx); err != nil {
					s.log.Warn("workflow runtime adopt failed", "error", err)
				}
				if s.reaper != nil {
					if err := s.reaper.Reap(ctx); err != nil {
						s.log.Warn("execution liveness reap failed", "error", err)
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-sweep.C:
				}
			}
		}()
	} else if s.reaper != nil {
		go func() {
			sweep := time.NewTicker(30 * time.Second)
			defer sweep.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(1 * time.Second):
					if err := s.reaper.Reap(ctx); err != nil {
						s.log.Warn("execution liveness reap failed", "error", err)
					}
				case <-sweep.C:
					if err := s.reaper.Reap(ctx); err != nil {
						s.log.Warn("execution liveness reap failed", "error", err)
					}
				}
			}
		}()
	}

	// Phase 9: webhook dispatcher (NATS consumer → HTTP POST + retries +
	// dead-letter — docs/07 §3.11). Degrades gracefully when NATS is
	// unavailable (the dispatcher logs and returns nil).
	if s.webhookD != nil {
		go func() { errCh <- s.webhookD.Run(ctx) }()
	}

	// Periodically heartbeat the in-process dev adapter so the
	// TaskReconciler can dispatch tasks beyond the initial 60s heartbeat
	// TTL (docs/03 §5). Dev-only: the seed adapter is in-process
	// (docs/04 §6.3); production adapters heartbeat themselves.
	go s.heartbeatDevAdapter(ctx)

	// Index-integrity sweep: at boot and every IndexCheckInterval, verify
	// every user btree index with amcheck and rebuild any corrupted ones.
	// A corrupt index silently hides rows from `=` lookups (planner uses
	// the index) while seq scans still see them — a hard host sleep
	// corrupted worker_versions_worker_version_idx in the field, and the
	// workflow reconciler failed a dispatch on a worker that existed. The
	// boot pass catches the corruption before it can wedge a run; the
	// periodic pass catches corruption that occurs while the plane is up.
	go s.indexHealthLoop(ctx)

	// Container mode: keep the project-mounts manifest (on the data volume)
	// in sync with the projects table so scripts/container.sh can mount the
	// project dirs/files the user selected. ORCHICON_DATA_DIR is only set by
	// the single-container supervisor. Saving a project dir refreshes the
	// manifest immediately (hook); the periodic writer is the safety net.
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

	// Start the scheduled backup loop. Reads backup_schedule and
	// backup_retention_days from tenant_settings every 60s and runs
	// a pg_dump snapshot when the cron expression matches.
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

	// Live-apply Settings → Defaults log management values to the
	// rotating on-disk log writer every few seconds (no restart needed
	// when the operator changes log size/roll/retention in the UI).
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
		s.pool.Close()
		s.shutdownOTel()
		return nil
	case err := <-errCh:
		s.authH.CloseEmbeddedOP()
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

// applyLogSettingsLoop polls the default tenant's Settings → Defaults log
// management values and live-applies them to the rotating on-disk log
// writer. Runs for the lifetime of the server (the ctx is the Run ctx,
// which is cancelled on shutdown). Failures (DB down, no row) are skipped
// silently — the writer keeps its last good config.
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

// logSettingsFromRow maps tenant settings to the rotating writer config,
// layered on the writer's CURRENT config (env/config defaults from boot).
// Only explicitly-set DB values override. Returns nil when nothing is set.
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

// heartbeatDevAdapter renews the in-process dev adapter's heartbeat
// every 30s so the TaskReconciler can dispatch tasks beyond the initial
// heartbeat TTL (docs/03 §5, docs/04 §6.3). Dev-only: the seed adapter
// is in-process; production adapters heartbeat themselves over the
// adapter gRPC lease path.
func (s *Server) heartbeatDevAdapter(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			caps := opencode.BuildCapabilitiesJSON()
			ttx, err := s.pool.BeginTenantTx(ctx, "tnt_dev")
			if err != nil {
				continue
			}
			if err := db.HeartbeatAdapter(ctx, ttx.Tx, "tnt_dev", "adp_opencode_dev", []byte(caps)); err != nil {
				s.log.Warn("dev adapter heartbeat failed", "error", err)
			}
			_ = ttx.Commit(ctx)
		}
	}
}

// indexHealthLoop runs the amcheck index-integrity sweep once at boot and
// then every cfg.IndexCheckInterval. A corrupted btree index silently hides
// rows from `=` lookups (the planner uses the index) while seq scans still
// see them — a hard host sleep corrupted worker_versions_worker_version_idx
// in the field, making the workflow reconciler fail a dispatch on a worker
// that existed. The boot pass catches pre-existing corruption; the periodic
// pass catches corruption that happens while the plane is up. An interval of
// 0 runs only the boot pass.
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

// seedDevAdapter registers an in-process OpenCode adapter so the
// TaskReconciler can find a ready adapter for dispatch during local
// development (docs/04 §6.3: "for local dev, an in-process adapter is
// supported for tests only, never production"). Idempotent — re-runs
// on every boot update the heartbeat timestamp.
func seedDevAdapter(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	tenantID := "tnt_dev"
	adapterID := "adp_opencode_dev"

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
		caps := opencode.BuildCapabilitiesJSON()
		if err := db.HeartbeatAdapter(ctx, ttx.Tx, tenantID, adapterID, []byte(caps)); err != nil {
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
		Kind:                    "opencode",
		Version:                 "0.1.0",
		Endpoint:                "in-process",
		Capabilities:            []byte(opencode.BuildCapabilitiesJSON()),
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
	log.Info("seeded dev opencode adapter", "id", adapterID)
}
