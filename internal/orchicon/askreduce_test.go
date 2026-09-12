package orchicon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// testLogger is a silent slog logger (the bridge logs reductions at warn).
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
}

// wedgeHistory builds a history shaped like the real conversation that wedged
// (images + fat tool results dominating, little text).
func wedgeHistory() []Message {
	userText := "please look at this screenshot"
	toolOut := strings.Repeat("x", 120_000)
	imageData := "data:image/png;base64," + strings.Repeat("A", 700_000)
	assistantText := "Here is what I see."

	return []Message{
		{Role: RoleUser, Content: []Content{{Text: &userText}, {Image: &imageData}}},
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "call_1", Name: "read", ArgsJSON: `{"path":"a.txt"}`}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call_1", Content: toolOut}}}},
		{Role: RoleAssistant, Content: []Content{{Text: &assistantText}}},
	}
}

func wedgeBridge() *NativeBridge {
	return &NativeBridge{log: testLogger(), chatHistory: map[string][]Message{"s1": wedgeHistory()}}
}

// The EXACT phrasing from the real stuck session (DeepSeek/OpenAI-compatible
// surface) must be recognised, or the recovery never triggers for the very case
// that motivated it.
func TestIsContextLengthErrorMatchesRealProviderPhrasings(t *testing.T) {
	cases := []string{
		`orchicon bridge: start Ask turn: provider status 400 400 Bad Request: {"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1049352 tokens (1016584 in the messages)"}}`,
		`provider status 400: context_length_exceeded`,
		`anthropic: prompt is too long: 210000 tokens > 200000 maximum`,
		`please reduce the length of the messages`,
		`too many tokens in request`,
	}
	for _, c := range cases {
		if !isContextLengthError(errors.New(c)) {
			t.Errorf("must be recognised as a context-length error: %s", c)
		}
	}
}

// A miss on an unrelated error must NOT trigger a lossy reduction.
func TestIsContextLengthErrorIgnoresUnrelatedErrors(t *testing.T) {
	cases := []string{
		"",
		"provider status 401 unauthorized",
		"connection refused",
		"No tool output found for function call call_1",
		"rate limit exceeded",
	}
	for _, c := range cases {
		if isContextLengthError(errors.New(c)) {
			t.Errorf("must NOT be treated as a context-length error: %s", c)
		}
	}
	if isContextLengthError(nil) {
		t.Error("nil error must not be a context-length error")
	}
}

func TestReduceDropsImagesAndElidesToolResultsButKeepsText(t *testing.T) {
	before := wedgeHistory()
	reduced, st := ReduceConversationHistoryForContext(before, 0)

	if st.ImagesDropped != 1 {
		t.Errorf("ImagesDropped = %d, want 1", st.ImagesDropped)
	}
	if st.ToolResultsElided != 1 {
		t.Errorf("ToolResultsElided = %d, want 1", st.ToolResultsElided)
	}
	// The pass must actually reclaim the bulk of the payload.
	if st.Reclaimed() < 700_000 {
		t.Errorf("Reclaimed() = %d, want >= 700000 (images + tool output)", st.Reclaimed())
	}

	for _, m := range reduced {
		for _, c := range m.Content {
			if c.Image != nil {
				t.Fatal("an image survived the reduction")
			}
		}
	}
	// Text is preserved VERBATIM (that is the part the reader follows).
	if got := *reduced[0].Content[0].Text; got != "please look at this screenshot" {
		t.Errorf("user text changed: %q", got)
	}
	if got := *reduced[3].Content[0].Text; got != "Here is what I see." {
		t.Errorf("assistant text changed: %q", got)
	}
	// The elided tool result keeps its head and states the loss.
	elided := reduced[2].Content[0].ToolResult
	if !strings.HasPrefix(elided.Content, strings.Repeat("x", 50)) {
		t.Error("elided tool result must keep a head of the original output")
	}
	if !strings.Contains(elided.Content, "tool result truncated") {
		t.Error("elided tool result must say it was truncated")
	}
	if len(elided.Content) > toolResultHeadChars+200 {
		t.Errorf("elided tool result still long: %d chars", len(elided.Content))
	}
}

