package orchicon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/agentmemory"
)

// ctxModelProvider is a mockProvider that ALSO reports a true context
// window for the session model (live hint) — used to exercise the window
// trigger. ctxTokens <= 0 means no live hint (window trigger inert).
type ctxModelProvider struct {
	mockProvider
	ctxTokens int64
	// models, when set, is returned verbatim by ListModels (overrides the
	// ctxTokens convenience) — lets a test carry live Pricing too.
	models []ModelInfo
}

func (p *ctxModelProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if p.models != nil {
		return p.models, nil
	}
	if p.ctxTokens <= 0 {
		return nil, nil
	}
	return []ModelInfo{{ID: "deepseek-v4-flash", Context: p.ctxTokens}}, nil
}

func compactTestSession(t *testing.T, budgets string, window int64) (*Session, *ctxModelProvider, *agentmemory.Store, string) {
	t.Helper()
	dir := t.TempDir()
	ms, err := agentmemory.Open(dir)
	if err != nil {
		t.Fatalf("agentmemory.Open: %v", err)
	}
	t.Cleanup(func() { ms.Close() })
	prov := &ctxModelProvider{ctxTokens: window}
	prov.turns = []scriptedTurn{}
	m := testManifest("orchicon/mockprov/deepseek-v4-flash")
	if budgets != "" {
		m.Budgets = []byte(budgets)
	}
	s, err := NewSession(SessionConfig{
		ExecRow:     testExecRow("exec_compact"),
		Manifest:    m,
		ProjectDir:  dir,
		Provider:    prov,
		MemoryStore: ms,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s, prov, ms, dir
}

func seedHistory(s *Session) {
	goal := "Goal: implement guarded compaction."
	s.history = []Message{
		{Role: RoleUser, Content: []Content{{Text: &goal}}},
		assistantToolUseMsg("call_1", "bash", `{"cmd":"ls"}`),
		toolResultMsg("call_1", "big old tool output line one"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("Continue")}}},
		assistantToolUseMsg("call_2", "glob", `{"pattern":"**/*.go"}`),
		toolResultMsg("call_2", "second big output that should also be evicted"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("Keep going")}}},
		assistantTextMsg("Final recent assistant message."),
	}
}

func ptrStr(s string) *string { return &s }

func assistantToolUseMsg(id, name, args string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: id, Name: name, ArgsJSON: args}}}}
}

func toolResultMsg(id, out string) Message {
	return Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: id, Content: out}}}}
}

func assistantTextMsg(text string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Text: &text}}}
}

