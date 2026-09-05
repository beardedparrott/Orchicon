package orchicon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// bridge_coalesce_test.go: the sessionPartsRecorder coalesces consecutive
// TransText events (emitted per token / tiny fragment by the session loop)
// into ONE growing SessionPartText per contiguous run instead of one
// micro-part per token -- so the ORCHICON WORKER SUMMARY + FACTS LEARNED
// tail stays inside the session pane fetch window. Reuses the
// recordingStore/partsOf drivers from bridge_parts_test.go.

func textPayload(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"text": s})
	if err != nil {
		t.Fatalf("marshal text payload: %v", err)
	}
	return b
}

func coalescedTexts(parts []db.SessionPart) []db.SessionPart {
	var out []db.SessionPart
	for _, p := range parts {
		if p.Kind == db.SessionPartText {
			out = append(out, p)
		}
	}
	return out
}

func textContent(t *testing.T, p db.SessionPart) string {
	t.Helper()
	var pl map[string]any
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		t.Fatalf("text part payload invalid: %v", err)
	}
	inner, _ := pl["part"].(map[string]any)
	s, _ := inner["text"].(string)
	return s
}

func TestSessionPartsRecorderCoalescesConsecutiveText(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_coalesce", "tnt_test")
	defer rec.Close()

	frags := []string{"I", "'ll", " start", " by", " reading", " the", " file", ".",
		"\n\nORCHICON WORKER SUMMARY: success -- did the thing.\n",
		"FACTS LEARNED: tokens stream per event.\n"}
	var want strings.Builder
	for i, f := range frags {
		want.WriteString(f)
		rec.observe(int64(10+i), TransText, textPayload(t, f))
	}
	rec.observe(int64(10+len(frags)), TransFinish, []byte(`{"stop_reason":"stop"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	texts := coalescedTexts(parts)
	if len(texts) != 1 {
		t.Fatalf("text parts = %d, want 1 coalesced part for %d TransText events", len(texts), len(frags))
	}
	if got := textContent(t, texts[0]); got != want.String() {
		t.Errorf("coalesced text dropped content:\n got %q\nwant %q", got, want.String())
	}
	if texts[0].Seq != int64(10)<<8 {
		t.Errorf("coalesced seq = %d, want %d (first event seq<<8)", texts[0].Seq, int64(10)<<8)
	}
	got := textContent(t, texts[0])
	if !strings.Contains(got, "ORCHICON WORKER SUMMARY:") || !strings.Contains(got, "FACTS LEARNED:") {
		t.Errorf("coalesced text missing summary/facts tail: %q", got)
	}
}

func TestSessionPartsRecorderTextFlushesOnNonText(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_flush", "tnt_test")
	defer rec.Close()

	rec.observe(1, TransText, textPayload(t, "hello "))
	rec.observe(2, TransText, textPayload(t, "world"))
	tc, _ := json.Marshal(map[string]any{
		"text":       "",
		"tool_calls": []ToolCall{{Index: 0, ToolCallID: "tc1", Name: "bash", ArgsJSON: `{"command":"ls"}`}},
	})
	rec.observe(3, TransToolCall, tc)
	rec.observe(4, TransToolResult, []byte(`{"tool_call":{"Index":0,"ToolCallID":"tc1","Name":"bash","ArgsJSON":"{}"},"output":"done"}`))
	rec.observe(5, TransFinish, []byte(`{"stop_reason":"stop"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	texts := coalescedTexts(parts)
	if len(texts) != 1 {
		t.Fatalf("text parts = %d, want 1 (run sealed at tool_call)", len(texts))
	}
	if got := textContent(t, texts[0]); got != "hello world" {
		t.Errorf("text = %q, want %q", got, "hello world")
	}
	var toolSeq int64
	found := false
	for _, p := range parts {
		if p.Kind != db.SessionPartToolUse {
			continue
		}
		found = true
		toolSeq = p.Seq
		var pl map[string]any
		_ = json.Unmarshal(p.Payload, &pl)
		part, _ := pl["part"].(map[string]any)
		state, _ := part["state"].(map[string]any)
		if state["output"] != "done" {
			t.Errorf("tool_use output = %v, want done", state["output"])
		}
	}
	if !found {
		t.Fatal("tool_use part missing after text flush")
	}
	if texts[0].Seq >= toolSeq {
		t.Errorf("text seq %d not before tool seq %d", texts[0].Seq, toolSeq)
	}
}

func TestSessionPartsRecorderTextChunkBounded(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_chunk", "tnt_test")
	defer rec.Close()

	frag := strings.Repeat("y", 1024)
	const n = 40
	for i := 0; i < n; i++ {
		rec.observe(int64(1+i), TransText, textPayload(t, frag))
	}
	rec.observe(int64(1+n), TransFinish, []byte(`{"stop_reason":"stop"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	texts := coalescedTexts(parts)
	if len(texts) != 2 {
		t.Fatalf("text parts = %d, want 2 bounded chunks for %dKiB", len(texts), n)
	}
	var got strings.Builder
	for _, p := range texts {
		got.WriteString(textContent(t, p))
	}
	if got.String() != strings.Repeat(frag, n) {
		t.Errorf("chunked text lost content (got %d bytes, want %d)", got.Len(), len(frag)*n)
	}
	if texts[0].Seq == texts[1].Seq {
		t.Errorf("chunk seq collision: %d", texts[0].Seq)
	}
	if texts[1].Seq <= texts[0].Seq {
		t.Errorf("chunk seqs not increasing: %d then %d", texts[0].Seq, texts[1].Seq)
	}
}
