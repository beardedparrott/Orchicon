// Package server boots the Orchicon control plane: opens the database
// pool, runs migrations (if enabled), seeds the dev tenant, starts the
// outbox relay, mounts the API, and serves HTTP + gRPC until shutdown.
// It is the single composition root.
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
	"github.com/beardedparrott/orchicon/internal/version"
)

// Server owns the running control plane process and its dependencies.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	pool    *db.Pool
	relay   *outbox.Relay
	httpSrv *http.Server
}

// New constructs a Server from configuration. It opens the DB pool and
// connects to NATS eagerly so boot failures surface before serving.
func New(cfg config.Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
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
		// Don't fail boot — the outbox is the source of truth; NATS is
		// a derivable sink. The relay is skipped if the publisher is nil.
	} else {
		log.Info("nats publisher connected", "url", cfg.NATSURL)
	}

	mux := http.NewServeMux()
	handler := api.Mount(mux, api.Dependencies{Pool: pool, Log: log})
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	s := &Server{cfg: cfg, log: log, pool: pool, httpSrv: httpSrv}
	if pub != nil {
		s.relay = outbox.NewRelay(pool, pub, log)
	}
	return s, nil
}

// Run blocks until ctx is cancelled, serving traffic, running the outbox
// relay, and shutting down gracefully within ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting orchicon control plane",
		"version", version.Current().String(), "http", s.cfg.HTTPAddr)

	errCh := make(chan error, 2)
	go func() { errCh <- s.httpSrv.ListenAndServe() }()

	if s.relay != nil {
		go func() { errCh <- s.relay.Run(ctx) }()
	}

	select {
	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.pool.Close()
			return fmt.Errorf("server: shutdown: %w", err)
		}
		s.pool.Close()
		return nil
	case err := <-errCh:
		s.pool.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: serve: %w", err)
	}
}