// AC (no-hint): with NO live context hint and budget gates disabled, the
// session never compacts — token volume alone never fires compaction.
func TestCompactionNoHintNoFireOnTokensAlone(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"compact_tiers":[true,true,true]}`, 0)
	seedHistory(s)
	for step := 2; step < 12; step++ {
		s.recordTurnUsage(Usage{InputTokens: 100_000, OutputTokens: 10_000})
		if fired := s.maybeCompact(context.Background(), step, Usage{InputTokens: 100_000, OutputTokens: 10_000}); fired {
			t.Fatalf("compaction fired at step %d with no window hint and no enabled gate", step)
		}
	}
	if s.cs.compactions != 0 {
		t.Errorf("compactions = %d, want 0", s.cs.compactions)
	}
}

// AC: budget-breach (escalate tier) fires compaction exactly once per tier
// and the session continues afterwards.
func TestCompactionBudgetGateFiresOnce(t *testing.T) {
	s, _, _, dir := compactTestSession(t, `{"tokens":200,"compact_tiers":[false,true,true],"context_compaction":{"recent_turns":2}}`, 0)
	seedHistory(s)
	ctx := context.Background()

	// Turn at 60/200 = 30% (warn) → no compaction.
	s.recordTurnUsage(Usage{InputTokens: 60})
	if s.maybeCompact(ctx, 3, Usage{InputTokens: 60}) {
		t.Fatalf("compaction fired at warn tier")
	}
	// 60 more → 120/200 = 60% (escalate, compact_tiers[1]=true) → fires.
	s.recordTurnUsage(Usage{InputTokens: 60})
	if !s.maybeCompact(ctx, 4, Usage{InputTokens: 60}) {
		t.Fatalf("compaction did not fire at escalate tier")
	}
	// Same-tier re-evaluation (session continues) → no re-fire.
	s.recordTurnUsage(Usage{})
	if s.maybeCompact(ctx, 5, Usage{}) {
		t.Fatalf("compaction re-fired on the same escalate tier")
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want exactly 1", s.cs.compactions)
	}
	// Evicted originals offloaded to the project dir.
	path := filepath.Join(dir, ".orchicon", "offload", s.id+".jsonl")
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		t.Errorf("offload file %s missing or empty (err=%v)", path, err)
	}
	// Session continues: history has a digest marker + recent tail.
	h := s.History()
	if len(h) == 0 {
		t.Fatalf("history empty after compaction — session cannot continue")
	}
	found := false
	for _, m := range h {
		for _, c := range m.Content {
			if c.Text != nil && strings.Contains(*c.Text, "Compacted old tool results") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no compaction digest marker in history")
	}
}

// AC: true-window pressure (live hint) fires at pressure_frac of the real
// window for the CURRENT TURN's live occupancy; below it nothing fires —
// and many sub-threshold turns never accumulate into a fire (F3: the
// pressure basis is the current request's occupancy, never the cumulative
// re-sent-prefix rollup, which over-counts by ~turn count).
func TestCompactionWindowPressureFires(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":2}}`, 1000)
	seedHistory(s)
	ctx := context.Background()
	// Sub-threshold turns: 500 tokens on a 1000-token window = 50% — never
	// fires, no matter how many turns accumulate (the old cumulative basis
	// fired at step 4; the corrected per-turn basis must not).
	for step := 2; step < 12; step++ {
		s.recordTurnUsage(Usage{InputTokens: 500})
		if fired := s.maybeCompact(ctx, step, Usage{InputTokens: 500}); fired {
			t.Fatalf("compaction fired at step %d from sub-threshold turns", step)
		}
	}
	if s.cs.compactions != 0 {
		t.Fatalf("compactions = %d after sub-threshold turns, want 0", s.cs.compactions)
	}
	// One over-threshold turn (850/1000 = 85% ≥ 0.8) fires exactly once.
	s.recordTurnUsage(Usage{InputTokens: 850})
	if !s.maybeCompact(ctx, 13, Usage{InputTokens: 850}) {
		t.Fatalf("compaction did not fire at window pressure")
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want exactly 1", s.cs.compactions)
	}
	// Same-band re-evaluation does not re-fire (at-most-once per band).
	s.recordTurnUsage(Usage{InputTokens: 860})
	if s.maybeCompact(ctx, 14, Usage{InputTokens: 860}) {
		t.Fatalf("compaction re-fired on the same pressure band")
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want exactly 1", s.cs.compactions)
	}
}

// AC (prefix stability): two sessions with DIFFERENT memory contents
// produce byte-identical block-0; the memory digest appears only in block 1
// (after the cache breakpoint).
func TestPrefixStabilityAcrossMemoryContents(t *testing.T) {
	s1, _, ms1, _ := compactTestSession(t, "", 0)
	s2, _, ms2, _ := compactTestSession(t, "", 0)
	writeMem(t, s1, ms1, "Alpha fact about caching")
	writeMem(t, s2, ms2, "Beta unrelated decision record")

	b1 := s1.AssembleSystem()
	b2 := s2.AssembleSystem()
	if len(b1) == 0 || len(b2) == 0 {
		t.Fatalf("empty system blocks")
	}
	if b1[0].Text != b2[0].Text {
		t.Errorf("block-0 (static prefix) differs across memory contents:\n%q\nvs\n%q", b1[0].Text, b2[0].Text)
	}
	if !b1[0].Cache {
		t.Errorf("block 0 must carry the cache breakpoint")
	}
	zone1, zone2 := "", ""
	for _, blk := range b1[1:] {
		zone1 += blk.Text
	}
	for _, blk := range b2[1:] {
		zone2 += blk.Text
	}
	if !strings.Contains(zone1, "Alpha fact about caching") {
		t.Errorf("session 1 digest missing from mutable zone")
	}
	if !strings.Contains(zone2, "Beta unrelated decision record") {
		t.Errorf("session 2 digest missing from mutable zone")
	}
}

