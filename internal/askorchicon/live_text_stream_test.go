package askorchicon

import (
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestDeltaOnlyTurnStreamsTextAsItArrives is the regression for the operator's report that Ask
// Orchicon's reply never appears as it is typed — the pane shows the reasoning block and then the
// finished message, with nothing streaming in between.
//
// WHY DELTA-ONLY IS THE RIGHT SHAPE. internal/opencode/chatsession.go emits the mid-generation
// stream as Kind "delta" (TokenDeltaInfoFromBus — "session.text.delta", "message.part.delta"),
// and internal/orchicon/chatturn.go's drainChatTurn does the same (bus.emit Kind "delta"),
// accumulating the reply and emitting ONE consolidated completed part at Finish. So on a real turn
// the collector sees DELTAS, not parts, for the whole generation.
//
// WHAT WAS WRONG. chat.go's `case "delta"` fed liveText/liveReasoning and the throttled mirror and
// called no onStreamEvent, so a client rendering the stream had nothing to draw: it learned about
// the reply only from the 2s ListMessages mirror poll or the final completed part.
func TestDeltaOnlyTurnStreamsTextAsItArrives(t *testing.T) {
	client := &fakeSessionClient{}

	var mu sync.Mutex
	var chunks []string
	var mirrored bool

	opts := turnCollectOpts{
		client:         client,
		sessionID:      "ses_live",
		reuseSystem:    "REUSE_SYSTEM",
		modelRef:       "opencode/deepseek-v4-flash-free",
		userMsg:        "hello",
		assistantMsgID: "msg_a",
		onStreamEvent: func(r *apiv1.ChatStreamResponse) {
			if tc := r.GetTextChunk(); tc != nil && tc.GetContent() != "" {
				mu.Lock()
				chunks = append(chunks, tc.GetContent())
				mu.Unlock()
			}
		},
		onPartial: func(string, []string) {
			mu.Lock()
			mirrored = true
			mu.Unlock()
		},
	}

	const answer = "The quick brown fox writes a long answer token by token"

	go func() {
		waitForSend(t, client, 1)
		// Stream the answer the way the adapter does: many small text deltas, no parts.
		for _, w := range strings.Fields(answer) {
			client.sub.feed(busDelta("ses_live", w+" "))
		}
		client.sub.feed(busIdle("ses_live"))
	}()

	reply, _, _, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("collect error: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), chunks...)
	sawMirror := mirrored
	mu.Unlock()

	// AC1 — the text reaches the client DURING the turn, not only at completion. Before the fix this
	// slice was empty (the pinning diagnostic logged `textChunks=0`).
	if len(got) == 0 {
		t.Fatal("a delta-only turn streamed no TextChunk: the reply cannot appear as it is typed")
	}
	if streamed := strings.TrimSpace(strings.Join(got, "")); streamed != answer {
		t.Errorf("streamed text = %q, want the full answer %q", streamed, answer)
	}
	// AC2 — the durable accumulation is unchanged by the forward: the finalize still persists the
	// whole reply, and the mirror still runs for a client that lost the stream.
	if strings.TrimSpace(reply) != strings.TrimSpace(answer) {
		t.Errorf("accumulated reply = %q, want %q", reply, answer)
	}
	if !sawMirror {
		t.Error("the partial mirror did not run: a client that lost the stream would see nothing")
	}
}

// TestDeltaForwardDoesNotLeakFoldedThink is the other half of the same rule, and the reason the
// forward lives INSIDE the segmenter's emitText callback rather than in a raw `else` beside it: a
// folded think body must never reach the visible reply, even now that the delta stream is streamed
// on. The segmenter strips the body and hands emitText only the clean text.
func TestDeltaForwardDoesNotLeakFoldedThink(t *testing.T) {
	client := &fakeSessionClient{}

	var mu sync.Mutex
	var streamed strings.Builder

	opts := turnCollectOpts{
		client:         client,
		sessionID:      "ses_live",
		reuseSystem:    "REUSE_SYSTEM",
		modelRef:       "opencode/deepseek-v4-flash-free",
		userMsg:        "hello",
		assistantMsgID: "msg_a",
		onStreamEvent: func(r *apiv1.ChatStreamResponse) {
			if tc := r.GetTextChunk(); tc != nil {
				mu.Lock()
				streamed.WriteString(tc.GetContent())
				mu.Unlock()
			}
		},
	}

	go func() {
		waitForSend(t, client, 1)
		// A folded think block wrapped around the answer, arriving split across deltas — the shape
		// the segmenter's carry-over state exists for.
		for _, d := range []string{"<thi", "nk>SECRET", "_THOUGHT", "</thi", "nk>", "VISIBLE", "_ANSWER"} {
			client.sub.feed(busDelta("ses_live", d))
		}
		client.sub.feed(busIdle("ses_live"))
	}()

	if _, _, _, err := collectTurn(t, client, opts); err != nil {
		t.Fatalf("collect error: %v", err)
	}

	mu.Lock()
	got := streamed.String()
	mu.Unlock()

	if strings.Contains(got, "SECRET") || strings.Contains(got, "THOUGHT") {
		t.Errorf("a folded think body leaked onto the streamed reply: %q", got)
	}
	if !strings.Contains(got, "VISIBLE") || !strings.Contains(got, "ANSWER") {
		t.Errorf("the visible text after the think block was not streamed: %q", got)
	}
}
