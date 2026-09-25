package server

// Tests for the dual-listener wiring (listen.go): the primary HTTP bind
// (loopback — host clients: orch, the GUI) and the optional docker-bridge
// bind runtime containers dial.
//
// The load-bearing property here is NOT "the bridge listener works" (that
// is proven end to end by a real dispatched run) but "a bridge listener
// that CANNOT bind does not take the plane down". Run returns from the
// whole control plane on any errCh value, so serveExtra is given no channel
// at all — these tests pin the behaviour that used to be a channel away from
// a plane-wide outage: the docker bridge address legitimately changes across
// docker restarts, and host clients must survive that.

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/config"
)

// syncBuf is a goroutine-safe log sink (serveExtra writes from its own
// goroutine while the test reads).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newListenerTestServer builds the minimum Server the listener primitives
// need: the config, a captured logger, and an http.Server answering
// /healthz. Run itself needs a live DB and reconcilers, so these tests
// exercise the primitives Run composes instead.
func newListenerTestServer(cfg config.Config, buf *syncBuf) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return &Server{
		cfg: cfg,
		log: slog.New(slog.NewTextHandler(buf, nil)),
		httpSrv: &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           mux,
			ReadHeaderTimeout: 2 * time.Second,
		},
	}
}

// getOK polls addr/healthz until it answers 200 or the deadline passes.
func getOK(t *testing.T, addr string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitForLog polls the captured log until it contains substr.
func waitForLog(t *testing.T, buf *syncBuf, substr string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestListenerAddrsHasNoWildcard pins the "not reachable from another
// machine" property at the bind-set level: every address the plane binds
// names a concrete interface address. A wildcard here would be the LAN
// exposure the operator rejected (and config.Validate refuses it, but the
// bind list is what actually gets bound).
func TestListenerAddrsHasNoWildcard(t *testing.T) {
	cfg := config.Config{HTTPAddr: "127.0.0.1:8091", ExtraBind: "172.17.0.1:8091"}
	addrs := listenerAddrs(cfg)
	if len(addrs) != 2 {
		t.Fatalf("listenerAddrs = %v, want 2 entries (primary + bridge)", addrs)
	}
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			t.Fatalf("addr %q not host:port: %v", a, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsUnspecified() || host == "" {
			t.Fatalf("addr %q is not a concrete interface address (wildcard/LAN exposure)", a)
		}
	}

	// Unset extra bind: loopback/primary only.
	if got := listenerAddrs(config.Config{HTTPAddr: "127.0.0.1:8091"}); len(got) != 1 {
		t.Fatalf("listenerAddrs without ExtraBind = %v, want 1 entry", got)
	}
}

// TestPrimaryListenerServes pins the host-client path: the primary bind is
// real, bound before serving, and answers /healthz on its own address.
func TestPrimaryListenerServes(t *testing.T) {
	var buf syncBuf
	s := newListenerTestServer(config.Config{HTTPAddr: "127.0.0.1:0"}, &buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary, err := s.startListeners(ctx)
	if err != nil {
		t.Fatalf("startListeners: %v", err)
	}
	addr := primary.Addr().String()
	go func() { _ = s.httpSrv.Serve(primary) }()
	defer func() { _ = s.httpSrv.Close() }()

	if !getOK(t, addr, 2*time.Second) {
		t.Fatalf("primary listener %s did not answer /healthz", addr)
	}
}

// TestServeExtraFailureIsNonFatal is the load-bearing one. The bridge bind
// points at TEST-NET-1 (192.0.2.1, never a local interface), so the bind
// CANNOT succeed. Required: the already-bound primary listener keeps serving
// host clients, and the failure is LOUD (the log names the env var and why it
// matters) and retried — never fatal, because Run treats any errCh value as
// "return from the whole plane".
func TestServeExtraFailureIsNonFatal(t *testing.T) {
	prev := extraBindRetryInterval
	extraBindRetryInterval = 20 * time.Millisecond
	defer func() { extraBindRetryInterval = prev }()

	var buf syncBuf
	cfg := config.Config{HTTPAddr: "127.0.0.1:0", ExtraBind: "192.0.2.1:9"}
	s := newListenerTestServer(cfg, &buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary, err := s.startListeners(ctx)
	if err != nil {
		t.Fatalf("startListeners must not fail because the bridge bind cannot: %v", err)
	}
	addr := primary.Addr().String()
	go func() { _ = s.httpSrv.Serve(primary) }()
	defer func() { _ = s.httpSrv.Close() }()

	// Host clients keep working while the bridge listener is dead.
	if !getOK(t, addr, 2*time.Second) {
		t.Fatalf("primary listener %s stopped serving after a bridge-bind failure", addr)
	}

	if !waitForLog(t, &buf, "bridge listener bind failed", 2*time.Second) {
		t.Fatalf("bridge bind failure was not logged loudly; log:\n%s", buf.String())
	}
	logs := buf.String()
	if !strings.Contains(logs, "ORCHICON_HTTP_EXTRA_BIND") {
		t.Fatalf("bridge bind failure must name ORCHICON_HTTP_EXTRA_BIND; log:\n%s", logs)
	}
	// Retried, not given up on: with a 20ms interval there must be a second
	// attempt (the "still failing" warn) quickly.
	if !waitForLog(t, &buf, "(still failing)", 2*time.Second) {
		t.Fatalf("bridge bind was not retried; log:\n%s", logs)
	}

	// The plane is still alive and serving — and cancelling ctx stops the
	// retry loop (it returns rather than spinning forever).
	if !getOK(t, addr, time.Second) {
		t.Fatalf("primary listener %s stopped serving after retries", addr)
	}
	cancel()
}

// TestServeExtraBindsWhenAddressAppears pins the recovery direction: an
// address that is busy on the first attempt (exactly the "the bridge address
// changed / something else holds it" case) is picked up as soon as it frees.
func TestServeExtraBindsWhenAddressAppears(t *testing.T) {
	prev := extraBindRetryInterval
	extraBindRetryInterval = 10 * time.Millisecond
	defer func() { extraBindRetryInterval = prev }()

	// Hold the address so serveExtra's first attempts fail.
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold address: %v", err)
	}
	addr := holder.Addr().String()

	var buf syncBuf
	s := newListenerTestServer(config.Config{HTTPAddr: "127.0.0.1:0", ExtraBind: addr}, &buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = s.httpSrv.Close() }()

	go s.serveExtra(ctx, addr)
	if !waitForLog(t, &buf, "bridge listener bind failed", 2*time.Second) {
		t.Fatalf("expected the busy address to fail the first attempts; log:\n%s", buf.String())
	}

	// Free it: the retry must pick it up and start serving.
	if err := holder.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	if !waitForLog(t, &buf, "bridge listener serving", 2*time.Second) {
		t.Fatalf("serveExtra did not bind the address once it appeared; log:\n%s", buf.String())
	}
	if !getOK(t, addr, 2*time.Second) {
		t.Fatalf("bridge listener %s did not answer /healthz after binding", addr)
	}
}
