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

	"github.com/beardedparrott/orchicon/internal/api"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/outbox"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
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
	deps := api.Dependencies{Pool: pool, Log: log, Subscriber: sub}
	handler := api.Mount(mux, deps)

	// Wrap with OTel tracing interceptor (spans on every API call).
	handler = telemetry.Middleware(handler)

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	s := &Server{cfg: cfg, log: log, pool: pool, httpSrv: httpSrv, otel: otelShutdown}
	if pub != nil {
		s.relay = outbox.NewRelay(pool, pub, log)
	}

	// Reconciler framework (docs/03 §2). Concrete reconcilers arrive
	// in later phases; the framework ships now: work queue, per-kind
	// leadership via Postgres advisory locks, graceful shutdown.
	s.rcmgr = reconciler.NewManager(pool, log)

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

	select {
	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
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
