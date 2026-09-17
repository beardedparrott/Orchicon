package execution

// live_update_test.go — DO EXECUTIONS AUTO-UPDATE LIVE?
//
// The operator, before running their first true end-to-end test: "I think we should probably also verify
// that executions auto update LIVE. I would rather verify that and fix it now BEFORE running my true live
// test."
//
// The chain has four hops and each has broken in this codebase before, so each is asserted here rather
// than assumed:
//
//	stream event → subs.EventPokeMsg → the screen's handler → the shell's RefreshExecutionSession
//	             → merged with the durable parts → the screen's RenderSession → the pane's transcript
//
// These cover the SCREEN's three hops. The App-side hop between the poke and the render is guarded on the
// open execution's id and is exercised in the tui package, where the shell lives.
//
// The hops here are the ones worth pinning: they are silent when they fail. A guard that compares the
// open detail's id answers "no" for the wrong id, the handler returns without complaint, and the pane
// simply stops changing — which reads as a hung execution, not as a bug.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// liveSpy records the live-repaint requests the screen makes.
type liveSpy struct {
	calls int
	gotID string
	gotN  int
}

func (l *liveSpy) RefreshExecutionSession(execID string, events []*apiv1.StreamExecutionEventsResponse) {
	l.calls++
	l.gotID = execID
	l.gotN = len(events)
}

// telemetryEvent is one live chunk carrying text, shaped as the server sends it.
func telemetryEvent(id, text string) *apiv1.StreamExecutionEventsResponse {
	return &apiv1.StreamExecutionEventsResponse{
		Sequence: 1,
		Event: &apiv1.ExecutionEvent{
			EventId:   id,
			EventType: apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY,
			Payload:   []byte(`{"text":"` + text + `"}`),
		},
	}
}

// HOP 1+2: a live event poke reaches the shell for the execution that is OPEN.
func TestExecutionEventPokeAsksTheShellToRefreshTheOpenExecution(t *testing.T) {
	m := newModel(t, &fakePlane{})
	spy := &liveSpy{}
	m.SetShell(spy)
	if !m.Base.SelectSource(srcExecutions) {
		t.Fatal("fixture: could not focus the Executions pane")
	}
	m.Base.SetDetailID("exec-1")

	// What the stream's re-armable poke delivers.
	m.Update(subs.EventPokeMsg{Name: "execution-events"})

	if spy.calls != 1 {
		t.Fatalf("a live execution event made %d refresh requests, want 1 — the pane does not "+
			"auto-update, so a running execution looks frozen", spy.calls)
	}
	if spy.gotID != "exec-1" {
		t.Errorf("refreshed %q, want the OPEN execution exec-1", spy.gotID)
	}
}

// HOP 3+4: the items reach the pane's TRANSCRIPT CACHE, which is what the pane draws as collapsible
// blocks. That cache is what makes the pane GROW as the run talks.
func TestExecutionRenderSessionRecordsTheLiveTranscript(t *testing.T) {
	m := newModel(t, &fakePlane{})
	m.SetShell(&liveSpy{})
	if !m.Base.SelectSource(srcExecutions) {
		t.Fatal("fixture: could not focus the Executions pane")
	}
	m.Base.SetDetailID("exec-1")

	m.RenderSession([]chat.ChatItem{{Kind: chat.KindText, Text: "live chunk arrived", Live: true}})

	var texts []string
	for _, it := range m.execDetail.blockItems("exec-1") {
		texts = append(texts, it.Text)
	}
	found := false
	for _, tx := range texts {
		if tx == "live chunk arrived" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the live chunk is not in the pane's transcript (got %q) — a running execution would "+
			"never show its output", texts)
	}
}

// And a transcript is recorded PER EXECUTION, so one run's live chunk can never appear in another's pane.
func TestExecutionTranscriptsDoNotBleedBetweenRuns(t *testing.T) {
	m := newModel(t, &fakePlane{})
	m.SetShell(&liveSpy{})
	if !m.Base.SelectSource(srcExecutions) {
		t.Fatal("fixture: could not focus the Executions pane")
	}
	m.Base.SetDetailID("exec-1")
	m.RenderSession([]chat.ChatItem{{Kind: chat.KindText, Text: "mine"}})

	m.Base.SetDetailID("exec-2")
	m.RenderSession([]chat.ChatItem{{Kind: chat.KindText, Text: "belongs to the second run"}})

	for _, it := range m.execDetail.blockItems("exec-1") {
		if it.Text == "belongs to the second run" {
			t.Fatal("another execution's event was recorded into the first run's transcript")
		}
	}
	var second []string
	for _, it := range m.execDetail.blockItems("exec-2") {
		second = append(second, it.Text)
	}
	if len(second) == 0 || second[0] != "belongs to the second run" {
		t.Fatalf("the second execution's transcript = %q, want its own chunk", second)
	}
}

// And the CONVERSION itself, held here rather than assumed: a live TELEMETRY event becomes visible text
// (not only tool rows), because a reply-only run would otherwise show nothing at all.
func TestExecutionLiveItemsCarryText(t *testing.T) {
	items := chat.LiveItems([]*apiv1.StreamExecutionEventsResponse{telemetryEvent("ev-1", "hello from the run")})
	if len(items) == 0 {
		t.Fatal("a TELEMETRY event produced no items at all")
	}
	if items[0].Kind != chat.KindText {
		t.Errorf("live item kind = %q, want %q", items[0].Kind, chat.KindText)
	}
	if items[0].Text != "hello from the run" {
		t.Errorf("live item text = %q", items[0].Text)
	}
}
