package opencode

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
)

// stallRecordingCallbacks records advisory stall reasons (OnStall).
type stallRecordingCallbacks struct {
	mu     *sync.Mutex
	stalls *[]string
}

func (c *stallRecordingCallbacks) OnStarted(context.Context, string)                          {}
func (c *stallRecordingCallbacks) OnHealth(context.Context, string, string)                   {}
func (c *stallRecordingCallbacks) OnRecovered(context.Context, string, string)                {}
func (c *stallRecordingCallbacks) OnText(context.Context, string, string)                     {}
func (c *stallRecordingCallbacks) OnToolCall(context.Context, string, string, []byte, []byte) {}
func (c *stallRecordingCallbacks) OnArtifact(context.Context, string, string, string, string) {}
func (c *stallRecordingCallbacks) OnWrittenFiles(context.Context, string, []string)           {}
func (c *stallRecordingCallbacks) OnResult(context.Context, string, bool, string, string)     {}
func (c *stallRecordingCallbacks) OnStall(_ context.Context, _ string, reason string, _ bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.stalls = append(*c.stalls, reason)
}

// TestLegacyEventFromBus verifies the bus→legacy event mapping matches the
// shapes the adapter's parseEvent pipeline consumes.
func TestLegacyEventFromBus(t *testing.T) {
	cases := []struct {
		name     string
		evt      BusEvent
		wantType string
		wantOK   bool
	}{
		{
			name: "text part at end",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "text", "text": "hello", "time": map[string]any{"start": 1, "end": 2}},
			}},
			wantType: "text", wantOK: true,
		},
		{
			name: "text part mid-stream is not emitted",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "text", "text": "hel", "time": map[string]any{"start": 1}},
			}},
			wantOK: false,
		},
		{
			name: "tool completed",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{"status": "completed", "output": "ok"}},
			}},
			wantType: "tool_use", wantOK: true,
		},
		{
			name: "tool running is not emitted",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{"status": "running"}},
			}},
			wantOK: false,
		},
		{
			name: "step-finish",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "step-finish", "tokens": map[string]any{"input": 10, "output": 5}},
			}},
			wantType: "step_finish", wantOK: true,
		},
		{
			name: "step-start",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "step-start"},
			}},
			wantType: "step_start", wantOK: true,
		},
		{
			name: "session error",
			evt: BusEvent{Type: "session.error", Properties: map[string]any{
				"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "rate limited"}},
			}},
			wantType: "error", wantOK: true,
		},
		{
			name:   "unrelated event is ignored",
			evt:    BusEvent{Type: "catalog.updated", Properties: map[string]any{}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := LegacyEventFromBus(tc.evt)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got["type"] != tc.wantType {
				t.Fatalf("type = %v, want %v", got["type"], tc.wantType)
			}
		})
	}
}

