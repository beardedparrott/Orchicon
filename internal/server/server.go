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
	"os/exec"
	"time"

	"github.com/beardedparrott/orchicon/internal/api"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/outbox"
	"github.com/beardedparrott/orchicon/internal/policy"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/recovery"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/telemetry"
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
}

// New constructs a Server from configuration. It opens the DB pool,
// connects to NATS, sets up OTel, starts the outbox relay, and mounts
// the API.
func New(cfg config.Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// OTel telemetry pipeline (tracer + meter + OTLP exporter → SigNoz).
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
	// Phase 8: SigNoz proxy client (docs/08 §5). Proxies tenant-scoped
	// queries to SigNoz/ClickHouse for the TelemetryService. Empty URL
	// disables proxying (queries degrade gracefully — docs/08 §8).
	signozClient := telemetry.NewSigNozClient(cfg.SigNozURL)
	log.Info("signoz proxy configured", "url", cfg.SigNozURL)

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

	deps := api.Dependencies{
		Pool:              pool,
		Log:               log,
		Subscriber:        sub,
		PolicyEngine:      policyEngine,
		RecoveryEngine:    recoveryEngine,
		SigNozClient:      signozClient,
		AuthHandler:       authHandler,
		WebhookDispatcher: webhookDisp,
		Mode:              cfg.Mode,
		ModelDiscoverer:   modelDiscoverer,
		MCPDiscoverer:     mcpDiscoverer,
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

	// Reconciler framework (docs/03 §2). Phase 5 registers the
	// TaskReconciler — the control loop that dispatches ready tasks to
	// runtime adapters (docs/03 §4). The OpenCode adapter bridge is the
	// CLI subprocess wrapper that drives the `opencode` binary
	// (docs/04 §6). If the binary is absent, the bridge runs in
	// simulation mode for dev verification.
	adapterBridge := opencode.New(log)
	// Phase 8: wire the AI Gateway usage recorder into the adapter so
	// step_finish token/cost telemetry is dual-written to Postgres
	// (source of truth) + OTel metrics (ClickHouse) — docs/08 §5.2.
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
		})
		return err
	})
	taskRec := scheduler.NewTaskReconciler(pool, log, adapterBridge)
	// Phase 7: the TaskReconciler triggers recovery when an execution
	// fails (docs/06 §2). The RecoveryEngine satisfies the scheduler's
	// RecoveryTrigger interface (loose coupling — no scheduler→recovery
	// import).
	taskRec.SetRecoveryTrigger(recoveryEngine)
	// Wire the direct NATS publisher for low-latency execution event
	// streaming. When set, the reconciler publishes streaming events
	// (text, tool_call, artifact, status changes) directly to NATS
	// after each callback commits, bypassing the outbox relay's 500ms
	// poll interval. The outbox is still written for durability.
	if pub != nil {
		taskRec.SetEventPublisher(pub)
	}
	workflowRec := scheduler.NewWorkflowReconciler(pool, log, policyEngine, taskRec)
	// Wire the workflow notifier: when a work item completes, enqueue
	// the workflow run ID so the WorkflowReconciler progresses the DAG
	// immediately instead of waiting for its next scan pass (200ms).
	taskRec.SetWorkflowNotifier(func(ctx context.Context, runID string) {
		if s.rcmgr != nil {
			s.rcmgr.Enqueue("workflow", runID)
		}
	})
	recoveryRec := recovery.NewReconciler(recoveryEngine)
	s.rcmgr = reconciler.NewManager(pool, log)
	s.rcmgr.Register(taskRec)
	s.rcmgr.Register(workflowRec)
	s.rcmgr.Register(recoveryRec)

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

	errCh := make(chan error, 4)
	go func() { errCh <- s.httpSrv.ListenAndServe() }()

	if s.relay != nil {
		go func() { errCh <- s.relay.Run(ctx) }()
	}

	if s.rcmgr != nil {
		go func() { errCh <- s.rcmgr.Run(ctx) }()
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
	caps := `{"model_providers":["anthropic","openai","local"],"tools":["file_edit","terminal","web_fetch","git"],"context":["file_index"],"telemetry":["tool_calls_streamed","file_diffs"],"execution":["checkpoint","pause_resume","cancellation"]}`
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
	capabilities := `{"model_providers":["anthropic","openai","local"],"tools":["file_edit","terminal","web_fetch","git"],"context":["file_index"],"telemetry":["tool_calls_streamed","file_diffs"],"execution":["checkpoint","pause_resume","cancellation"]}`

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
		if err := db.HeartbeatAdapter(ctx, ttx.Tx, tenantID, adapterID, []byte(capabilities)); err != nil {
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
		Capabilities:            []byte(capabilities),
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
