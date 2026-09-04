package orchicon

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// bridge_parts_test.go: the sessionPartsRecorder — the DB transcript
// mirroring the session pane consumes. Pins: every JSONL transcript event
// class converts into the opencode part shape the frontend's
// transcriptItems parses (user_message / text / reasoning / tool_use with
// callID+state / step boundaries / error / session_info), batching
// flushes through the SessionStoreFunc, and the recorder is best-effort
// (store errors never panic or block).
//
// Seq contract: the DB store's unique key is (tenant_id, execution_id,
// seq) with ON CONFLICT DO NOTHING, so parts derive DISTINCT seqs
// (seq<<8 | subIndex) — a same-seq collision would silently DROP parts
// (observed bug: step boundaries and tool inputs vanished).
//
// Deadlock contract: Close must never wait on the flush pump when start()
// was never called (the observed 10-minute test hang: defer rec.Close()
// blocked on <-r.stopped with no pump running).

type recordingStore struct {
	batches [][]db.SessionPart
}

func (r *recordingStore) record(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error {
	r.batches = append(r.batches, parts)
	return nil
}

func partsOf(r *recordingStore) []db.SessionPart {
	var out []db.SessionPart
	for _, b := range r.batches {
		out = append(out, b...)
	}
	return out
}

func TestSessionPartsRecorderShapes(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_parts", "tnt_test")

	// Drive the observer exactly as the transcript does (seq, typ, data).
	id := Identity{ExecutionID: "exec_parts", TenantID: "tnt_test", WorkerID: "w1", ModelRef: "orchicon/deepseek/deepseek-v4-flash", Goal: "do the thing"}
	idJSON, _ := json.Marshal(map[string]any{"identity": id})
	rec.observe(1, TransSession, idJSON)
	rec.observe(2, TransUserMessage, []byte(`{"text":"fix the bug","source":"goal"}`))
	rec.observe(3, TransReasoning, []byte(`{"text":"let me think…"}`))
	rec.observe(4, TransText, []byte(`{"text":"I will write the file."}`))
	tc, _ := json.Marshal(map[string]any{
		"text":       "",
		"tool_calls": []ToolCall{{Index: 0, ToolCallID: "tc1", Name: "write", ArgsJSON: `{"filePath":"a.md","content":"x"}`}},
	})
	rec.observe(5, TransToolCall, tc)
	// The loop marshals ToolCall WITHOUT json tags (Go field names) —
	// drive the result event in that exact durable shape.
	rec.observe(6, TransToolResult, []byte(`{"tool_call":{"Index":0,"ToolCallID":"tc1","Name":"write","ArgsJSON":"{}"},"output":"ok"}`))
	rec.observe(7, TransFinish, []byte(`{"stop_reason":"stop"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	if len(parts) == 0 {
		t.Fatal("no parts flushed")
	}

	byKind := map[string][]map[string]any{}
	var kinds []string
	for _, p := range parts {
		var pl map[string]any
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			t.Fatalf("part %d payload invalid: %v", p.Seq, err)
		}
		byKind[p.Kind] = append(byKind[p.Kind], pl)
		kinds = append(kinds, p.Kind)
	}
	// session_info present (pane header).
	if len(byKind[db.SessionPartSessionInfo]) != 1 {
		t.Fatalf("session_info parts = %d, want 1 (kinds: %v)", len(byKind[db.SessionPartSessionInfo]), kinds)
	}
	// user goal with source.
	if len(byKind[db.SessionPartUserMessage]) != 1 {
		t.Fatalf("user_message parts = %d, want 1", len(byKind[db.SessionPartUserMessage]))
	}
	um := byKind[db.SessionPartUserMessage][0]
	if um["text"] != "fix the bug" || um["source"] != "goal" {
		t.Errorf("user_message = %v, want text/source parity", um)
	}
	// text + reasoning in part-wrapped shape.
	if len(byKind[db.SessionPartText]) != 1 {
		t.Fatalf("text parts = %d, want 1", len(byKind[db.SessionPartText]))
	}
	if inner, _ := byKind[db.SessionPartText][0]["part"].(map[string]any); inner["text"] != "I will write the file." {
		t.Errorf("text part shape: %v", byKind[db.SessionPartText][0])
	}
	if len(byKind[db.SessionPartReasoning]) != 1 {
		t.Fatalf("reasoning parts = %d, want 1", len(byKind[db.SessionPartReasoning]))
	}
	// tool_use: exactly ONE completed part for tc1 carrying BOTH
	// state.input and state.output (opencode parity; duplicate
	// invocation/output parts would render two bubbles).
	tu := byKind[db.SessionPartToolUse]
	if len(tu) != 1 {
		t.Fatalf("tool_use parts = %d, want 1 (one completed part per call; kinds: %v)", len(tu), kinds)
	}
	part := tu[0]["part"].(map[string]any)
	if part["tool"] != "write" || part["callID"] != "tc1" {
		t.Errorf("tool_use = %v, want write/tc1", part)
	}
	state, _ := part["state"].(map[string]any)
	if state == nil {
		t.Fatalf("tool_use missing state: %v", part)
	}
	if state["status"] != "completed" {
		t.Errorf("state.status = %v, want completed", state["status"])
	}
	in, ok := state["input"].(map[string]any)
	if !ok || in["filePath"] != "a.md" {
		t.Errorf("state.input = %v, want the parsed write args", state["input"])
	}
	if state["output"] != "ok" {
		t.Errorf("state.output = %v, want the tool result output", state["output"])
	}
	// Step boundaries: tool round closes+opens, finish closes.
	if len(byKind[db.SessionPartStepStart]) != 1 || len(byKind[db.SessionPartStepFinish]) < 2 {
		t.Errorf("step boundaries: start=%d finish=%d (kinds: %v)", len(byKind[db.SessionPartStepStart]), len(byKind[db.SessionPartStepFinish]), kinds)
	}
	// Seqs strictly increasing across ALL parts (the pane's ordering —
	// the same-seq collision bug dropped parts silently).
	last := int64(0)
	for _, p := range parts {
		if p.Seq <= last {
			t.Errorf("seq not increasing: %d after %d", p.Seq, last)
		}
		last = p.Seq
	}
}

func TestSessionPartsRecorderOrphanResultAndHold(t *testing.T) {
	// A result with no matching held call flushes standalone (never lost);
	// a call with no result is drained at Close with its input preserved.
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_orphan", "tnt_test")
	defer rec.Close() // pump never started — must NOT deadlock

	tc, _ := json.Marshal(map[string]any{
		"text":       "",
		"tool_calls": []ToolCall{
			{Index: 0, ToolCallID: "hold1", Name: "bash", ArgsJSON: `{"command":"ls"}`},
		},
	})
	rec.observe(1, TransToolCall, tc)
	// An unrelated orphan result at a later seq (tagged shape — exercises
	// the tolerant decoder).
	rec.observe(9, TransToolResult, []byte(`{"tool_call":{"index":0,"tool_call_id":"orphan9","name":"grep","args_json":"{}"},"output":"found"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	byID := map[string]map[string]any{}
	for _, p := range parts {
		var pl map[string]any
		_ = json.Unmarshal(p.Payload, &pl)
		if p.Kind != db.SessionPartToolUse {
			continue
		}
		part, _ := pl["part"].(map[string]any)
		byID[part["callID"].(string)] = pl
	}
	if len(byID) != 2 {
		t.Fatalf("tool_use parts = %d (%v), want 2 (held + orphan)", len(byID), parts)
	}
	// The held call (never completed) drains with input + empty output.
	held := byID["hold1"]["part"].(map[string]any)
	if held["tool"] != "bash" {
		t.Errorf("held part tool = %v, want bash", held["tool"])
	}
	st, _ := held["state"].(map[string]any)
	if st == nil || st["status"] != "completed" || st["output"] != "" {
		t.Errorf("held call state = %v, want completed/empty output", st)
	}
	in, ok := st["input"].(map[string]any)
	if !ok || in["command"] != "ls" {
		t.Errorf("held call input = %v, want the bash args", st["input"])
	}
	// The orphan result carries its output.
	orphan := byID["orphan9"]["part"].(map[string]any)
	ost, _ := orphan["state"].(map[string]any)
	if ost == nil || ost["output"] != "found" {
		t.Errorf("orphan state = %v, want output found", ost)
	}
	// Distinct seqs across all flushed parts (no collision). Arrival order
	// within a batch is not seq-ordered (the DB and pane order by seq), so
	// sort first.
	seqs := make([]int64, 0, len(parts))
	for _, p := range parts {
		seqs = append(seqs, p.Seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("seq collision: %d appears twice (or out of order)", seqs[i])
		}
	}
}

func TestSessionPartsRecorderBestEffort(t *testing.T) {
	// A failing store must never panic and never block the loop.
	rec := newSessionPartsRecorder(func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error {
		return context.DeadlineExceeded
	}, "exec_fail", "tnt_test")
	defer rec.Close() // no pump started — must return promptly
	rec.observe(1, TransText, []byte(`{"text":"hi"}`))
	rec.flush() // must not panic
}

func TestSessionPartsRecorderPumpLifecycle(t *testing.T) {
	// start() + Close() converge (the production path): the pump's final
	// flush lands and Close returns promptly.
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_pump", "tnt_test")
	rec.start()
	rec.observe(1, TransSession, []byte(`{"identity":{"execution_id":"exec_pump"}}`))
	deadline := time.Now().Add(3 * time.Second)
	for len(store.batches) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(store.batches) == 0 {
		t.Error("pump never flushed")
	}
	done := make(chan struct{})
	go func() {
		rec.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return with a running pump")
	}
}

func TestSessionSetTranscriptObserverBeforeRun(t *testing.T) {
	// The bridge attaches the observer before Run (transcript nil) —
	// OpenTranscript must apply it before the FIRST event appends.
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_obs", "tnt_test")
	defer rec.Close()

	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "hello"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	s := qaSession(t, prov, nil)
	s.SetTranscriptObserver(rec.observe)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	foundSessionInfo, foundText := false, false
	for _, p := range parts {
		switch p.Kind {
		case db.SessionPartSessionInfo:
			foundSessionInfo = true
		case db.SessionPartText:
			var pl map[string]any
			_ = json.Unmarshal(p.Payload, &pl)
			if inner, _ := pl["part"].(map[string]any); inner["text"] == "hello" {
				foundText = true
			}
		}
	}
	if !foundSessionInfo {
		t.Error("session_info part missing — observer attached late (first events lost)")
	}
	if !foundText {
		t.Error("text part missing from DB mirror")
	}
}