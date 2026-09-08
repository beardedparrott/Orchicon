package chat

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func part(seq int64, kind string, payload string) *apiv1.ExecutionSessionPart {
	return &apiv1.ExecutionSessionPart{
		Seq:       seq,
		Kind:      kind,
		Payload:   []byte(payload),
		CreatedAt: timestamppb.New(timeAt(1000 + seq*1000)),
	}
}

func timeAt(ms int64) time.Time { return time.UnixMilli(ms) }

func TestHistoryItemsTextAndReasoningConvergePerStep(t *testing.T) {
	parts := []*apiv1.ExecutionSessionPart{
		part(1, "session_info", `{"session_id":"ses_1","serve_url":"http://x"}`),
		part(2, "step_start", `{}`),
		part(3, "text", `{"part":{"text":"hello "}}`),
		part(4, "text", `{"part":{"text":"world"}}`),
		part(5, "reasoning", `{"part":{"text":"thinking "}}`),
		part(6, "reasoning", `{"part":{"text":"hard"}}`),
		part(7, "step_finish", `{}`),
	}
	items := HistoryItems(parts)
	// session bubble, one text bubble, one reasoning bubble (step-0 phase
	// groups), then step_finish sealed the run.
	var texts, reasons, sessions int
	for _, i := range items {
		switch i.Kind {
		case KindText:
			texts++
			if i.Text != "hello world" {
				t.Fatalf("text = %q", i.Text)
			}
			// TS semantics: step_start increments the counter before the
			// text part is tagged, so the first step is step-1.
			if i.Phase != "step-1" {
				t.Fatalf("text phase = %q", i.Phase)
			}
		case KindReasoning:
			reasons++
			if i.Text != "thinking hard" {
				t.Fatalf("reasoning = %q", i.Text)
			}
		case KindSession:
			sessions++
			if i.SessionID != "ses_1" {
				t.Fatalf("session = %q", i.SessionID)
			}
		}
	}
	if texts != 1 || reasons != 1 || sessions != 1 {
		t.Fatalf("texts=%d reasons=%d sessions=%d", texts, reasons, sessions)
	}
}

func TestHistoryItemsToolUse(t *testing.T) {
	parts := []*apiv1.ExecutionSessionPart{
		part(1, "tool_use", `{"part":{"tool":"bash","state":{"input":"ls -la","output":"total 4"}}}`),
	}
	items := HistoryItems(parts)
	if len(items) != 1 || items[0].Kind != KindTool {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Tool.ToolName != "bash" || items[0].Tool.Input != "ls -la" || items[0].Tool.Output != "total 4" {
		t.Fatalf("tool = %+v", items[0].Tool)
	}
}

func TestHistoryItemsUserAndError(t *testing.T) {
	parts := []*apiv1.ExecutionSessionPart{
		part(1, "user_message", `{"text":"do the thing","source":"chat"}`),
		part(2, "error", `{"error":{"message":"boom"}}`),
	}
	items := HistoryItems(parts)
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
	if items[0].Kind != KindUser || items[0].Text != "do the thing" || items[0].Source != "chat" {
		t.Fatalf("user = %+v", items[0])
	}
	if items[1].Kind != KindError || items[1].Text != "boom" {
		t.Fatalf("error = %+v", items[1])
	}
}

func TestHistoryItemsStepCounterIncrements(t *testing.T) {
	parts := []*apiv1.ExecutionSessionPart{
		part(1, "step_start", `{}`),
		part(2, "text", `{"part":{"text":"one"}}`),
		part(3, "step_finish", `{}`),
		part(4, "step_start", `{}`),
		part(5, "text", `{"part":{"text":"two"}}`),
		part(6, "step_finish", `{}`),
	}
	items := HistoryItems(items2phase(parts))
	phases := map[string]bool{}
	for _, i := range items {
		phases[i.Phase] = true
	}
	if !phases["step-1"] || !phases["step-3"] {
		// TS increments the counter on BOTH step_start and step_finish, so
		// the second step's text lands in step-3.
		t.Fatalf("phases = %v, want step-1 and step-3", keys(phases))
	}
}

func items2phase(parts []*apiv1.ExecutionSessionPart) []*apiv1.ExecutionSessionPart {
	return parts
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestHistoryItemsSkipsEmptyParts(t *testing.T) {
	parts := []*apiv1.ExecutionSessionPart{
		part(1, "text", `{}`),                  // no part.text → skipped
		part(2, "user_message", `{"text":""}`), // empty text → skipped
		part(3, "unknown_kind", `{"x":1}`),     // default → skipped
		nil,                                    // nil part → skipped
	}
	if got := HistoryItems(parts); len(got) != 0 {
		t.Fatalf("items = %d, want 0", len(got))
	}
}

func execEvent(id string, seq int64, typ apiv1.ExecutionEventType, payload string) *apiv1.StreamExecutionEventsResponse {
	return &apiv1.StreamExecutionEventsResponse{
		Sequence: seq,
		Event: &apiv1.ExecutionEvent{
			EventId:    id,
			EventType:  typ,
			OccurredAt: timestamppb.New(timeAt(5000 + seq)),
			Payload:    []byte(payload),
		},
	}
}

func TestLiveItemsTextAndReasoningChunks(t *testing.T) {
	// Reasoning chunks arrive as a JSON string INSIDE the payload's text
	// field (the adapter's emitReasoningChunked, double-wrapped) —
	// detected by parsing that string, exactly like SessionChatPane.tsx.
	events := []*apiv1.StreamExecutionEventsResponse{
		execEvent("e1", 1, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"answer "}`),
		execEvent("e2", 2, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"soon"}`),
		execEvent("e3", 3, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"{\"kind\":\"reasoning\",\"text\":\"hmm \",\"seq\":9}"}`),
		execEvent("e4", 4, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"{\"kind\":\"reasoning\",\"text\":\"why\"}"}`),
	}
	items := LiveItems(events)
	// liveItems returns ungrouped items (mergeSessionItems applies
	// grouping), matching SessionChatPane.tsx.
	if len(items) != 4 {
		t.Fatalf("len = %d, want 4", len(items))
	}
	if items[0].Kind != KindText || items[0].Text != "answer " || !items[0].Live || items[0].Phase != "live-0" {
		t.Fatalf("items[0] = %+v", items[0])
	}
	if items[1].Kind != KindText || items[1].Text != "soon" {
		t.Fatalf("items[1] = %+v", items[1])
	}
	if items[2].Kind != KindReasoning || items[2].Text != "hmm " || !items[2].Live {
		t.Fatalf("items[2] = %+v", items[2])
	}
	if items[3].Kind != KindReasoning || items[3].Text != "why" {
		t.Fatalf("items[3] = %+v", items[3])
	}
}

