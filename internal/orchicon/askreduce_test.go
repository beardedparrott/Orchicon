package orchicon

import (
	"context"
	"encoding/json"
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
func TestIsOversizePromptErrorMatchesRealProviderPhrasings(t *testing.T) {
	cases := []string{
		// THE OPERATOR'S OWN REPORT, VERBATIM — the DeepSeek free model on the Windows
		// plane. It is the most important case in this list because it is the one that
		// reached a human: the free tier's window is small, so it is hit routinely rather
		// than on a freak 1M-token session.
		`turn failed: Your request resulted in more tokens than this model's maximum context length allows. Please reduce the length of the messages or run compaction.`,
		`orchicon bridge: start Ask turn: provider status 400 400 Bad Request: {"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1049352 tokens (1016584 in the messages)"}}`,
		`provider status 400: context_length_exceeded`,
		`anthropic: prompt is too long: 210000 tokens > 200000 maximum`,
		`please reduce the length of the messages`,
		`too many tokens in request`,
		// THE LIVE 413, VERBATIM from the dev plane's log. Its body is HTML served by
		// Cloudflare, so it carries no JSON error object — the phrase list is the only
		// thing that can catch it. Without these the hatch never opens and the
		// conversation re-sends the same oversized body forever.
		"orchicon bridge: start Ask turn: provider status 413 413 Request Entity Too Large: <html>\r\n<head><title>413 Payload Too Large</title></head>\r\n<body>\r\n<center><h1>413 Payload Too Large</h1></center>\r\n<hr><center>cloudflare</center>\r\n</body>\r\n</html>\r\n",
		`provider status 413: Request Entity Too Large`,
		`413 Payload Too Large`,
		`request body too large`,
	}
	for _, c := range cases {
		if !IsOversizePromptError(errors.New(c)) {
			t.Errorf("must be recognised as an oversize-prompt error: %s", c)
		}
	}
}

// TestIsOversizePromptMessageSharesTheOneList pins the seam the second wire
// depends on: a caller holding only a MESSAGE (the opencode serve's session.error)
// classifies against the SAME signature list, so the 413 and context-window
// phrasings cannot drift into two lists that disagree.
func TestIsOversizePromptMessageSharesTheOneList(t *testing.T) {
	for _, c := range OversizePromptSignatures {
		if !IsOversizePromptMessage("prefix " + c + " suffix") {
			t.Errorf("signature %q must classify through the message form", c)
		}
	}
	if IsOversizePromptMessage("") {
		t.Error("an empty message must not be an oversize-prompt error")
	}
	if IsOversizePromptMessage("provider status 401 unauthorized") {
		t.Error("an unrelated message must not be an oversize-prompt error")
	}
}

// A miss on an unrelated error must NOT trigger a lossy reduction.
func TestIsOversizePromptErrorIgnoresUnrelatedErrors(t *testing.T) {
	cases := []string{
		"",
		"provider status 401 unauthorized",
		"connection refused",
		"No tool output found for function call call_1",
		"rate limit exceeded",
		// A BARE 413 IS NOT ENOUGH: the number appears in token counts and ids, so only
		// the provider-status form or the phrases may match. A false positive here is not
		// harmless — it permanently elides real tool results from the history.
		"413 tokens remain",
		"provider status 404 not found",
		"the entity was not found",
	}
	for _, c := range cases {
		if IsOversizePromptError(errors.New(c)) {
			t.Errorf("must NOT be treated as an oversize-prompt error: %s", c)
		}
	}
	if IsOversizePromptError(nil) {
		t.Error("nil error must not be an oversize-prompt error")
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

// Tool-call ARGUMENTS are trimmed, but the pairing and the tool name survive: a
// file-writing call carries a whole file body in its arguments, which is the same
// class of bloat as a tool result. The result must stay valid JSON (a malformed
// tool call could itself be rejected on replay).
func TestReduceTrimsToolArgsButKeepsIDNameAndValidJSON(t *testing.T) {
	bigArgs := `{"path":"a.go","content":"` + strings.Repeat("Y", 50_000) + `"}`
	history := []Message{
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "call_9", Name: "batch_write", ArgsJSON: bigArgs}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call_9", Content: "ok"}}}},
	}
	reduced, st := ReduceConversationHistoryForContext(history, 0)

	if st.ToolArgsTruncated != 1 {
		t.Errorf("ToolArgsTruncated = %d, want 1", st.ToolArgsTruncated)
	}
	use := reduced[0].Content[0].ToolUse
	if use == nil {
		t.Fatal("the tool call was dropped — its result would then be unpaired")
	}
	if use.ToolCallID != "call_9" {
		t.Errorf("tool call id changed: %q (pairing broken)", use.ToolCallID)
	}
	if use.Name != "batch_write" {
		t.Errorf("tool name changed: %q (the model must still see WHICH tool ran)", use.Name)
	}
	if len(use.ArgsJSON) >= len(bigArgs)/10 {
		t.Errorf("arguments barely shrank: %d of %d bytes", len(use.ArgsJSON), len(bigArgs))
	}
	if !json.Valid([]byte(use.ArgsJSON)) {
		t.Errorf("truncated arguments are not valid JSON: %q", use.ArgsJSON)
	}
	if !strings.Contains(use.ArgsJSON, "tool arguments truncated") {
		t.Errorf("the elision must be stated, got %q", use.ArgsJSON)
	}
	// Small arguments are untouched.
	small := `{"path":"a.txt"}`
	kept, truncated := capToolArgs(small)
	if truncated || kept != small {
		t.Errorf("small args must pass through verbatim, got (%q, %v)", kept, truncated)
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
	if !IsOversizePromptError(err) {
		t.Errorf("the ORIGINAL oversize-prompt error must surface, got: %v", err)
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
		t.Error("history must be untouched when the error is not an oversize-prompt error")
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

// THE SHAPE THAT ACTUALLY WEDGED THE LIVE CONVERSATION: a screenshot returned as
// TOOL-RESULT TEXT, not as an Image content part.
//
// The existing wedge fixture uses Content.Image, so it never exercised this. A real
// `obs` MCP turn carries a base64 PNG inside the tool result's Content string, which
// is why the live persist pass reported images_dropped=0 while the history reached
// 25.8MB gzipped — trimAskHistoryImages only knows Content.Image. THIS pass is the
// one that must reclaim it, and it does so by eliding the tool result rather than by
// understanding what an image is, which is why it is the right net for both.
func TestOversizeToolResultScreenshotIsReclaimed(t *testing.T) {
	ask := "is the scene showing up?"
	var b64 strings.Builder
	b64.WriteString("data:image/png;base64,")
	for i := 0; i < 200_000; i++ { // ~4.6MB of near-incompressible payload
		b64.WriteString("iVBORw0KGgoAAAANSUhEUg")
	}
	screenshot := b64.String()

	use := ContentToolUse{ToolCallID: "call_obs", Name: "obs-get-source-screenshot", ArgsJSON: `{"sourceName":"Screen Capture"}`}
	result := ContentToolResult{ToolCallID: "call_obs", Content: screenshot}

	history := []Message{
		{Role: RoleUser, Content: []Content{{Text: &ask}}},
		{Role: RoleAssistant, Content: []Content{{ToolUse: &use}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &result}}},
	}
	before := conversationBytes(history)

	reduced, st := ReduceConversationHistoryForContext(history, 0)

	if st.ImagesDropped != 0 {
		t.Fatalf("fixture error: this shape carries NO Image content part, got ImagesDropped=%d — that "+
			"zero is precisely why trimAskHistoryImages could not shrink the live history", st.ImagesDropped)
	}
	if st.ToolResultsElided != 1 {
		t.Errorf("the screenshot tool result must be elided, got ToolResultsElided=%d", st.ToolResultsElided)
	}
	after := conversationBytes(reduced)
	if after > 5_000 {
		t.Errorf("the screenshot survived: %d bytes before, %d after", before, after)
	}
	if st.Reclaimed() < before/2 {
		t.Errorf("Reclaimed()=%d of %d — the bulk must be gone", st.Reclaimed(), before)
	}
	// The pairing survives, or the retry is rejected for a different reason.
	if reduced[2].Content[0].ToolResult.ToolCallID != "call_obs" {
		t.Error("the tool result lost its ToolCallID — the retry would be rejected as an unpaired call")
	}
}

// THE OPERATOR'S ERROR, END TO END: a payload-too-large rejection must open the hatch
// and RESCUE the turn, not be re-sent forever.
//
// Before this, `provider status 413 413 Request Entity Too Large` matched nothing in
// the signature list, so startTurnWithContextRecovery returned on the FIRST refusal
// and every retry re-sent the identical oversized body. The dev plane's log shows
// exactly that shape: three consecutive 413s and ZERO "history reduced, retrying"
// lines — the hatch was never reached.
func TestAPayloadTooLarge413RescuesTheTurn(t *testing.T) {
	b := wedgeBridge()
	tooLarge := errors.New("orchicon bridge: start Ask turn: provider status 413 413 Request Entity Too Large: " +
		"<html>\r\n<head><title>413 Payload Too Large</title></head>\r\n<body>\r\n<center><h1>413 " +
		"Payload Too Large</h1></center>\r\n<hr><center>cloudflare</center>\r\n</body>\r\n</html>\r\n")
	prov := &scriptedProvider{errs: []error{tooLarge, nil}}
	req := &TurnRequest{Messages: b.chatHistory["s1"]}

	if _, err := b.startTurnWithContextRecovery(context.Background(), prov, req, "s1"); err != nil {
		t.Fatalf("the 413 must be recovered exactly like a context overflow, got: %v", err)
	}
	if prov.calls != 2 {
		t.Errorf("StreamTurn calls = %d, want 2 (refused once, accepted once)", prov.calls)
	}
	if conversationBytes(req.Messages) > 50_000 {
		t.Errorf("the ACCEPTED request must be the reduced one, got %d bytes", conversationBytes(req.Messages))
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