// TestTokenDeltaFromBus verifies the mid-generation delta detection: streamed
// text/reasoning deltas (modern `message.part.delta` and legacy
// `message.part.updated`-with-delta shapes) are recognized so the progress
// monitors can count a slow generation as alive, while completed parts and
// non-token events are excluded.
func TestTokenDeltaFromBus(t *testing.T) {
	cases := []struct {
		name     string
		evt      BusEvent
		wantText string
		wantOK   bool
	}{
		{
			name: "modern delta text field",
			evt: BusEvent{Type: "message.part.delta", Properties: map[string]any{
				"sessionID": "s", "messageID": "m", "partID": "p", "field": "text", "delta": "hel",
			}},
			wantText: "hel", wantOK: true,
		},
		{
			name: "modern delta reasoning field",
			evt: BusEvent{Type: "message.part.delta", Properties: map[string]any{
				"sessionID": "s", "messageID": "m", "partID": "p", "field": "reasoning", "delta": "think",
			}},
			wantText: "think", wantOK: true,
		},
		{
			name: "modern delta empty field still counts",
			evt: BusEvent{Type: "message.part.delta", Properties: map[string]any{
				"sessionID": "s", "messageID": "m", "partID": "p", "delta": "hi",
			}},
			wantText: "hi", wantOK: true,
		},
		{
			name: "modern delta non-token field is not progress",
			evt: BusEvent{Type: "message.part.delta", Properties: map[string]any{
				"sessionID": "s", "messageID": "m", "partID": "p", "field": "metadata", "delta": "{}",
			}},
			wantOK: false,
		},
		{
			name: "modern delta empty delta is not progress",
			evt: BusEvent{Type: "message.part.delta", Properties: map[string]any{
				"sessionID": "s", "messageID": "m", "partID": "p", "field": "text", "delta": "",
			}},
			wantOK: false,
		},
		{
			name: "legacy updated text part with delta and no time.end",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "text", "delta": map[string]any{"text": "hel"}, "time": map[string]any{"start": 1}},
			}},
			wantText: "hel", wantOK: true,
		},
		{
			name: "legacy updated reasoning part with delta",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "reasoning", "delta": map[string]any{"text": "think"}},
			}},
			wantText: "think", wantOK: true,
		},
		{
			name: "completed text part is not a delta",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "text", "text": "hello", "time": map[string]any{"start": 1, "end": 2}},
			}},
			wantOK: false,
		},
		{
			name: "legacy part without delta is not a delta",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "text", "text": "hel", "time": map[string]any{"start": 1}},
			}},
			wantOK: false,
		},
		{
			name: "tool part is not a delta",
			evt: BusEvent{Type: "message.part.updated", Properties: map[string]any{
				"part": map[string]any{"type": "tool", "state": map[string]any{"status": "running"}},
			}},
			wantOK: false,
		},
		{
			name:   "unrelated event is not a delta",
			evt:    BusEvent{Type: "session.status", Properties: map[string]any{}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := TokenDeltaFromBus(tc.evt)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if text != tc.wantText {
				t.Fatalf("delta text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

// TestSubscriptionParsesSSE verifies the SSE frame parser yields bus events
// from a realistic /event stream (data: frames, comments, keepalives).
func TestSubscriptionParsesSSE(t *testing.T) {
	stream := ": connected\n\n" +
		`data: {"id":"1","type":"server.connected","properties":{}}` + "\n\n" +
		`data: {"id":"2","type":"message.part.updated","properties":{"sessionID":"ses_x","part":{"type":"text","text":"hi"}}}` + "\n\n" +
		"event: keepalive\n:ping\n\n" +
		`data: {"id":"3","type":"session.idle","properties":{"sessionID":"ses_x"}}` + "\n\n"
	sub := &Subscription{events: make(chan BusEvent, 16), done: make(chan struct{}), once: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub.read(context.Background(), io.NopCloser(strings.NewReader(stream)))
	}()

	var got []BusEvent
	for evt := range sub.events {
		got = append(got, evt)
	}
	<-done

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[0].Type != "server.connected" {
		t.Errorf("first event type = %s", got[0].Type)
	}
	if got[2].Type != "session.idle" {
		t.Errorf("last event type = %s", got[2].Type)
	}
}

// TestSubscriptionCloseUnblocks verifies Close terminates the reader.
func TestSubscriptionCloseUnblocks(t *testing.T) {
	pr, pw := io.Pipe()
	sub := &Subscription{events: make(chan BusEvent, 16), done: make(chan struct{}), once: make(chan struct{}), body: pw}
	go sub.read(context.Background(), pr)
	sub.Close()
	select {
	case <-sub.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not terminate the reader")
	}
	pw.Close()
}

// TestStallReasonSurvivesAbortEcho verifies the abort-path ordering fix
// (D2): when a fatal stall fires, the terminal reason is recorded BEFORE the
// session is aborted, so the serve's `session.error: Aborted` echo is a no-op
// and the true cause (e.g. stalled:no_progress) survives into OnResult. This
// is the exact-300s Aborted root cause: previously onStall aborted first and
// recordStreamError could win the finish() race and mask the stall reason.
func TestStallReasonSurvivesAbortEcho(t *testing.T) {
	// The Abort HTTP call lands on a local test server (error ignored by
	// onStall, but the call must not hit the network).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	client := NewSessionClient(srv.URL, "", "")

	callbacks := &liveCallbacks{}
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-abort-race", TenantID: "tnt_dev"},
		callbacks: callbacks,
		client:    client,
		done:      make(chan struct{}),
		stats:     &execStreamState{},
	}
	// The stall fires: the terminal reason is recorded first, then the
	// session is aborted.
	r.onStall("stalled:no_progress")
	// The abort echo arrives on the SSE bus — it must NOT overwrite the
	// stall reason or mark the run unhealthy.
	r.recordStreamError(BusEvent{Type: "session.error", Properties: map[string]any{
		"error": map[string]any{"message": "Aborted"},
	}})

	r.mu.Lock()
	fin, ok, resultErr, lastErr := r.finished, r.resultOk, r.resultErr, r.lastStreamErr
	r.mu.Unlock()
	if !fin {
		t.Fatal("run did not finish after the fatal stall")
	}
	if ok {
		t.Fatal("fatal stall must fail the run")
	}
	if resultErr != "stalled:no_progress" {
		t.Fatalf("resultErr = %q, want stalled:no_progress (true cause must survive the abort echo)", resultErr)
	}
	if lastErr != "" {
		t.Fatalf("lastStreamErr = %q, want empty (abort echo must not be recorded)", lastErr)
	}
}

// TestRecordStreamErrorGuard verifies recordStreamError on an already-finished
// run is a no-op (the abort-echo guard): it must not re-mark health, bump the
// recycle counter, or change the terminal reason.
func TestRecordStreamErrorGuard(t *testing.T) {
	callbacks := &liveCallbacks{}
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-abort-guard", TenantID: "tnt_dev"},
		callbacks: callbacks,
		done:      make(chan struct{}),
		stats:     &execStreamState{},
	}
	r.finish(false, "stalled:no_progress")
	r.recordStreamError(BusEvent{Type: "session.error", Properties: map[string]any{
		"error": map[string]any{"message": "Aborted"},
	}})
	r.mu.Lock()
	resultErr, lastErr := r.resultErr, r.lastStreamErr
	r.mu.Unlock()
	if resultErr != "stalled:no_progress" {
		t.Fatalf("resultErr = %q, want stalled:no_progress", resultErr)
	}
	if lastErr != "" {
		t.Fatalf("lastStreamErr = %q, want empty (guard must skip the abort echo)", lastErr)
	}
}

// TestSessionRunPendingAccounting verifies completion is driven by
// session.idle (allTurnsDone), not by per-step signals.
func TestSessionRunPendingAccounting(t *testing.T) {
	r := &sessionRun{done: make(chan struct{}), stats: &execStreamState{}}

	r.bumpPending() // goal
	if r.pendingTurns != 1 {
		t.Fatalf("pending = %d, want 1", r.pendingTurns)
	}
	// A step-finish alone must NOT finish the execution (one user message
	// can span many steps).
	if r.isFinished() {
		t.Fatal("execution finished before session.idle")
	}
	// session.idle (queue drained) triggers the settle-finish.
	r.allTurnsDone()
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not finish after session.idle settle")
	}
	r.mu.Lock()
	fin, o := r.finished, r.resultOk
	r.mu.Unlock()
	if !fin || !o {
		t.Fatalf("finished=%v ok=%v, want true/true", fin, o)
	}
}

// TestSessionRunAllTurnsDone verifies session.idle force-settles even when
// a message was missed.
func TestSessionRunAllTurnsDone(t *testing.T) {
	r := &sessionRun{done: make(chan struct{}), stats: &execStreamState{}}
	r.bumpPending()
	r.bumpPending()
	r.allTurnsDone()
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("allTurnsDone did not settle the execution")
	}
}

