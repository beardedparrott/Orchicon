package orchicon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/beardedparrott/orchicon/internal/agentmemory"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// The compact_max_turns turn-count context-hygiene gate, ported from the
// opencode adapter (internal/opencode/compact.go effectiveCompactMaxTurns +
// session_run.maybeCompact): a chatty session is compacted periodically at
// the quiet turn boundary even when NO budget dimension breached and NO
// window pressure exists. Orthogonal to the ladder — it compacts but never
// warns and never aborts — and it reuses the shared doCompact latches:
// the step<2 min-turn floor, the same-turn/min-turn re-arm floor, and the
// per-execution CompactionMax cap.

// turnGateSession builds a minimal session (no provider, no history
// helpers needed beyond a seeded evictable history) with the given merged
// budget JSON. Budgets with every dimension disabled (`tokens:0`,
// `cost_usd:0`, `tool_call_count:0`) leave the ladder with nothing live —
// exactly the tenant configuration the gate exists to rescue.
func turnGateSession(t *testing.T, budgets string) *Session {
	t.Helper()
	cp, _ := policyFromSettings([]byte(budgets))
	s := &Session{
		id:  "test-turn-gate",
		log: slog.Default(),
		cp:  cp,
		cs: compactState{
			budget: opencode.ParseBudgetLadder([]byte(budgets)),
			spend:  opencode.NewBudgetSpend(),
		},
	}
	seedHistory(s)
	return s
}

// AC: with every budget dimension disabled (explicit 0 → the gate never
// evaluates) and no window hint, a session that runs past compact_max_turns
// turns still gets periodic compaction at the quiet boundary. This is THE
// regression the port exists for: the incident's 1.27M-token churn sessions
// had no live trigger left at all.
func TestTurnCountGateFiresWithoutAnyBudgetTier(t *testing.T) {
	s := turnGateSession(t, `{"tokens":0,"cost_usd":0,"tool_call_count":0,"compact_max_turns":3,"context_compaction":{"recent_turns":2}}`)
	ctx := context.Background()
	// Turn usage never approaches anything: zero-priced, tiny.
	usage := Usage{InputTokens: 10}
	// Steps 2 and 3: below the 3-turn cap (turns-since-compact = step while
	// lastCompactStep is 0) → step 2 no fire; step 3 reaches cap 3 → fires.
	if got := s.maybeCompact(ctx, 2, usage); got != "" {
		t.Fatalf("step 2 < cap 3 → %q, want no compaction", got)
	}
	if got := s.maybeCompact(ctx, 3, usage); got != "compacted:turn_count" {
		t.Fatalf("step 3 == cap 3 (>=) → %q, want compacted:turn_count", got)
	}
	if s.cs.compactions != 1 || s.cs.lastCompactStep != 3 {
		t.Fatalf("compactions=%d lastStep=%d, want 1 at step 3", s.cs.compactions, s.cs.lastCompactStep)
	}
	// Re-arm: within the min-turn floor of the compact → no immediate re-fire.
	if got := s.maybeCompact(ctx, 4, usage); got != "" {
		t.Fatalf("step 4 within floor → %q, want no compaction (re-arm latch)", got)
	}
	// turns-since-compact = 5-3=2 → inside cap-1, below cap 3.
	if got := s.maybeCompact(ctx, 5, usage); got != "" {
		t.Fatalf("step 5 (2 turns since compact) → %q, want no compaction", got)
	}
	// step 6: 3 turns since the last compact → the gate re-arms. The first
	// compaction exhausted the seeded eviction pool, so re-seed fresh tool
	// rounds (the session gained new material since) — the gate fires again
	// (periodic).
	seedHistory(s)
	if got := s.maybeCompact(ctx, 6, usage); got != "compacted:turn_count" {
		t.Fatalf("step 6 (3 turns since compact) → %q, want compacted:turn_count", got)
	}
	if s.cs.compactions != 2 {
		t.Fatalf("compactions = %d, want 2 (periodic, no budget tier ever breached)", s.cs.compactions)
	}
}

