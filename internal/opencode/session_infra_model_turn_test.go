package opencode

import (
	"io"
	"log/slog"
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestIsInfraModelTurnError verifies the socket/transport class of
// model-turn failures is classified as infra — so recordStreamError recycles
// the runtime container immediately — while per-request / server-decision
// rejections (4xx/5xx, auth, quota, policy) stay on the retry path.
func TestIsInfraModelTurnError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		// Socket / transport-layer connect failures → infra.
		{"cannot connect to API", "opencode_session_error: Cannot connect to API: Unable to connect. Is the computer able to access the url?", true},
		{"unable to connect bare", "Unable to connect", true},
		{"connection refused", "connection refused", true},
		{"dial tcp", "dial tcp 10.0.0.5:443: i/o timeout", true},
		{"dial udp", "dial udp 10.0.0.5:53: no such host", true},
		{"no such host", "no such host: api.example.com", true},
		{"i/o timeout", "context deadline exceeded: i/o timeout", true},
		{"connection reset", "read tcp: connection reset by peer", true},
		{"network unreachable", "network unreachable", true},
		// Blank / unknown → not infra.
		{"empty", "", false},
		{"unrelated", "opencode session error", false},
		{"generic provider", "model returned a non-2xx response", false},
		// Server-decision / per-request class → NOT infra, even when it also
		// carries a socket phrase (guard checked first).
		{"http 400", "http 400 bad request", false},
		{"http 500 with connect phrase", "Unable to connect ... http 500 internal server error", false},
		{"401 unauthorized", "unauthorized: 401 invalid api key", false},
		{"403 forbidden", "403 forbidden", false},
		{"rate limit 429", "rate limit exceeded: 429", false},
		{"insufficient quota", "insufficient credits: quota exceeded", false},
		{"policy rejection", "model-api policy: content not permitted", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isInfraModelTurnError(c.msg); got != c.want {
				t.Fatalf("isInfraModelTurnError(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

// TestRecycleOnInfraModelTurn verifies the immediate-recycle path's
// per-dispatch repair budget: the FIRST infra model-turn failure recycles
// (counter increments), further recycles stop once the budget is spent, and
// any progress resets the counter so a healthy step never inherits a prior
// dispatch's spent budget.
func TestRecycleOnInfraModelTurn(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_REPAIR_ATTEMPTS", "2")
	// rt points at a non-existent daemon socket so the best-effort Kill fails
	// fast (a failed recycle is just a Warn log; it never panics or hangs).
	a := &Adapter{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		rt:  runtime.NewClient("/tmp/orchicon/no-such-daemon.sock", "test"),
	}
	r := &sessionRun{
		a:        a,
		manifest: scheduler.ExecutionManifest{RuntimeWorkflowID: "wf-recycle-test"},
	}

	// First infra model-turn error → recycle (counter 1, Kill attempted).
	r.recycleOnInfraModelTurn("opencode_session_error: Cannot connect to API: Unable to connect.")
	if got := a.infraModelTurnRecycleCount(); got != 1 {
		t.Fatalf("infraModelTurnRecycleCount = %d after first error, want 1", got)
	}

	// Second error: budget (2) not yet spent → recycle again (counter 2).
	r.recycleOnInfraModelTurn("opencode_session_error: Cannot connect to API: Unable to connect.")
	if got := a.infraModelTurnRecycleCount(); got != 2 {
		t.Fatalf("infraModelTurnRecycleCount = %d after second error, want 2", got)
	}

	// Third error: budget exhausted → no further recycle, counter unchanged.
	r.recycleOnInfraModelTurn("opencode_session_error: Cannot connect to API: Unable to connect.")
	if got := a.infraModelTurnRecycleCount(); got != 2 {
		t.Fatalf("infraModelTurnRecycleCount = %d after budget exhausted, want 2", got)
	}

	// Progress resets the per-dispatch budget.
	r.noteSessionProgress()
	if got := a.infraModelTurnRecycleCount(); got != 0 {
		t.Fatalf("infraModelTurnRecycleCount = %d after progress reset, want 0", got)
	}
}

// TestRecycleOnInfraModelTurnDisabled verifies a repair budget < 1 disables
// the immediate infra-model-turn recycle entirely (legacy retry-only path).
func TestRecycleOnInfraModelTurnDisabled(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_REPAIR_ATTEMPTS", "0")
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &sessionRun{a: a, manifest: scheduler.ExecutionManifest{RuntimeWorkflowID: "wf-recycle-test"}}

	r.recycleOnInfraModelTurn("opencode_session_error: Cannot connect to API: Unable to connect.")
	if got := a.infraModelTurnRecycleCount(); got != 0 {
		t.Fatalf("infraModelTurnRecycleCount = %d with repair budget 0, want 0", got)
	}
}
