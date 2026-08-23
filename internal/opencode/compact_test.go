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
	spec := budgetSpec{costUSD: float64Ptr(4.0)}
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

// TestBudgetBreachedNoGate verifies that when neither cost_usd nor tokens is
// configured, the gate never trips (wall-clock + stall remain the only live
// killers).
func TestBudgetBreachedNoGate(t *testing.T) {
	acc := &budgetAccumulator{costUSD: 999, prompt: 1_000_000, completion: 1_000_000, cacheRead: 1_000_000}
	if budgetBreached(budgetSpec{}, acc) {
		t.Fatal("expected no breach when no budget gate is configured")
	}
	if budgetBreached(budgetSpec{}, nil) {
		t.Fatal("expected no breach on a nil accumulator")
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

func float64Ptr(f float64) *float64 { return &f }
