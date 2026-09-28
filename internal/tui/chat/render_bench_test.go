package chat

// render_bench_test.go — what a LONG conversation costs to RENDER.
//
// The operator: "I had a conversation that had over 250 messages. Not only does
// it make the conversation incredibly slow, it makes the entire TUI unresponsive
// and I had to kill it multiple times. I am guessing that is because we always
// have the conversation technically up and live on every pane down near the
// composer box."
//
// The renderer is the suspect and the guess is right about the mechanism: both
// surfaces that draw a conversation are O(ALL messages) on every call —
//
//	panelTranscript   (chatpanel.go:216) — called from baseView, i.e. on EVERY
//	                  FRAME of every tab while the strip is open;
//	syncTranscript    (app.go:3517)      — called on every chat wake.
//
// and RenderItems re-renders every item from scratch: markdown, lipgloss, and
// box drawing for each one, with no memoization anywhere and no revision
// counter on chatStore to memoize against.
//
// These benchmarks exist to make that cost a NUMBER, at the size the operator
// actually hit, so a fix can be judged against it rather than against a feeling.
// Run:
//
//	go test ./internal/tui/chat/ -run '^$' -bench BenchmarkRenderItems -benchtime 5x

import (
	"fmt"
	"strings"
	"testing"
)

// benchmarkConversation builds a conversation of n items shaped like a real one:
// the operator's messages interleaved with model replies that carry the things
// that actually cost money to lay out — prose, fenced code, and lists.
func benchmarkConversation(n int) []ChatItem {
	items := make([]ChatItem, 0, n)
	prose := strings.Repeat("Here is what I found in the reconciler: the row is written, and then the binding is dereferenced without a nil check. ", 4)
	reply := prose + "\n\n```go\nif shouldStart {\n\tworkflow.StartWorkflowDirect(ctx, tenant, *updated.WorkflowID)\n}\n```\n\n- the tool path skips the branch\n- the Connect handler routes it to the chain\n- the parent is stored unbound\n"
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			items = append(items, ChatItem{
				Kind: KindUser,
				Key:  fmt.Sprintf("u-%d", i),
				Text: "Can you look at the roll-out path and tell me where it breaks?",
			})
			continue
		}
		items = append(items, ChatItem{
			Kind: KindText,
			Key:  fmt.Sprintf("m-%d", i),
			Text: reply,
		})
	}
	return items
}

// BenchmarkRenderItems250 is the operator's conversation: 250 messages, the size
// at which the whole TUI stopped responding.
func BenchmarkRenderItems250(b *testing.B) {
	items := GroupByPhase(benchmarkConversation(250))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RenderItems(items, 100)
	}
}

// BenchmarkRenderItems50 is the same render at a size that feels fine, to show
// how the cost scales with conversation length rather than with what changed.
func BenchmarkRenderItems50(b *testing.B) {
	items := GroupByPhase(benchmarkConversation(50))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RenderItems(items, 100)
	}
}