func TestLiveItemsPhaseBumpsOnToolAndError(t *testing.T) {
	events := []*apiv1.StreamExecutionEventsResponse{
		execEvent("e1", 1, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"a"}`),
		execEvent("e2", 2, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TOOL_CALL, `{"tool_name":"bash","input":"ls","output":"x"}`),
		execEvent("e3", 3, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"b"}`),
		execEvent("e4", 4, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_ERROR, `{"text":"kaput"}`),
		execEvent("e5", 5, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"c"}`),
	}
	items := LiveItems(events)
	// e1 (live-0), tool, e3 (live-1), error, e5 (live-2)
	if len(items) != 5 {
		t.Fatalf("len = %d, want 5", len(items))
	}
	if items[0].Phase != "live-0" || items[2].Phase != "live-1" || items[4].Phase != "live-2" {
		t.Fatalf("phases: %q %q %q", items[0].Phase, items[2].Phase, items[4].Phase)
	}
	if items[1].Kind != KindTool || items[1].Tool.ToolName != "bash" {
		t.Fatalf("tool = %+v", items[1])
	}
	if items[3].Kind != KindError || items[3].Text != "kaput" {
		t.Fatalf("error = %+v", items[3])
	}
}

func TestLiveItemsArtifact(t *testing.T) {
	events := []*apiv1.StreamExecutionEventsResponse{
		execEvent("a1", 1, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_ARTIFACT, `{"artifact_name":"main.go","artifact_type":"file","content":"package main"}`),
	}
	items := LiveItems(events)
	if len(items) != 1 || items[0].Kind != KindArtifact {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Name != "main.go" || items[0].Type != "file" || items[0].Content != "package main" {
		t.Fatalf("artifact = %+v", items[0])
	}
}

func TestLiveItemsMalformedPayloadsSafe(t *testing.T) {
	events := []*apiv1.StreamExecutionEventsResponse{
		execEvent("e1", 1, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{invalid json`),
		execEvent("e2", 2, apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY, `{"text":"fine"}`),
		execEvent("bad", 3, apiv1.ExecutionEventType(99), `{}`), // unknown enum → skipped
		{Sequence: 4, Event: nil}, // nil event → skipped
	}
	items := LiveItems(events)
	if len(items) != 1 || items[0].Text != "fine" {
		t.Fatalf("items = %+v", items)
	}
	if !strings.Contains(items[0].Key, "e2") && items[0].Key != "e2" {
		t.Fatalf("key = %q", items[0].Key)
	}
}
