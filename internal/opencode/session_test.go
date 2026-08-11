package opencode

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

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
			name: "unrelated event is ignored",
			evt:  BusEvent{Type: "catalog.updated", Properties: map[string]any{}},
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, fail := completionProbeDecision(tc.output, tc.nudges, tc.lastNudge, now)
			if probe != tc.wantProbe || fail != tc.wantFail {
				t.Fatalf("probe=%v fail=%v, want probe=%v fail=%v", probe, fail, tc.wantProbe, tc.wantFail)
			}
		})
	}
}
