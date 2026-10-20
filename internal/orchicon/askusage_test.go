package orchicon

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// usageTurnStream emits one Finish carrying real usage, then ends.
type usageTurnStream struct {
	usage Usage
	sent  bool
}

func (s *usageTurnStream) Next(context.Context) (Event, bool, error) {
	if s.sent {
		return nil, false, nil
	}
	s.sent = true
	return Finish{StopReason: StopReason("stop"), Usage: s.usage}, true, nil
}

func (s *usageTurnStream) Close() error { return nil }

// The round's provider usage must reach the caller, or nothing downstream can
// attribute it (and the context gate would have no numerator).
func TestDrainOneRoundSurfacesFinishUsage(t *testing.T) {
	b := &NativeBridge{log: testLogger()}
	bus := newChatBus()
	defer bus.Close()

	stream := &usageTurnStream{usage: Usage{
		InputTokens:      1_016_584,
		OutputTokens:     120,
		CacheReadTokens:  500_000,
		CacheWriteTokens: 80,
		ReasoningTokens:  7,
		CostUSD:          0.42,
	}}
	var reply, reasoning, roundReasoning strings.Builder

	done, calls, usage, aborted := b.drainOneRound(context.Background(), bus, stream, &reply, &reasoning, &roundReasoning)
	if !done {
		t.Fatal("drainOneRound must report a Finish")
	}
	if aborted {
		t.Fatal("a Finish is not an abort")
	}
	if len(calls) != 0 {
		t.Errorf("unexpected tool calls: %v", calls)
	}
	if usage.InputTokens != 1_016_584 || usage.CacheReadTokens != 500_000 || usage.CostUSD != 0.42 {
		t.Errorf("usage not surfaced faithfully: %+v", usage)
	}
}

// The whole point of the change: an Ask turn's usage is attributed to the
// CONVERSATION (not an execution), with the adapter kind parsed from the
// conversation's model_ref. That attribution is what the conversation delete
// path de-links, so it must be exactly the conversation id.
func TestAskUsageSinkAttributesToConversation(t *testing.T) {
	var got []scheduler.UsageRecord
	b := &NativeBridge{
		log: testLogger(),
		usageRecorder: func(_ context.Context, in scheduler.UsageRecord) error {
			got = append(got, in)
			return nil
		},
	}
	sink := b.askUsageSink(nil, "tnt_dev", "conv-1", "orchicon-ask:conv-1", "orchicon/deepseek/deepseek-flash", "deepseek", "deepseek-flash")
	if sink == nil {
		t.Fatal("a wired recorder must produce a sink")
	}
	sink(context.Background(), Usage{
		InputTokens:     1_016_584,
		OutputTokens:    120,
		CacheReadTokens: 500_000,
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(got))
	}
	r := got[0]
	if r.SessionID != "conv-1" {
		t.Errorf("SessionID = %q, want the conversation id (this is the de-link key)", r.SessionID)
	}
	if r.AdapterKind != "orchicon" {
		t.Errorf("AdapterKind = %q, want \"orchicon\" (parsed from the model_ref)", r.AdapterKind)
	}
	if r.Provider != "deepseek" || r.Model != "deepseek-flash" {
		t.Errorf("provider/model = %q/%q, want deepseek/deepseek-flash", r.Provider, r.Model)
	}
	if r.PromptTokens != 1_016_584 {
		t.Errorf("PromptTokens = %d, want the round's real input tokens", r.PromptTokens)
	}
	if r.CacheReadTokens != 500_000 {
		t.Errorf("CacheReadTokens = %d, want 500000", r.CacheReadTokens)
	}
	// A chat sample must NOT masquerade as an execution.
	if r.ExecutionID != "" || r.TaskID != "" || r.WorkerID != "" {
		t.Errorf("Ask sample carries execution attribution: %+v", r)
	}
}

// No recorder means no sink (Ask records nothing, matching the worker path),
// and a genuinely empty round is not a zero-cost sample.
func TestAskUsageSinkNilRecorderAndEmptyUsage(t *testing.T) {
	b := &NativeBridge{log: testLogger()}
	if s := b.askUsageSink(nil, "t", "c", "s", "orchicon/x/y", "p", "m"); s != nil {
		t.Error("a nil recorder must yield a nil sink (no usage recorded)")
	}

	n := 0
	b2 := &NativeBridge{log: testLogger(), usageRecorder: func(context.Context, scheduler.UsageRecord) error {
		n++
		return nil
	}}
	b2.askUsageSink(nil, "t", "c", "s", "orchicon/x/y", "p", "m")(context.Background(), Usage{})
	if n != 0 {
		t.Error("an empty round must not be recorded as a zero-cost sample")
	}
	// A cache-only round IS real usage and must be kept (parity with emitTurnUsage).
	b2.askUsageSink(nil, "t", "c", "s", "orchicon/x/y", "p", "m")(context.Background(), Usage{CacheReadTokens: 123})
	if n != 1 {
		t.Errorf("a cache-only round recorded %d samples, want 1 (it is real usage)", n)
	}
}