func writeMem(t *testing.T, s *Session, ms *agentmemory.Store, title string) {
	t.Helper()
	_, err := ms.Write(context.Background(), agentmemory.WriteInput{
		TenantID:    s.identity.TenantID,
		ProjectDir:  s.projectDir,
		ExecutionID: s.identity.ExecutionID,
		WorkerID:    s.identity.WorkerID,
		Title:       title,
		Body:        "durable body content",
		Tags:        []string{"tag1"},
	})
	if err != nil {
		t.Fatalf("writeMem: %v", err)
	}
}

// F2 regression: after a middle-only eviction, every assistant tool_use
// that REMAINS in history has a matching tool_result (and vice versa) — no
// orphaned tool_use (invalid Anthropic/OpenAI history). The eviction unit
// is the assistant tool_use message WITH its paired tool-result message:
// middle rounds are evicted wholesale, while a pair that lands in the
// recent tail survives verbatim.
func TestCompactionEvictionPreservesToolUseResultPairing(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":4}}`, 1000)
	// History: two old fully-evictable middle rounds, then a user turn and
	// a RECENT round whose pair lands in the pinned tail (recent_turns=4 →
	// last 4 messages verbatim).
	goal := "Goal."
	s.history = []Message{
		{Role: RoleUser, Content: []Content{{Text: &goal}}},
		assistantToolUseMsg("old_1", "bash", `{"cmd":"ls"}`),
		toolResultMsg("old_1", "old output one"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("continue")}}},
		assistantToolUseMsg("old_2", "glob", `{}`),
		toolResultMsg("old_2", "old output two"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("go")}}},
		assistantToolUseMsg("recent_use", "read", `{"path":"x"}`),
		toolResultMsg("recent_use", "recent tail result"),
		assistantTextMsg("Final assistant text."),
	}
	// Fire window pressure on an over-threshold turn.
	if !s.maybeCompact(context.Background(), 3, Usage{InputTokens: 900, CacheReadTokens: 100}) {
		t.Fatalf("expected compaction to fire")
	}
	h := s.History()
	// Every remaining assistant tool_use must have a matching tool_result in
	// history AND every remaining tool_result must have a matching tool_use.
	toolUses := map[string]bool{}
	toolResults := map[string]bool{}
	for _, m := range h {
		for _, c := range m.Content {
			if c.ToolUse != nil {
				toolUses[c.ToolUse.ToolCallID] = true
			}
			if c.ToolResult != nil {
				toolResults[c.ToolResult.ToolCallID] = true
			}
		}
	}
	for id := range toolUses {
		if !toolResults[id] {
			t.Errorf("ORPHANED tool_use %q (no tool_result in history)", id)
		}
	}
	for id := range toolResults {
		if !toolUses[id] {
			t.Errorf("ORPHANED tool_result %q (no tool_use in history)", id)
		}
	}
	// The recent tail pair survives; the old middle rounds are gone.
	if !toolUses["recent_use"] || !toolResults["recent_use"] {
		t.Errorf("recent tail pair missing: uses=%v results=%v", toolUses, toolResults)
	}
	for _, gone := range []string{"old_1", "old_2"} {
		if toolUses[gone] || toolResults[gone] {
			t.Errorf("middle round %q survived eviction: uses=%v results=%v", gone, toolUses, toolResults)
		}
	}
}

// The recent tail is verbatim: a tool_result that lands in the pinned tail
// keeps its paired assistant tool_use even when the tool_use sits in the
// middle (evicting the use would orphan the kept result), and a tool_use in
// the tail keeps its middle result (evicting the result would orphan the
// kept use).
func TestCompactionTailPairingIsPreserved(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":3}}`, 1000)
	// History: goal, old pair (fully evictable), then a user + a tool_use
	// whose RESULT message sits in the pinned tail (recent_turns=3 → the
	// last 3 messages are verbatim).
	goal := "Goal."
	s.history = []Message{
		{Role: RoleUser, Content: []Content{{Text: &goal}}},
		assistantToolUseMsg("old_1", "bash", `{}`),
		toolResultMsg("old_1", "old output"),
		{Role: RoleUser, Content: []Content{{Text: ptrStr("continue")}}},
		assistantToolUseMsg("tail_use", "read", `{"path":"x"}`),
		toolResultMsg("tail_use", "tail result output"),
	}
	if !s.maybeCompact(context.Background(), 3, Usage{InputTokens: 900, CacheReadTokens: 100}) {
		t.Fatalf("expected compaction to fire")
	}
	h := s.History()
	foundUse, foundResult := false, false
	for _, m := range h {
		for _, c := range m.Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == "tail_use" {
				foundUse = true
			}
			if c.ToolResult != nil && c.ToolResult.ToolCallID == "tail_use" {
				foundResult = true
			}
		}
	}
	if !foundUse || !foundResult {
		t.Fatalf("tail tool_use/result pair was broken: use=%v result=%v", foundUse, foundResult)
	}
	// The evictable old pair (old_1 use+result) is gone.
	for _, m := range h {
		for _, c := range m.Content {
			if (c.ToolUse != nil && c.ToolUse.ToolCallID == "old_1") ||
				(c.ToolResult != nil && c.ToolResult.ToolCallID == "old_1") {
				t.Fatalf("old_1 pair survived — expected eviction of the full round")
			}
		}
	}
}

