package orchicon

// Tests for ADR-0009 native prompt assembly: composite reuse, the thin
// authoritative native layer, cache-first two-zone ordering, and
// per-session cache metrics.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// promptSession builds a session for prompt-assembly tests.
func promptSession(t *testing.T, prov Provider, manifest scheduler.ExecutionManifest) *Session {
	t.Helper()
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_prompt_test"),
		Manifest:   manifest,
		ProjectDir: t.TempDir(),
		Provider:   prov,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

func promptManifest(sp string) scheduler.ExecutionManifest {
	m := testManifest("orchicon/mockprov/deepseek-v4-flash")
	m.SystemPrompt = sp
	return m
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// AC (byte-identity): two assemblies with identical inputs produce
// byte-identical static prefix bytes — and the SAME prefix across two
// executions (the cache lever only pays if this holds).
func TestNativeStaticPrefixByteIdentity(t *testing.T) {
	manifest := promptManifest("You are the test worker.\nComposite body.")
	s1 := promptSession(t, &mockProvider{}, manifest)
	s2 := promptSession(t, &mockProvider{}, manifest)
	b1 := s1.AssembleSystem()
	b2 := s2.AssembleSystem()
	if len(b1) != 1 || len(b2) != 1 {
		t.Fatalf("fresh sessions must assemble the single cached block, got %d/%d", len(b1), len(b2))
	}
	if b1[0].Text != b2[0].Text {
		t.Fatal("static prefix bytes differ across identical-input assemblies")
	}
	if !b1[0].Cache {
		t.Error("static prefix block must carry the cache breakpoint flag")
	}
	// The composite is consumed verbatim at the head (no re-authoring).
	if !strings.HasPrefix(b1[0].Text, manifest.SystemPrompt+"\n\n") {
		t.Error("static prefix must begin with the composite prompt verbatim")
	}
	if !strings.Contains(b1[0].Text, NativeStaticLayer) {
		t.Error("static prefix must contain the native layer")
	}
	if !strings.Contains(b1[0].Text, "### Environment facts") {
		t.Error("static prefix must contain env facts")
	}
}

// AC (order test): mutable content NEVER precedes the cache breakpoint.
func TestNativeMutableZoneNeverPrecedesBreakpoint(t *testing.T) {
	s := promptSession(t, &mockProvider{}, promptManifest("base composite"))
	// No mutable state → single cached block only.
	if blocks := s.AssembleSystem(); len(blocks) != 1 || !blocks[0].Cache {
		t.Fatalf("expected 1 cached block, got %+v", blocks)
	}
	s.AddMemoryNote("durable fact one")
	s.stashMutableToolCall([]ToolCall{{Name: "todowrite", ArgsJSON: `{"todos":[{"content":"step","status":"in_progress","priority":"high"}]}`}})
	blocks := s.AssembleSystem()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (static + mutable), got %d", len(blocks))
	}
	if !blocks[0].Cache {
		t.Error("block 0 (static prefix) must be cache-flagged")
	}
	if blocks[1].Cache {
		t.Error("mutable zone must NEVER be cache-flagged")
	}
	if strings.Contains(blocks[0].Text, "durable fact one") || strings.Contains(blocks[0].Text, "Current todo list") {
		t.Error("mutable content leaked into the static prefix")
	}
	if !strings.Contains(blocks[1].Text, "durable fact one") || !strings.Contains(blocks[1].Text, "[in_progress] step") {
		t.Errorf("mutable zone missing notes/todo digest: %q", blocks[1].Text)
	}
	// Mutable-zone rendering is deterministic (no nondeterministic bytes).
	if blocks[1].Text != s.AssembleSystem()[1].Text {
		t.Error("mutable zone must render deterministically")
	}
	// Malformed todo payload on a fresh session degrades to empty digest,
	// never garbage; on a session with a last-good digest it keeps the
	// last-good (deterministic either way).
	s2 := promptSession(t, &mockProvider{}, promptManifest("caps"))
	s2.stashMutableToolCall([]ToolCall{{Name: "todowrite", ArgsJSON: `{"todos":`}})
	if got := s2.TodosDigest(); got != "" {
		t.Errorf("malformed todos must degrade to empty digest, got %q", got)
	}
	if got := s.TodosDigest(); got != "- [in_progress] step (priority high)\n" {
		t.Errorf("last-good digest must survive a malformed update, got %q", got)
	}
}

// AC (creep guard): the native layer stays ≤ 2 KiB and covers exactly
// the four sanctioned topics and nothing more.
func TestNativeLayerCapAndTopics(t *testing.T) {
	if len(NativeStaticLayer) > 2048 {
		t.Errorf("native layer = %d bytes, cap is 2048", len(NativeStaticLayer))
	}
	for _, topic := range []string{"Identity authority", "Tool discipline", "Todo & memory usage", "orchicon_memory_note"} {
		if !strings.Contains(NativeStaticLayer, topic) {
			t.Errorf("native layer missing topic %q", topic)
		}
	}
}

// AC (Anthropic fixture): with ANY explicit flag, ONLY flagged blocks
// get cache_control — the mutable zone after the breakpoint must never
// be cached; no-flag callers keep the last-block fallback; TTL opt-in
// flows into every breakpoint.
func TestAnthropicBreakpointsTwoZone(t *testing.T) {
	twoZone := TurnRequest{Model: "m", MaxTokens: 10, System: []SystemBlock{
		{Text: "static", Cache: true},
		{Text: "mutable", Cache: false},
	}}
	// Flagged-only rule: block 0 carries it, last block does NOT.
	wire := buildAnthropicRequest(twoZone, "")
	if len(wire.System) != 2 {
		t.Fatalf("system blocks = %d", len(wire.System))
	}
	if wire.System[0].CacheControl == nil {
		t.Error("flagged static block must carry the breakpoint")
	}
	if wire.System[1].CacheControl != nil {
		t.Error("mutable block after the breakpoint must NOT be cached (would cache mutable content)")
	}
	// Legacy no-flag callers: last-block fallback preserved.
	legacy := TurnRequest{Model: "m", MaxTokens: 10, System: []SystemBlock{{Text: "a"}, {Text: "b"}}}
	lw := buildAnthropicRequest(legacy, "")
	if lw.System[0].CacheControl != nil || lw.System[1].CacheControl == nil {
		t.Error("no-flag callers keep the last-block fallback")
	}
	// Single flagged block (static-only session) carries it.
	single := buildAnthropicRequest(TurnRequest{Model: "m", MaxTokens: 10, System: []SystemBlock{{Text: "s", Cache: true}}}, "")
	if single.System[0].CacheControl == nil {
		t.Error("single cached block must carry the breakpoint")
	}
	// CacheControlNone → nothing cached.
	none := buildAnthropicRequest(TurnRequest{Model: "m", MaxTokens: 10, CacheControl: CacheControlNone, System: []SystemBlock{{Text: "s", Cache: true}}}, "")
	if none.System[0].CacheControl != nil {
		t.Error("CacheControlNone must suppress all breakpoints")
	}
	// TTL opt-in flows through; 5m default omits the JSON field.
	if wire.System[0].CacheControl.TTL != "" {
		t.Errorf("default TTL = %q, want empty (API default)", wire.System[0].CacheControl.TTL)
	}
	wireTTL := buildAnthropicRequest(twoZone, "1h")
	if wireTTL.System[0].CacheControl.TTL != "1h" {
		t.Errorf("TTL = %q, want 1h", wireTTL.System[0].CacheControl.TTL)
	}
	b := string(mustMarshal(t, buildAnthropicRequest(twoZone, "")))
	if strings.Contains(b, `"ttl"`) {
		t.Error("ttl must be omitted at the API default")
	}
	b1h := string(mustMarshal(t, buildAnthropicRequest(twoZone, "1h")))
	if !strings.Contains(b1h, `"ttl":"1h"`) {
		t.Error("ttl=1h must be emitted when opted in")
	}
}

// AC (OpenAI-compatible): byte-stable prefix ordering yields implicit
// prefix caching — the static prefix is a byte-identical head of the
// joined system text across mutable-state changes, and the wire message
// order never puts mutable content before the system message.
func TestOpenAICompatByteStableOrder(t *testing.T) {
	manifest := promptManifest("composite A\nB\nC")
	s := promptSession(t, &mockProvider{}, manifest)
	blocks := s.AssembleSystem()
	joined := systemText(blocks)
	s.AddMemoryNote("note-1")
	joined2 := systemText(s.AssembleSystem())
	static := blocks[0].Text
	if !strings.HasPrefix(joined, static) || !strings.HasPrefix(joined2, static) {
		t.Fatal("static prefix must remain the byte-identical head of the system text")
	}
	if !strings.Contains(joined2, "note-1") || strings.Contains(joined, "note-1") {
		t.Error("mutable note must appear only after the static prefix")
	}
	// Wire-level: system message precedes history (append-only history
	// never reorders).
	wire := buildOpenAIRequest(TurnRequest{Model: "m", System: s.AssembleSystem(), Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("hi")}}}}}, Quirks{})
	if wire.Messages[0].Role != "system" {
		t.Fatalf("first wire message = %q, want system", wire.Messages[0].Role)
	}
	b1 := mustMarshal(t, buildOpenAIRequest(TurnRequest{Model: "m", System: promptSession(t, &mockProvider{}, manifest).AssembleSystem()}, Quirks{}))
	b2 := mustMarshal(t, buildOpenAIRequest(TurnRequest{Model: "m", System: promptSession(t, &mockProvider{}, manifest).AssembleSystem()}, Quirks{}))
	if string(b1) != string(b2) {
		t.Error("identical inputs must marshal byte-identically")
	}
}

