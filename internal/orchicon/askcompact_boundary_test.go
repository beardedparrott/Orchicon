package orchicon

// The compaction TAIL BOUNDARY regression. CompactConversationSession used to
// keep the last askCompactTailMessages messages with a BLIND slice:
//
//	tail := history
//	if len(tail) > askCompactTailMessages {
//		tail = tail[len(tail)-askCompactTailMessages:]
//	}
//
// When the cut fell inside a tool round, the new history LED with a bare tool
// result whose declaring assistant tool_use had just been collapsed away. That
// is the orphaned-result shape providers reject outright:
//
//	Messages with role 'tool' must be a response to a preceding message with
//	'tool_calls'
//
// and because the native transport re-sends the whole history every turn, the
// conversation wedged permanently. Observed LIVE on Ask conversation
// 01M2C8VXFQY5ZE26PYBSNKA2CA: after compaction its persisted session file began
// assistant(summary), tool, assistant, tool, assistant, tool, assistant — the
// tail slice [tool, assistant, tool, assistant, tool, assistant] leading on an
// orphaned result.

import (
	"context"
	"fmt"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestCompactTailBoundaryWouldOrphanOnABlindSlice pins the reproduction
// independently of the fix: the message at the blind cut index really is a tool
// result whose declaring assistant sits one index outside the window. If this
// ever stops holding, the regression test below has stopped exercising the bug.
func TestCompactTailBoundaryWouldOrphanOnABlindSlice(t *testing.T) {
	hist := toolRoundHistory(5, true)
	cut := len(hist) - askCompactTailMessages

	if cut <= 0 || cut >= len(hist) {
		t.Fatalf("fixture does not exercise a mid-history cut: len=%d cut=%d", len(hist), cut)
	}
	if hist[cut].Role != RoleTool {
		t.Fatalf("fixture no longer cuts mid-round: history[%d].Role = %q, want %q", cut, hist[cut].Role, RoleTool)
	}
	blind := hist[cut:]
	if !hasOrphanedToolResults(blind) {
		t.Fatalf("the blind slice is not orphaned, so the fixture proves nothing: %s", describeHistory(blind))
	}
	if hist[cut-1].Role != RoleAssistant {
		t.Fatalf("the declaring assistant is not one index before the cut: history[%d].Role = %q", cut-1, hist[cut-1].Role)
	}
}

// TestCompactConversationSessionTailNeverStartsOnOrphanedToolResult is the
// regression: compaction must keep the round WHOLE (walking the cut back to the
// declaring assistant) so the history the next turn re-sends is well-formed in
// both directions.
func TestCompactConversationSessionTailNeverStartsOnOrphanedToolResult(t *testing.T) {
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "SUMMARY"}}}
	b := newCompactBridge(t, prov)
	const convID = "01COMPACTAILBOUNDARY00000"
	ctx := tenant.WithID(context.Background(), "tnt")
	sid, err := b.CreateConversationSession(ctx, convID, "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}

	hist := toolRoundHistory(5, true)
	b.mu.Lock()
	b.chatHistory[sid] = hist
	b.mu.Unlock()

	res, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: convID, SessionID: sid, ModelRef: "orchicon/deepseek/deepseek-flash",
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected Compacted=true, detail=%q", res.Detail)
	}

	b.mu.Lock()
	got := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()

	// The whole point: what the next turn re-sends is valid in BOTH directions.
	assertHistoryToolPairsWellFormed(t, got)

	// The tail does not lead on a tool result — the orphan shape.
	if got[1].Role == RoleTool {
		t.Fatalf("the compacted tail leads on a tool result: %s", describeHistory(got))
	}
	// The boundary round was kept WHOLE rather than split: aligning the cut back
	// to the declaring assistant is what preserves call_2's result. Dropping it
	// would also be "valid", but it would silently discard a tool result the
	// operator paid for — the worker path's middle eviction sets the same
	// precedent (keep the round, never orphan one half of it).
	if !historyContainsToolResult(got, "call_2") {
		t.Fatalf("the boundary round was dropped instead of kept whole: %s", describeHistory(got))
	}
	// summary + the aligned tail (one message more than the blind slice kept).
	if len(got) != askCompactTailMessages+2 {
		t.Fatalf("history len = %d, want %d (summary + aligned tail): %s",
			len(got), askCompactTailMessages+2, describeHistory(got))
	}
	// The summary framing is untouched by the boundary fix.
	if !historyContainsText(got, "Earlier conversation compacted") {
		t.Fatal("the compact summary was lost")
	}
}

// TestCompactConversationSessionTailAlignmentClampsAtZero covers a history
// whose entire content is one unfinished round: there is no declaring message
// before the cut, so the walk-back clamps at 0 and the sanitizer's backward half
// (not the alignment) is what makes the result valid. Either way the compacted
// history must never be the orphan shape.
func TestCompactConversationSessionTailAlignmentClampsAtZero(t *testing.T) {
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "SUMMARY"}}}
	b := newCompactBridge(t, prov)
	const convID = "01COMPACTAILCLAMP000000"
	ctx := tenant.WithID(context.Background(), "tnt")
	sid, err := b.CreateConversationSession(ctx, convID, "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}

	// A long enough history to compact (>= askCompactMinMessages), whose trailing
	// messages are all tool results of calls declared before them — so a walk-back
	// from the cut crosses several tool messages.
	hist := toolRoundHistory(8, true)
	b.mu.Lock()
	b.chatHistory[sid] = hist
	b.mu.Unlock()

	res, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: convID, SessionID: sid, ModelRef: "orchicon/deepseek/deepseek-flash",
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected Compacted=true, detail=%q", res.Detail)
	}

	b.mu.Lock()
	got := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	assertHistoryToolPairsWellFormed(t, got)
	if len(got) > 1 && got[1].Role == RoleTool {
		t.Fatalf("the compacted tail leads on a tool result: %s", describeHistory(got))
	}
}

// toolRoundHistory builds the shape that wedges compaction: a user goal, then n
// complete tool rounds (assistant call + its result), and — when textTail is set
// — a final text-only assistant reply. With n rounds and a text tail the message
// count is 1+2n+1, so a blind last-6 slice starts one index past a declaring
// assistant whenever n >= 3: the cut falls between an assistant and its result.
func toolRoundHistory(rounds int, textTail bool) []Message {
	hist := []Message{{Role: RoleUser, Content: []Content{{Text: compactTestStr("do the work")}}}}
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("call_%d", i)
		hist = append(hist,
			Message{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{
				ToolCallID: id, Name: "bash", ArgsJSON: `{"command":"ls"}`,
			}}}},
			Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{
				ToolCallID: id, Content: "ok",
			}}}},
		)
	}
	if textTail {
		hist = append(hist, Message{Role: RoleAssistant, Content: []Content{{Text: compactTestStr("here is what I found")}}})
	}
	return hist
}
