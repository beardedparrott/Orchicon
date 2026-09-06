package askorchicon

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// feedSeg drives a fresh thinkSegmenter over deltas and returns the
// accumulated plain text and committed think bodies.
func feedSeg(t *testing.T, deltas []string, flush bool) (text string, bodies []string) {
	t.Helper()
	seg := thinkSegmenter{}
	var txt strings.Builder
	feed := func(s string) {
		seg.feed(s,
			func(t string) { txt.WriteString(t) },
			func(b string) {},
			func(b string) { bodies = append(bodies, b) },
		)
	}
	for _, d := range deltas {
		feed(d)
	}
	if flush {
		seg.flushBody(func(b string) { bodies = append(bodies, b) })
	}
	return txt.String(), bodies
}

// Acceptance A: plain <think>body</think> streamed as TEXT deltas lands
// 100% in reasoning, zero bytes in content/TextChunk.
func TestSegmenterPlainThinkToReasoning(t *testing.T) {
	open, close := "<"+"think>", "<"+"/think>"
	text, bodies := feedSeg(t, []string{"answer start ", open + "deep thought here" + close + " answer end"}, true)
	if len(bodies) != 1 || bodies[0] != "deep thought here" {
		t.Fatalf("bodies = %q, want [deep thought here]", bodies)
	}
	if text != "answer start  answer end" {
		t.Fatalf("text = %q, want clean answer without tag remnant or body", text)
	}
}

// Acceptance A: <thought>, <reasoning>, bare <think> each route to
// reasoning with no tag remnant in text.
func TestSegmenterThoughtReasoningTags(t *testing.T) {
	for _, tc := range []struct{ open, close, body string }{
		{"<" + "thought>", "<" + "/thought>", "thought body"},
		{"<" + "reasoning>", "<" + "/reasoning>", "reasoning body"},
		{"<" + "think>", "<" + "/think>", "think body"},
		{"|" + "<thinking>", "|" + "</thinking>", "pipe body"},
	} {
		text, bodies := feedSeg(t, []string{"a " + tc.open + tc.body + tc.close + " b"}, true)
		if len(bodies) != 1 || bodies[0] != tc.body {
			t.Errorf("%s: bodies = %q, want [%s]", tc.open, bodies, tc.body)
		}
		if text != "a  b" {
			t.Errorf("%s: text = %q, want %q", tc.open, text, "a  b")
		}
	}
}

// Acceptance A: open tag fragmented token-by-token across 3+ deltas never
// leaks a tag fragment into text (each spelling, incl. pipe forms).
func TestSegmenterFragmentedOpenAcrossDeltas(t *testing.T) {
	fragment := func(tag string) []string {
		// Split into 1-rune deltas (worst case: token-by-token).
		var out []string
		for _, r := range tag {
			out = append(out, string(r))
		}
		return out
	}
	for _, tag := range []string{
		"<" + "think>", "<" + "thought>", "<" + "reasoning>",
		"|" + "<thinking>", "|" + "think",
	} {
		// Feed open fragmented, then a body, then the matching close
		// fragmented rune-by-rune too.
		var deltas2 []string
		deltas2 = append(deltas2, fragment(tag)...)
		deltas2 = append(deltas2, "hidden body")
		var close string
		switch tag {
		case "<" + "think>":
			close = "<" + "/think>"
		case "<" + "thought>":
			close = "<" + "/thought>"
		case "<" + "reasoning>":
			close = "<" + "/reasoning>"
		case "|" + "<thinking>":
			close = "|" + "</thinking>"
		default:
			close = "|" + "/think"
		}
		// Fragment the close tag rune-by-rune too.
		deltas2 = append(deltas2, fragment(close)...)
		text, bodies := feedSeg(t, deltas2, true)
		if len(bodies) != 1 || bodies[0] != "hidden body" {
			t.Errorf("%s: bodies = %q, want [hidden body]", tag, bodies)
		}
		if text != "" {
			t.Errorf("%s: text = %q, want empty (tag fragment leaked)", tag, text)
		}
	}
}

// Acceptance A: unterminated trailing think flushes to reasoning, reply
// content stays empty/clean.
func TestSegmenterUnterminatedFlush(t *testing.T) {
	text, bodies := feedSeg(t, []string{"|" + "<thinking>" + "partial thought"}, true)
	if len(bodies) != 1 || bodies[0] != "partial thought" {
		t.Fatalf("bodies = %q, want [partial thought]", bodies)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	// Plain-<think> variant.
	text, bodies = feedSeg(t, []string{"<" + "think>" + "cut off mid-stream"}, true)
	if len(bodies) != 1 || bodies[0] != "cut off mid-stream" {
		t.Fatalf("bodies = %q, want [cut off mid-stream]", bodies)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

// Acceptance A (turn level): native reasoning part + folded think body in
// the SAME turn coexist as two separate reasoning[] entries; content has
// no tag remnant and no leaked body.
func TestTurnNativeReasoningPlusFoldedBodyCoexist(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")
	ack, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	waitForSend(t, client, 1)
	ses := client.sendCalls[0].sessionID
	foldOpen, foldClose := "<"+"think>", "<"+"/think>"
	client.sub.feed(busReasoning(ses, "native thinking block"))
	client.sub.feed(busText(ses, "visible "+foldOpen+"folded body"+foldClose+" answer"))
	client.sub.feed(busIdle(ses))
	row := waitForMessage(t, pool, convID, ack)
	if len(row.Reasoning) != 2 {
		t.Fatalf("reasoning = %q, want 2 entries (native + folded)", row.Reasoning)
	}
	if row.Reasoning[0] != "native thinking block" {
		t.Errorf("reasoning[0] = %q, want native block first", row.Reasoning[0])
	}
	if row.Reasoning[1] != "folded body" {
		t.Errorf("reasoning[1] = %q, want folded body second", row.Reasoning[1])
	}
	if strings.Contains(row.Content, "think") || strings.Contains(row.Content, "folded body") {
		t.Errorf("content = %q, leaks think markup/body", row.Content)
	}
	if strings.TrimSpace(row.Content) != "visible  answer" {
		t.Errorf("content = %q, want %q", row.Content, "visible  answer")
	}
}

// Acceptance A (turn level): plain <think> streamed as TEXT deltas lands
// in reasoning with zero bytes in content.
func TestTurnPlainThinkDeltasToReasoning(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")
	ack, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	waitForSend(t, client, 1)
	ses := client.sendCalls[0].sessionID
	client.sub.feed(busDelta(ses, "pre "))
	client.sub.feed(busDelta(ses, "<"+"think>"))
	client.sub.feed(busDelta(ses, "streamed thought"))
	client.sub.feed(busDelta(ses, "<"+"/think>"))
	client.sub.feed(busDelta(ses, " post"))
	client.sub.feed(busText(ses, "pre  post"))
	client.sub.feed(busIdle(ses))
	row := waitForMessage(t, pool, convID, ack)
	if len(row.Reasoning) != 1 || row.Reasoning[0] != "streamed thought" {
		t.Fatalf("reasoning = %q, want [streamed thought]", row.Reasoning)
	}
	if strings.Contains(row.Content, "think") || strings.Contains(row.Content, "streamed thought") {
		t.Errorf("content = %q, leaks think markup/body", row.Content)
	}
}
