package orchicon

import (
	"os"
	"testing"
)

// --- Normalized usage semantics (D2) -------------------------------------------

func TestUsageTotals(t *testing.T) {
	u := Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 10}
	// Reasoning is a sub-bucket of output; cache read/write are input
	// sub-classes — none may be added into the totals.
	if u.TotalTokens() != 150 {
		t.Fatalf("total = %d, want 150", u.TotalTokens())
	}
}

func TestModelInfoTriState(t *testing.T) {
	m := ModelInfo{Tools: boolPtr(true), ReasoningEfforts: []string{"low", "high"}}
	if !m.SupportsTools() || !m.SupportsReasoningEffort() {
		t.Fatal("flags")
	}
	m2 := ModelInfo{}
	if m2.SupportsTools() || m2.SupportsReasoningEffort() {
		t.Fatal("unknown must be false")
	}
}

// --- CacheControl policy -------------------------------------------------------

func TestCacheControlPolicyValues(t *testing.T) {
	if CacheControlSystemAndTools != "system+tools" || CacheControlNone != "none" || CacheControlSystem != "system" {
		t.Fatal("policy values are wire-visible — locked")
	}
}

// --- Env-gated live smokes (never run in CI) ------------------------------------

// liveEnv reports whether the live smoke for a provider is enabled
// (ORCHICON_TEST_LIVE_<PROVIDER>=1) and returns the credential if any.
func liveEnv(provider string) bool {
	return os.Getenv("ORCHICON_TEST_LIVE_"+provider) == "1"
}

func TestLiveAnthropicSmoke(t *testing.T) {
	if !liveEnv("ANTHROPIC") {
		t.Skip("live smoke disabled: set ORCHICON_TEST_LIVE_ANTHROPIC=1 + ANTHROPIC_API_KEY (cost-capped, single tiny call)")
	}
	c := &AnthropicClient{APIKey: os.Getenv("ANTHROPIC_API_KEY")}
	liveStreamSmoke(t, c, "claude-haiku-4")
}

func TestLiveCommandCodeGoPlanLegacySmoke(t *testing.T) {
	if !liveEnv("COMMANDCODE") || os.Getenv(envCommandCodePlan) != "go" {
		t.Skip("legacy transport smoke disabled: set ORCHICON_TEST_LIVE_COMMANDCODE=1 + COMMANDCODE_PLAN=go + COMMANDCODE_API_KEY (cost-capped, single tiny call)")
	}
	c := &CommandCodeClient{APIKey: os.Getenv("COMMANDCODE_API_KEY"), ThreadID: "orchicon-smoke"}
	liveStreamSmoke(t, c, "z-ai/glm-5.3-flash")
}

// liveStreamSmoke runs ONE tiny turn (few tokens) — the end-to-end proof the
// wire clients talk to the real services.
func liveStreamSmoke(t *testing.T, p Provider, model string) {
	t.Helper()
	ts, err := p.StreamTurn(testCtx(), TurnRequest{
		Model: model, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("Reply with the single word: ok")}}}},
	})
	if err != nil {
		t.Fatalf("live smoke: %v", err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("live stream: %v", err)
	}
	var sawText, sawFinish bool
	for _, ev := range evs {
		switch e := ev.(type) {
		case TextDelta:
			sawText = true
			_ = e
		case Finish:
			sawFinish = true
			if e.Usage.OutputTokens <= 0 {
				t.Fatalf("live usage zeroed: %+v", e.Usage)
			}
		}
	}
	if !sawText || !sawFinish {
		t.Fatalf("live smoke incomplete: text=%v finish=%v", sawText, sawFinish)
	}
}