// TestCompletionProbeDecision covers the pure decision core of the
// completion-signal probe (H1): a session that goes idle with the decision
// marker present settles; one without the marker gets probed while budget
// remains and fails once the budget is spent.
func TestCompletionProbeDecision(t *testing.T) {
	now := time.Now()
	withMarker := "Did the work.\n\nORCHICON WORKER SUMMARY: success — done"
	withoutMarker := "Did the work, but the summary line never arrived."

	cases := []struct {
		name      string
		output    string
		nudges    int
		lastNudge time.Time
		wantProbe bool
		wantFail  bool
	}{
		{
			name:      "marker present settles",
			output:    withMarker,
			nudges:    0,
			lastNudge: time.Time{},
			wantProbe: false, wantFail: false,
		},
		{
			name:      "marker present settles even with budget left",
			output:    withMarker,
			nudges:    1,
			lastNudge: now,
			wantProbe: false, wantFail: false,
		},
		{
			name:      "missing marker probes while budget remains",
			output:    withoutMarker,
			nudges:    0,
			lastNudge: time.Time{},
			wantProbe: true, wantFail: false,
		},
		{
			name:      "missing marker fails when budget exhausted",
			output:    withoutMarker,
			nudges:    nudgeMax(),
			lastNudge: time.Time{},
			wantProbe: false, wantFail: true,
		},
		{
			name:      "missing marker fails inside cooldown window",
			output:    withoutMarker,
			nudges:    0,
			lastNudge: now, // just nudged → cooldown blocks another probe
			wantProbe: false, wantFail: true,
		},
		{
			name:      "placeholder marker echo is not a real decision",
			output:    "The plan: ORCHICON WORKER SUMMARY: success — <summary> (example only)",
			nudges:    0,
			lastNudge: time.Time{},
			wantProbe: true, wantFail: false,
		},
		{
			name:      "recovery-seed inline-code marker is not a real decision",
			output:    "If missing at start, finish with `ORCHICON WORKER SUMMARY: failure` reason `recovery seed file missing`. Let me read the classifier.",
			nudges:    0,
			lastNudge: time.Time{},
			wantProbe: true, wantFail: false,
		},
		{
			name:      "backtick success example is not a real decision",
			output:    "Note the contract: `ORCHICON WORKER SUMMARY: success` — wrapping matters.",
			nudges:    0,
			lastNudge: time.Time{},
			wantProbe: true, wantFail: false,
		},
		{
			name: "real marker after an earlier placeholder echo is the decision",
			output: "Plan: ORCHICON WORKER SUMMARY: success — <summary>.\n" +
				"ORCHICON WORKER SUMMARY: success — the ADR is complete and the summary was delivered.",
			nudges:    0,
			lastNudge: time.Time{},
			wantProbe: false, wantFail: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, fail := completionProbeDecision(tc.output, tc.nudges, tc.lastNudge, now, nudgeMax(), nudgeCooldown())
			if probe != tc.wantProbe || fail != tc.wantFail {
				t.Fatalf("probe=%v fail=%v, want probe=%v fail=%v", probe, fail, tc.wantProbe, tc.wantFail)
			}
		})
	}
}

