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
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestBudgetBreachedCostGate verifies the PRIMARY gate: accumulated cache-aware cost trips when it
// reaches the configured cost_usd, with no margin or fraction.
func TestBudgetBreachedCostGate(t *testing.T) {
	// tokens explicitly disabled so this test isolates the cost gate — an unset tokens field would
	// otherwise resolve to the built-in 1,000,000 default and mask the assertion below.
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

// TestBudgetBreachedTokenFreshWeight verifies the token gate counts FRESH tokens only
// (prompt+completion+reasoning) and EXCLUDES cache reads.
func TestBudgetBreachedTokenFreshWeight(t *testing.T) {
	// 100 input + 50 output + 10 reasoning = 160 fresh; cache 40 is excluded.
	acc := &budgetAccumulator{prompt: 100, completion: 50, reasoning: 10, cacheRead: 40}
	if budgetBreached(budgetSpec{tokens: float64Ptr(200)}, acc) {
		t.Fatal("expected no breach: 160 fresh < 200 token budget (cache excluded)")
	}
	if !budgetBreached(budgetSpec{tokens: float64Ptr(160)}, acc) {
		t.Fatal("expected breach once fresh token count reaches the token budget")
	}
	// A cache-heavy session with LOTS of cache reads but small fresh tokens must NOT breach by cache
	// alone.
	if budgetBreached(budgetSpec{tokens: float64Ptr(200_000)},
		&budgetAccumulator{prompt: 5_000, completion: 2_000, reasoning: 100, cacheRead: 1_100_000}) {
		t.Fatal("expected cache reads to be excluded from the token budget")
	}
	// But a cache-heavy session can still breach via the COST gate, which prices cache reads (cheap but
	// not free).
	if !budgetBreached(budgetSpec{costUSD: float64Ptr(0.01), tokens: float64Ptr(200_000)},
		&budgetAccumulator{prompt: 5_000, completion: 2_000, reasoning: 100, cacheRead: 1_100_000, costUSD: 0.02}) {
		t.Fatal("expected cache-heavy session to breach the cost gate")
	}
}

// THERE ARE NO BUILT-IN SPEND CEILINGS ANY MORE, and this pins it.
func TestNoConfiguredBudgetMeansNoGate(t *testing.T) {
	if budgetBreached(budgetSpec{}, nil) {
		t.Fatal("expected no breach on a nil accumulator")
	}
	// An ENORMOUS turn under an empty budget must not breach: unset = no gate.
	huge := &budgetAccumulator{costUSD: 5000, prompt: 50_000_000, completion: 5_000_000}
	if budgetBreached(budgetSpec{}, huge) {
		t.Fatal("an empty budget breached on a spend ceiling nobody configured — the old defaults are back")
	}
	for name, frac := range map[string]float64{
		"tokens":          budgetSpec{}.fraction(dimTokens, huge, 0, 0),
		"cost_usd":        budgetSpec{}.fraction(dimCost, huge, 0, 0),
		"tool_call_count": budgetSpec{}.fraction(dimTools, huge, 0, 1_000_000),
		"wall_clock":      budgetSpec{}.fraction(dimTime, huge, 400*24*time.Hour, 0),
	} {
		if frac != -1 {
			t.Errorf("%s resolves to fraction %v with no configured limit, want -1 (no effective limit)", name, frac)
		}
	}
}

// AND SETTING ONE STILL WORKS — the defaults are gone, not the feature. This is
// the half that must not regress: an operator who sets a ceiling gets it.
func TestConfiguredCeilingsStillApply(t *testing.T) {
	spec := parseBudgetSpec([]byte(`{"tokens":1000,"cost_usd":2,"tool_call_count":50,"wall_clock_seconds":600}`))
	over := &budgetAccumulator{costUSD: 2, prompt: 1000}
	if !budgetBreached(spec, over) {
		t.Fatal("a configured ceiling must still breach when crossed")
	}
	if got := spec.fraction(dimTools, &budgetAccumulator{}, 0, 50); got != 1 {
		t.Errorf("tool_call_count at its configured 50 = fraction %v, want 1", got)
	}
	if got := spec.fraction(dimTime, &budgetAccumulator{}, 600*time.Second, 0); got != 1 {
		t.Errorf("wall_clock_seconds at its configured 600 = fraction %v, want 1", got)
	}
	// Below the configured ceilings nothing fires.
	if budgetBreached(spec, &budgetAccumulator{costUSD: 1.99, prompt: 999}) {
		t.Fatal("a budget below every configured ceiling must not breach")
	}
}

// TestBudgetBreachedExplicitZeroDisables verifies that an explicit 0 (or negative) cost_usd/tokens
// value opts a worker/tenant OUT of that gate entirely.
func TestBudgetBreachedExplicitZeroDisables(t *testing.T) {
	zero := float64Ptr(0)
	acc := &budgetAccumulator{costUSD: 999, prompt: 5_000_000, completion: 5_000_000}
	if budgetBreached(budgetSpec{costUSD: zero, tokens: zero}, acc) {
		t.Fatal("expected no breach when both gates are explicitly disabled (0)")
	}
}

// TestBudgetAccumulatorAdd verifies the accumulator folds a step_finish's tokens + cost exactly
// (same bucket semantics as recordUsage) and counts the step.
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

// TestParseBudgetSpec verifies the single merged-budget parse yields the cost + token + wall-clock
// gates AND the optional warning ladder (fractions + messages).
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

// TestParseBudgetSpecWarnings verifies the optional `warnings` block is parsed into the ladder
// thresholds + message templates, with defaults falling back to the built-in schedule.
func TestParseBudgetSpecWarnings(t *testing.T) {
	spec := parseBudgetSpec([]byte(`{
	  "tokens": 1000,
	  "warnings": {
	    "fractions": {"tokens": [0.4, 0.6, 0.8], "tool_call_count": [0.3, 0.5, 0.7]},
	    "messages": {"tokens": ["warn", "esc", "final"]}
	  }
	}`))
	// tokens fractions overridden.
	if spec.warnFracs[dimTokens][0] != 0.4 || spec.warnFracs[dimTokens][2] != 0.8 {
		t.Fatalf("tokens fractions = %v, want [0.4, 0.6, 0.8]", spec.warnFracs[dimTokens])
	}
	// tools fractions overridden; cost/time remain default.
	if spec.warnFracs[dimTools][1] != 0.5 {
		t.Fatalf("tools escalate fraction = %v, want 0.5", spec.warnFracs[dimTools][1])
	}
	if spec.warnFracs[dimCost][0] != 0.25 {
		t.Fatalf("cost warn fraction = %v, want default 0.25", spec.warnFracs[dimCost][0])
	}
	// messages override for tokens; cost falls back to built-in.
	if spec.warnMsgs[dimTokens][0] != "warn" || spec.warnMsgs[dimTokens][2] != "final" {
		t.Fatalf("tokens messages = %v, want [warn, esc, final]", spec.warnMsgs[dimTokens])
	}
	if !strings.Contains(spec.warnMsgs[dimCost][0], "cost budget") {
		t.Fatalf("cost message should fall back to the built-in default, got %q", spec.warnMsgs[dimCost][0])
	}
}

// TestParseBudgetSpecCompactTiers verifies the optional `compact_tiers` [warn, escalate, final]
// policy is parsed, defaulting to the built-in {false, true.
func TestParseBudgetSpecCompactTiers(t *testing.T) {
	// Absent → built-in default: warn off, escalate + final on.
	def := parseBudgetSpec([]byte(`{"tokens":1000}`))
	if def.compactTiers != [3]bool{false, true, true} {
		t.Fatalf("default compactTiers = %v, want [false true true]", def.compactTiers)
	}
	// Explicit override honored.
	spec := parseBudgetSpec([]byte(`{"tokens":1000,"compact_tiers":[true,false,true]}`))
	if spec.compactTiers != [3]bool{true, false, true} {
		t.Fatalf("compactTiers = %v, want [true false true]", spec.compactTiers)
	}
	if !spec.compactsAt(levelWarn) || spec.compactsAt(levelEscalate) || !spec.compactsAt(levelFinal) {
		t.Fatalf("compactsAt mismatch: warn=%v escalate=%v final=%v", spec.compactsAt(levelWarn), spec.compactsAt(levelEscalate), spec.compactsAt(levelFinal))
	}
	// All off → nothing compacts.
	allOff := parseBudgetSpec([]byte(`{"tokens":1000,"compact_tiers":[false,false,false]}`))
	if allOff.compactsAt(levelWarn) || allOff.compactsAt(levelEscalate) || allOff.compactsAt(levelFinal) {
		t.Fatalf("all-off tiers must not compact")
	}
}

// TestParseBudgetSpecCompactDims verifies the per-dimension compaction policy: absent → built-in
// default (tokens + cost compact; tools + time do not).
func TestParseBudgetSpecCompactDims(t *testing.T) {
	def := parseBudgetSpec([]byte(`{"tokens":1000}`))
	if def.compactDims != [dimCount]bool{true, true, false, false} {
		t.Fatalf("default compactDims = %v, want [tokens,cost] only", def.compactDims)
	}
	if !def.compactsDim(dimTokens) || !def.compactsDim(dimCost) {
		t.Fatal("default must permit compaction on tokens + cost")
	}
	if def.compactsDim(dimTools) || def.compactsDim(dimTime) {
		t.Fatal("default must NOT permit compaction on tools or time")
	}
	// An explicit array is the complete allowed set.
	spec := parseBudgetSpec([]byte(`{"tokens":1000,"compact_dims":["tokens","cost_usd","tool_call_count"]}`))
	if !spec.compactsDim(dimTools) {
		t.Fatal("explicit compact_dims should permit tool_call_count")
	}
	if spec.compactsDim(dimTime) {
		t.Fatal("wall_clock must stay out of an explicit compact_dims set")
	}
	// Unknown names are ignored → a present array with no valid dims disables all.
	spec2 := parseBudgetSpec([]byte(`{"compact_dims":["bogus"]}`))
	if spec2.compactsDim(dimTokens) || spec2.compactsDim(dimCost) {
		t.Fatal("a present compact_dims set with only unknown names must disable compaction everywhere")
	}
}

// TestLevelForAndMessage verifies the ladder tier computation and the
// built-in message copy with {pct} substitution.
func TestLevelForAndMessage(t *testing.T) {
	spec := parseBudgetSpec(nil) // default thresholds 0.25/0.5/0.75
	if lvl := spec.levelFor(dimTokens, 0.24); lvl != levelNone {
		t.Fatalf("0.24 frac → %d, want levelNone", lvl)
	}
	if lvl := spec.levelFor(dimTokens, 0.25); lvl != levelWarn {
		t.Fatalf("0.25 frac → %d, want levelWarn", lvl)
	}
	if lvl := spec.levelFor(dimTokens, 0.5); lvl != levelEscalate {
		t.Fatalf("0.5 frac → %d, want levelEscalate", lvl)
	}
	if lvl := spec.levelFor(dimTokens, 0.75); lvl != levelFinal {
		t.Fatalf("0.75 frac → %d, want levelFinal", lvl)
	}
	if lvl := spec.levelFor(dimTokens, 1.0); lvl != levelAbort {
		t.Fatalf("1.0 frac → %d, want levelAbort", lvl)
	}
	// Disabled dimension (0 limit) → fraction -1 → levelNone.
	disabled := budgetSpec{tokens: float64Ptr(0)}
	if lvl := disabled.levelFor(dimTokens, disabled.fraction(dimTokens, &budgetAccumulator{}, 0, 0)); lvl != levelNone {
		t.Fatalf("disabled dim → %d, want levelNone", lvl)
	}
	// Message substitution: {pct} is replaced with the percent used.
	msg := spec.message(dimTokens, levelWarn, 0.6)
	if !strings.Contains(msg, "60%") {
		t.Fatalf("warning message missing {pct} substitution, got %q", msg)
	}
	// The reformed copy is calm-but-severe and instructive: it names the driver and tells the worker
	// what to do, rather than panicking.
	if !strings.Contains(msg, "round-trip") {
		t.Fatalf("warning message should give concrete batching guidance, got %q", msg)
	}
}

// compactTestServer spins an httptest server that records POST /session/{id}/summarize calls
// (method, path, body) so a test can assert the Compact client contract and the session-run gate.
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
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async") {
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

// summarizeCount returns how many /summarize (compact) calls were recorded,
// excluding the post-compact reminder prompt_async messages.
func (c *compactRecorder) summarizeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if strings.HasSuffix(call.path, "/summarize") {
			n++
		}
	}
	return n
}

