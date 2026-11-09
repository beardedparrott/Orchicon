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

// extraBindRetryInterval is how long serveExtra waits before re-attempting the bridge bind after a
// failure.
var extraBindRetryInterval = 30 * time.Second

// listenerAddrs returns the addresses the plane binds: the primary HTTP address plus the optional
// bridge bind (when configured).
func listenerAddrs(cfg config.Config) []string {
	addrs := []string{cfg.HTTPAddr}
	if cfg.ExtraBind != "" {
		addrs = append(addrs, cfg.ExtraBind)
	}
	return addrs
}

// bindPrimary binds cfg.HTTPAddr and returns the listener.
func (s *Server) bindPrimary() (net.Listener, error) {
	ln, err := listenRetryingEphemeral(s.cfg.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("server: bind primary http %s: %w", s.cfg.HTTPAddr, err)
	}
	return ln, nil
}

// ephemeralBindAttempts and ephemeralBindBackoff bound the retry for a PORT-0 bind. IT CANNOT MASK
// A REAL COLLISION, and that is the safety argument for having it.
var (
	ephemeralBindAttempts = 5
	ephemeralBindBackoff  = 100 * time.Millisecond
)

// listenFn is the bind seam. Production never reassigns it; a test does, so "the retry ENGAGES" can be
// asserted rather than inferred from the loop's presence in the source.
var listenFn = net.Listen

// listenRetryingEphemeral binds addr, retrying ONLY when the port is 0.
func listenRetryingEphemeral(addr string) (net.Listener, error) {
	ln, err := listenFn("tcp", addr)
	if err == nil {
		return ln, nil
	}
	_, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil || port != "0" {
		// A specific port (or an address we cannot parse): report the first failure. A collision on a
		// named port is information, not noise, and retrying it would only delay the same answer.
		return nil, err
	}
	for attempt := 1; attempt < ephemeralBindAttempts; attempt++ {
		time.Sleep(ephemeralBindBackoff)
		if ln, rerr := listenFn("tcp", addr); rerr == nil {
			return ln, nil
		}
	}
	// Exhausted: return the ORIGINAL error, so the message names the address the caller asked for rather
	// than whichever attempt happened to be last.
	return nil, err
}

// startListeners binds the primary listener (fatal on failure) and starts the optional bridge
// listener (best-effort, non-fatal).
func (s *Server) startListeners(ctx context.Context) (net.Listener, error) {
	primary, err := s.bindPrimary()
	if err != nil {
		return nil, err
	}
	s.log.Info("listener bind set", "addrs", listenerAddrs(s.cfg))
	if s.cfg.ExtraBind != "" {
		if bindSetCollides(s.cfg.HTTPAddr, s.cfg.ExtraBind) {
			// A wildcard primary already owns the extra bind's address, so serveExtra's net.Listen can NEVER
			// succeed — it would only retry a doomed bind every extraBindRetryInterval for the life.
			s.log.Error("bridge listener skipped: the primary bind is a wildcard and already owns the bridge address — ORCHICON_HTTP_ADDR must name a concrete interface address (e.g. 127.0.0.1:<port>) so the two binds cannot collide",
				"http_addr", s.cfg.HTTPAddr, "extra_bind", s.cfg.ExtraBind, "env", "ORCHICON_HTTP_ADDR")
		} else {
			go s.serveExtra(ctx, s.cfg.ExtraBind)
		}
	}
	return primary, nil
}

// bindSetCollides reports whether binding primary makes extra unbindable: a wildcard primary (empty
// host, or an unspecified IP such as 0.0.0.0 / ::) owns EVERY interface at its port.
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

// It is deliberately NOT fatal and NEVER reported on errCh — Run returns from the whole plane on
// any errCh value, and the docker bridge address can change across a docker restart.
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
