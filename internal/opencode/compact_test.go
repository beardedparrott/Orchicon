package opencode

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestBudgetBreachedCostGate verifies the PRIMARY gate: accumulated
// cache-aware cost trips when it reaches the configured cost_usd, with no
// margin or fraction — the configured number is the threshold.
func TestBudgetBreachedCostGate(t *testing.T) {
	// tokens explicitly disabled so this test isolates the cost gate — an
	// unset tokens field would otherwise resolve to the built-in 1,000,000
	// default and mask the assertion below.
	spec := budgetSpec{costUSD: float64Ptr(4.0), tokens: float64Ptr(0)}
	// At the threshold → breach.
	if !budgetBreached(spec, &budgetAccumulator{costUSD: 4.0}) {
		t.Fatal("expected breach at exactly the cost threshold")
	}
	// Above the threshold → breach.
	if !budgetBreached(spec, &budgetAccumulator{costUSD: 4.01}) {
		t.Fatal("expected breach above the cost threshold")
	}
	// Below the threshold → no breach (even with huge token counts: cost,
	// not raw tokens, is the primary signal).
	if budgetBreached(spec, &budgetAccumulator{costUSD: 3.99, prompt: 1_000_000, completion: 500_000}) {
		t.Fatal("expected no breach below the cost threshold regardless of tokens")
	}
}

// TestBudgetBreachedTokenFallback verifies the token fallback (used when no
// cost_usd is configured) counts fresh input+output+reasoning PLUS cache_read
// weighted by the cache-discount factor — so cache is never excluded from the
// estimate, but cheap-cache providers do not false-fire on cache bloat.
func TestBudgetBreachedTokenFallback(t *testing.T) {
	spec := budgetSpec{tokens: float64Ptr(300), cacheDiscount: 0.1}
	// 100 input + 50 output + 10 reasoning + 1000 cache*0.1 = 260 budgeted.
	acc := &budgetAccumulator{prompt: 100, completion: 50, reasoning: 10, cacheRead: 1000}
	if budgetBreached(spec, acc) {
		t.Fatal("expected no breach: 260 budgeted < 300 token budget")
	}
	// The cache IS discounted money: without the discount the same sample
	// would count 1160 and breach — the discount keeps cheap cache under.
	if !budgetBreached(budgetSpec{tokens: float64Ptr(250), cacheDiscount: 0.1}, acc) {
		t.Fatal("expected breach once budgeted spend reaches the token budget")
	}
	// A DeepSeek cache-heavy session (1.1M cache read) with a large token
	// budget must NOT fire early.
	if budgetBreached(budgetSpec{tokens: float64Ptr(200_000), cacheDiscount: 0.1},
		&budgetAccumulator{prompt: 5_000, completion: 2_000, reasoning: 100, cacheRead: 1_100_000}) {
		t.Fatal("expected no false-fire on cheap cache bloat")
	}
	// A paid-cache model resending a big prefix accrues real fresh + cache
	// cost and should count toward the budget exactly. Using the higher
	// Claude-style cache-discount (0.3): 80k + 10k + 90k*0.3 = 117k ≥ 100k.
	paid := &budgetAccumulator{prompt: 80_000, completion: 10_000, reasoning: 0, cacheRead: 90_000}
	if !budgetBreached(budgetSpec{tokens: float64Ptr(100_000), cacheDiscount: 0.3}, paid) {
		t.Fatal("expected paid-cache resend to trip the token budget")
	}
}

// TestBudgetBreachedBuiltInDefaults verifies that when neither cost_usd nor
// tokens is configured, the gate falls back to the built-in defaults
// (defaultCompactCostUSD / defaultCompactTokens) instead of never
// tripping — matching what Settings has always told operators an empty
// field falls back to (frontend/src/routes/settings.tsx DefaultsTab).
// A nil accumulator (no usage recorded yet) never breaches regardless.
func TestBudgetBreachedBuiltInDefaults(t *testing.T) {
	if budgetBreached(budgetSpec{}, nil) {
		t.Fatal("expected no breach on a nil accumulator")
	}
	// Below both built-in defaults → no breach.
	if budgetBreached(budgetSpec{}, &budgetAccumulator{costUSD: defaultCompactCostUSD - 0.01, prompt: 100}) {
		t.Fatal("expected no breach below the built-in cost default")
	}
	// Cost crosses the built-in default → breach.
	if !budgetBreached(budgetSpec{}, &budgetAccumulator{costUSD: defaultCompactCostUSD}) {
		t.Fatalf("expected breach at the built-in cost default ($%v) with no configured budget", defaultCompactCostUSD)
	}
	// No cost recorded, but fresh tokens cross the built-in 1,000,000
	// default → breach via the token fallback.
	if !budgetBreached(budgetSpec{}, &budgetAccumulator{prompt: 900_000, completion: 100_000}) {
		t.Fatal("expected breach at the built-in token default (1,000,000) with no configured budget")
	}
}