// THE ASK TURN MUST PRICE ITS OWN ROUND, or every Ask usage record lands at $0.00.
//
// This is the operator's report — "once a conversation has been compacted, the price
// calculation resets and stays at $0.00" — and the compation was a red herring: the
// rows carried correct TOKENS and CACHE counters (which is why the composer strip kept
// showing 155.8M tokens) while cost_usd was 0 on every one of them, because the wire
// clients report tokens and never a price, and nothing on this path priced them. The
// worker loop does exactly this before its own sink (loop.go, s.priceUsage).
func TestAskUsageSinkPricesTheRound(t *testing.T) {
	prov := &ctxModelProvider{models: []ModelInfo{{
		ID: "deepseek-flash",
		Pricing: &Pricing{
			InputPerM:     0.15,
			OutputPerM:    0.60,
			CacheReadPerM: 0.003,
			Currency:      "USD",
			Source:        "test",
		},
	}}}

	var got []scheduler.UsageRecord
	b := &NativeBridge{log: testLogger(), usageRecorder: func(_ context.Context, in scheduler.UsageRecord) error {
		got = append(got, in)
		return nil
	}}
	sink := b.askUsageSink(prov, "tnt_dev", "conv-1", "sess-1", "orchicon/deepseek/deepseek-flash", "deepseek", "deepseek-flash")

	// One million of each class prices straight at the per-million rates, so the
	// expected number is readable rather than a computed constant.
	sink(context.Background(), Usage{
		InputTokens:     1_000_000,
		OutputTokens:    1_000_000,
		CacheReadTokens: 1_000_000,
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(got))
	}
	want := 0.15 + 0.60 + 0.003
	if diff := got[0].CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %v, want %v (0.15 input + 0.60 output + 0.003 cache-read per million).\n"+
			"An unpriced round is the bug: the Ask sink recorded the provider's cost verbatim, and\n"+
			"the native wire clients never report one.", got[0].CostUSD, want)
	}
}

// PRICING IS RESOLVED ONCE PER SESSION, not once per round: a turn is many rounds and
// a ListModels call each would be a per-round provider query for a value that cannot
// change mid-conversation.
func TestAskUsageSinkResolvesPricingOncePerSession(t *testing.T) {
	prov := &countingModelProvider{models: []ModelInfo{{
		ID:      "deepseek-flash",
		Pricing: &Pricing{InputPerM: 1.0, Currency: "USD"},
	}}}
	b := &NativeBridge{log: testLogger(), usageRecorder: func(context.Context, scheduler.UsageRecord) error {
		return nil
	}}
	sink := b.askUsageSink(prov, "t", "conv-1", "sess-1", "orchicon/deepseek/deepseek-flash", "deepseek", "deepseek-flash")
	sink(context.Background(), Usage{InputTokens: 10})
	sink(context.Background(), Usage{InputTokens: 10})
	sink(context.Background(), Usage{InputTokens: 10})

	if prov.calls != 1 {
		t.Errorf("ListModels called %d times, want 1 (pricing is cached per session)", prov.calls)
	}
}

// A model the provider does not price is recorded UNPRICED, never guessed: prices are
// not synthesized, so an unknown model keeps the previous behaviour rather than
// acquiring invented rates. Resolved-and-absent is cached too.
func TestAskUsageSinkLeavesAnUnpricedModelAtZero(t *testing.T) {
	prov := &countingModelProvider{models: []ModelInfo{{ID: "mystery-model"}}}
	var got []scheduler.UsageRecord
	b := &NativeBridge{log: testLogger(), usageRecorder: func(_ context.Context, in scheduler.UsageRecord) error {
		got = append(got, in)
		return nil
	}}
	sink := b.askUsageSink(prov, "t", "conv-1", "sess-1", "orchicon/p/mystery-model", "p", "mystery-model")
	sink(context.Background(), Usage{InputTokens: 10})
	sink(context.Background(), Usage{InputTokens: 10})

	if len(got) != 2 {
		t.Fatalf("recorded %d samples, want 2 (tokens are still real usage)", len(got))
	}
	for i, r := range got {
		if r.CostUSD != 0 {
			t.Errorf("sample %d CostUSD = %v, want 0 — an unpriced model must not acquire a price", i, r.CostUSD)
		}
	}
	if prov.calls != 1 {
		t.Errorf("ListModels called %d times, want 1 (resolved-and-absent is cached)", prov.calls)
	}
}

// countingModelProvider counts ListModels calls so the caching claims above are
// asserted rather than assumed.
type countingModelProvider struct {
	ctxModelProvider
	models []ModelInfo
	calls  int
}

func (p *countingModelProvider) ListModels(context.Context) ([]ModelInfo, error) {
	p.calls++
	return p.models, nil
}
