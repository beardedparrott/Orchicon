package chat

// page_tool_calls_test.go — THE WIRE -> SHELL HALF OF THE ACTIVITY LINE'S ROLLING COUNTER.
//
// pageToolCalls reads the ListMessages page and pulls the durable ToolCall rows (name + issue stamp)
// off the SAME page the transcript is built from, which is what lets the counter cost no fetch of its
// own (AC10). Nothing else in either package exercises it: the shell's AC1 test seeds chatStore
// directly, so a regression HERE — a renamed proto accessor, a dropped stamp, or a nil entry
// dereferenced on a poll that runs once a second — leaves every other test green while the operator's
// counter renders "" over a turn that is plainly working, with no error anywhere.
//
// These drive the real proto shape, so the seam the whole feature rests on fails loudly.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/toolclass"
)

// TestPageToolCallsReadsNameAndStamp pins the two fields the counter needs, off a mixed page.
func TestPageToolCallsReadsNameAndStamp(t *testing.T) {
	msgs := []*apiv1.ChatMessage{
		{Id: "m2", ToolCalls: []*apiv1.ToolCall{
			{Id: "tc-1", FunctionName: "write", IssuedAtUnixMs: 1_700_000_000_123},
			{Id: "tc-2", FunctionName: "read", IssuedAtUnixMs: 1_700_000_000_456},
		}},
		{Id: "m1"}, // a message with no tool calls contributes nothing
	}
	want := []toolclass.Call{
		{ToolName: "write", AtMs: 1_700_000_000_123},
		{ToolName: "read", AtMs: 1_700_000_000_456},
	}
	got := pageToolCalls(msgs)
	if len(got) != len(want) {
		t.Fatalf("pageToolCalls returned %d calls, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v — the counter's name or issue stamp is being dropped at the "+
				"wire, and every client would then count nothing", i, got[i], want[i])
		}
	}
}

// TestPageToolCallsCarriesAnUnstampedRowThrough pins the pre-change row's shape: a call with no stamp
// is carried as AtMs 0, which SummarizeCalls SKIPS. Dropping it, or reading it as the epoch, would place
// a genuinely old call inside the rolling window and render a wrong count for an old conversation.
func TestPageToolCallsCarriesAnUnstampedRowThrough(t *testing.T) {
	got := pageToolCalls([]*apiv1.ChatMessage{
		{ToolCalls: []*apiv1.ToolCall{{FunctionName: "bash"}}},
	})
	if len(got) != 1 || got[0].AtMs != 0 || got[0].ToolName != "bash" {
		t.Fatalf("an unstamped row came through as %+v, want {bash, 0}", got)
	}
}

// TestPageToolCallsToleratesNilEntries guards the extraction against a panic. It runs on EVERY
// transcript poll (1s while a turn runs), so a nil *ToolCall or a nil *ChatMessage dereferenced here is
// a dead TUI — and a malformed page is exactly when that is least affordable.
func TestPageToolCallsToleratesNilEntries(t *testing.T) {
	got := pageToolCalls([]*apiv1.ChatMessage{
		{Id: "m1", ToolCalls: []*apiv1.ToolCall{nil, {FunctionName: "write", IssuedAtUnixMs: 9}}},
		nil,
	})
	if len(got) != 1 || got[0].ToolName != "write" || got[0].AtMs != 9 {
		t.Fatalf("a nil tool entry or a nil message broke the extraction: %+v", got)
	}
}