func (c *compactRecorder) lastCall() compactCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return compactCall{}
	}
	return c.calls[len(c.calls)-1]
}

// lastSummarizeCall returns the last /summarize (compact) call, ignoring the
// post-compact reminder prompt_async messages.
func (c *compactRecorder) lastSummarizeCall() compactCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if strings.HasSuffix(c.calls[i].path, "/summarize") {
			return c.calls[i]
		}
	}
	return compactCall{}
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
		budget:     &budgetAccumulator{steps: 1},
		budgetSpec: budgetSpec{compactMaxTurns: float64Ptr(2)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "2")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	r.maybeCompact()
	if rec.summarizeCount() != 0 {
		t.Fatalf("expected no compact below the min-turn floor, got %d", rec.summarizeCount())
	}
}

// TestMaybeCompactFiresOncePerExecution verifies the turn-count gate compacts once, and the
// per-execution cap (default 1) prevents a compact loop even though the cadence keeps tripping.
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
		budget:     &budgetAccumulator{steps: 1},
		budgetSpec: budgetSpec{compactMaxTurns: float64Ptr(2)},
		done:       make(chan struct{}),
	}
	// Note: parseEvent feeds budget.steps; here we set it directly.
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	for r.budget.steps < 8 {
		r.maybeCompact()
		r.budget.steps++
	}
	if rec.summarizeCount() != 1 {
		t.Fatalf("expected exactly 1 compact (per-execution cap), got %d", rec.summarizeCount())
	}
}