// TestBudgetBreachedExplicitZeroDisables verifies that an explicit 0 (or
// negative) cost_usd/tokens value opts a worker/tenant OUT of that gate
// entirely, rather than resolving to the built-in default — mirroring
// wallClockDeadline's "0 disables" convention so operators have a way to
// turn a dimension off instead of just leaving it unset.
func TestBudgetBreachedExplicitZeroDisables(t *testing.T) {
	zero := float64Ptr(0)
	acc := &budgetAccumulator{costUSD: 999, prompt: 5_000_000, completion: 5_000_000}
	if budgetBreached(budgetSpec{costUSD: zero, tokens: zero}, acc) {
		t.Fatal("expected no breach when both gates are explicitly disabled (0)")
	}
}

// TestBudgetAccumulatorAdd verifies the accumulator folds a step_finish's
// tokens + cost exactly (same bucket semantics as recordUsage) and counts
// the step.
func TestBudgetAccumulatorAdd(t *testing.T) {
	acc := &budgetAccumulator{}
	acc.add(map[string]any{
		"input": float64(100), "output": float64(50), "reasoning": float64(10),
		"cache": map[string]any{"read": float64(1000), "write": float64(5)},
	}, 0.012)
	if acc.costUSD != 0.012 || acc.prompt != 100 || acc.completion != 50 || acc.reasoning != 10 || acc.cacheRead != 1000 || acc.steps != 1 {
		t.Fatalf("unexpected accumulator state after add: %+v", acc)
	}
}

// TestParseBudgetSpec verifies the single merged-budget parse yields the
// cost + token + wall-clock gates, with empty/unparseable budgets yielding
// no gate (built-in defaults apply downstream).
func TestParseBudgetSpec(t *testing.T) {
	spec := parseBudgetSpec([]byte(`{"cost_usd":0.5,"tokens":1000,"wall_clock_seconds":3600}`))
	if spec.costUSD == nil || *spec.costUSD != 0.5 {
		t.Fatalf("costUSD = %v, want 0.5", spec.costUSD)
	}
	if spec.tokens == nil || *spec.tokens != 1000 {
		t.Fatalf("tokens = %v, want 1000", spec.tokens)
	}
	if spec.wallClockSeconds == nil || *spec.wallClockSeconds != 3600 {
		t.Fatalf("wallClockSeconds = %v, want 3600", spec.wallClockSeconds)
	}
	// Empty budgets → no gate fields (wall-clock default applies downstream).
	empty := parseBudgetSpec(nil)
	if empty.costUSD != nil || empty.tokens != nil || empty.wallClockSeconds != nil {
		t.Fatalf("empty budgets must yield no gate fields, got %+v", empty)
	}
	// Unparseable → same as empty (mirrors wallClockDeadline's default fallback).
	bad := parseBudgetSpec([]byte(`not-json`))
	if bad.costUSD != nil || bad.tokens != nil || bad.wallClockSeconds != nil {
		t.Fatalf("unparseable budgets must yield no gate fields, got %+v", bad)
	}
}

// compactTestServer spins an httptest server that records POST
// /session/{id}/summarize calls (method, path, body) so a test can assert the
// Compact client contract and the session-run gate behaviour.
type compactRecorder struct {
	mu     sync.Mutex
	calls  []compactCall
	status int
}

type compactCall struct {
	path string
	body map[string]any
}

func newCompactRecorder(status int) *compactRecorder {
	return &compactRecorder{status: status}
}

func (c *compactRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/summarize") {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.calls = append(c.calls, compactCall{path: r.URL.Path, body: body})
		c.mu.Unlock()
	}
	w.WriteHeader(c.status)
}

func (c *compactRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *compactRecorder) lastCall() compactCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return compactCall{}
	}
	return c.calls[len(c.calls)-1]
}

