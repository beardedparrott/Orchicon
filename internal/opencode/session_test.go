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
			got, ok := legacyEventFromBus(tc.evt)
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

// TestSessionRunPendingAccounting verifies the per-turn accounting drives
// completion exactly once.
func TestSessionRunPendingAccounting(t *testing.T) {
	r := &sessionRun{done: make(chan struct{}), stats: &execStreamState{}}

	r.bumpPending() // goal
	if r.pendingTurns != 1 {
		t.Fatalf("pending = %d, want 1", r.pendingTurns)
	}
	r.turnCompleted() // goal turn finishes
	select {
	case <-r.done:
		t.Fatal("execution should not finish synchronously (settle window)")
	case <-time.After(50 * time.Millisecond):
	}
	// Settle window is 1s; it should finish by then.
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not finish after settle")
	}
	r.mu.Lock()
	fin, o := r.finished, r.resultOk
	r.mu.Unlock()
	if !fin || !o {
		t.Fatalf("finished=%v ok=%v, want true/true", fin, o)
	}
}

// TestSessionRunAllTurnsDone verifies session.idle force-settles even when
// a step-finish was missed.
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
