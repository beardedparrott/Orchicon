package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMCPHealthyAndProbeUsable verifies ADR-0002 D1 — the MCP watchdog gate.
// A serve that answers /global/health but cannot perform a session-create
// round-trip (a wedged/unusable MCP) is NOT "healthy" for dispatch: MCPHealthy
// is false and ProbeUsable errors. A serve that answers both is healthy. This
// is the plane-level watchdog signal: HostServe.Watch restarts a serve that
// passes /global/health but fails MCP usability, healing a single wedged MCP
// for every session on the serve.
func TestMCPHealthyAndProbeUsable(t *testing.T) {
	// Healthy: /global/health 200, /session 200 returns a probe id, /abort 204.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "probe1"})
		case strings.HasSuffix(r.URL.Path, "/abort"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ok.Close()
	okC := NewSessionClient(ok.URL, "", "")
	if !okC.MCPHealthy(context.Background()) {
		t.Fatal("a healthy serve must report MCP healthy")
	}
	if err := okC.ProbeUsable(context.Background()); err != nil {
		t.Fatalf("ProbeUsable on a healthy serve = %v, want nil", err)
	}

	// Wedged MCP: /global/health 200 but the session-create round-trip fails.
	wedge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer wedge.Close()
	wC := NewSessionClient(wedge.URL, "", "")
	if wC.MCPHealthy(context.Background()) {
		t.Fatal("a serve that fails session-create (wedged MCP) must report MCP unhealthy")
	}
	if err := wC.ProbeUsable(context.Background()); err == nil {
		t.Fatal("ProbeUsable must error when the MCP-driven session create fails")
	}

	// Process down: /global/health fails outright.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	dC := NewSessionClient(down.URL, "", "")
	if dC.MCPHealthy(context.Background()) {
		t.Fatal("an unhealthy serve must report MCP unhealthy")
	}
}

// TestSubscriptionReadDropsWhenFull verifies ADR-0002 D6 — a slow consumer
// must not park the /event SSE reader on a full channel. Before the fix a
// full 256-event buffer blocked `read`, stalling that subscription and, with
// every session reading the one bus, degrading all of them. Now the reader
// drops a telemetry event (the durable record is the persisted reply, so a
// dropped event never loses data) and keeps scanning.
func TestSubscriptionReadDropsWhenFull(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, `data: {"id":"%d","type":"message.part.delta","properties":{"field":"text","delta":"x"}}`+"\n\n", i)
	}
	// Deliberately tiny buffer so the reader overflows in a couple of frames.
	sub := &Subscription{events: make(chan BusEvent, 4), done: make(chan struct{}), once: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub.read(context.Background(), io.NopCloser(strings.NewReader(b.String())))
	}()
	// Read nothing: leave the buffer full. If `read` still blocks on a full
	// channel (pre-D6), it will not return within the timeout. With D6 it drops
	// and finishes scanning all frames, then closes the event channel.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Subscription.read blocked on a full event buffer (D6 not honored)")
	}
	count := 0
	for range sub.events {
		count++
	}
	if count == 0 {
		t.Fatal("expected some events buffered before the reader dropped the overflow")
	}
}

// TestSubscriptionReadNeverDropsSessionIdle verifies the ADR-0002 D6 WATCH
// constraint: session.idle is the SOLE completion signal for a turn — the event
// a collector relies on to mark a reply complete. A slow consumer's buffer
// overflowing must DROP telemetry events, but it must NEVER drop session.idle,
// or a completed turn is silently reported as timed out. The reader blocks
// (bounded by the subscription's own Close) to deliver the completion signal
// instead of discarding it.
func TestSubscriptionReadNeverDropsSessionIdle(t *testing.T) {
	var b strings.Builder
	// Fill + overflow the buffer with non-idle telemetry, then a terminal
	// session.idle at the end. All prior frames are droppable; the idle is not.
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, `data: {"id":"%d","type":"message.part.delta","properties":{"field":"text","delta":"x"}}`+"\n\n", i)
	}
	fmt.Fprintf(&b, `data: {"id":"idle","type":"session.idle","properties":{"sessionID":"ses_x"}}`+"\n\n")

	sub := &Subscription{events: make(chan BusEvent, 4), done: make(chan struct{}), once: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub.read(context.Background(), io.NopCloser(strings.NewReader(b.String())))
	}()

	// Drain whatever the reader buffers. The full-buffer telemetry frames are
	// dropped; the trailing session.idle must STILL be delivered (the reader
	// blocks on the send rather than discarding the completion signal).
	var gotIdle bool
	timer := time.After(3 * time.Second)
	for {
		select {
		case evt, ok := <-sub.events:
			if !ok {
				if !gotIdle {
					t.Fatal("session.idle was dropped on a full buffer — it is the sole completion signal (D6 WATCH)")
				}
				return
			}
			if evt.Type == "session.idle" {
				gotIdle = true
			}
		case <-timer:
			t.Fatal("Subscription.read blocked or never delivered session.idle")
		}
	}
}