// Regression for QA bug B: a failed offload must leave history byte-identical
// (plan-then-offload-then-commit). The probe pre-creates the offload path as
// a DIRECTORY so offloadToolResults fails; the middle tool rounds must still
// be present in the live session afterwards — never evicted without offload.
func TestCompactionOffloadFailureLeavesHistoryIntact(t *testing.T) {
	s, _, _, dir := compactTestSession(t, `{"tokens":200,"compact_tiers":[false,true,true],"context_compaction":{"recent_turns":2}}`, 0)
	seedHistory(s)
	before := s.History()
	beforeJSON := mustMarshalMessages(before)

	// Sabotage the offload destination: pre-create the path as a directory,
	// so os.OpenFile on it fails (the QA probe shape).
	path := filepath.Join(dir, ".orchicon", "offload", s.id+".jsonl")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll offload-path-as-dir: %v", err)
	}

	ctx := context.Background()
	s.recordTurnUsage(Usage{InputTokens: 60})
	if s.maybeCompact(ctx, 3, Usage{InputTokens: 60}) {
		t.Fatalf("compaction fired at warn tier")
	}
	s.recordTurnUsage(Usage{InputTokens: 60})
	if s.maybeCompact(ctx, 4, Usage{InputTokens: 60}) {
		t.Fatalf("compaction fired despite a failing offload")
	}
	if s.cs.compactions != 0 {
		t.Fatalf("compactions = %d, want 0 after a failed offload", s.cs.compactions)
	}
	// History is byte-identical to before the attempt.
	after := s.History()
	if len(after) != len(before) || mustMarshalMessages(after) != beforeJSON {
		t.Fatalf("history mutated despite offload failure:\nbefore=%s\nafter =%s", beforeJSON, mustMarshalMessages(after))
	}
	// The evictable middle originals are still in the live session.
	var sawOld, sawMarker bool
	for _, m := range after {
		for _, c := range m.Content {
			if c.ToolResult != nil && strings.Contains(c.ToolResult.Content, "big old tool output") {
				sawOld = true
			}
			if c.Text != nil && strings.Contains(*c.Text, "Compacted old tool results") {
				sawMarker = true
			}
		}
	}
	if !sawOld {
		t.Fatalf("evictable middle originals lost from the live session after a failed offload")
	}
	if sawMarker {
		t.Fatalf("digest marker injected despite a failed offload")
	}
}

