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
	var reply, reasoning strings.Builder

	done, calls, usage, aborted := b.drainOneRound(context.Background(), bus, stream, &reply, &reasoning)
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
	sink := b.askUsageSink("tnt_dev", "conv-1", "orchicon/deepseek/deepseek-flash", "deepseek", "deepseek-flash")
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
	if s := b.askUsageSink("t", "c", "orchicon/x/y", "p", "m"); s != nil {
		t.Error("a nil recorder must yield a nil sink (no usage recorded)")
	}

	n := 0
	b2 := &NativeBridge{log: testLogger(), usageRecorder: func(context.Context, scheduler.UsageRecord) error {
		n++
		return nil
	}}
	b2.askUsageSink("t", "c", "orchicon/x/y", "p", "m")(context.Background(), Usage{})
	if n != 0 {
		t.Error("an empty round must not be recorded as a zero-cost sample")
	}
	// A cache-only round IS real usage and must be kept (parity with emitTurnUsage).
	b2.askUsageSink("t", "c", "orchicon/x/y", "p", "m")(context.Background(), Usage{CacheReadTokens: 123})
	if n != 1 {
		t.Errorf("a cache-only round recorded %d samples, want 1 (it is real usage)", n)
	}
}