// TestCompletionProbeSuppressedAfterCompact verifies that a compacted run's
// markerless session.idle is a mid-task pause, NOT a truncated final turn: the
// completion probe must not interject (and the budget-exhausted fail must not
// fire) while the run has been compacted. A marker-present idle still settles.
func TestCompletionProbeSuppressedAfterCompact(t *testing.T) {
	now := time.Now()
	mkRun := func(output string, nudges int, compacted bool) *sessionRun {
		r := &sessionRun{
			a:           &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
			parentCtx:   context.Background(),
			execRow:     db.ExecutionRow{ID: "exec-probe-compact", TenantID: "tnt_dev"},
			callbacks:   &liveCallbacks{},
			client:      NewSessionClient("http://localhost:1", "", ""),
			done:        make(chan struct{}),
			stats:       &execStreamState{},
			output:      strings.Builder{},
			nudgesSent:  nudges,
			lastNudgeAt: now,
			// Budget spent → absent the compact gate this would FAIL. The gate
			// must take precedence so a compacted mid-task pause never fails.
			nudgeMaxVal:         nudgeMax(),
			nudgeReplyWindowVal: time.Hour,
			nudgeCooldownVal:    time.Nanosecond,
		}
		r.output.WriteString(output)
		if compacted {
			r.compactsPerformed = 1
			r.lastCompactStep = 1
		}
		return r
	}

	// Marker present + compacted → settle (return false), never wait.
	r := mkRun("Did the work.\n\nORCHICON WORKER SUMMARY: success — done", 0, true)
	if v := r.maybeProbeCompletion(); v {
		t.Fatal("marker-present idle after compact must settle (return false), not wait")
	}

	// Markerless + compacted → wait for the next turn, even with budget spent:
	// no probe, no fail, no settle.
	r = mkRun("mid-task pause text", nudgeMax(), true)
	if v := r.maybeProbeCompletion(); !v {
		t.Fatal("markerless idle after compact must NOT settle (return true): waiting for next turn")
	}
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("compacted mid-task pause must not fail the run on the missing marker")
	}

	// Markerless + NOT compacted + budget spent → still fails (probe budget
	// exhausted remains the terminal guard for non-compacted runs).
	r = mkRun("plain text pause", nudgeMax(), false)
	if v := r.maybeProbeCompletion(); !v {
		t.Fatal("non-compacted markerless idle with spent budget must fail (return true)")
	}
	r.mu.Lock()
	fin, ok := r.finished, r.resultOk
	r.mu.Unlock()
	if !fin || ok {
		t.Fatal("non-compacted markerless idle with spent budget must fail, not succeed")
	}
}

