package opencode

// Tier A (tool-hang watchdog) + Tier B (stream-drop turn retry) unit tests.
// Detection lives in progressMonitor (clock-injectable); actuation lives in
// sessionRun (httptest serve). Acceptance criteria covered:
// trip/reset/latch/escalation + the disabled gate, abort-then-redirect
// order, the abort-echo guard, the bounded-2 retry + fall-through kill,
// no-duplication (exactly one user part per intervention), and the
// session-survives-abort integration shape.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func newToolHangManifest(secs int64) scheduler.ExecutionManifest {
	return scheduler.ExecutionManifest{StallToolHangSeconds: secs}
}

func toolHangTestWindows() stallWindows {
	return stallWindows{
		noProgress:  time.Hour,
		noFileDiff:  time.Hour,
		textLoop:    time.Hour,
		repetitionN: 1000,
		repetitionW: time.Minute,
		toolHang:    time.Second,
	}
}

// TestToolHangTrip verifies detection trips after a silent window.
func TestToolHangTrip(t *testing.T) {
	m := newTestMonitor(toolHangTestWindows())
	base := time.Now()
	m.now = func() time.Time { return base }
	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	// check() is the production entry (monitor run loop); it latches.
	if reason := m.check(); reason != "stalled:tool_hang:bash" {
		t.Fatalf("check = %q, want stalled:tool_hang:bash", reason)
	}
	// The latch is consumed: a direct checkToolHang now reports silence
	// (the still-stuck worker escalates via probe, never a second hang).
	if name, ok := m.checkToolHang(); ok {
		t.Fatalf("checkToolHang = (%q,true) after check() latched, want silence", name)
	}
}

// TestToolHangReset verifies a resolving event before the window prevents
// the trip (the window resets when the call completes).
func TestToolHangReset(t *testing.T) {
	m := newTestMonitor(toolHangTestWindows())
	base := time.Now()
	m.now = func() time.Time { return base }
	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(500 * time.Millisecond) }
	m.observeToolEnd()
	m.now = func() time.Time { return base.Add(10 * time.Second) }
	if name, ok := m.checkToolHang(); ok {
		t.Fatalf("checkToolHang = (%q,true) after reset, want no trip", name)
	}
	if reason := m.check(); reason != "" {
		t.Fatalf("check = %q after reset, want empty", reason)
	}
}

// TestToolHangActivityRefresh verifies ANY event while in flight refreshes
// the window (zero-events-only trips).
func TestToolHangActivityRefresh(t *testing.T) {
	m := newTestMonitor(toolHangTestWindows())
	base := time.Now()
	m.now = func() time.Time { return base }
	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(900 * time.Millisecond) }
	m.noteToolActivity()
	m.now = func() time.Time { return base.Add(1500 * time.Millisecond) }
	if name, ok := m.checkToolHang(); ok {
		t.Fatalf("checkToolHang = (%q,true) after activity refresh, want no trip", name)
	}
	// Silence for a full window after the refresh trips.
	m.now = func() time.Time { return base.Add(2500 * time.Millisecond) }
	if _, ok := m.checkToolHang(); !ok {
		t.Fatal("checkToolHang did not trip after a full silent window")
	}
}

// TestToolHangSingleSlotSupersede verifies only one call is tracked: a new
// start supersedes the previous slot.
func TestToolHangSingleSlotSupersede(t *testing.T) {
	m := newTestMonitor(toolHangTestWindows())
	base := time.Now()
	m.now = func() time.Time { return base }
	m.observeToolStart("read")
	m.now = func() time.Time { return base.Add(900 * time.Millisecond) }
	m.observeToolStart("bash") // supersedes: hang start restarts
	m.now = func() time.Time { return base.Add(1500 * time.Millisecond) }
	if name, ok := m.checkToolHang(); ok {
		t.Fatalf("checkToolHang = (%q,true) after supersede, want no trip", name)
	}
	m.now = func() time.Time { return base.Add(2500 * time.Millisecond) }
	name, ok := m.checkToolHang()
	if !ok || name != "bash" {
		t.Fatalf("checkToolHang = (%q,%v), want (bash,true)", name, ok)
	}
}

