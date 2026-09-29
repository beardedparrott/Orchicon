package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// costGatedBudgets is a merged-budget payload exercising the SHARED ladder:
// a cheap cost gate, a tax-free token gate (disabled), compaction permitted
// only on the cost dimension, and tenant-authored ladder MESSAGES. Every one
// of these values must be read out of this JSON — the claude package holds
// none of them as literals.
const costGatedBudgets = `{
  "tokens": 0,
  "cost_usd": 0.06,
  "compact_dims": ["cost_usd"],
  "compact_tiers": [false, true, true],
  "warnings": {
    "fractions": {"cost_usd": [0.25, 0.5, 0.75]},
    "messages": {"cost_usd": ["TENANT-WARN-{pct}", "TENANT-ESCALATE-{pct}", "TENANT-FINAL-{pct}"]}
  }
}`

// cacheHeavyResult is a result line whose prompt is almost entirely CACHE
// READS (40k of them) with negligible fresh tokens: the token gate could
// never fire on this turn, so any trigger must come from the CACHE-AWARE
// cost the provider priced (total_cost_usd).
const cacheHeavyResult = `{"type":"result","subtype":"success","session_id":"sess-1","result":"ok","total_cost_usd":0.02,"usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":40000,"cache_creation_input_tokens":0}}`

// noAutoCompactBudgets is the same ladder with EVERY dimension opted out of
// compact_dims, so only an explicit bridge-level compact can fire.
const noAutoCompactBudgets = `{
  "tokens": 0,
  "cost_usd": 0.06,
  "compact_dims": [],
  "warnings": {
    "fractions": {"cost_usd": [0.25, 0.5, 0.75]},
    "messages": {"cost_usd": ["N-WARN-{pct}", "N-ESCALATE-{pct}", "N-FINAL-{pct}"]}
  }
}`

// freeResult is a follow-up turn with no priced spend, so the compacted
// session finishes INSIDE its budget instead of tripping the abort tier.
const freeResult = `{"type":"result","subtype":"success","session_id":"sess-1","result":"ok","total_cost_usd":0,"usage":{"input_tokens":5,"output_tokens":7}}`

func budgetCompactedParts(rec *captureCallbacks) []capturedPart {
	var out []capturedPart
	for _, p := range rec.snap().parts {
		if p.kind == db.SessionPartCompacted {
			out = append(out, p)
		}
	}
	return out
}

func turnsContaining(fp *fakeProc, needle string) []string {
	var out []string
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, t := range fp.turns {
		if strings.Contains(string(t), needle) {
			out = append(out, string(t))
		}
	}
	return out
}

// budgetSession wires a bridge + fake subprocess for a budget-gated run and
// returns the harness so the test can assert liveness on the same bridge.
func budgetSession(t *testing.T, fp *fakeProc, manifest scheduler.ExecutionManifest, rec *captureCallbacks) (*harness, <-chan error) {
	t.Helper()
	h := newHarness(t, func() *fakeProc { return fp })
	h.b.SetSessionStore(rec.recordParts)
	h.b.SetFileEditHook(rec.fileEdit)
	h.b.SetUsageRecorder(rec.recordUsage)
	if manifest.ProjectDir == "" {
		manifest.ProjectDir = t.TempDir()
	}
	if manifest.ExecutionID == "" {
		manifest.ExecutionID = "exec-budget"
	}
	if manifest.ModelRef == "" {
		manifest.ModelRef = "claude/anthropic/claude-sonnet-5"
	}
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(context.Background(), db.ExecutionRow{ID: manifest.ExecutionID, TenantID: "t1"}, manifest, rec)
	}()
	waitFor(t, func() bool { return fp.turnCount() == 1 }, "initial turn write")
	return h, done
}