// TestStallNudgeFirstEscalation verifies the nudge-first routing core (the
// exact repro of the blocked-write loop): an advisory stall (repetition /
// text_loop / no_file_progress) nudges the live session instead of killing
// it, and only escalates to a fatal kill + recovery after the nudge budget
// (nudgeMax) is spent. no_progress stays fatal from the first trip.
func TestStallNudgeFirstEscalation(t *testing.T) {
	// The Abort / SendMessage HTTP calls land on a local test server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	client := NewSessionClient(srv.URL, "", "")

	callbacks := &liveCallbacks{}
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-nudge-escalate", TenantID: "tnt_dev"},
		callbacks: callbacks,
		client:    client,
		done:      make(chan struct{}),
		stats:     &execStreamState{},
		// Explicit budget of 2 nudges; reply window long enough that the
		// background probe-timeout goroutine never fires during the test;
		// cooldown negligible so consecutive trips can nudge.
		nudgeMaxVal:         2,
		nudgeReplyWindowVal: time.Hour,
		nudgeCooldownVal:    time.Nanosecond,
	}
	r.monitor = newProgressMonitor(r.execRow.ID, stallWindows{noProgress: time.Hour, noFileDiff: time.Hour})

	// Trip 1: an advisory repetition stall must NUDGE, not kill.
	r.onStall("stalled:repetition:bash|[\"go test\"]")
	r.mu.Lock()
	nudges, pending, fin := r.nudgesSent, r.probePending, r.finished
	r.mu.Unlock()
	if nudges != 1 || !pending {
		t.Fatalf("after first advisory trip: nudges=%d pending=%v, want 1/true (must nudge, not kill)", nudges, pending)
	}
	if fin {
		t.Fatal("advisory stall must not kill on the first trip")
	}

	// The worker keeps tripping. Reset the probe so the next trip nudges
	// again (the worker responded to the nudge but didn't break the loop).
	r.mu.Lock()
	r.probePending = false
	r.mu.Unlock()

	// Trip 2: budget not yet spent → second nudge, still no kill.
	r.onStall("stalled:repetition:bash|[\"go test\"]")
	r.mu.Lock()
	nudges, pending, fin = r.nudgesSent, r.probePending, r.finished
	r.mu.Unlock()
	if nudges != 2 || !pending {
		t.Fatalf("after trip 2: nudges=%d pending=%v, want 2/true", nudges, pending)
	}
	if fin {
		t.Fatal("advisory stall must not kill on the second trip (budget has one more nudge before exhaustion)")
	}

	r.mu.Lock()
	r.probePending = false
	r.mu.Unlock()

	// Trip 3: budget exhausted (2 nudges consumed) → escalate to fatal.
	r.onStall("stalled:repetition:bash|[\"go test\"]")
	r.mu.Lock()
	fin, ok, errMsg := r.finished, r.resultOk, r.resultErr
	r.mu.Unlock()
	if !fin || ok {
		t.Fatalf("third trip must escalate to a fatal kill (finished=%v ok=%v)", fin, ok)
	}
	if errMsg != "stalled:repetition:bash|[\"go test\"]" {
		t.Fatalf("escalation reason = %q, want the advisory reason", errMsg)
	}
}

// TestStallNoProgressFatal verifies no_progress stays FATAL from the first
// trip — total silence means there is no responsive surface to nudge, so it
// aborts immediately rather than nudging.
func TestStallNoProgressFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	client := NewSessionClient(srv.URL, "", "")
	callbacks := &liveCallbacks{}
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-noprogress-fatal", TenantID: "tnt_dev"},
		callbacks: callbacks,
		client:    client,
		done:      make(chan struct{}),
		stats:     &execStreamState{},
	}
	r.monitor = newProgressMonitor(r.execRow.ID, stallWindows{noProgress: time.Hour, noFileDiff: time.Hour})

	r.onStall("stalled:no_progress")
	r.mu.Lock()
	fin, ok, errMsg := r.finished, r.resultOk, r.resultErr
	r.mu.Unlock()
	if !fin || ok {
		t.Fatalf("no_progress must kill immediately (finished=%v ok=%v)", fin, ok)
	}
	if errMsg != "stalled:no_progress" {
		t.Fatalf("reason = %q, want stalled:no_progress", errMsg)
	}
}