// TestMaybeCompactReArmsAcrossForwardProgress verifies the re-arm rule: with a higher per-execution
// cap, compacts are separated by the minimum-turn floor across normal forward progress (never.
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
		budget:     &budgetAccumulator{steps: 1},
		budgetSpec: budgetSpec{compactMaxTurns: float64Ptr(2)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "2")
	t.Setenv("ORCHICON_COMPACT_MAX", "5")
	r.maybeCompact() // steps=1 < 2 → no compact (floor)
	if rec.summarizeCount() != 0 {
		t.Fatalf("expected no compact at step 1 (floor), got %d", rec.summarizeCount())
	}
	r.budget.steps = 2
	r.maybeCompact() // floor met, turn cadence reached → compact #1
	if rec.summarizeCount() != 1 {
		t.Fatalf("expected compact #1 at step 2, got %d", rec.summarizeCount())
	}
	r.budget.steps = 3
	r.maybeCompact() // within re-arm window (2+2) → no compact
	if rec.summarizeCount() != 1 {
		t.Fatalf("expected no compact at step 3 (re-arm window), got %d", rec.summarizeCount())
	}
	r.budget.steps = 4
	r.maybeCompact() // re-armed (>= 4) → compact #2
	if rec.summarizeCount() != 2 {
		t.Fatalf("expected compact #2 at step 4 (re-armed), got %d", rec.summarizeCount())
	}
	// Verify the compact resolved the model ref and used the session id.
	call := rec.lastSummarizeCall()
	if call.body["providerID"] != "opencode" || call.body["modelID"] != "deepseek-v4-flash-free" {
		t.Fatalf("compact called with wrong provider/model: %v", call.body)
	}
}