// TestClaudeBudgetCompactFiresOnCacheAwareCost proves the keystone: a budget
// breach on the SHARED ladder compacts the claude transcript, the trigger
// fires on cache-aware COST (a turn whose tokens are almost all cache reads
// still counts at the provider's real price), the compaction is written as a
// NATIVE in-session directive turn (never an HTTP call — claude has no
// serve), and the ladder's own tenant-authored message is the text injected.
func TestClaudeBudgetCompactFiresOnCacheAwareCost(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	h, done := budgetSession(t, fp, scheduler.ExecutionManifest{
		ExecutionID: "exec-budget",
		Goal:        "ship the feature",
		Budgets:     []byte(costGatedBudgets),
	}, rec)

	fp.push(initLine)
	// Turn 1 (0.02 / 0.06 = 0.33 → warn): the ladder injects the TENANT's
	// message. The min-turn floor keeps compaction off at session start.
	fp.push(cacheHeavyResult)
	waitFor(t, func() bool { return len(turnsContaining(fp, "TENANT-WARN-33")) > 0 }, "tenant warn message injection")
	if got := budgetCompactedParts(rec); len(got) != 0 {
		t.Fatalf("compacted at session start: %+v", got)
	}

	// Turn 2 (0.04 / 0.06 = 0.67 → escalate, which compacts): the directive
	// turn goes onto the SAME stdin and the compaction is recorded durably.
	fp.push(cacheHeavyResult)
	waitFor(t, func() bool { return len(budgetCompactedParts(rec)) == 1 }, "budget compaction")
	if len(turnsContaining(fp, "CONTEXT COMPACTION")) == 0 {
		t.Fatal("no compact directive turn was written onto the live session")
	}
	if len(turnsContaining(fp, "TENANT-ESCALATE-6")) == 0 {
		t.Fatal("the ladder's own escalate message was not injected")
	}
	var payload map[string]any
	_ = json.Unmarshal(mustJSON(t, budgetCompactedParts(rec)[0].payload), &payload)
	if payload["reason"] != "budget:cost_usd:escalate" {
		t.Fatalf("compaction reason = %v", payload["reason"])
	}
	if !strings.Contains(payload["scope"].(string), "ship the feature") {
		t.Fatalf("the compact directive did not direct the remaining scope: %v", payload["scope"])
	}

	// The compacted session is REVERSIBLE: it is still live, the CLI's own
	// compaction boundary is observed, and a further turn still terminates
	// the execution normally.
	if !h.b.IsExecutionActive("exec-budget") {
		t.Fatal("the session did not survive its own compaction")
	}
	fp.push(`{"type":"system","subtype":"compact_boundary","session_id":"sess-1"}`)
	waitFor(t, func() bool { return len(boundaryParts(rec)) == 1 }, "compact boundary recorded")
	// Two further turns: the first consumes the outstanding queued turn the
	// compact directive opened, the second reaches the boundary with an empty
	// queue and ends the execution cleanly (nothing new is injected — the
	// escalate tier is already latched and the min-turn floor holds).
	fp.push(freeResult)
	fp.push(freeResult)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after compaction: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the compacted session never reached a terminal turn")
	}
	s := rec.snap()
	if len(s.results) != 1 || !s.results[0].succeeded {
		t.Fatalf("OnResult = %+v, want one success", s.results)
	}
	if got := turnsContaining(fp, "CONTEXT COMPACTION"); len(got) != 1 {
		t.Fatalf("compact directive turns = %d, want exactly one", len(got))
	}
}

func boundaryParts(rec *captureCallbacks) []capturedPart {
	var out []capturedPart
	for _, p := range rec.snap().parts {
		if p.kind == db.SessionPartCompacted && p.payload["source"] == "claude_auto_compact" {
			out = append(out, p)
		}
	}
	return out
}