// TestToolHangWatchdogFiresAndLoopCompletes is the D6 mock-provider test:
// a tool call goes silent for longer than the hang window. The watchdog
// must (1) latch once, (2) fire the advisory OnStall with the
// stalled:tool_hang: reason, (3) inject the course-correcting redirect as
// the next user turn (SendMessage lands on the mock provider), (4) record
// the redirect in the durable transcript, and (5) NOT re-fire on a second
// hang (latch is once per session). The run then completes normally.
func TestToolHangWatchdogFiresAndLoopCompletes(t *testing.T) {
	var (
		mu         sync.Mutex
		sentBodies []string
		hangSends  int
		stalls     []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		case strings.HasSuffix(r.URL.Path, "/prompt_async"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			sentBodies = append(sentBodies, string(body))
			if strings.Contains(string(body), "tool-hang") {
				hangSends++
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	callbacks := &stallRecordingCallbacks{stalls: &stalls, mu: &mu}
	var storedMu sync.Mutex
	var storedParts []db.SessionPart
	a := &Adapter{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionStore: func(_ context.Context, _, _ string, parts []db.SessionPart) error {
			storedMu.Lock()
			defer storedMu.Unlock()
			storedParts = append(storedParts, parts...)
			return nil
		},
	}
	r := &sessionRun{
		a:                 a,
		parentCtx:         context.Background(),
		execRow:           db.ExecutionRow{ID: "exec-toolhang", TenantID: "tnt_dev"},
		callbacks:         callbacks,
		client:            NewSessionClient(srv.URL, "", ""),
		sessionID:         "ses-toolhang",
		done:              make(chan struct{}),
		stats:             &execStreamState{},
		store:             a.sessionStore,
		toolHangWindowVal: 30 * time.Millisecond,
	}
	// A short manual probe timeout would otherwise fire in the background;
	// the monitor is not started here so no probe goroutines run.

	// The tool call starts, then goes silent past the window.
	r.observeToolStart("bash")
	time.Sleep(40 * time.Millisecond)
	r.checkToolHang()

	// (1) Latched once.
	if !r.hangLatched {
		t.Fatal("watchdog did not latch after the hang window elapsed")
	}
	// (2) Advisory OnStall fired with the stalled:tool_hang: reason.
	mu.Lock()
	stallList := append([]string(nil), stalls...)
	mu.Unlock()
	if len(stallList) != 1 || stallList[0] != "stalled:tool_hang:bash" {
		t.Fatalf("OnStall reasons = %v, want exactly [stalled:tool_hang:bash]", stallList)
	}
	// (3) The redirect was sent to the mock provider exactly once.
	mu.Lock()
	hangN := hangSends
	bodies := append([]string(nil), sentBodies...)
	mu.Unlock()
	if hangN != 1 {
		t.Fatalf("hang redirect sends = %d, want exactly 1", hangN)
	}
	if len(bodies) == 0 || !strings.Contains(bodies[len(bodies)-1], "tool exceeded tool-hang window") {
		t.Fatalf("redirect message not sent; bodies=%v", bodies)
	}
	// (4) The redirect is in the durable transcript (source tool_hang_redirect).
	storedMu.Lock()
	parts := append([]db.SessionPart(nil), storedParts...)
	storedMu.Unlock()
	found := false
	for _, p := range parts {
		if p.Kind == db.SessionPartUserMessage && strings.Contains(string(p.Payload), "tool_hang_redirect") {
			found = true
		}
	}
	r.flushParts()
	storedMu.Lock()
	parts = append([]db.SessionPart(nil), storedParts...)
	storedMu.Unlock()
	found = false
	for _, p := range parts {
		if p.Kind == db.SessionPartUserMessage && strings.Contains(string(p.Payload), "tool_hang_redirect") {
			found = true
		}
	}
	if !found {
		t.Fatalf("redirect not recorded in transcript; parts=%+v", parts)
	}
	// (5) A second hang does NOT re-fire (latch is once per session).
	r.observeToolStart("bash")
	time.Sleep(40 * time.Millisecond)
	r.checkToolHang()
	mu.Lock()
	hangN = hangSends
	mu.Unlock()
	if hangN != 1 {
		t.Fatalf("second hang re-fired the watchdog (hangSends=%d), want 1 (latched)", hangN)
	}

	// The loop continues and completes: the tool resolves and the run ends
	// successfully (the watchdog never poisoned the run).
	r.observeToolEnd()
	r.handleEvent(BusEvent{Type: "session.idle", Properties: map[string]any{"sessionID": "ses-toolhang"}})
	// The run's finish path is exercised by the run loop; here we verify the
	// watchdog left the run in a state where completion is still reachable:
	// finished must be false (no fatal kill from the watchdog) and the
	// latch is the only side effect.
	if r.isFinished() {
		t.Fatal("watchdog must not kill the session; finished should remain false")
	}
}
