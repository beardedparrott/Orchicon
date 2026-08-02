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
	"time"

	"github.com/beardedparrott/orchicon/internal/api"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/backup"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/outbox"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/beardedparrott/orchicon/internal/version"
	"github.com/beardedparrott/orchicon/internal/webhook"
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
}

// New constructs a Server from configuration. It opens the DB pool,
// connects to NATS, sets up OTel, starts the outbox relay, and mounts
// the API.
func New(cfg config.Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// OTel telemetry pipeline (tracer + meter + OTLP exporter → Grafana stack).
	// If the collector is unreachable, telemetry is dropped with bounded
	// in-process buffering; control flow is not blocked (docs/08 §8).
	otelShutdown, err := telemetry.Setup(context.Background(), cfg, log)
	if err != nil {
		log.Warn("otel setup failed (telemetry disabled)", "error", err)
	}

	pool, err := db.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("server: open db: %w", err)
	}

	// Seed the dev tenant so the control plane has a tenant context
	// before auth (Phase 9) lands. Idempotent.
	if err := db.SeedDevTenant(context.Background(), pool); err != nil {
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
	taskRec := scheduler.NewTaskReconciler(pool, log, adapterBridge)
	// Phase 7: the TaskReconciler triggers recovery when an execution
	// fails (docs/06 §2). The RecoveryEngine satisfies the scheduler's
	// RecoveryTrigger interface (loose coupling — no scheduler→recovery
	// import).
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
		blobs: blobs, authH: authHandler, webhookD: webhookDisp}
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
	var runtimeLifecycle *runtime.Lifecycle
	if cfg.RuntimeSocket != "" {
		if rtClient := runtime.NewClient(cfg.RuntimeSocket, cfg.Instance); rtClient.Ready(context.Background()) {
			runtimeLifecycle = runtime.NewLifecycle(rtClient, pool, log)
			// Route executions that belong to a workflow run into that
			// workflow's runtime container instead of a local subprocess.
			adapterBridge.SetRuntimeClient(rtClient)
			// Execution liveness: fail executions orphaned by a plane
			// restart or a lost runtime container so recovery re-dispatches.
			s.reaper = scheduler.NewExecutionReaper(pool, rtClient, adapterBridge.IsExecutionActive, taskRec.FailLostExecution, log)
			log.Info("workflow runtime daemon connected", "socket", cfg.RuntimeSocket)
		} else {
			log.Warn("workflow runtime daemon not reachable — in-process execution", "socket", cfg.RuntimeSocket)
		}
	}
	// Headless: still reap in-process executions orphaned by a restart.
	if s.reaper == nil {
		s.reaper = scheduler.NewExecutionReaper(pool, nil, adapterBridge.IsExecutionActive, taskRec.FailLostExecution, log)
	}
	workflowRec := scheduler.NewWorkflowReconciler(pool, log, policyEngine, taskRec, recoveryEngine, runtimeLifecycle)
	s.runtime = runtimeLifecycle
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
	s.rcmgr = reconciler.NewManager(pool, log)
	s.rcmgr.Register(taskRec)
	s.rcmgr.Register(workflowRec)
	s.rcmgr.Register(recoveryRec)
	s.rcmgr.Register(scheduledRunRec)

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

	select {
	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
		if s.webhookD != nil {
			s.webhookD.Stop()
		}
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
