package runtime

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

// TestRunServeLivenessGate verifies the liveness-gated idempotent path: a
// serve that is registered but NO LONGER answers /global/health must NOT
// be reported as "up" (the cached port+password are not returned blindly)
// — the wedge that previously made every dispatch burn its 30s probe and
// then degrade. The test registers a wedged serve under the reserved
// serveExecID, then drives runServe with a child that dies immediately
// (so the restart can never become healthy). The handshake must surface
// an error, never the cached serve event.
func TestRunServeLivenessGate(t *testing.T) {
	h := newChildRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Register a serve session whose process is dead (nothing listens on
	// the default port, so serveHealthy fails).
	h.mu.Lock()
	h.cmd[serveExecID] = newExecSession(serveExecID)
	h.servePw = "dead-serve-password"
	h.serveStarted = true
	h.mu.Unlock()

	pr, pw := io.Pipe()
	defer pr.Close()
	enc := json.NewEncoder(pw)

	// The wedge is liveness-gated: runServe kills the dead serve and tries
	// a fresh start. A child that exits immediately can never become
	// healthy, so the handshake must return an error — the cached creds
	// must never be handed back for a serve that isn't usable.
	req := AgentRequest{
		Cmd:  "serve",
		Argv: []string{"bash", "-c", "exit 0"},
	}
	go func() {
		h.runServe(enc, req)
		pw.Close()
	}()

	dec := json.NewDecoder(pr)
	for {
		var ev AgentEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("runServe stream ended without an error event: %v", err)
		}
		if ev.Event == "serve" {
			t.Fatalf("liveness-gate failed: dead serve reported as up with password %q", ev.Password)
		}
		if ev.Event == "error" {
			return // expected: the wedge is surfaced as an error, not a serve
		}
	}
}