// TestToolHangLatch verifies the intervention latches once per execution:
// the second window never trips again.
func TestToolHangLatch(t *testing.T) {
	m := newTestMonitor(toolHangTestWindows())
	base := time.Now()
	m.now = func() time.Time { return base }
	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	if _, ok := m.checkToolHang(); !ok {
		t.Fatal("first trip missing")
	}
	m.now = func() time.Time { return base.Add(time.Hour) }
	if name, ok := m.checkToolHang(); ok {
		t.Fatalf("second checkToolHang = (%q,true), want latched silence", name)
	}
}

// TestToolHangDisabled verifies a non-positive window never trips.
func TestToolHangDisabled(t *testing.T) {
	for _, d := range []struct {
		name string
		w    stallWindows
	}{
		{"zero", func() stallWindows { w := toolHangTestWindows(); w.toolHang = 0; return w }()},
		{"negative", func() stallWindows { w := toolHangTestWindows(); w.toolHang = -time.Second; return w }()},
	} {
		m := newTestMonitor(d.w)
		base := time.Now()
		m.now = func() time.Time { return base }
		m.observeToolStart("bash")
		m.now = func() time.Time { return base.Add(24 * time.Hour) }
		if name, ok := m.checkToolHang(); ok {
			t.Fatalf("%s window: checkToolHang = (%q,true), want disabled", d.name, name)
		}
	}
}

// TestToolHangEscalationNeutral verifies the hang signal stays advisory:
// isFatalStall is false for it, and no_progress still fires.
func TestToolHangEscalationNeutral(t *testing.T) {
	if isFatalStall("stalled:tool_hang:bash") {
		t.Fatal("tool_hang must stay advisory (isFatalStall=false)")
	}
	if !isFatalStall("stalled:no_progress") {
		t.Fatal("no_progress must stay fatal (the silent-death backstop)")
	}
	m := newTestMonitor(stallWindows{noProgress: time.Second, noFileDiff: time.Hour, repetitionN: 1000, repetitionW: time.Minute, toolHang: time.Second})
	base := time.Now()
	m.now = func() time.Time { return base }
	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	// lastStepFinish is stale too (same silence) — tool_hang wins the
	// ordering (redirect before silence kills).
	if reason := m.check(); reason != "stalled:tool_hang:bash" {
		t.Fatalf("check = %q, want tool_hang first", reason)
	}
}

// TestToolHangManifestBranch verifies stallWindowsFromManifest plumbing:
// positive overrides, 0 keeps env/code default, negative disables.
func TestToolHangManifestBranch(t *testing.T) {
	t.Setenv("ORCHICON_STALL_TOOL_HANG_WINDOW", "")
	t.Setenv("ORCHICON_TOOL_HANG_WINDOW", "")
	w := stallWindowsFromManifest(newToolHangManifest(90))
	if w.toolHang != 90*time.Second {
		t.Fatalf("manifest 90 -> %v, want 90s", w.toolHang)
	}
	w = stallWindowsFromManifest(newToolHangManifest(0))
	if w.toolHang != 180*time.Second {
		t.Fatalf("manifest 0 -> %v, want 180s default", w.toolHang)
	}
	w = stallWindowsFromManifest(newToolHangManifest(-5))
	if w.toolHang > 0 {
		t.Fatalf("manifest -5 -> %v, want disabled (<=0)", w.toolHang)
	}
	t.Setenv("ORCHICON_STALL_TOOL_HANG_WINDOW", "7s")
	w = stallWindowsFromManifest(newToolHangManifest(90))
	if w.toolHang != 7*time.Second {
		t.Fatalf("env 7s + manifest 90 -> %v, want env wins (7s)", w.toolHang)
	}
}

// TestStreamDropClassifier verifies the Tier B trigger classification:
// truncation/drop signatures retry; clean errors and abort echoes fail.
func TestStreamDropClassifier(t *testing.T) {
	drops := []string{
		"model response stream truncated",
		"event dropped from stream",
		"stream reset by peer: connection reset",
		"unexpected EOF reading stream",
		"SSE disconnect mid-turn",
		"stream was cut off",
	}
	for _, d := range drops {
		if !isStreamDropError(d) {
			t.Fatalf("isStreamDropError(%q) = false, want true", d)
		}
	}
	cleans := []string{
		"unauthorized: invalid api key",
		"HTTP 429 rate limit exceeded",
		"quota exhausted",
		"permission denied by policy",
		"not found: model foo",
		"Aborted",
		"session.error: Aborted",
		"",
		"some unknown provider hiccup",
	}
	for _, c := range cleans {
		if isStreamDropError(c) {
			t.Fatalf("isStreamDropError(%q) = true, want false", c)
		}
	}
}