// TestSessionClientCompact verifies Compact POSTs /session/{id}/summarize with
// the resolved provider/model (camelCase keys, per the verified live contract).
func TestSessionClientCompact(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	client := NewSessionClient(srv.URL, "", "")

	if err := client.Compact(context.Background(), "sess-1", "opencode", "deepseek-v4-flash-free"); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	call := rec.lastCall()
	if call.path != "/session/sess-1/summarize" {
		t.Fatalf("path = %q, want /session/sess-1/summarize", call.path)
	}
	if call.body["providerID"] != "opencode" || call.body["modelID"] != "deepseek-v4-flash-free" {
		t.Fatalf("body = %v, want {providerID:opencode, modelID:deepseek-v4-flash-free}", call.body)
	}
}

// TestSessionClientCompactError verifies a non-2xx summarize response is
// returned as an error (best-effort callers log it and continue).
func TestSessionClientCompactError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	client := NewSessionClient(srv.URL, "", "")
	if err := client.Compact(context.Background(), "sess-1", "opencode", "m"); err == nil {
		t.Fatal("expected an error on a 500 summarize response")
	}
}

// TestMaybeCompactMinTurnFloor verifies the gate never fires before the
// minimum-turn floor (compact-at-start is impossible).
func TestMaybeCompactMinTurnFloor(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-compact-floor", TenantID: "tnt_dev"},
		manifest:   scheduler.ExecutionManifest{ModelRef: "opencode/deepseek-v4-flash-free"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{costUSD: 5.0, steps: 1},
		budgetSpec: budgetSpec{costUSD: float64Ptr(4.0)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "2")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	r.maybeCompact()
	if rec.count() != 0 {
		t.Fatalf("expected no compact below the min-turn floor, got %d", rec.count())
	}
}

// TestMaybeCompactFiresOncePerExecution verifies a budget breach after the
// floor compacts once, and the per-execution cap (default 1) prevents a
// compact loop even though cumulative spend stays tripped.
func TestMaybeCompactFiresOncePerExecution(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-compact-once", TenantID: "tnt_dev"},
		manifest:   scheduler.ExecutionManifest{ModelRef: "opencode/deepseek-v4-flash-free"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{costUSD: 5.0, steps: 2},
		budgetSpec: budgetSpec{costUSD: float64Ptr(4.0)},
		done:       make(chan struct{}),
	}
	// Note: parseEvent feeds budget.steps; here we set it directly. Advance
	// the accumulator to simulate a session that stays over budget for many
	// steps (cumulative spend) — the cap must hold it to a single compact.
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	for r.budget.steps < 8 {
		r.maybeCompact()
		r.budget.steps++
	}
	if rec.count() != 1 {
		t.Fatalf("expected exactly 1 compact (per-execution cap), got %d", rec.count())
	}
}

// TestMaybeCompactReArmsAcrossForwardProgress verifies the re-arm rule: with a
// higher per-execution cap, compacts are separated by the minimum-turn floor
// across normal forward progress (never back-to-back on the same step), so a
// fresh post-compact summary gets to run before the gate can fire again.
func TestMaybeCompactReArmsAcrossForwardProgress(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-compact-rearm", TenantID: "tnt_dev"},
		manifest:   scheduler.ExecutionManifest{ModelRef: "opencode/deepseek-v4-flash-free"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{costUSD: 5.0, steps: 1},
		budgetSpec: budgetSpec{costUSD: float64Ptr(4.0)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "2")
	t.Setenv("ORCHICON_COMPACT_MAX", "5")
	r.maybeCompact() // steps=1 < 2 → no compact (floor)
	if rec.count() != 0 {
		t.Fatalf("expected no compact at step 1 (floor), got %d", rec.count())
	}
	r.budget.steps = 2
	r.maybeCompact() // floor met, breach → compact #1
	if rec.count() != 1 {
		t.Fatalf("expected compact #1 at step 2, got %d", rec.count())
	}
	r.budget.steps = 3
	r.maybeCompact() // within re-arm window (2+2) → no compact
	if rec.count() != 1 {
		t.Fatalf("expected no compact at step 3 (re-arm window), got %d", rec.count())
	}
	r.budget.steps = 4
	r.maybeCompact() // re-armed (>= 4) → compact #2
	if rec.count() != 2 {
		t.Fatalf("expected compact #2 at step 4 (re-armed), got %d", rec.count())
	}
	// Verify the compact resolved the model ref and used the session id.
	call := rec.lastCall()
	if call.body["providerID"] != "opencode" || call.body["modelID"] != "deepseek-v4-flash-free" {
		t.Fatalf("compact called with wrong provider/model: %v", call.body)
	}
}

// TestMaybeCompactNoBreach verifies no compact fires when spend is under the
// budget.
func TestMaybeCompactNoBreach(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-compact-nobreach", TenantID: "tnt_dev"},
		manifest:   scheduler.ExecutionManifest{ModelRef: "opencode/deepseek-v4-flash-free"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{costUSD: 2.0, prompt: 100, completion: 10, steps: 3},
		budgetSpec: budgetSpec{costUSD: float64Ptr(4.0)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	r.maybeCompact()
	if rec.count() != 0 {
		t.Fatalf("expected no compact under budget, got %d", rec.count())
	}
}

// TestMaybeCompactMalformedModelRef verifies a malformed model ref makes the
// compact a no-op (best-effort) rather than calling the serve with a bad ref.
func TestMaybeCompactMalformedModelRef(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-compact-badref", TenantID: "tnt_dev"},
		manifest:   scheduler.ExecutionManifest{ModelRef: "nomodelslash"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "nomodelslash",
		budget:     &budgetAccumulator{costUSD: 5.0, steps: 3},
		budgetSpec: budgetSpec{costUSD: float64Ptr(4.0)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	r.maybeCompact()
	if rec.count() != 0 {
		t.Fatalf("expected no compact on a malformed model ref, got %d", rec.count())
	}
}

// TestMaybeCompactTurnCountTriggerWithNoBudgetBreach verifies the turn-count
// gate fires compaction on its own cadence even when cost/tokens never
// breach — the scenario a cheap-cache provider (DeepSeek) produces: many
// turns, individually inexpensive, but cumulatively resending an
// ever-growing cached prefix. costUSD/tokens are explicitly disabled (0) so
// only the turn-count gate is live.
func TestMaybeCompactTurnCountTriggerWithNoBudgetBreach(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-compact-turns", TenantID: "tnt_dev"},
		manifest:  scheduler.ExecutionManifest{ModelRef: "opencode/deepseek-v4-flash-free"},
		client:    NewSessionClient(srv.URL, "", ""),
		sessionID: "sess-1",
		modelRef:  "opencode/deepseek-v4-flash-free",
		budget:    &budgetAccumulator{steps: 1},
		budgetSpec: budgetSpec{
			costUSD: float64Ptr(0), tokens: float64Ptr(0), compactMaxTurns: float64Ptr(4),
		},
		done: make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "5")
	for step := 1; step <= 9; step++ {
		r.budget.steps = step
		r.maybeCompact()
	}
	// Turns 1-3: below the 4-turn cap → no compact. Turn 4: compact #1.
	// Turns 5-7: within the re-arm window of the NEXT floor, but the
	// turn-count gate re-checks turnsSinceLastCompact every call, so it
	// re-fires once turnsSinceLastCompact reaches 4 again at turn 8.
	if rec.count() != 2 {
		t.Fatalf("expected 2 turn-count-triggered compacts over 9 steps (cap=4), got %d", rec.count())
	}
	firstCall := rec.calls[0]
	if firstCall.body["providerID"] != "opencode" || firstCall.body["modelID"] != "deepseek-v4-flash-free" {
		t.Fatalf("unexpected compact call body: %v", firstCall.body)
	}
}

// TestMaybeCompactTurnCountGateDisabled verifies an explicit 0 for
// compact_max_turns disables the turn-count trigger, leaving only
// cost/tokens as live signals (so a worker/tenant can opt back into the
// pre-fix one-shot-only-on-breach behavior for a specific dimension).
func TestMaybeCompactTurnCountGateDisabled(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-compact-turns-off", TenantID: "tnt_dev"},
		manifest:  scheduler.ExecutionManifest{ModelRef: "opencode/deepseek-v4-flash-free"},
		client:    NewSessionClient(srv.URL, "", ""),
		sessionID: "sess-1",
		modelRef:  "opencode/deepseek-v4-flash-free",
		budget:    &budgetAccumulator{steps: 1},
		budgetSpec: budgetSpec{
			costUSD: float64Ptr(0), tokens: float64Ptr(0), compactMaxTurns: float64Ptr(0),
		},
		done: make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "5")
	for step := 1; step <= 20; step++ {
		r.budget.steps = step
		r.maybeCompact()
	}
	if rec.count() != 0 {
		t.Fatalf("expected no compacts with all gates explicitly disabled, got %d", rec.count())
	}
}

// TestEffectiveCompactMaxTurns verifies the turn-count gate resolver: unset
// → built-in default (12), configured → that value, explicit 0/negative →
// disabled.
func TestEffectiveCompactMaxTurns(t *testing.T) {
	if turns, ok := effectiveCompactMaxTurns(budgetSpec{}); !ok || turns != defaultCompactMaxTurns {
		t.Fatalf("unset compactMaxTurns = (%d, %v), want (%d, true)", turns, ok, defaultCompactMaxTurns)
	}
	if turns, ok := effectiveCompactMaxTurns(budgetSpec{compactMaxTurns: float64Ptr(15)}); !ok || turns != 15 {
		t.Fatalf("configured compactMaxTurns = (%d, %v), want (15, true)", turns, ok)
	}
	if _, ok := effectiveCompactMaxTurns(budgetSpec{compactMaxTurns: float64Ptr(0)}); ok {
		t.Fatal("expected explicit 0 to disable the turn-count gate")
	}
	if _, ok := effectiveCompactMaxTurns(budgetSpec{compactMaxTurns: float64Ptr(-1)}); ok {
		t.Fatal("expected explicit negative value to disable the turn-count gate")
	}
}

// TestEffectiveToolCallLimit verifies the tool-call HARD-abort gate
// resolver: unset → built-in default (100), configured → that value,
// explicit 0/negative → disabled.
func TestEffectiveToolCallLimit(t *testing.T) {
	if limit, ok := effectiveToolCallLimit(budgetSpec{}); !ok || limit != defaultToolCallLimit {
		t.Fatalf("unset toolCallCount = (%d, %v), want (%d, true)", limit, ok, defaultToolCallLimit)
	}
	if limit, ok := effectiveToolCallLimit(budgetSpec{toolCallCount: float64Ptr(25)}); !ok || limit != 25 {
		t.Fatalf("configured toolCallCount = (%d, %v), want (25, true)", limit, ok)
	}
	if _, ok := effectiveToolCallLimit(budgetSpec{toolCallCount: float64Ptr(0)}); ok {
		t.Fatal("expected explicit 0 to disable the tool-call limit")
	}
	if _, ok := effectiveToolCallLimit(budgetSpec{toolCallCount: float64Ptr(-1)}); ok {
		t.Fatal("expected explicit negative value to disable the tool-call limit")
	}
}

// TestCheckToolCallLimitAbortsAtBuiltInDefault verifies checkToolCallLimit
// hard-aborts the execution once stats.toolUses reaches the built-in
// default (100) when no tool_call_count is configured — making Settings'
// long-standing "Empty = built-in default (100)" claim actually true.
func TestCheckToolCallLimitAbortsAtBuiltInDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-toolcall-default", TenantID: "tnt_dev"},
		callbacks: &liveCallbacks{},
		client:    NewSessionClient(srv.URL, "", ""),
		sessionID: "sess-1",
		done:      make(chan struct{}),
		stats:     &execStreamState{toolUses: defaultToolCallLimit - 1},
	}
	r.checkToolCallLimit() // 99 < 100 → no abort
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("expected no abort below the built-in tool-call default")
	}
	r.stats.toolUses = defaultToolCallLimit
	r.checkToolCallLimit() // 100 >= 100 → abort
	r.mu.Lock()
	fin, ok, resultErr := r.finished, r.resultOk, r.resultErr
	r.mu.Unlock()
	if !fin {
		t.Fatal("expected the execution to finish once the tool-call limit is reached")
	}
	if ok {
		t.Fatal("a tool-call limit breach must fail the execution")
	}
	if resultErr != "tool_call_limit_exceeded" {
		t.Fatalf("resultErr = %q, want tool_call_limit_exceeded", resultErr)
	}
}

// TestCheckToolCallLimitDisabled verifies an explicit 0 for tool_call_count
// disables the hard-abort gate entirely, even with a huge tool-call count.
func TestCheckToolCallLimitDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-toolcall-disabled", TenantID: "tnt_dev"},
		callbacks:  &liveCallbacks{},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		done:       make(chan struct{}),
		stats:      &execStreamState{toolUses: 10_000},
		budgetSpec: budgetSpec{toolCallCount: float64Ptr(0)},
	}
	r.checkToolCallLimit()
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("expected no abort with the tool-call limit explicitly disabled")
	}
}

// TestCheckToolCallLimitNilStatsNoop verifies a nil stats pointer (should
// never happen in production — adapter.go always initializes it — but
// matches maybeCompact's defensive nil-budget guard) is a safe no-op rather
// than a panic.
func TestCheckToolCallLimitNilStatsNoop(t *testing.T) {
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-toolcall-nilstats", TenantID: "tnt_dev"},
		done:      make(chan struct{}),
	}
	r.checkToolCallLimit()
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("expected a no-op with nil stats, not a finish")
	}
}

func float64Ptr(f float64) *float64 { return &f }