// AC: unset compact_max_turns → the opencode built-in default
// (DefaultCompactMaxTurns). ONE source of the default: the native policy
// falls back to the opencode constant, never a re-stated number.
func TestTurnCountGateUnsetUsesOpencodeDefault(t *testing.T) {
	def := opencode.DefaultCompactMaxTurns()
	if def < 2 {
		t.Fatalf("opencode default %d < min-turn floor — gate could never fire", def)
	}
	s := turnGateSession(t, `{"tokens":0,"cost_usd":0,"tool_call_count":0,"context_compaction":{"recent_turns":2}}`)
	if s.cp.CompactMaxTurns != def {
		t.Fatalf("policy CompactMaxTurns = %d, want the opencode default %d", s.cp.CompactMaxTurns, def)
	}
	ctx := context.Background()
	usage := Usage{InputTokens: 10}
	// Just below the default cap → no fire.
	if got := s.maybeCompact(ctx, def-1, usage); got != "" {
		t.Fatalf("step %d < default cap → %q, want no compaction", def-1, got)
	}
	// At the default cap → fires.
	if got := s.maybeCompact(ctx, def, usage); got != "compacted:turn_count" {
		t.Fatalf("step %d == default cap → %q, want compacted:turn_count", def, got)
	}
}

// AC: explicit 0 disables the turn-count gate entirely
// (effectiveCompactMaxTurns parity) — a long session with the gate
// explicitly off never turn-count-compacts.
func TestTurnCountGateExplicitZeroDisables(t *testing.T) {
	s := turnGateSession(t, `{"tokens":0,"cost_usd":0,"tool_call_count":0,"compact_max_turns":0,"context_compaction":{"recent_turns":2}}`)
	if s.cp.CompactMaxTurns != 0 {
		t.Fatalf("explicit 0 → CompactMaxTurns = %d, want 0 (disabled)", s.cp.CompactMaxTurns)
	}
	ctx := context.Background()
	for step := 2; step < 60; step++ {
		if got := s.maybeCompact(ctx, step, Usage{InputTokens: 10}); got != "" {
			t.Fatalf("disabled gate fired at step %d: %q", step, got)
		}
	}
	if s.cs.compactions != 0 {
		t.Fatalf("compactions = %d, want 0 with the gate explicitly disabled", s.cs.compactions)
	}
}

// AC: negative values disable too (opencode parity: explicit value <= 0 →
// ok=false).
func TestTurnCountGateNegativeDisables(t *testing.T) {
	s := turnGateSession(t, `{"compact_max_turns":-1}`)
	if s.cp.CompactMaxTurns != 0 {
		t.Fatalf("explicit -1 → CompactMaxTurns = %d, want 0 (disabled)", s.cp.CompactMaxTurns)
	}
}

// AC: malformed compact_max_turns JSON falls back to the built-in default —
// never a guessed value.
func TestTurnCountGateMalformedFallsBackToDefault(t *testing.T) {
	s := turnGateSession(t, `{"compact_max_turns":"not-a-number"}`)
	if s.cp.CompactMaxTurns != opencode.DefaultCompactMaxTurns() {
		t.Fatalf("malformed value → CompactMaxTurns = %d, want the built-in default", s.cp.CompactMaxTurns)
	}
}

// AC: the min-turn floor + re-arm latch — never at session start (the
// step<2 floor in maybeCompact), never within minT turns of a prior compact
// (no compact loop): with a tiny cap the gate still cannot fire on the
// step right after a compaction.
func TestTurnCountGateRespectsMinTurnFloor(t *testing.T) {
	s := turnGateSession(t, `{"compact_max_turns":2,"context_compaction":{"recent_turns":2}}`)
	ctx := context.Background()
	usage := Usage{InputTokens: 10}
	// step 1 < 2: the session-start floor — never compact.
	if got := s.maybeCompact(ctx, 1, usage); got != "" {
		t.Fatalf("step 1 (session start) → %q, want the floor to hold", got)
	}
	// step 2 reaches the cap → fires.
	if got := s.maybeCompact(ctx, 2, usage); got != "compacted:turn_count" {
		t.Fatalf("step 2 → %q, want compacted:turn_count", got)
	}
	// step 3: 1 turn since the compact — inside the min-turn floor → held.
	if got := s.maybeCompact(ctx, 3, usage); got != "" {
		t.Fatalf("step 3 within min-turn floor → %q, want no compact loop", got)
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want 1 (floor held)", s.cs.compactions)
	}
}

// AC: the per-execution CompactionMax cap bounds turn-count compactions.
func TestTurnCountGateRespectsPerExecutionCap(t *testing.T) {
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	s := turnGateSession(t, `{"compact_max_turns":2,"context_compaction":{"recent_turns":2}}`)
	ctx := context.Background()
	usage := Usage{InputTokens: 10}
	if got := s.maybeCompact(ctx, 2, usage); got != "compacted:turn_count" {
		t.Fatalf("first compaction → %q, want compacted:turn_count", got)
	}
	// Cap (1) already reached → further gate crossings never compact.
	for step := 5; step < 20; step++ {
		if got := s.maybeCompact(ctx, step, usage); got != "" {
			t.Fatalf("step %d past the cap → %q, want the cap to hold", step, got)
		}
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want 1 (per-execution cap)", s.cs.compactions)
	}
}

