package orchicon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// windowProvider serves a fixed model list (and an optional error) so the
// window resolution can be exercised without a live provider.
type windowProvider struct {
	models []ModelInfo
	err    error
	calls  int
}

func (p *windowProvider) StreamTurn(context.Context, TurnRequest) (TurnStream, error) {
	return &noopTurnStream{}, nil
}

func (p *windowProvider) ListModels(context.Context) ([]ModelInfo, error) {
	p.calls++
	return p.models, p.err
}

func (p *windowProvider) Capabilities() Capabilities { return Capabilities{} }

// The threshold is 95% by default, overridable, and a bad override falls back to
// the default rather than silently disarming or firing at 100%.
func TestAskPressureThresholdResolvesSafely(t *testing.T) {
	t.Setenv(askPressureEnv, "")
	if got := askPressureThreshold(); got != defaultAskPressureFrac {
		t.Errorf("default = %v, want %v", got, defaultAskPressureFrac)
	}
	if defaultAskPressureFrac != 0.95 {
		t.Errorf("the documented default must be 0.95, got %v", defaultAskPressureFrac)
	}
	t.Setenv(askPressureEnv, "0.8")
	if got := askPressureThreshold(); got != 0.8 {
		t.Errorf("override = %v, want 0.8", got)
	}
	for _, bad := range []string{"nonsense", "0", "-1", "1.5", ""} {
		t.Setenv(askPressureEnv, bad)
		if got := askPressureThreshold(); got != defaultAskPressureFrac {
			t.Errorf("bad override %q = %v, want the default %v", bad, got, defaultAskPressureFrac)
		}
	}
}

// The window comes from the provider's live list — the SAME source executions
// use. A zero hint is "unknown", never a guess.
func TestResolveAskContextWindowUsesLiveProviderList(t *testing.T) {
	prov := &windowProvider{models: []ModelInfo{
		{ID: "other-model", Context: 999},
		{ID: "deepseek-flash", Context: 1_048_576},
	}}
	b := &NativeBridge{log: testLogger()}

	w, reason := b.resolveAskContextWindow(context.Background(), prov, "s1", "deepseek-flash")
	if w != 1_048_576 {
		t.Errorf("window = %d, want 1048576", w)
	}
	if reason != "live" {
		t.Errorf("reason = %q, want live", reason)
	}
	// Cached per session: a second call must not re-list.
	if _, _ = b.resolveAskContextWindow(context.Background(), prov, "s1", "deepseek-flash"); prov.calls != 1 {
		t.Errorf("ListModels calls = %d, want 1 (resolved once per session)", prov.calls)
	}

	// A model with no context value is unknown, not zero-overflow.
	zero := &windowProvider{models: []ModelInfo{{ID: "m", Context: 0}}}
	if w, reason := b.resolveAskContextWindow(context.Background(), zero, "s2", "m"); w != 0 || !strings.Contains(reason, "context_zero") {
		t.Errorf("zero-context model = (%d, %q), want (0, context_zero...)", w, reason)
	}
	// An unknown model id is unknown too.
	if w, reason := b.resolveAskContextWindow(context.Background(), zero, "s3", "absent"); w != 0 || !strings.Contains(reason, "model_not_found") {
		t.Errorf("absent model = (%d, %q), want (0, model_not_found...)", w, reason)
	}
}

// A transient ListModels failure must NOT be cached — otherwise one blip would
// disarm the gate for the rest of the session.
func TestResolveAskContextWindowDoesNotCacheLookupFailure(t *testing.T) {
	prov := &windowProvider{err: errors.New("provider unreachable")}
	b := &NativeBridge{log: testLogger()}

	if _, reason := b.resolveAskContextWindow(context.Background(), prov, "s1", "m"); !strings.Contains(reason, "list_models_error") {
		t.Errorf("reason = %q, want list_models_error", reason)
	}
	prov.err = nil
	prov.models = []ModelInfo{{ID: "m", Context: 200_000}}
	w, reason := b.resolveAskContextWindow(context.Background(), prov, "s1", "m")
	if w != 200_000 {
		t.Errorf("after recovery window = %d, want 200000 (the failure must not have been cached)", w)
	}
	if reason != "live" {
		t.Errorf("reason = %q, want live", reason)
	}
}

// DISARMED without a measurement: the gate can never fire on a guess.
func TestPressureGateDisarmedWithoutMeasurement(t *testing.T) {
	b := &NativeBridge{log: testLogger(), chatHistory: map[string][]Message{"s1": wedgeHistory()}}
	prov := &windowProvider{models: []ModelInfo{{ID: "m", Context: 100}}}

	b.maybeCompactForPressure(context.Background(), prov, "c1", "s1", "orchicon/x/m", "m")
	if prov.calls != 0 {
		t.Error("no measurement must mean no work at all (not even a window lookup)")
	}
	if len(b.chatHistory["s1"]) != len(wedgeHistory()) {
		t.Error("history must be untouched when the gate is disarmed")
	}
}