// TestMaybeCompactNoBreach verifies no compact fires when the turn cadence
// has not been reached (spend is irrelevant to the turn-count gate).
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
		budget:     &budgetAccumulator{steps: 3},
		budgetSpec: budgetSpec{compactMaxTurns: float64Ptr(6)},
		done:       make(chan struct{}),
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "1")
	r.maybeCompact()
	if rec.summarizeCount() != 0 {
		t.Fatalf("expected no compact below the turn cadence, got %d", rec.summarizeCount())
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
	if rec.summarizeCount() != 0 {
		t.Fatalf("expected no compact on a malformed model ref, got %d", rec.summarizeCount())
	}
}

// TestMaybeCompactTurnCountTriggerWithNoBudgetBreach verifies the turn-count gate fires compaction
// on its own cadence even when cost/tokens never breach.
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
	// Turns 1-3: below the 4-turn cap → no compact.
	if rec.summarizeCount() != 2 {
		t.Fatalf("expected 2 turn-count-triggered compacts over 9 steps (cap=4), got %d", rec.summarizeCount())
	}
	firstCall := rec.calls[0]
	if firstCall.body["providerID"] != "opencode" || firstCall.body["modelID"] != "deepseek-v4-flash-free" {
		t.Fatalf("unexpected compact call body: %v", firstCall.body)
	}
}