// TestAbortEchoGuard verifies an own-Abort echo never settles/fails the run.
func TestAbortEchoGuard(t *testing.T) {
	for _, m := range []string{"Aborted", "session.error: Aborted", "Request aborted by client", "turn cancelled by user"} {
		if !isAbortEcho(m) {
			t.Fatalf("isAbortEcho(%q) = false, want true", m)
		}
	}
	if isAbortEcho("model response stream truncated") {
		t.Fatal("truncation must not classify as an abort echo")
	}
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &sessionRun{a: a, parentCtx: context.Background(), execRow: db.ExecutionRow{ID: "e", TenantID: "t"}, done: make(chan struct{}), stats: &execStreamState{}}
	r.hangAbortAt = time.Now()
	if !r.recentOwnAbort() {
		t.Fatal("recentOwnAbort must be true right after our Abort")
	}
	r.hangAbortAt = time.Now().Add(-time.Hour)
	if r.recentOwnAbort() {
		t.Fatal("recentOwnAbort must expire outside the echo window")
	}
}

// TestTruncatedStepFinishClassifier verifies the mid-turn truncation
// signature: unknown/empty reason + zero tokens.
func TestTruncatedStepFinishClassifier(t *testing.T) {
	if !isTruncatedStepFinish(map[string]any{"reason": "unknown", "tokens": map[string]any{}}) {
		t.Fatal("unknown reason + zero tokens must classify truncated")
	}
	if !isTruncatedStepFinish(map[string]any{"reason": "", "tokens": map[string]any{}}) {
		t.Fatal("empty reason + zero tokens must classify truncated")
	}
	if isTruncatedStepFinish(map[string]any{"reason": "stop", "tokens": map[string]any{}}) {
		t.Fatal("clean stop must not classify truncated")
	}
	if isTruncatedStepFinish(map[string]any{"reason": "unknown", "tokens": map[string]any{"input": 5.0}}) {
		t.Fatal("non-zero tokens must not classify truncated")
	}
	if isTruncatedStepFinish(nil) {
		t.Fatal("nil part must not classify truncated")
	}
}

// TestStreamRetryBound verifies the bounded-2 retry then fall-through kill.
func TestStreamRetryBound(t *testing.T) {
	var stalls []string
	var mu sync.Mutex
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &sessionRun{
		a: a, parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "e", TenantID: "t"},
		callbacks: &stallRecordingCallbacks{stalls: &stalls, mu: &mu},
		done:      make(chan struct{}), stats: &execStreamState{},
		store: func(context.Context, string, string, []db.SessionPart) error { return nil },
	}
	r.retryStreamTurn("drop 1")
	r.retryStreamTurn("drop 2")
	if r.streamRetries != 2 {
		t.Fatalf("streamRetries = %d, want 2", r.streamRetries)
	}
	if r.nudgesSent != 0 {
		t.Fatalf("retry touched the nudge budget (nudgesSent=%d)", r.nudgesSent)
	}
	if r.isFinished() {
		t.Fatal("run must stay alive within the retry budget")
	}
	r.retryStreamTurn("drop 3")
	if !r.isFinished() {
		t.Fatal("third drop did not fall through to the kill path")
	}
	r.mu.Lock()
	reason := r.resultErr
	r.mu.Unlock()
	if !strings.Contains(reason, "drop 3") {
		t.Fatalf("kill reason = %q, want the drop reason", reason)
	}
}

// TestAbortThenRedirectOrder verifies the Tier A order: Abort BEFORE
// SendMessage, execution stays running, exactly one redirect part.
func TestAbortThenRedirectOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/abort"):
			order = append(order, "abort")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/prompt_async"):
			order = append(order, "send")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var stalls []string
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &sessionRun{
		a: a, parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "e", TenantID: "t"},
		callbacks: &stallRecordingCallbacks{stalls: &stalls, mu: &mu},
		client:    NewSessionClient(srv.URL, "", ""), sessionID: "s",
		done: make(chan struct{}), stats: &execStreamState{},
		toolHangWindowVal: time.Second,
		store:             func(context.Context, string, string, []db.SessionPart) error { return nil },
	}
	r.actuateToolHang("bash")
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "abort" || got[1] != "send" {
		t.Fatalf("call order = %v, want [abort send]", got)
	}
	if r.isFinished() {
		t.Fatal("actuator must never finish the execution")
	}
	// Second actuation no-ops (latched once per execution).
	r.actuateToolHang("bash")
	mu.Lock()
	got = append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("latched re-actuation made calls: %v", got)
	}
}