// AC (metrics): per-session cache hit/miss classification + cached
// tokens accumulate from real provider usage and surface via
// Session.CacheStats.
func TestSessionCacheMetrics(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "n1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 100, OutputTokens: 10, CacheWriteTokens: 500}},
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "n2", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 20, OutputTokens: 5, CacheReadTokens: 500}},
		{events: []Event{TextDelta{Text: "three"}}, finish: StopStop, usage: Usage{InputTokens: 20, OutputTokens: 5}},
	}}
	s := promptSession(t, prov, promptManifest("metrics composite"))
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := s.CacheStats()
	if st.Turns != 3 {
		t.Fatalf("turns = %d, want 3", st.Turns)
	}
	if st.MissWrites != 1 || st.Hits != 1 || st.NoneTurns != 1 {
		t.Errorf("classification = %+v, want 1 miss-write / 1 hit / 1 none", st)
	}
	if st.CacheReadTokens != 500 || st.CacheWriteTokens != 500 {
		t.Errorf("cache tokens = %d/%d, want 500/500", st.CacheReadTokens, st.CacheWriteTokens)
	}
	if st.InputTokens != 140 || st.OutputTokens != 20 {
		t.Errorf("token rollup = %d/%d, want 140/20", st.InputTokens, st.OutputTokens)
	}
	if st.PrefixFingerprint == "" || st.PrefixFingerprint != fingerprintPrefix("metrics composite") {
		t.Errorf("prefix fingerprint = %q", st.PrefixFingerprint)
	}
}