// TestMaybeCompactTurnCountGateDisabled verifies an explicit 0 for compact_max_turns disables the
// turn-count trigger, leaving only cost/tokens as live signals (so a worker/tenant can opt back.
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
	if rec.summarizeCount() != 0 {
		t.Fatalf("expected no compacts with all gates explicitly disabled, got %d", rec.summarizeCount())
	}
}

// TestEffectiveCompactMaxTurns verifies the turn-count gate resolver: unset → DISABLED (this is the
// default that collapsed a healthy run at turn 30), configured → that value.
func TestEffectiveCompactMaxTurns(t *testing.T) {
	if turns, ok := effectiveCompactMaxTurns(budgetSpec{}); ok {
		t.Fatalf("unset compactMaxTurns = (%d, %v), want (_, false): an unset turn-count "+
			"gate must not collapse anything", turns, ok)
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

// TestEffectiveToolCallLimit verifies the tool-call HARD-abort gate resolver: unset → DISABLED (100
// was a ceiling nobody chose, and 120 honest calls is not a loop), configured → that value.
func TestEffectiveToolCallLimit(t *testing.T) {
	if limit, ok := effectiveToolCallLimit(budgetSpec{}); ok {
		t.Fatalf("unset toolCallCount = (%d, %v), want (_, false): an unset tool-call gate "+
			"must not abort anything", limit, ok)
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

// TestLadderToolCallAbortsAtLimit verifies the tool-call dimension hard-aborts once stats.toolUses
// reaches a CONFIGURED limit.
func TestLadderToolCallAbortsAtLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-toolcall-default", TenantID: "tnt_dev"},
		callbacks:  &liveCallbacks{},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		done:       make(chan struct{}),
		stats:      &execStreamState{toolUses: 99},
		budget:     &budgetAccumulator{},
		budgetSpec: parseBudgetSpec([]byte(`{"tool_call_count":100}`)),
		startedAt:  time.Now(),
	}
	r.maybeEnforceLadder(dimTools) // 99 < the configured 100 → no abort
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("expected no abort below the configured tool-call ceiling")
	}
	r.stats.toolUses = 100
	r.maybeEnforceLadder(dimTools) // 100 >= the configured 100 → abort
	r.mu.Lock()
	fin, ok, resultErr := r.finished, r.resultOk, r.resultErr
	r.mu.Unlock()
	if !fin {
		t.Fatal("expected the execution to finish once the tool-call limit is reached")
	}
	if ok {
		t.Fatal("a tool-call limit breach must fail the execution")
	}
	if resultErr != "budget_abort:tool_call_count" {
		t.Fatalf("resultErr = %q, want budget_abort:tool_call_count", resultErr)
	}
}

// TestLadderToolCallDisabled verifies an explicit 0 for tool_call_count disables the tool-call
// dimension entirely (fraction -1 → levelNone), even with a huge tool-call count.
func TestLadderToolCallDisabled(t *testing.T) {
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
		budget:     &budgetAccumulator{},
		budgetSpec: budgetSpec{toolCallCount: float64Ptr(0)},
		startedAt:  time.Now(),
	}
	r.maybeEnforceLadder(dimTools)
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("expected no abort with the tool-call limit explicitly disabled")
	}
}

// TestLadderNilBudgetNoop verifies a nil budget pointer is a safe no-op
// rather than a panic.
func TestLadderNilBudgetNoop(t *testing.T) {
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-toolcall-nilstats", TenantID: "tnt_dev"},
		done:      make(chan struct{}),
	}
	r.maybeEnforceLadder(dimTools)
	r.mu.Lock()
	fin := r.finished
	r.mu.Unlock()
	if fin {
		t.Fatal("expected a no-op with nil budget, not a finish")
	}
}

