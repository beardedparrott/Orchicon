package activitye2e

// plane_test.go — the FIXTURE PLANE's own state machine, pinned WITHOUT the live gate.
//
// The end-to-end legs (internal/tui/activity_e2e_*.go, frontend/tests/activity-line-e2e.spec.ts) are
// opt-in and need a real pty / a real browser. The control-surface behaviours those legs DEPEND on
// would otherwise be asserted nowhere in the standing suite — a harness bug in them would make a
// green capstone meaningless, which is precisely the class of defect this capstone already found
// once (the double-script race). So the cheap, deterministic half lives here and runs everywhere.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEndTurnIsUndoneByReset pins the fix that lets an END-TURN observation be composed with the
// legs around it. EndTurn closes the turn's done channel once; before Reset replaced the channel and
// the Once, the FIRST end of the process poisoned every later stream (each returned immediately) and
// the control surface worked exactly once. The reset a later leg performs must restore a live turn.
func TestEndTurnIsUndoneByReset(t *testing.T) {
	p := New()

	p.EndTurn()
	select {
	case <-p.done:
		// closed: the stream sees the end, which is the point of EndTurn
	case <-time.After(time.Second):
		t.Fatal("EndTurn did not close the turn's done channel — a running stream would never see the turn end")
	}
	if p.running {
		t.Error("EndTurn left the plane reporting a turn in flight")
	}

	p.Reset(PhaseFlight)
	select {
	case <-p.done:
		t.Fatal("Reset left the PREVIOUS turn's done channel closed: every stream opened after a reset " +
			"returns at once, so the control surface would serve exactly one turn per process")
	case <-time.After(50 * time.Millisecond):
		// open: a new turn can actually run
	}
	if p.running {
		t.Error("Reset left the plane reporting a turn in flight")
	}
	if got := len(p.Calls()); got != 0 {
		t.Errorf("Reset left %d ledger rows behind — the zero-tool-call leg would inherit the with-tools "+
			"leg's counter", got)
	}
}

// TestReissueLandsFreshCalls pins the browser half of Plane.Reissue: after a long stall the earlier
// calls have honestly aged out of the summarizer's 30s rolling window, so a leg that wants counters
// BACK must earn them with new work rather than rewind a clock. The rows must carry a stamp that is
// genuinely NOW, or the summarizer would age them out too.
func TestReissueLandsFreshCalls(t *testing.T) {
	p := New()
	before := len(p.Calls())
	if before != 0 {
		t.Fatalf("a fresh plane already holds %d rows", before)
	}

	start := time.Now().UnixMilli()
	p.Reissue()
	calls := p.Calls()
	if len(calls) != 2 {
		t.Fatalf("Reissue appended %d calls, want 2", len(calls))
	}
	for _, c := range calls {
		if c.AtMs < start {
			t.Errorf("Reissue stamped %q at %d, before the call was made (%d) — the summarizer would "+
				"immediately age it out", c.ToolName, c.AtMs, start)
		}
	}
	if got := p.SummarizeNow(); got == "" {
		t.Error("the server's own render of the reissued ledger is empty, so the counters would not " +
			"come back on the row")
	}
}

// TestResetClearsThePlaneState is the "clean slate" contract the two-leg structure rests on: a reset
// leaves no ledger, no streamed text and no turn, so the zero-tool-call leg cannot inherit the
// with-tools leg's counter (which is exactly the false claim the honesty check exists to catch).
func TestResetClearsThePlaneState(t *testing.T) {
	p := New()
	p.issueCall("read")
	p.appendStreamed("some reply text")

	p.Reset(PhaseNoTools)
	if got := len(p.Calls()); got != 0 {
		t.Errorf("Reset left %d ledger rows", got)
	}
	if got := p.Streamed(); got != "" {
		t.Errorf("Reset left streamed text %q", got)
	}
	if p.Phase() != PhaseNoTools {
		t.Errorf("Reset put the plane in phase %q, want %q", p.Phase(), PhaseNoTools)
	}
}

// TestControlSurfaceServesEndResetAndReissue drives the three harness routes the two clients call,
// through the REAL mux, so a route that stops being wired is a test failure rather than a capstone
// that silently observes nothing.
func TestControlSurfaceServesEndResetAndReissue(t *testing.T) {
	p := New()
	srv := httptest.NewServer(Mux(p, &Sessions{}, t.TempDir()))
	defer srv.Close()

	post := func(path string) int {
		t.Helper()
		res, err := http.Post(srv.URL+path, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	if code := post("/__e2e/reissue"); code != http.StatusOK {
		t.Fatalf("POST /__e2e/reissue = %d, want 200", code)
	}
	if got := len(p.Calls()); got != 2 {
		t.Errorf("/__e2e/reissue left %d rows, want 2", got)
	}
	if code := post("/__e2e/end"); code != http.StatusOK {
		t.Fatalf("POST /__e2e/end = %d, want 200", code)
	}
	select {
	case <-p.done:
	default:
		t.Error("/__e2e/end did not end the turn")
	}
	if code := post("/__e2e/reset?p=no-tools"); code != http.StatusOK {
		t.Fatalf("POST /__e2e/reset = %d, want 200", code)
	}
	if got := len(p.Calls()); got != 0 {
		t.Errorf("/__e2e/reset left %d rows, want 0", got)
	}
	if p.Phase() != PhaseNoTools {
		t.Errorf("/__e2e/reset put the plane in phase %q, want %q", p.Phase(), PhaseNoTools)
	}
	select {
	case <-p.done:
		t.Error("/__e2e/reset left the turn ended — a later leg could not run a turn")
	default:
	}
}

// TestResetReenablesAuthRoutes pins the other half of "a reset is a clean slate for the NEXT leg":
// a run that stopped the plane (`down`/`rpc-down`) flipped the SPA's auth routes off, and a reset
// that left them off would serve the browser an unauthenticated page — a green-looking leg that
// observes nothing.
func TestResetReenablesAuthRoutes(t *testing.T) {
	p := New()
	sessions := &Sessions{}
	srv := httptest.NewServer(Mux(p, sessions, t.TempDir()))
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/__e2e/phase?p=down", "application/json", nil); err != nil {
		t.Fatalf("POST phase down: %v", err)
	}
	if !sessions.IsDown() {
		t.Fatal("stopping the plane did not stop the auth routes")
	}
	if _, err := http.Post(srv.URL+"/__e2e/reset?p=flight", "application/json", nil); err != nil {
		t.Fatalf("POST reset: %v", err)
	}
	if sessions.IsDown() {
		t.Error("a reset left the auth routes failing: the next leg's browser would never mount the " +
			"authenticated route")
	}
}