// Memory-note tool is loop-registered, session-scoped, and never routed
// to the registry: the model can call it and the note lands in the
// mutable zone on the NEXT turn's assembly.
func TestNativeMemoryNoteToolLoopPath(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "m1", Name: "orchicon_memory_note"}, ToolCallDelta{Index: 0, ArgsJSONDelta: `{"text":"keep this fact`}, ToolCallDelta{Index: 0, ArgsJSONDelta: `"}`}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 10, OutputTokens: 2}},
		{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 10, OutputTokens: 2}},
	}}
	s := promptSession(t, prov, promptManifest("note composite"))
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	notes := s.MemoryNotes()
	if len(notes) != 1 || notes[0] != "keep this fact" {
		t.Fatalf("notes = %v", notes)
	}
	// The tool def was offered to the provider even with a nil registry.
	found := false
	for _, td := range prov.requests[0].Tools {
		if td.Name == "orchicon_memory_note" {
			found = true
		}
	}
	if !found {
		t.Error("memory tool must be loop-registered into the turn request")
	}
	// Second turn's system blocks carry the note after the breakpoint.
	if len(prov.requests[1].System) != 2 || prov.requests[1].System[1].Cache {
		t.Errorf("turn 2 system = %+v, want static + unflagged mutable", prov.requests[1].System)
	}
	if !strings.Contains(prov.requests[1].System[1].Text, "keep this fact") {
		t.Error("note must render into the mutable zone")
	}
}