func mustJSON(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestClaudeBudgetAbortIsTerminal proves the ladder ceiling ENDS the
// execution (opencode budget_abort parity): the abort tier is not a
// compaction, it fails the run and hard-kills the child.
func TestClaudeBudgetAbortIsTerminal(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	_, done := budgetSession(t, fp, scheduler.ExecutionManifest{
		ExecutionID: "exec-abort",
		Budgets:     []byte(`{"tokens":0,"cost_usd":0.01}`),
	}, rec)
	fp.push(initLine)
	fp.push(cacheHeavyResult) // 0.02 / 0.01 = 2.0 → abort
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a budget abort must fail the session")
		}
		if !strings.Contains(err.Error(), "budget_abort:cost_usd") {
			t.Fatalf("error = %v, want the terminal budget abort reason", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the budget abort did not terminate the session")
	}
	s := rec.snap()
	if len(s.results) != 1 || s.results[0].succeeded {
		t.Fatalf("OnResult = %+v, want one failure", s.results)
	}
	if !strings.Contains(s.results[0].errMsg, "budget_abort:cost_usd") {
		t.Fatalf("errMsg = %q", s.results[0].errMsg)
	}
}

// TestClaudeCompactGuardsAtMostOncePerStep drives the shared guards directly:
// never at session start, at most once per step, min-turn floor between
// compactions and the per-execution cap.
func TestClaudeCompactGuardsAtMostOncePerStep(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	s := newDirectSession(t, scheduler.ExecutionManifest{
		ExecutionID: "exec-guards",
		Goal:        "goal text",
		Budgets:     []byte(costGatedBudgets),
	}, rec, fp)

	// Never at session start: step 0 is below the ladder's min-turn floor.
	if err := s.doCompactErr(context.Background()); err == nil {
		t.Fatal("compaction fired at session start")
	}
	if s.compactionsSoFar() != 0 {
		t.Fatalf("compactions = %d, want 0", s.compactionsSoFar())
	}

	s.setStep(2)
	if !s.doCompact(context.Background(), "test") {
		t.Fatal("compaction did not fire on an armed step")
	}
	// At most once per step: the same step cannot compact twice.
	if s.doCompact(context.Background(), "test") {
		t.Fatal("the same step compacted twice")
	}
	if s.compactionsSoFar() != 1 {
		t.Fatalf("compactions = %d, want 1", s.compactionsSoFar())
	}
	if len(budgetCompactedParts(rec)) != 1 {
		t.Fatalf("compacted parts = %+v, want exactly one", budgetCompactedParts(rec))
	}
	// The min-turn floor keeps the next step from compacting immediately.
	s.setStep(3)
	if s.doCompact(context.Background(), "test") {
		t.Fatal("compaction ignored the min-turn floor")
	}
}

// twoDimBudgets is a merged-budget payload where BOTH spend dimensions
// escalate into a compacting tier, so one step that crosses a compacting tier
// on BOTH dimensions must still yield exactly ONE compaction. compact_tiers
// index 0 (warn) does not compact; escalate and final do.
const twoDimBudgets = `{
  "tokens": 1000,
  "cost_usd": 0.06,
  "compact_max_turns": 0,
  "compact_dims": ["tokens", "cost_usd"],
  "compact_tiers": [false, true, true],
  "warnings": {
    "fractions": {"tokens": [0.25, 0.5, 0.75], "cost_usd": [0.25, 0.5, 0.75]},
    "messages": {
      "tokens": ["T-WARN-{pct}", "T-ESCALATE-{pct}", "T-FINAL-{pct}"],
      "cost_usd": ["C-WARN-{pct}", "C-ESCALATE-{pct}", "C-FINAL-{pct}"]
    }
  }
}`

// twoDimTurnOne / twoDimTurnTwo are the two completed turns the cross-dimension
// guard test needs: turn one crosses only the non-compacting WARN tier of the
// cost dimension (so the session stays alive with a queued warning turn and the
// token dimension is still below every tier), turn two crosses the compacting
// ESCALATE tier on BOTH dimensions on the SAME step.
const (
	twoDimTurnOne = `{"type":"result","subtype":"success","session_id":"sess-1","result":"ok","total_cost_usd":0.02,"usage":{"input_tokens":80,"output_tokens":20}}`
	twoDimTurnTwo = `{"type":"result","subtype":"success","session_id":"sess-1","result":"ok","total_cost_usd":0.02,"usage":{"input_tokens":400,"output_tokens":100}}`
)

// TestClaudeCompactFiresAtMostOncePerStepAcrossDimensions drives the AC
// literally on the LADDER path (not just through armCompact): when cost AND
// fresh tokens both reach a compacting tier on the SAME completed step, exactly
// one compact directive turn is written and exactly one compaction is recorded
// — while EVERY crossed tier's own warning is still injected, nothing compacts
// at session start, and the session stays live (a compaction is best-effort,
// never terminal).
func TestClaudeCompactFiresAtMostOncePerStepAcrossDimensions(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	h, done := budgetSession(t, fp, scheduler.ExecutionManifest{
		ExecutionID: "exec-two-dims",
		Goal:        "ship the two-dimension feature",
		Budgets:     []byte(twoDimBudgets),
	}, rec)

	fp.push(initLine)
	// Step 1: cost 0.02/0.06 = 0.33 (warn, injects but does NOT compact) and
	// fresh tokens 100/1000 = 0.10 (below every tier).
	fp.push(twoDimTurnOne)
	waitFor(t, func() bool {
		s := h.b.liveSession("exec-two-dims")
		return s != nil && s.stepSnapshot() >= 1
	}, "first completed turn")
	if got := turnsContaining(fp, "CONTEXT COMPACTION"); len(got) != 0 {
		t.Fatalf("compacted at session start: %v", got)
	}
	if len(turnsContaining(fp, "C-WARN-")) == 0 {
		t.Fatal("the crossed warn tier's own message was not injected")
	}

	// Step 2: fresh tokens 600/1000 = 0.60 (escalate) AND cost 0.04/0.06 = 0.67
	// (escalate) — two compacting tiers on ONE step.
	fp.push(twoDimTurnTwo)
	waitFor(t, func() bool {
		s := h.b.liveSession("exec-two-dims")
		return s != nil && s.compactionsSoFar() == 1
	}, "exactly one compaction on the two-dimension step")

	if got := turnsContaining(fp, "CONTEXT COMPACTION"); len(got) != 1 {
		t.Fatalf("compact directive turns = %d, want exactly one per step", len(got))
	}
	if got := budgetCompactedParts(rec); len(got) != 1 {
		t.Fatalf("compacted parts = %d, want exactly one per step", len(got))
	}
	if len(turnsContaining(fp, "T-ESCALATE-")) == 0 || len(turnsContaining(fp, "C-ESCALATE-")) == 0 {
		t.Fatal("a crossed escalate tier's own warning was not injected on the two-dimension step")
	}
	if !h.b.IsExecutionActive("exec-two-dims") {
		t.Fatal("the two-dimension step terminated the session instead of compacting it")
	}

	// The queued warning/directive turns drain, then the session terminates
	// normally with a single successful result.
	for i := 0; i < 4; i++ {
		fp.push(freeResult)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after the two-dimension compaction: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the two-dimension session never reached a terminal turn")
	}
	s := rec.snap()
	if len(s.results) != 1 || !s.results[0].succeeded {
		t.Fatalf("OnResult = %+v, want one success", s.results)
	}
}

// setStep is a test-only step setter (the run loop owns step in production).
func (s *session) setStep(n int) {
	s.mu.Lock()
	s.step = n
	s.mu.Unlock()
}

// doCompactErr exposes the arm guards as an error (test-only).
func (s *session) doCompactErr(ctx context.Context) error {
	if err := s.armCompact(); err != nil {
		return err
	}
	return s.writeCompactDirective(ctx, "test", s.remainingScope())
}

// newDirectSession builds a session whose subprocess is the given fake, so
// the compaction guards can be driven without a spawned run loop.
func newDirectSession(t *testing.T, manifest scheduler.ExecutionManifest, rec *captureCallbacks, fp *fakeProc) *session {
	t.Helper()
	h := newHarness(t, func() *fakeProc { return fp })
	if manifest.ProjectDir == "" {
		manifest.ProjectDir = t.TempDir()
	}
	if manifest.ModelRef == "" {
		manifest.ModelRef = "claude/anthropic/claude-sonnet-5"
	}
	h.b.SetSessionStore(rec.recordParts)
	s := newSession(h.b, manifest.ExecutionID, "t1", manifest, rec)
	s.proc = fp
	return s
}

// failWriteProc is a live ProcSession whose stdin writes always fail, so the
// compact path's error FIDELITY can be pinned: the session stays live, and a
// refused directive write surfaces its own cause rather than a misleading
// "no live session" diagnosis.
type failWriteProc struct{ ProcSession }

func (failWriteProc) WriteTurn([]byte) error { return errors.New("stdin pipe broken") }

// TestClaudeCompactSurfacesTheWriteCause proves the compact API reports WHY a
// live session could not be compacted (and records no phantom compaction): a
// transport failure must not be reported as "no live session".
func TestClaudeCompactSurfacesTheWriteCause(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	s := newDirectSession(t, scheduler.ExecutionManifest{
		ExecutionID: "exec-write-fail",
		Budgets:     []byte(costGatedBudgets),
	}, rec, fp)
	s.setStep(2) // past the min-turn floor: the guards arm the compaction
	s.proc = failWriteProc{ProcSession: fp}

	err := s.Compact(context.Background(), "anthropic", "claude-sonnet-5", "scope")
	if err == nil {
		t.Fatal("a refused compact directive write must surface an error")
	}
	if !strings.Contains(err.Error(), "stdin pipe broken") {
		t.Fatalf("error = %v, want the underlying write cause", err)
	}
	if !strings.Contains(err.Error(), "exec-write-fail") {
		t.Fatalf("error = %v, want the execution named", err)
	}
	if got := budgetCompactedParts(rec); len(got) != 0 {
		t.Fatalf("a failed compact directive must not be recorded as a compaction: %+v", got)
	}
}

// TestClaudeBudgetLadderComesFromTheMergedJSON proves there is exactly ONE
// source of truth: the values the claude bridge gates on are the same values
// opencode's parseBudgetSpec resolves from the same merged JSON.
func TestClaudeBudgetLadderComesFromTheMergedJSON(t *testing.T) {
	rec := &captureCallbacks{}
	s := newDirectSession(t, scheduler.ExecutionManifest{
		ExecutionID: "exec-ladder",
		Budgets:     []byte(costGatedBudgets),
	}, rec, newFakeProc())

	if got := s.ladderFor().Message("cost_usd", 0.67); !strings.Contains(got, "TENANT-ESCALATE-67") {
		t.Fatalf("ladder message = %q, want the tenant's own escalate text", got)
	}
	// The opencode-side ladder resolved from the same bytes agrees verbatim.
	other := opencode.ParseBudgetLadder([]byte(costGatedBudgets))
	if other.Message("cost_usd", 0.67) != s.ladderFor().Message("cost_usd", 0.67) {
		t.Fatal("the claude ladder disagrees with the opencode ladder on the same merged JSON")
	}
	if tok, ok := s.ladderFor().EffectiveTokens(); ok {
		t.Fatalf("explicit tokens:0 must disable the dimension, got %v", tok)
	}
	if cost, ok := s.ladderFor().EffectiveCost(); !ok || cost != 0.06 {
		t.Fatalf("cost gate = %v/%v, want 0.06/enabled", cost, ok)
	}
	if !s.ladderFor().CompactsDim("cost_usd") || s.ladderFor().CompactsDim("tokens") {
		t.Fatal("compact_dims from the merged JSON was not honored")
	}
	if !s.ladderFor().CompactsAt("escalate") || s.ladderFor().CompactsAt("warn") {
		t.Fatal("compact_tiers from the merged JSON was not honored")
	}
}

// TestClaudeBudgetSpendIsCacheAware pins the accumulator semantics the cost
// gate depends on: cache reads are PRICED (they cost real money) but never
// counted as fresh work.
func TestClaudeBudgetSpendIsCacheAware(t *testing.T) {
	rec := &captureCallbacks{}
	s := newDirectSession(t, scheduler.ExecutionManifest{
		ExecutionID: "exec-spend",
		Budgets:     []byte(`{"tokens":1000,"cost_usd":1.0}`),
	}, rec, newFakeProc())

	s.spendFor().AddFromUsage(10, 5, 0, 4000, 0.2)
	if got := s.spendFor().FreshTokens(); got != 15 {
		t.Fatalf("fresh tokens = %v, want 15 (cache reads excluded)", got)
	}
	if got := s.spendFor().TotalTokens(); got != 4015 {
		t.Fatalf("total tokens = %v, want 4015 (cache reads included)", got)
	}
	if got := s.spendFor().CostUSD(); got != 0.2 {
		t.Fatalf("cost = %v, want the provider-priced 0.2", got)
	}
	if frac := s.spendFor().Fraction(s.ladderFor(), "tokens", 0, 0); frac >= 0.1 {
		t.Fatalf("token fraction = %v, want the fresh-token share only", frac)
	}
	if frac := s.spendFor().Fraction(s.ladderFor(), "cost_usd", 0, 0); frac != 0.2 {
		t.Fatalf("cost fraction = %v, want 0.2", frac)
	}
}

// TestClaudePackageHoldsNoBudgetThresholdsOrLadderText is the "one source of
// truth" guard: no production file in the claude package may restate a
// ladder threshold or the ladder's message copy — all of it comes from the
// merged budget JSON through the shared facade.
func TestClaudePackageHoldsNoBudgetThresholdsOrLadderText(t *testing.T) {
	banned := []string{
		"You have used", "{pct}", "compact_max_turns", "compact_dims", "compact_tiers",
		"warning_frac", "defaultWarnFracs", "defaultWarnMsgs",
	}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // comments may NAME the key; code may not restate it
			}
			if idx := strings.Index(trimmed, "//"); idx >= 0 {
				trimmed = trimmed[:idx]
			}
			for _, needle := range banned {
				if strings.Contains(trimmed, needle) {
					t.Errorf("%s restates budget-ladder configuration (%q): the ladder must come from the merged budget JSON", name, needle)
				}
			}
		}
	}
}

// TestClaudeCompactionIsNeverHTTP proves the compact is NOT implemented by any
// HTTP call: no serve, no endpoint, no client — only the session's own stdin
// write path.
func TestClaudeCompactionIsNeverHTTP(t *testing.T) {
	for _, name := range []string{"compact.go", "session.go", "adapter.go"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"http.Post", "http.Client", "/session/", "/summarize", "Do(req"} {
			if strings.Contains(string(body), needle) {
				t.Errorf("%s contains an HTTP compaction path (%q): claude has no serve", name, needle)
			}
		}
	}
}
