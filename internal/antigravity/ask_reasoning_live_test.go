package antigravity

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ask_reasoning_live_test.go — the LIVE half of the streaming claim (AC4's "streamed reasoning").
//
// WHY THE MODE MATTERS. `ask_mapper.go` maps `AgentThoughtChunk` to a reasoning delta unconditionally, and
// `TestAskThoughtChunkStreamsAsReasoning` / `TestAskNoThoughtChunkMeansNoReasoning` pin both halves off a
// fixture. But a fixture cannot say whether the REAL agy ever emits a thought chunk, and for a long time it
// did not: in `default` / `accept-edits` / `auto` it reports only `usage.thoughtTokens` and no text
// (docs/adapters/antigravity-acp.md §3.1, §12.7). It was measured emitting real reasoning chunks under
// `plan` — which is exactly the mode BRAINSTORM is put into (`prepareTurnMode`).
//
// So the honest claim this probe makes is: an ASK conversation in BRAINSTORM streams reasoning to the bus. An
// ITERATION conversation does not, and no amount of client work can change that — the adapter must not
// fabricate it (§12.7).
//
//	ORCHICON_ANTIGRAVITY_LIVE=1 go test ./internal/antigravity/ -run TestAskLive_BrainstormStreamsReasoning -v
func TestAskLive_BrainstormStreamsReasoning(t *testing.T) {
	if os.Getenv("ORCHICON_ANTIGRAVITY_LIVE") != "1" {
		t.Skip("live agy probe: set ORCHICON_ANTIGRAVITY_LIVE=1 to spend one model turn")
	}
	t.Setenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT", "120s")

	workspace := t.TempDir()
	b := New(quiet())
	b.SetAskRoot(t.TempDir())
	b.SetProjectDirResolver(func(_ context.Context, _, _ string) (string, error) { return workspace, nil })
	b.SetDiffLedger(nil, func(context.Context, string, string, string, string, map[string]any, string) {})
	t.Cleanup(b.CloseAsk)

	ctx := askmode.WithMode(context.Background(), askmode.Brainstorm)
	ctx = askmode.WithConversationScope(ctx, askmode.ConversationScope{
		ConversationID: "live_reasoning", ProjectID: "prj_live"})
	model := "antigravity/google/" + liveModel()

	sid, err := b.CreateConversationSession(ctx, "live_reasoning", "live")
	if err != nil {
		t.Fatalf("CreateConversationSession against the real agy: %v", err)
	}
	t.Logf("live agy session id = %s", sid)

	sub, err := b.Subscribe(ctx, "live_reasoning")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// A prompt that forces a plan rather than a one-line answer: reasoning text is what agy emits while it
	// thinks, so a trivially answerable prompt may legitimately produce none.
	turn := "Think through, step by step, three different ways to structure a small Go HTTP handler package, " +
		"and weigh each one. Do not write any files. Then recommend one."

	var (
		mu        sync.Mutex
		reasoning []string
		thoughts  int64
	)
	done := make(chan error, 1)
	go func() {
		done <- b.SendTurnMessage(ctx, "live_reasoning", sid, "You are a test harness.", model, turn)
	}()

	collectTurn(t, sub, 180*time.Second, func(ev scheduler.SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.IsReasoning && ev.Text != "" {
			reasoning = append(reasoning, ev.Text)
			t.Logf("REASONING %s: %.120q", ev.Type, ev.Text)
		}
		if ev.Kind == "part" && ev.IsReasoning && ev.Text != "" {
			if len(reasoning) == 0 || reasoning[len(reasoning)-1] != ev.Text {
				reasoning = append(reasoning, ev.Text)
				t.Logf("REASONING part: %.120q", ev.Text)
			}
		}
	})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}

	mu.Lock()
	n, total := len(reasoning), int64(0)
	for _, r := range reasoning {
		total += int64(len(r))
	}
	mu.Unlock()

	// THE MEASUREMENT, NOT AN ASSERTION OF WHAT AGY MUST DO. §12.7 records that agy 1.5.13 emits reasoning
	// under plan mode; if a future agy stops doing so this probe should say so loudly rather than fail on a
	// client-side rule the adapter cannot control. The usage row is the fallback that must ALWAYS hold.
	if n == 0 {
		t.Fatalf("agy emitted no agent_thought_chunk in Brainstorm/plan mode: reasoning text is unreachable "+
			"on this agy build, and a client showing an empty fold is showing the truth (§12.7). Re-measure "+
			"and update docs/adapters/antigravity-acp.md §12.7 before this claim is repeated.")
	}
	t.Logf("BRAINSTORM STREAMS REASONING: %d reasoning event(s), %d bytes of thought text", n, total)
	_ = thoughts
}
