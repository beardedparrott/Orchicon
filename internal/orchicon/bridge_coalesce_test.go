package orchicon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// bridge_coalesce_test.go covers sessionPartsRecorder's streaming coalescing.
//
// The engine loop appends TransText and TransReasoning per token/delta
// (loop.go), so mapping each event straight to its own part wrote tens of
// thousands of micro-rows for one long turn. Measured on the live plane before
// this fix: 30,146 reasoning rows and 1,073 text rows (avg payload 37 bytes)
// for a single succeeded execution — 31,365 parts in total. The session pane
// fetches with a bounded window (default limit 1000, ORDER BY seq ASC), so the
// worker's ORCHICON WORKER SUMMARY + FACTS LEARNED tail was structurally
// unreachable: no text part in any of the sampled executions carried it.
//
// Reuses the recordingStore/partsOf drivers declared in bridge_parts_test.go.

// paneFetchWindow mirrors db.GetExecutionSession's default part limit: the
// number of parts the session pane renders for a run.
const paneFetchWindow = 1000

// streamDelta builds a TransText/TransReasoning payload (both carry "text").
func streamDelta(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"text": s})
	if err != nil {
		t.Fatalf("marshal delta payload: %v", err)
	}
	return b
}

func kindParts(parts []db.SessionPart, kind string) []db.SessionPart {
	var out []db.SessionPart
	for _, p := range parts {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

func partText(t *testing.T, p db.SessionPart) string {
	t.Helper()
	var pl map[string]any
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		t.Fatalf("part payload invalid: %v", err)
	}
	inner, _ := pl["part"].(map[string]any)
	s, _ := inner["text"].(string)
	return s
}

// TestSessionPartsRecorderCoalescesATextRun is the core regression: N
// consecutive text deltas become ONE part carrying the concatenation, anchored
// at the run's first event seq.
func TestSessionPartsRecorderCoalescesATextRun(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_text_run", "tnt_test")

	frags := []string{"I", "'ll", " start", " by", " reading", " the", " file", ".",
		"\n\nORCHICON WORKER SUMMARY: success — did the thing.\n",
		"FACTS LEARNED: the loop streams text per token.\n"}
	var want strings.Builder
	for i, f := range frags {
		want.WriteString(f)
		rec.observe(int64(10+i), TransText, streamDelta(t, f))
	}
	rec.observe(90, TransUserMessage, []byte(`{"text":"next","source":"follow_up"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)
	texts := kindParts(parts, db.SessionPartText)
	if len(texts) != 1 {
		t.Fatalf("text parts = %d, want 1 coalesced part for %d deltas", len(texts), len(frags))
	}
	if got := partText(t, texts[0]); got != want.String() {
		t.Errorf("coalesced text dropped content:\n got %q\nwant %q", got, want.String())
	}
	if texts[0].Seq != int64(10)<<8 {
		t.Errorf("coalesced seq = %d, want %d (run's first event seq<<8)", texts[0].Seq, int64(10)<<8)
	}
	got := partText(t, texts[0])
	if !strings.Contains(got, "ORCHICON WORKER SUMMARY:") || !strings.Contains(got, "FACTS LEARNED:") {
		t.Errorf("coalesced text is missing the summary/facts tail: %q", got)
	}
}

// TestSessionPartsRecorderCoalescesAReasoningRun pins the dominant class: the
// live plane's 30k-65k reasoning rows per execution were the bulk of the
// transcript that buried the report.
func TestSessionPartsRecorderCoalescesAReasoningRun(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_reason_run", "tnt_test")

	frags := []string{"Let", " me", " check", " the", " reconciler", " loop", "."}
	var want strings.Builder
	for i, f := range frags {
		want.WriteString(f)
		rec.observe(int64(20+i), TransReasoning, streamDelta(t, f))
	}
	rec.observe(80, TransUserMessage, []byte(`{"text":"next","source":"follow_up"}`))

	rec.drainOpen()
	rec.flush()
	reasons := kindParts(partsOf(store), db.SessionPartReasoning)
	if len(reasons) != 1 {
		t.Fatalf("reasoning parts = %d, want 1 coalesced part for %d deltas", len(reasons), len(frags))
	}
	if got := partText(t, reasons[0]); got != want.String() {
		t.Errorf("coalesced reasoning dropped content:\n got %q\nwant %q", got, want.String())
	}
	if reasons[0].Seq != int64(20)<<8 {
		t.Errorf("coalesced reasoning seq = %d, want %d", reasons[0].Seq, int64(20)<<8)
	}
}

// TestSessionPartsRecorderSealsRunsInTranscriptOrder covers the boundaries: a
// run is sealed by any event that is not its own kind (here a tool call and a
// follow-up message), so runs never span a transcript boundary and the parts
// stay in transcript order by seq.
func TestSessionPartsRecorderSealsRunsInTranscriptOrder(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_order", "tnt_test")

	rec.observe(1, TransText, streamDelta(t, "one "))
	rec.observe(2, TransText, streamDelta(t, "two"))
	tc, _ := json.Marshal(map[string]any{
		"text":       "",
		"tool_calls": []ToolCall{{Index: 0, ToolCallID: "tc1", Name: "bash", ArgsJSON: `{"command":"ls"}`}},
	})
	rec.observe(3, TransToolCall, tc)
	rec.observe(4, TransToolResult, []byte(`{"tool_call":{"Index":0,"ToolCallID":"tc1","Name":"bash","ArgsJSON":"{}"},"output":"done"}`))
	rec.observe(5, TransReasoning, streamDelta(t, "think "))
	rec.observe(6, TransReasoning, streamDelta(t, "more"))
	rec.observe(7, TransText, streamDelta(t, "three"))
	rec.observe(8, TransUserMessage, []byte(`{"text":"next","source":"follow_up"}`))

	rec.drainOpen()
	rec.flush()
	parts := partsOf(store)

	texts := kindParts(parts, db.SessionPartText)
	if len(texts) != 2 {
		t.Fatalf("text parts = %d, want 2 (one run before the tool call, one after)", len(texts))
	}
	if got := partText(t, texts[0]); got != "one two" {
		t.Errorf("first text run = %q, want %q", got, "one two")
	}
	if got := partText(t, texts[1]); got != "three" {
		t.Errorf("second text run = %q, want %q", got, "three")
	}
	reasons := kindParts(parts, db.SessionPartReasoning)
	if len(reasons) != 1 {
		t.Fatalf("reasoning parts = %d, want 1", len(reasons))
	}
	if got := partText(t, reasons[0]); got != "think more" {
		t.Errorf("reasoning run = %q, want %q", got, "think more")
	}
	tools := kindParts(parts, db.SessionPartToolUse)
	if len(tools) != 1 {
		t.Fatalf("tool_use parts = %d, want 1 (one completed part per call)", len(tools))
	}
	var toolState map[string]any
	_ = json.Unmarshal(tools[0].Payload, &toolState)
	if inner, _ := toolState["part"].(map[string]any); inner != nil {
		if st, _ := inner["state"].(map[string]any); st == nil || st["output"] != "done" {
			t.Errorf("tool_use state = %v, want output done", st)
		}
	}

	// Ordering invariant the pane depends on: strictly increasing seq.
	if !(texts[0].Seq < tools[0].Seq && tools[0].Seq < reasons[0].Seq && reasons[0].Seq < texts[1].Seq) {
		t.Errorf("seq order broken: text1=%d tool=%d reason=%d text2=%d",
			texts[0].Seq, tools[0].Seq, reasons[0].Seq, texts[1].Seq)
	}
	if texts[0].Seq != int64(1)<<8 {
		t.Errorf("first text run seq = %d, want %d", texts[0].Seq, int64(1)<<8)
	}
	if reasons[0].Seq != int64(5)<<8 {
		t.Errorf("reasoning run seq = %d, want %d (its own first event)", reasons[0].Seq, int64(5)<<8)
	}
}

// TestSessionPartsRecorderCoalesceChunkBounded proves a pathological run stays
// bounded instead of accumulating one unbounded part.
func TestSessionPartsRecorderCoalesceChunkBounded(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_chunk", "tnt_test")

	frag := strings.Repeat("y", 1024)
	const n = 40 // 40 KiB of text
	for i := 0; i < n; i++ {
		rec.observe(int64(1+i), TransText, streamDelta(t, frag))
	}
	rec.observe(500, TransUserMessage, []byte(`{"text":"next","source":"follow_up"}`))

	rec.drainOpen()
	rec.flush()
	texts := kindParts(partsOf(store), db.SessionPartText)
	if len(texts) != 2 {
		t.Fatalf("text parts = %d, want 2 bounded chunks for %dKiB", len(texts), n)
	}
	var got strings.Builder
	for _, p := range texts {
		got.WriteString(partText(t, p))
	}
	if got.String() != strings.Repeat(frag, n) {
		t.Errorf("chunked text lost content (got %d bytes, want %d)", got.Len(), len(frag)*n)
	}
	if texts[1].Seq <= texts[0].Seq {
		t.Errorf("chunk seqs not increasing: %d then %d", texts[0].Seq, texts[1].Seq)
	}
}

// TestSessionPartsRecorderCoalesceHonoursBaseSeq guards the follow-up path: a
// coalesced part must carry the baseSeq offset, or the store's unique key
// (tenant, execution, seq) collides with the original run and ON CONFLICT DO
// NOTHING silently drops it.
func TestSessionPartsRecorderCoalesceHonoursBaseSeq(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_baseseq", "tnt_test")
	rec.baseSeq = 100 // original run's max event seq (follow-up offset)

	rec.observe(10, TransText, streamDelta(t, "hello "))
	rec.observe(11, TransText, streamDelta(t, "again"))
	rec.drainOpen()
	rec.flush()

	texts := kindParts(partsOf(store), db.SessionPartText)
	if len(texts) != 1 {
		t.Fatalf("text parts = %d, want 1", len(texts))
	}
	want := (int64(10) + int64(100)) << 8
	if texts[0].Seq != want {
		t.Errorf("coalesced seq = %d, want %d (run seq off by baseSeq)", texts[0].Seq, want)
	}
}

// TestSessionPartsRecorderLongTurnStaysWithinPaneWindow is the operator-visible
// acceptance criterion, on the shape the live plane actually produced: tens of
// thousands of per-delta reasoning/text events in ONE turn. Before coalescing
// this run wrote >31,000 parts, so the worker's report sat far past the pane's
// 1000-part window; after it, the report is inside the window.
func TestSessionPartsRecorderLongTurnStaysWithinPaneWindow(t *testing.T) {
	store := &recordingStore{}
	rec := newSessionPartsRecorder(store.record, "exec_long_turn", "tnt_test")

	// ~1.1 MiB of reasoning across 30k deltas (the measured dominant class),
	// then ~1000 one-byte text deltas ending in the worker's report.
	reasoningDelta := strings.Repeat("r", 37)
	const reasoningDeltas = 30000
	seq := int64(1)
	for i := 0; i < reasoningDeltas; i++ {
		rec.observe(seq, TransReasoning, streamDelta(t, reasoningDelta))
		seq++
	}
	const textDeltas = 1000
	for i := 0; i < textDeltas; i++ {
		rec.observe(seq, TransText, streamDelta(t, "t"))
		seq++
	}
	report := "\n\nORCHICON WORKER SUMMARY: success — coalesced.\n" +
		"FACTS LEARNED: one part per contiguous run.\n"
	rec.observe(seq, TransText, streamDelta(t, report))
	rec.drainOpen()
	rec.flush()

	parts := partsOf(store)
	if len(parts) >= paneFetchWindow {
		t.Fatalf("parts = %d, want < %d so the tail stays inside the pane window", len(parts), paneFetchWindow)
	}
	// The report must be reachable within the window the pane fetches.
	summaryRank := -1
	for i, p := range parts {
		if p.Kind == db.SessionPartText && strings.Contains(partText(t, p), "ORCHICON WORKER SUMMARY:") {
			summaryRank = i + 1
			break
		}
	}
	if summaryRank == -1 {
		t.Fatal("no part carries the worker summary — it was fragmented or dropped")
	}
	if summaryRank > paneFetchWindow {
		t.Errorf("summary sits at part %d, past the pane window %d", summaryRank, paneFetchWindow)
	}
	if got := len(kindParts(parts, db.SessionPartReasoning)); got > 40 {
		t.Errorf("reasoning parts = %d, want <= 40 for ~1.1MiB chunked at 32KiB", got)
	}
	if got := len(kindParts(parts, db.SessionPartText)); got > 3 {
		t.Errorf("text parts = %d, want <= 3 for %d deltas", got, textDeltas+1)
	}
}