// TestLadderTokenTiersGatedByCompactPolicy verifies the unified ladder for the token dimension
// respects the DEFAULT per-tier compaction policy.
func TestLadderTokenTiersGatedByCompactPolicy(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	// tokens limit 1000, front-loaded ladder → warn at 250, escalate at 500,
	// final at 750.
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-ladder-tokens", TenantID: "tnt_dev"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{},
		budgetSpec: parseBudgetSpec([]byte(`{"tokens":1000}`)),
		startedAt:  time.Now(),
		done:       make(chan struct{}),
		stats:      &execStreamState{},
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "3")

	// 25% → warn: injects a message but does NOT compact (default warn off).
	r.budget.prompt = 250
	r.maybeEnforceLadder(dimTokens)
	if rec.summarizeCount() != 0 {
		t.Fatalf("warn tier must NOT compact by default, summarize=%d", rec.summarizeCount())
	}
	if rec.count() != 1 {
		t.Fatalf("warn tier should inject a warning message, calls=%d", rec.count())
	}
	// Re-entering the same tier must not re-fire (latched).
	before := rec.count()
	r.maybeEnforceLadder(dimTokens)
	if rec.count() != before {
		t.Fatalf("warn should latch (no re-send), calls %d->%d", before, rec.count())
	}

	// 50% → escalate: inject + compact (#1).
	r.budget.prompt = 500
	r.budget.steps = 1
	r.maybeEnforceLadder(dimTokens)
	if rec.summarizeCount() != 1 {
		t.Fatalf("escalate tier should compact, summarize=%d", rec.summarizeCount())
	}

	// 75% → final: inject + compact (#2) after the re-arm window.
	r.budget.prompt = 750
	r.budget.steps = 2
	r.maybeEnforceLadder(dimTokens)
	if rec.summarizeCount() != 2 {
		t.Fatalf("final tier should compact again, summarize=%d", rec.summarizeCount())
	}
}

// TestLadderCompactTiersAllOff verifies an operator can disable compaction at EVERY tier
// (compact_tiers=[false,false,false]).
func TestLadderCompactTiersAllOff(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-ladder-alloff", TenantID: "tnt_dev"},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{},
		budgetSpec: parseBudgetSpec([]byte(`{"tokens":1000,"compact_tiers":[false,false,false]}`)),
		startedAt:  time.Now(),
		done:       make(chan struct{}),
		stats:      &execStreamState{},
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "3")

	// Cross every non-abort tier; compaction must stay zero while warnings fire.
	r.budget.prompt = 250
	r.maybeEnforceLadder(dimTokens) // warn
	r.budget.prompt = 500
	r.budget.steps = 1
	r.maybeEnforceLadder(dimTokens) // escalate
	r.budget.prompt = 750
	r.budget.steps = 2
	r.maybeEnforceLadder(dimTokens) // final
	if rec.summarizeCount() != 0 {
		t.Fatalf("compact_tiers=[false,false,false] must disable all ladder compaction, summarize=%d", rec.summarizeCount())
	}
	if rec.count() < 3 {
		t.Fatalf("all three tiers should still inject warning messages, calls=%d", rec.count())
	}
}

// TestLadderCompactReArm verifies the shared re-arm latch: after a ladder compact, a DIFFERENT
// trigger cannot compact again until min-turn turns have passed.
func TestLadderCompactReArm(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-ladder-rearm", TenantID: "tnt_dev"},
		client:    NewSessionClient(srv.URL, "", ""),
		sessionID: "sess-1",
		modelRef:  "opencode/deepseek-v4-flash-free",
		budget:    &budgetAccumulator{},
		// All tiers compact, so the re-arm latch is the only thing gating.
		budgetSpec: parseBudgetSpec([]byte(`{"tokens":1000,"tool_call_count":100,"compact_tiers":[true,true,true]}`)),
		startedAt:  time.Now(),
		done:       make(chan struct{}),
		stats:      &execStreamState{},
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "2")
	t.Setenv("ORCHICON_COMPACT_MAX", "5")

	// tokens escalate (step 2) compacts #1.
	r.budget.prompt = 500 // 50% of 1000 → escalate
	r.budget.steps = 2
	r.maybeEnforceLadder(dimTokens)
	if rec.summarizeCount() != 1 {
		t.Fatalf("tokens escalate should compact, summarize=%d", rec.summarizeCount())
	}
	// tools escalate on the NEXT step (step 3) must inject its warning but NOT compact: within the
	// re-arm window (lastCompactStep=2, minT=2 → next compact needs step >= 4).
	r.stats.toolUses = 50 // 50% of 100 → escalate
	r.budget.steps = 3
	r.maybeEnforceLadder(dimTools)
	if rec.summarizeCount() != 1 {
		t.Fatalf("tools escalate within the re-arm window must not compact, summarize=%d", rec.summarizeCount())
	}
	if rec.count() < 2 {
		t.Fatalf("tools escalate should still inject its warning message, calls=%d", rec.count())
	}
	// After the window (step 4), a fresh tier can compact (#2).
	r.budget.prompt = 750
	r.budget.steps = 4
	r.maybeEnforceLadder(dimTokens)
	if rec.summarizeCount() != 2 {
		t.Fatalf("tokens final after the re-arm window should compact, summarize=%d", rec.summarizeCount())
	}
}