// TestOnStallToolHangRoutesToActuator verifies monitor trips route to the
// abort-and-redirect actuator, never the nudge path.
func TestOnStallToolHangRoutesToActuator(t *testing.T) {
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/abort"):
			order = append(order, "abort")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/prompt_async"):
			order = append(order, "send")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var stalls []string
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &sessionRun{
		a: a, parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "e", TenantID: "t"},
		callbacks: &stallRecordingCallbacks{stalls: &stalls, mu: &mu},
		client:    NewSessionClient(srv.URL, "", ""), sessionID: "s",
		done: make(chan struct{}), stats: &execStreamState{},
		toolHangWindowVal: time.Second,
		store:             func(context.Context, string, string, []db.SessionPart) error { return nil },
	}
	r.onStall("stalled:tool_hang:bash")
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "abort" || got[1] != "send" {
		t.Fatalf("onStall(tool_hang) order = %v, want [abort send]", got)
	}
	if r.nudgesSent != 0 {
		t.Fatalf("tool_hang must not spend the nudge budget (nudgesSent=%d)", r.nudgesSent)
	}
	if r.isFinished() {
		t.Fatal("tool_hang onStall must not finish the execution")
	}
}

// TestSessionSurvivesAbortAndCompletes is the AC integration shape: abort
// the in-flight turn, land the redirect, and prove the session completes
// its task afterwards (mock serve: redirect answered + summary delivered).
func TestSessionSurvivesAbortAndCompletes(t *testing.T) {
	var mu sync.Mutex
	var aborted, redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/abort"):
			aborted = true
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/prompt_async"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "tool exceeded tool-hang window") {
				redirected = true
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var stalls []string
	var stored []db.SessionPart
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &sessionRun{
		a: a, parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "e", TenantID: "t"},
		callbacks: &stallRecordingCallbacks{stalls: &stalls, mu: &mu},
		client:    NewSessionClient(srv.URL, "", ""), sessionID: "s",
		done: make(chan struct{}), stats: &execStreamState{},
		toolHangWindowVal: 20 * time.Millisecond,
		store: func(_ context.Context, _, _ string, parts []db.SessionPart) error {
			stored = append(stored, parts...)
			return nil
		},
	}
	// Tool goes silent past the window through the real event pipeline.
	r.handleEvent(BusEvent{Type: "message.part.updated", Properties: map[string]any{
		"sessionID": "s",
		"part":      map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{"status": "running"}},
	}})
	time.Sleep(30 * time.Millisecond)
	r.checkToolHang()
	mu.Lock()
	aOK, rOK := aborted, redirected
	mu.Unlock()
	if !aOK || !rOK {
		t.Fatalf("abort=%v redirect=%v, want both true", aOK, rOK)
	}
	// The session answers the redirect: tool resolves, model delivers the
	// decision summary, session idles — the task completes afterwards.
	r.handleEvent(BusEvent{Type: "message.part.updated", Properties: map[string]any{
		"sessionID": "s",
		"part":      map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{"status": "completed", "output": "ok"}},
	}})
	r.output.WriteString("done. ORCHICON WORKER SUMMARY: success — recovered after redirect")
	r.handleEvent(BusEvent{Type: "session.idle", Properties: map[string]any{"sessionID": "s"}})
	if r.isFinished() {
		t.Fatal("run must stay alive through abort-and-redirect")
	}
	r.flushParts()
	found := false
	for _, p := range stored {
		if p.Kind == db.SessionPartUserMessage && strings.Contains(string(p.Payload), "tool_hang_redirect") {
			found = true
		}
	}
	if !found {
		t.Fatal("redirect missing from durable transcript")
	}
	if realDecisionMarkerIn(r.output.String()) < 0 {
		t.Fatal("decision summary must survive the abort-and-redirect")
	}
}