// AC: composition — the turn-count compaction resets the shared latches
// like any doCompact success (lastCompactStep), so a window-pressure or
// budget-tier crossing that lands later still latches correctly and the
// turn gate cannot double-fire behind a budget compaction in the same
// call. Here a budget escalate-tier compaction fires first; the turn gate
// must not re-compact on the same step.
func TestTurnCountGateComposesWithBudgetTrigger(t *testing.T) {
	s := turnGateSession(t, `{"tokens":200,"compact_tiers":[false,true,true],"compact_max_turns":2,"context_compaction":{"recent_turns":2}}`)
	// Seed enough fresh tokens to cross the escalate tier on the first
	// call: 120/200 = 60% ≥ 0.5 (escalate).
	usage := Usage{InputTokens: 120}
	// lastCompactStep == 0 → turns-since = step ≥ 2 would ALSO satisfy the
	// turn gate, but the budget trigger runs FIRST and its doCompact success
	// updates lastCompactStep + compactions; the turn gate must not fire a
	// second time this call.
	got := s.maybeCompact(context.Background(), 2, usage)
	if got != "compacted:budget:tokens:escalate" {
		t.Fatalf("budget trigger → %q, want compacted:budget:tokens:escalate", got)
	}
	if s.cs.compactions != 1 || s.cs.lastCompactStep != 2 {
		t.Fatalf("compactions=%d lastStep=%d, want 1 at step 2", s.cs.compactions, s.cs.lastCompactStep)
	}
	// Same turn's step-3 re-evaluation: turns-since = 3-2 = 1 (inside the
	// min-turn floor) → the turn gate holds. But the spend is cumulative —
	// folding the same 120 tokens again puts fresh tokens at 240/200 ≥ 1.0,
	// and the ABORT tier is terminal regardless of latches (opencode
	// parity: abort is evaluated unconditionally and never masks). The
	// session ends with budget_abort:tokens — exactly the ladder's own
	// contract, unchanged by this port.
	if got := s.maybeCompact(context.Background(), 3, usage); got != "budget_abort:tokens" {
		t.Fatalf("step 3 with spend at 240/200 → %q, want the terminal budget_abort:tokens", got)
	}
}

// AC: the gate composes with a budget abort — the abort tier stays
// TERMINAL and is returned before the turn gate is even evaluated (opencode
// parity: abort kills the session; compaction never masks it).
func TestTurnCountGateDoesNotMaskBudgetAbort(t *testing.T) {
	s := turnGateSession(t, `{"tool_call_count":3,"compact_max_turns":2}`)
	s.toolUses = 3 // 3/3 calls → frac 1.0 → the abort tier
	if got := s.maybeCompact(context.Background(), 2, Usage{}); got != "budget_abort:tool_call_count" {
		t.Fatalf("abort tier → %q, want budget_abort:tool_call_count (terminal, never a compaction)", got)
	}
	if s.cs.compactions != 0 {
		t.Fatalf("compactions = %d, want 0 — abort must not compact", s.cs.compactions)
	}
}

// AC (loop-level): a session whose budget disables every dimension and
// whose provider exposes no context window still compacts mid-Run on the
// turn gate, and the session continues to a successful StopStop.
func TestTurnCountGateFiresMidRunAndSessionContinues(t *testing.T) {
	dir := t.TempDir()
	ms, err := agentmemory.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	man := testManifest("orchicon/mockprov/deepseek-v4-flash")
	man.Budgets = []byte(`{"tokens":0,"cost_usd":0,"tool_call_count":0,"compact_max_turns":3,"context_compaction":{"recent_turns":2}}`)
	prov := &ctxModelProvider{ctxTokens: 0}
	prov.turns = []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallDelta{Index: 0, ArgsJSONDelta: `{"a":1}`}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 10}},
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t2", Name: "noop"}, ToolCallDelta{Index: 0, ArgsJSONDelta: `{"a":2}`}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 10}},
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t3", Name: "noop"}, ToolCallDelta{Index: 0, ArgsJSONDelta: `{"a":3}`}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 10}},
		{events: []Event{TextDelta{Text: "Done after turn-count compaction."}}, finish: StopStop, usage: Usage{InputTokens: 5}},
	}
	s, err := NewSession(SessionConfig{ExecRow: testExecRow("exec_turn_gate"), Manifest: man, ProjectDir: dir, Provider: prov, MemoryStore: ms})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want success (session continues after turn-count compaction)", results)
	}
	if s.cs.compactions != 1 {
		t.Fatalf("compactions = %d, want exactly 1 from the turn-count gate", s.cs.compactions)
	}
}