// Structure must survive: a dropped tool_use/tool_result pairing is itself a
// provider 400, so ids and roles are load-bearing.
func TestReducePreservesRolesAndToolPairing(t *testing.T) {
	before := wedgeHistory()
	reduced, _ := ReduceConversationHistoryForContext(before, 0)

	if len(reduced) != len(before) {
		t.Fatalf("message count changed: %d -> %d", len(before), len(reduced))
	}
	for i := range before {
		if reduced[i].Role != before[i].Role {
			t.Errorf("message %d role changed: %s -> %s", i, before[i].Role, reduced[i].Role)
		}
		if len(reduced[i].Content) != len(before[i].Content) {
			t.Errorf("message %d content count changed: %d -> %d", i, len(before[i].Content), len(reduced[i].Content))
		}
	}
	if id := reduced[2].Content[0].ToolResult.ToolCallID; id != "call_1" {
		t.Errorf("tool result lost its ToolCallID (pairing broken): %q", id)
	}
	if reduced[1].Content[0].ToolUse == nil {
		t.Error("assistant tool_use was dropped — the result would then be unpaired")
	}
}

// keepTail leaves the live thread untouched.
func TestReduceKeepsTailVerbatim(t *testing.T) {
	before := wedgeHistory()
	reduced, st := ReduceConversationHistoryForContext(before, 2)

	// Everything from index len-2 on is preserved byte-for-byte...
	if reduced[2].Content[0].ToolResult.Content != before[2].Content[0].ToolResult.Content {
		t.Error("message inside the keepTail window was reduced")
	}
	if reduced[3].Content[0].Text != before[3].Content[0].Text {
		t.Error("message inside the keepTail window was reduced")
	}
	// ...and the surgical pass still reclaims the older bloat (the image).
	if st.ImagesDropped != 1 {
		t.Errorf("ImagesDropped = %d, want 1 (the image is outside the tail)", st.ImagesDropped)
	}
	if st.Reclaimed() <= 0 {
		t.Error("surgical pass reclaimed nothing")
	}
	// A full pass reclaims at least as much as the surgical one.
	_, full := ReduceConversationHistoryForContext(before, 0)
	if full.Reclaimed() < st.Reclaimed() {
		t.Errorf("full reduction (%d) reclaimed less than surgical (%d)", full.Reclaimed(), st.Reclaimed())
	}
}

// A history with nothing to reclaim reports ok=false so the caller does not
// waste a retry.
func TestReduceSessionHistoryReportsNothingToDo(t *testing.T) {
	small := "hi"
	b := &NativeBridge{
		log:         testLogger(),
		chatHistory: map[string][]Message{"s1": {{Role: RoleUser, Content: []Content{{Text: &small}}}}},
	}
	if _, _, ok := b.reduceSessionHistory("s1", 6); ok {
		t.Error("a tiny history must report ok=false (nothing reclaimed)")
	}
	if _, _, ok := b.reduceSessionHistory("missing", 6); ok {
		t.Error("an unknown session must report ok=false")
	}
}

// The end-to-end rescue: a wedged session's history shrinks enough to be
// re-sendable, and the reduced history is what gets persisted.
func TestReduceSessionHistoryShrinksAndPersists(t *testing.T) {
	b := wedgeBridge()
	reduced, st, ok := b.reduceSessionHistory("s1", 0)
	if !ok {
		t.Fatal("a wedge-sized history must be reducible")
	}
	if st.Reclaimed() < 700_000 {
		t.Errorf("Reclaimed() = %d, want >= 700000", st.Reclaimed())
	}
	// The bridge's history must BE the reduced one (otherwise the next turn
	// re-sends the oversized original).
	if conversationBytes(b.chatHistory["s1"]) != conversationBytes(reduced) {
		t.Error("the bridge's history was not replaced with the reduced history")
	}
	if conversationBytes(reduced) > 50_000 {
		t.Errorf("reduced history is still %d bytes — it would not fit", conversationBytes(reduced))
	}
}

