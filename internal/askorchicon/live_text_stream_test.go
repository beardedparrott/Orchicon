package askorchicon

import (
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// WHY DELTA-ONLY IS THE RIGHT SHAPE.
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
// forward lives INSIDE the segmenter's emitText callback rather than in a raw `else` beside it.
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

// TestReasoningIsNotStreamedAsDeltas is the regression for "your reasoning block is repeating over
// and over in the TUI as you stream".
func TestReasoningIsNotStreamedAsDeltas(t *testing.T) {
	client := &fakeSessionClient{}

	var mu sync.Mutex
	var reasoningChunks int
	var textChunks int

	opts := turnCollectOpts{
		client:         client,
		sessionID:      "ses_live",
		reuseSystem:    "REUSE_SYSTEM",
		modelRef:       "opencode/deepseek-v4-flash-free",
		userMsg:        "hello",
		assistantMsgID: "msg_a",
		onStreamEvent: func(r *apiv1.ChatStreamResponse) {
			mu.Lock()
			defer mu.Unlock()
			if rc := r.GetReasoning(); rc != nil && rc.GetContent() != "" {
				reasoningChunks++
			}
			if tc := r.GetTextChunk(); tc != nil && tc.GetContent() != "" {
				textChunks++
			}
		},
	}

	go func() {
		waitForSend(t, client, 1)
		// The shape the adapter streams: interleaved reasoning and text DELTAS.
		client.sub.feed(busReasoningDelta("ses_live", "thinking one. "))
		client.sub.feed(busDelta("ses_live", "answer text "))
		client.sub.feed(busReasoningDelta("ses_live", "thinking two. "))
		client.sub.feed(busDelta("ses_live", "more answer text"))
		client.sub.feed(busIdle("ses_live"))
	}()

	reply, reasoning, _, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("collect error: %v", err)
	}

	mu.Lock()
	rsnChunks, txtChunks := reasoningChunks, textChunks
	mu.Unlock()

	// THE INVARIANT: no reasoning on the stream, because the mirror row already carries it.
	if rsnChunks != 0 {
		t.Errorf("the collector streamed %d reasoning chunk(s): reasoning would then reach the TUI from BOTH "+
			"the stream and the mirror row, which is the repeating-block bug", rsnChunks)
	}
	// AND THE OTHER DIRECTION, so this test documents the asymmetry rather than half of it: text MUST stream,
	// and its durable copy must still be intact.
	if txtChunks == 0 {
		t.Error("no text was streamed: text has no live path other than the stream, which is what this work " +
			"was for")
	}
	if !strings.Contains(reply, "answer text") || !strings.Contains(reply, "more answer text") {
		t.Errorf("the durable reply lost its text: %q", reply)
	}
	if !strings.Contains(strings.Join(reasoning, " "), "thinking one") {
		t.Errorf("the durable reasoning is missing, so the mirror row has nothing to render: %q", reasoning)
	}
}
