// Package server boots the Orchicon control plane: opens the database
// pool, runs migrations (if enabled), mounts the API, and serves HTTP
// + gRPC until shutdown. It is the single composition root.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/beardedparrott/orchicon/internal/api"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/version"
)

// Server owns the running control plane process.
type Server struct {
	cfg    config.Config
	log    *slog.Logger
	httpSrv *http.Server
}

// New constructs a Server from configuration.
func New(cfg config.Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	handler := api.Mount(mux)
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}
	return &Server{cfg: cfg, log: log, httpSrv: httpSrv}, nil
}

// Run blocks until ctx is cancelled, serving traffic and shutting down
// gracefully within ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting orchicon control plane", "version", version.Current().String(), "http", s.cfg.HTTPAddr)
	errCh := make(chan error, 1)
	go func() { errCh <- s.httpSrv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: serve: %w", err)
	}
}