// A provider that refuses every attempt must be retried at most
// len(contextReduceStages) times, then the ORIGINAL error surfaces.
func TestStartTurnRetriesBoundedAndSurfacesOriginalError(t *testing.T) {
	b := wedgeBridge()
	limitErr := errors.New("This model's maximum context length is 1048576 tokens")
	prov := &scriptedProvider{errs: []error{limitErr, limitErr, limitErr, limitErr}}
	req := &TurnRequest{Messages: b.chatHistory["s1"]}

	_, err := b.startTurnWithContextRecovery(context.Background(), prov, req, "s1")
	if err == nil {
		t.Fatal("expected the turn to fail when every stage is refused")
	}
	if !isContextLengthError(err) {
		t.Errorf("the ORIGINAL context-length error must surface, got: %v", err)
	}
	// Bounded: 1 initial + 1 retry = 2 calls, never unbounded. The SECOND
	// stage contributes no retry here because stage 1 (keepTail=6 on this
	// 4-message history) already clamps to a FULL reduction, so stage 2 has
	// nothing left to reclaim — and a stage that cannot reclaim anything is
	// deliberately not retried.
	if prov.calls != 2 {
		t.Errorf("StreamTurn calls = %d, want 2 (initial + one productive retry)", prov.calls)
	}
	if ceiling := 1 + len(contextReduceStages); prov.calls > ceiling {
		t.Errorf("StreamTurn calls = %d exceeds the bounded ceiling %d", prov.calls, ceiling)
	}
	// The history was reduced along the way, so the persisted session heals.
	if conversationBytes(b.chatHistory["s1"]) > 50_000 {
		t.Error("the session history should have been reduced despite the final failure")
	}
}

// A non-context error must NOT trigger any reduction (no lossy change on an
// unrelated failure).
func TestStartTurnDoesNotReduceOnUnrelatedError(t *testing.T) {
	b := wedgeBridge()
	before := conversationBytes(b.chatHistory["s1"])
	prov := &scriptedProvider{errs: []error{errors.New("provider status 401 unauthorized")}}
	req := &TurnRequest{Messages: b.chatHistory["s1"]}

	if _, err := b.startTurnWithContextRecovery(context.Background(), prov, req, "s1"); err == nil {
		t.Fatal("expected an error")
	}
	if prov.calls != 1 {
		t.Errorf("StreamTurn calls = %d, want 1 (no retry on an unrelated error)", prov.calls)
	}
	if conversationBytes(b.chatHistory["s1"]) != before {
		t.Error("history must be untouched when the error is not a context-length error")
	}
}

// The happy path is untouched: no reduction, one call.
func TestStartTurnPassesThroughOnSuccess(t *testing.T) {
	b := wedgeBridge()
	before := conversationBytes(b.chatHistory["s1"])
	prov := &scriptedProvider{}
	req := &TurnRequest{Messages: b.chatHistory["s1"]}

	if _, err := b.startTurnWithContextRecovery(context.Background(), prov, req, "s1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.calls != 1 {
		t.Errorf("StreamTurn calls = %d, want 1", prov.calls)
	}
	if conversationBytes(b.chatHistory["s1"]) != before {
		t.Error("a successful turn must not alter history")
	}
}

// A recovery that SUCCEEDS must hand the caller the reduced request (so the
// drain commits against what the provider accepted).
func TestStartTurnSucceedsAfterReduction(t *testing.T) {
	b := wedgeBridge()
	prov := &scriptedProvider{errs: []error{errors.New("maximum context length exceeded"), nil}}
	req := &TurnRequest{Messages: b.chatHistory["s1"]}

	if _, err := b.startTurnWithContextRecovery(context.Background(), prov, req, "s1"); err != nil {
		t.Fatalf("the retry should have succeeded, got: %v", err)
	}
	if conversationBytes(req.Messages) > 50_000 {
		t.Errorf("req.Messages should be the REDUCED history, got %d bytes", conversationBytes(req.Messages))
	}
}

// scriptedProvider returns queued errors (nil = success) per StreamTurn call.
type scriptedProvider struct {
	errs  []error
	calls int
}

func (p *scriptedProvider) StreamTurn(context.Context, TurnRequest) (TurnStream, error) {
	i := p.calls
	p.calls++
	if i < len(p.errs) && p.errs[i] != nil {
		return nil, fmt.Errorf("stream turn: %w", p.errs[i])
	}
	return &noopTurnStream{}, nil
}

func (p *scriptedProvider) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }

func (p *scriptedProvider) Capabilities() Capabilities { return Capabilities{} }

// noopTurnStream satisfies TurnStream with an immediately-ended stream.
type noopTurnStream struct{}

func (s *noopTurnStream) Next(context.Context) (Event, bool, error) { return nil, false, nil }
func (s *noopTurnStream) Close() error                              { return nil }