// mustMarshalMessages renders history as stable JSON for identity compares.
func mustMarshalMessages(h []Message) string {
	b, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Regression for QA bug C: the compaction digest marker must be MERGED into
// the pinned head user message — never a standalone user message — so the
// marshaled Anthropic history stays strictly role-alternating (a second
// consecutive plain-text user message is rejected by the Messages API). The
// seed mirrors a REAL mid-session history (goal head + alternating assistant
// tool_use / tool-result rounds — no injected plain user turns), which
// marshals role-alternating before compaction.
func TestCompactionDigestMarkerMergedIntoHeadUserMessage(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"tokens":0,"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":2}}`, 1000)
	goal := "Goal."
	s.history = []Message{
		{Role: RoleUser, Content: []Content{{Text: &goal}}},
		assistantToolUseMsg("old_1", "bash", `{}`),
		toolResultMsg("old_1", "old output one"),
		assistantToolUseMsg("old_2", "glob", `{}`),
		toolResultMsg("old_2", "old output two"),
		assistantToolUseMsg("recent", "read", `{"path":"x"}`),
		toolResultMsg("recent", "recent tail result"),
	}
	if !s.maybeCompact(context.Background(), 3, Usage{InputTokens: 900, CacheReadTokens: 100}) {
		t.Fatalf("expected compaction to fire")
	}
	h := s.History()
	// The marker text lives INSIDE the first user message's text content,
	// and there is no standalone user-role message carrying ONLY the marker.
	markerOnlyMsg := -1
	goalHasMarker := false
	for i, m := range h {
		if m.Role != RoleUser {
			continue
		}
		texts := []string{}
		for _, c := range m.Content {
			if c.Text != nil {
				texts = append(texts, *c.Text)
			}
		}
		joined := strings.Join(texts, "\n")
		if strings.Contains(joined, "Compacted old tool results") && !strings.Contains(joined, "Goal.") {
			markerOnlyMsg = i
		}
		if strings.Contains(joined, "Goal.") && strings.Contains(joined, "Compacted old tool results") {
			goalHasMarker = true
		}
	}
	if markerOnlyMsg >= 0 {
		t.Fatalf("standalone marker-only user message at index %d — must be merged into the goal head", markerOnlyMsg)
	}
	if !goalHasMarker {
		t.Fatalf("digest marker not merged into the pinned head (goal) user message")
	}

	// Marshal for Anthropic: roles must alternate strictly (the eviction
	// removes whole assistant/tool-result rounds; the merged marker adds no
	// message), and the marker survives inside the head message.
	wire := marshalAnthropicHistory(h)
	if len(wire) < 2 {
		t.Fatalf("marshaled history too short: %#v", wire)
	}
	for i := 1; i < len(wire); i++ {
		if wire[i].Role == wire[i-1].Role {
			t.Fatalf("non-alternating roles in marshalAnthropicHistory output at %d-%d (%q %q): %#v", i-1, i, wire[i-1].Role, wire[i].Role, wire)
		}
	}
	foundMarker := false
	for _, c := range wire[0].Content {
		if c.Type == "text" && strings.Contains(c.Text, "Compacted old tool results") {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatalf("digest marker missing from the marshaled head user message")
	}
}

// F4: a budget whose ONLY over-budget dimension is cost_usd fires exactly
// once when the session prices real usage through the model's live pricing.
// Mirrors the loop: each turn's usage is priced via priceUsage (the shared
// ModelInfo.CostFor path) before maybeCompact folds it in.
func TestCompactionCostBudgetGateFiresWithPricing(t *testing.T) {
	// recent_turns:2 so the seeded middle tool rounds fall OUTSIDE the
	// pinned tail and are evictable (the default 6 would pin them and
	// doCompact would correctly no-op on an empty eviction).
	s, prov, _, _ := compactTestSession(t, `{"tokens":0,"cost_usd":0.01,"compact_tiers":[false,true,true],"context_compaction":{"recent_turns":2}}`, 0)
	seedHistory(s)
	// The mock provider reports NO context window (window trigger inert) but
	// DOES carry live pricing for the session model.
	prov.ctxTokens = 0
	prov.models = []ModelInfo{{
		ID:      "deepseek-v4-flash",
		Pricing: &Pricing{InputPerM: 1.0, OutputPerM: 2.0, CacheReadPerM: 0.1, Currency: "USD", Source: "test"},
	}}
	ctx := context.Background()
	price := func(u Usage) Usage { u.CostUSD = s.priceUsage(ctx, u); return u }
	// 3000 input tokens at $1/M = $0.003; two such turns = $0.006 (escalate
	// at 0.5 of a $0.01 gate) — fires on the second.
	if s.maybeCompact(ctx, 2, price(Usage{InputTokens: 3000})) {
		t.Fatalf("compaction fired below the cost escalate tier")
	}
	if !s.maybeCompact(ctx, 3, price(Usage{InputTokens: 3000})) {
		t.Fatalf("compaction did not fire at the cost escalate tier")
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want exactly 1", s.cs.compactions)
	}
	// Same-tier re-evaluation does not re-fire.
	if s.maybeCompact(ctx, 4, price(Usage{InputTokens: 3000})) {
		t.Fatalf("compaction re-fired on the same cost tier")
	}
}