// DISARMED without a live window hint — the platform's no-guess rule. This is
// the case that matters most: a model with no window metadata must NOT be
// compacted on a guessed denominator.
func TestPressureGateDisarmedWithoutLiveWindow(t *testing.T) {
	b := &NativeBridge{log: testLogger(), chatHistory: map[string][]Message{"s1": wedgeHistory()}}
	b.recordAskPromptTokens("s1", 1_000_000)

	before := conversationBytes(b.chatHistory["s1"])
	// No provider at all → no window hint.
	b.maybeCompactForPressure(context.Background(), nil, "c1", "s1", "orchicon/x/m", "m")
	if conversationBytes(b.chatHistory["s1"]) != before {
		t.Error("a disarmed gate must not touch the history")
	}
	// Provider reports the model with no context value → still disarmed.
	zero := &windowProvider{models: []ModelInfo{{ID: "m", Context: 0}}}
	b.maybeCompactForPressure(context.Background(), zero, "c1", "s1", "orchicon/x/m", "m")
	if conversationBytes(b.chatHistory["s1"]) != before {
		t.Error("a zero-context model must not be compacted on a guess")
	}
}

// UNDER threshold: no compaction.
func TestPressureGateBelowThresholdDoesNothing(t *testing.T) {
	b := &NativeBridge{log: testLogger(), chatHistory: map[string][]Message{"s1": wedgeHistory()}}
	before := conversationBytes(b.chatHistory["s1"])
	prov := &windowProvider{models: []ModelInfo{{ID: "m", Context: 1_000_000}}}

	// 900k / 1M = 90% < 95%.
	b.recordAskPromptTokens("s1", 900_000)
	b.maybeCompactForPressure(context.Background(), prov, "c1", "s1", "orchicon/x/m", "m")
	if conversationBytes(b.chatHistory["s1"]) != before {
		t.Error("90% must not compact at a 95% threshold")
	}
}

// OVER threshold with no resolver: the compaction cannot summarize, so the FAILURE
// must be swallowed (best-effort) and the history left intact — a turn must never
// be blocked or damaged by a failed compaction.
func TestPressureGateOverThresholdCompactionFailureIsBestEffort(t *testing.T) {
	b := &NativeBridge{log: testLogger(), chatHistory: map[string][]Message{"s1": wedgeHistory()}}
	before := conversationBytes(b.chatHistory["s1"])
	prov := &windowProvider{models: []ModelInfo{{ID: "m", Context: 1_000_000}}}

	// 1M / 1M = 100% ≥ 95%, and b.resolver is nil so the summarize step fails.
	b.recordAskPromptTokens("s1", 1_000_000)
	b.maybeCompactForPressure(context.Background(), prov, "c1", "s1", "orchicon/x/m", "m")
	if conversationBytes(b.chatHistory["s1"]) != before {
		t.Error("a failed compaction must leave the history intact")
	}
}

// The measurement is dropped only when a compaction actually happened, so a
// measurement is never silently forgotten (which would disarm the gate forever).
func TestRecordedMeasurementIsKeptAndPruned(t *testing.T) {
	b := &NativeBridge{log: testLogger()}
	b.recordAskPromptTokens("s1", 42)
	if got := b.askLastPromptTokens("s1"); got != 42 {
		t.Errorf("measurement = %d, want 42", got)
	}
	// A zero/negative sample never overwrites a real one (0 is "forget", tracked
	// separately by the caller).
	b.recordAskPromptTokens("s1", 0)
	if got := b.askLastPromptTokens("s1"); got != 42 {
		t.Errorf("a zero sample overwrote the measurement: %d", got)
	}
}

// A deleted conversation must leave no per-session pressure state behind, or the
// maps would grow forever and a reused session id could inherit a stale trigger.
func TestPurgeClearsPressureState(t *testing.T) {
	b := &NativeBridge{
		log:             testLogger(),
		chatHistory:     map[string][]Message{"orchicon-ask:c1": wedgeHistory()},
		askPromptTokens: map[string]int64{"orchicon-ask:c1": 999},
		askWindowTokens: map[string]int64{"orchicon-ask:c1": 1000},
	}
	if err := b.PurgeConversationHistory(context.Background(), "c1", ""); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, ok := b.chatHistory["orchicon-ask:c1"]; ok {
		t.Error("history not purged")
	}
	if got := b.askLastPromptTokens("orchicon-ask:c1"); got != 0 {
		t.Errorf("prompt measurement survived the purge: %d", got)
	}
	b.mu.Lock()
	_, windowKept := b.askWindowTokens["orchicon-ask:c1"]
	b.mu.Unlock()
	if windowKept {
		t.Error("resolved window survived the purge")
	}
}

// The sink must feed the gate's numerator on every real round.
func TestAskUsageSinkRecordsPromptTokensForGate(t *testing.T) {
	n := 0
	b := &NativeBridge{log: testLogger(), usageRecorder: func(context.Context, scheduler.UsageRecord) error {
		n++
		return nil
	}}
	sink := b.askUsageSink("t", "conv-1", "orchicon-ask:conv-1", "orchicon/x/m", "p", "m")
	sink(context.Background(), Usage{InputTokens: 1_016_584, OutputTokens: 10})

	if got := b.askLastPromptTokens("orchicon-ask:conv-1"); got != 1_016_584 {
		t.Errorf("gate numerator = %d, want 1016584 (must be the provider's real report)", got)
	}
	if n != 1 {
		t.Errorf("recorded %d usage samples, want 1", n)
	}
}