// TestLadderToolDimensionNeverCompacts verifies the per-dimension compaction gate: a budget that
// crosses the tool-call escalate tier is warned but NEVER compacts.
func TestLadderToolDimensionNeverCompacts(t *testing.T) {
	rec := newCompactRecorder(http.StatusOK)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	r := &sessionRun{
		a:         &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx: context.Background(),
		execRow:   db.ExecutionRow{ID: "exec-tool-nocompact", TenantID: "tnt_dev"},
		client:    NewSessionClient(srv.URL, "", ""),
		sessionID: "sess-1",
		modelRef:  "opencode/deepseek-v4-flash-free",
		budget:    &budgetAccumulator{},
		// All tiers compact on, so the ONLY thing gating tools here is the
		// per-dimension policy.
		budgetSpec: parseBudgetSpec([]byte(`{"tool_call_count":100,"cost_usd":1.0,"compact_tiers":[true,true,true]}`)),
		startedAt:  time.Now(),
		done:       make(chan struct{}),
		stats:      &execStreamState{},
	}
	t.Setenv("ORCHICON_COMPACT_MIN_TURNS", "1")
	t.Setenv("ORCHICON_COMPACT_MAX", "5")

	// tools at escalate (50% of 100) → warn, NEVER compact.
	r.stats.toolUses = 50
	r.budget.steps = 1
	r.maybeEnforceLadder(dimTools)
	if rec.summarizeCount() != 0 {
		t.Fatalf("tool-call escalate must NOT compact (dimension-gated), summarize=%d", rec.summarizeCount())
	}
	if rec.count() != 1 {
		t.Fatalf("tool-call escalate should still inject its warning, calls=%d", rec.count())
	}

	// cost at escalate (50% of 1.0) on the next step → compacts (cost is a
	// compact-permitted dimension).
	r.budget.costUSD = 0.5
	r.budget.steps = 2
	r.maybeEnforceLadder(dimCost)
	if rec.summarizeCount() != 1 {
		t.Fatalf("cost escalate should compact, summarize=%d", rec.summarizeCount())
	}
}

// TestLadderTokenAbortAtLimit verifies the token dimension hard-aborts once
// FRESH token count reaches the configured limit (100% → budget_abort:tokens).
func TestLadderTokenAbortAtLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	r := &sessionRun{
		a:          &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		parentCtx:  context.Background(),
		execRow:    db.ExecutionRow{ID: "exec-ladder-token-abort", TenantID: "tnt_dev"},
		callbacks:  &liveCallbacks{},
		client:     NewSessionClient(srv.URL, "", ""),
		sessionID:  "sess-1",
		modelRef:   "opencode/deepseek-v4-flash-free",
		budget:     &budgetAccumulator{},
		budgetSpec: parseBudgetSpec([]byte(`{"tokens":1000}`)),
		startedAt:  time.Now(),
		done:       make(chan struct{}),
		stats:      &execStreamState{},
	}
	r.budget.prompt = 1000 // fresh token count reaches the limit (cache excluded)
	r.maybeEnforceLadder(dimTokens)
	r.mu.Lock()
	fin, ok, resultErr := r.finished, r.resultOk, r.resultErr
	r.mu.Unlock()
	if !fin || ok {
		t.Fatalf("expected a failed abort, got fin=%v ok=%v", fin, ok)
	}
	if resultErr != "budget_abort:tokens" {
		t.Fatalf("resultErr = %q, want budget_abort:tokens", resultErr)
	}
}

func float64Ptr(f float64) *float64 { return &f }
