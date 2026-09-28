package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/beardedparrott/orchicon/internal/config"
)

// extraBindRetryInterval is how long serveExtra waits before re-attempting
// the bridge bind after a failure. It is a package var so tests can shorten
// it (the runtime package uses the same var-seam pattern for its clocks).
var extraBindRetryInterval = 30 * time.Second

// listenerAddrs returns the addresses the plane binds: the primary HTTP
// address plus the optional bridge bind (when configured). Pure, so the boot
// log and the tests read the same list — the "the bind set contains no
// wildcard address" assertion rides on this.
//
// WHY NO WILDCARD: the two binds must be DISTINCT, and a wildcard primary is
// not — `:8080` already owns <bridge ip>:8080 on every interface, so the
// extra bind can never bind (see bindSetCollides). The HOST shape therefore
// binds loopback + the docker bridge (scripts/container.sh's plane_env and
// bridge_bind_env); the CONTAINER shape keeps its one wildcard `:8080` and
// configures no extra bind, so nothing can collide there
// (cmd/orchicon/container.go's containerChildEnv).
func listenerAddrs(cfg config.Config) []string {
	addrs := []string{cfg.HTTPAddr}
	if cfg.ExtraBind != "" {
		addrs = append(addrs, cfg.ExtraBind)
	}
	return addrs
}

// bindPrimary binds cfg.HTTPAddr and returns the listener. A failure is
// FATAL — host clients (orch, the GUI) reach the plane on the primary
// listener and nothing substitutes for it. Bound explicitly (rather than
// ListenAndServe) so the bound address is known before serving starts and
// so the optional bridge listener can share the same http.Server.
func (s *Server) bindPrimary() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("server: bind primary http %s: %w", s.cfg.HTTPAddr, err)
	}
	return ln, nil
}

// startListeners binds the primary listener (fatal on failure) and starts
// the optional bridge listener (best-effort, non-fatal). Run serves the
// returned primary listener on errCh; the bridge listener is deliberately
// disconnected from errCh (see serveExtra).
func (s *Server) startListeners(ctx context.Context) (net.Listener, error) {
	primary, err := s.bindPrimary()
	if err != nil {
		return nil, err
	}
	s.log.Info("listener bind set", "addrs", listenerAddrs(s.cfg))
	if s.cfg.ExtraBind != "" {
		if bindSetCollides(s.cfg.HTTPAddr, s.cfg.ExtraBind) {
			// A wildcard primary already owns the extra bind's address, so
			// serveExtra's net.Listen can NEVER succeed — it would only retry a
			// doomed bind every extraBindRetryInterval for the life of the
			// process (the observed host-plane boot: "bridge listener bind
			// failed … address already in use" every 30s, forever). Say what is
			// wrong ONCE, name the fix, and do not start the goroutine: the
			// wildcard already answers on that address, so nothing is lost, and
			// the plane must not be a wildcard bind anyway.
			s.log.Error("bridge listener skipped: the primary bind is a wildcard and already owns the bridge address — ORCHICON_HTTP_ADDR must name a concrete interface address (e.g. 127.0.0.1:<port>) so the two binds cannot collide",
				"http_addr", s.cfg.HTTPAddr, "extra_bind", s.cfg.ExtraBind, "env", "ORCHICON_HTTP_ADDR")
		} else {
			go s.serveExtra(ctx, s.cfg.ExtraBind)
		}
	}
	return primary, nil
}

// bindSetCollides reports whether binding primary makes extra unbindable: a
// wildcard primary (empty host, or an unspecified IP such as 0.0.0.0 / ::)
// owns EVERY interface at its port, so a second listener for the same port
// cannot bind. The ports must match — a wildcard primary on a different port
// does not own the extra bind's port. Malformed addresses report false and let
// the ordinary bind paths speak.
func bindSetCollides(primary, extra string) bool {
	if primary == "" || extra == "" {
		return false
	}
	primaryHost, primaryPort, err := net.SplitHostPort(primary)
	if err != nil {
		return false
	}
	_, extraPort, err := net.SplitHostPort(extra)
	if err != nil {
		return false
	}
	if primaryPort == "" || primaryPort != extraPort {
		return false
	}
	if primaryHost == "" {
		return true
	}
	ip := net.ParseIP(primaryHost)
	return ip != nil && ip.IsUnspecified()
}

// serveExtra binds cfg.ExtraBind and serves it from the shared s.httpSrv,
// retrying while ctx is live.
//
// The extra bind exists so runtime containers reach a HOST-RESIDENT plane
// across the docker bridge (the port in it is this instance's own HTTP
// port, never a hardcoded 8080). It is deliberately NOT fatal and NEVER
// reported on errCh — Run returns from the whole plane on any errCh value,
// and the docker bridge address can change across a docker restart. A dead
// bridge listener must degrade to "workers cannot reach the plane" (loud,
// retried every extraBindRetryInterval) instead of taking the plane down
// with it, because host clients reach the still-healthy primary listener.
// There is no channel to write to here precisely so a future refactor
// cannot reintroduce that failure mode by accident.
//
// Returning is only correct on shutdown: ctx cancelled (plane going down)
// or http.ErrServerClosed (s.httpSrv.Shutdown closes every listener it
// serves, this one included).
func (s *Server) serveExtra(ctx context.Context, addr string) {
	firstAttempt := true
	for {
		if ctx.Err() != nil {
			return
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			const msg = "bridge listener bind failed — runtime-container workers cannot reach the plane (the orchicon-plane MCP channel cannot dial it)"
			attrs := []any{
				"bind", addr,
				"env", "ORCHICON_HTTP_EXTRA_BIND",
				"retry_in", extraBindRetryInterval,
				"error", err,
			}
			if firstAttempt {
				s.log.Error(msg+" — check that ORCHICON_HTTP_EXTRA_BIND names this host's current docker bridge address; the plane keeps serving host clients on the primary listener", attrs...)
			} else {
				s.log.Warn(msg+" (still failing)", attrs...)
			}
			firstAttempt = false
			if !s.waitRetry(ctx) {
				return
			}
			continue
		}
		firstAttempt = false
		s.log.Info("bridge listener serving", "addr", ln.Addr().String(), "env", "ORCHICON_HTTP_EXTRA_BIND")
		err = s.httpSrv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		s.log.Error("bridge listener serve failed — will retry", "bind", addr, "error", err)
		if !s.waitRetry(ctx) {
			return
		}
	}
}

// waitRetry sleeps for extraBindRetryInterval, returning false when ctx
// ended first (the caller must stop).
func (s *Server) waitRetry(ctx context.Context) bool {
	t := time.NewTimer(extraBindRetryInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
