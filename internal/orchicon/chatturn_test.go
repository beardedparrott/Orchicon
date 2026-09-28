package orchicon

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// chatTestProvider is a scripted Provider for the ChatTurnClient tests. It
// streams a fixed event sequence per StreamTurn call and records the requests
// so tests can assert the history re-send (sessionless emulation).
type chatTestProvider struct {
	mu       sync.Mutex
	events   []Event
	rounds   [][]Event // per-StreamTurn sequences; call i gets rounds[min(i,len-1)]
	preErr   error     // pre-stream failure returned from StreamTurn
	stream   TurnStream
	requests []TurnRequest
}

func (p *chatTestProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.preErr != nil {
		return nil, p.preErr
	}
	if len(p.rounds) > 0 {
		i := len(p.requests) - 1
		if i >= len(p.rounds) {
			i = len(p.rounds) - 1
		}
		return &chatTestStream{events: p.rounds[i]}, nil
	}
	if p.stream != nil {
		return p.stream, nil
	}
	return &chatTestStream{events: p.events}, nil
}
func (p *chatTestProvider) ListModels(ctx context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *chatTestProvider) Capabilities() Capabilities                          { return Capabilities{Streaming: true} }

func (p *chatTestProvider) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}
func (p *chatTestProvider) lastRequest() TurnRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		return TurnRequest{}
	}
	return p.requests[len(p.requests)-1]
}

// chatTestStream is an in-memory TurnStream over a fixed event slice.
type chatTestStream struct {
	mu     sync.Mutex
	events []Event
	i      int
}

func (s *chatTestStream) Next(ctx context.Context) (Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	return nil, false, nil
}
func (s *chatTestStream) Close() error { return nil }

// newChatBridge builds a NativeBridge with a scripted provider resolver.
func newChatBridge(t *testing.T, prov *chatTestProvider) *NativeBridge {
	t.Helper()
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	return b
}

// drainBus collects SessionEvents from a bus until it closes (or a timeout).
// It prefers draining Events() so buffered events are never lost when the
// bus closes (Done() and Events() become ready together).
func drainBus(t *testing.T, bus scheduler.SessionBus) []scheduler.SessionEvent {
	t.Helper()
	var out []scheduler.SessionEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-bus.Events():
			if !ok {
				return out
			}
			out = append(out, evt)
		case <-bus.Done():
			// Bus closed — drain any remaining buffered events before
			// returning (Events() is closed too, so this terminates).
			for {
				select {
				case evt, ok := <-bus.Events():
					if !ok {
						return out
					}
					out = append(out, evt)
				default:
					return out
				}
			}
		case <-timeout:
			t.Fatal("drainBus timed out waiting for bus close")
		}
	}
}

func TestChatTurnClientTurnLifecycle(t *testing.T) {
	prov := &chatTestProvider{events: []Event{
		TextDelta{Text: "Hello "},
		TextDelta{Text: "world"},
		Finish{StopReason: StopStop},
	}}
	b := newChatBridge(t, prov)

	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-1", "ask-orchicon:conv-1")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if !strings.HasPrefix(sid, "orchicon-ask:") {
		t.Fatalf("synthetic session id %q missing prefix", sid)
	}

	bus, err := b.Subscribe(ctx, "conv-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := b.SendTurnMessage(ctx, "conv-1", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainBus(t, bus)

	// Expect delta(text) x2, part(text) with the full reply, then idle.
	var deltas, parts, idles int
	var partText string
	for _, e := range evts {
		switch e.Kind {
		case "delta":
			deltas++
		case "part":
			parts++
			partText = e.Text
		case "idle":
			idles++
		}
	}
	if deltas != 2 {
		t.Fatalf("got %d delta events, want 2", deltas)
	}
	if parts != 1 {
		t.Fatalf("got %d part events, want 1", parts)
	}
	if partText != "Hello world" {
		t.Fatalf("part text %q, want %q", partText, "Hello world")
	}
	if idles != 1 {
		t.Fatalf("got %d idle events, want 1", idles)
	}

	// The turn's request carried the user message as context.
	req := prov.lastRequest()
	if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
		t.Fatalf("first turn request messages = %+v, want one user message", req.Messages)
	}
}

func TestChatTurnClientFollowUpReusesHistory(t *testing.T) {
	prov := &chatTestProvider{events: []Event{
		TextDelta{Text: "first reply"},
		Finish{StopReason: StopStop},
	}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-2", "ask-orchicon:conv-2")

	bus, _ := b.Subscribe(ctx, "conv-2")
	if err := b.SendTurnMessage(ctx, "conv-2", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "q1"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	drainBus(t, bus)

	// Second turn: the history must now carry user q1 + assistant "first reply"
	// + user q2 (the sessionless emulation re-sends full context).
	bus2, _ := b.Subscribe(ctx, "conv-2")
	if err := b.SendTurnMessage(ctx, "conv-2", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "q2"); err != nil {
		t.Fatalf("second send: %v", err)
	}
	drainBus(t, bus2)

	req := prov.lastRequest()
	if len(req.Messages) != 3 {
		t.Fatalf("follow-up request has %d messages, want 3 (user, assistant, user)", len(req.Messages))
	}
	if req.Messages[0].Role != RoleUser || req.Messages[1].Role != RoleAssistant || req.Messages[2].Role != RoleUser {
		t.Fatalf("follow-up message roles = %+v, want user/assistant/user", req.Messages)
	}
}

func TestChatTurnClientPreStreamFailure(t *testing.T) {
	prov := &chatTestProvider{preErr: errors.New("auth failed")}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-3", "ask-orchicon:conv-3")

	// A pre-stream failure must surface as a SendTurnMessage error (the
	// collector fails the turn as a send-accept failure) — never a dropped
	// first delta.
	err := b.SendTurnMessage(ctx, "conv-3", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi")
	if err == nil {
		t.Fatal("SendTurnMessage succeeded; want pre-stream failure error")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("error %q does not carry the pre-stream cause", err)
	}
}

func TestChatTurnClientAbortCancelsTurn(t *testing.T) {
	// A provider whose stream blocks until the context is cancelled.
	prov := &chatTestProvider{stream: &blockingStream{}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-4", "ask-orchicon:conv-4")

	bus, _ := b.Subscribe(ctx, "conv-4")
	if err := b.SendTurnMessage(ctx, "conv-4", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	// Abort the in-flight turn; the drain goroutine's Next(ctx) returns
	// ctx.Err and the bus closes without an idle.
	if err := b.AbortConversationSession(ctx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}
	evts := drainBus(t, bus)
	for _, e := range evts {
		if e.Kind == "idle" {
			t.Fatal("aborted turn emitted an idle event; want no completion")
		}
	}
	// Abort is a safe no-op for unknown sessions.
	if err := b.AbortConversationSession(ctx, "unknown-session"); err != nil {
		t.Fatalf("AbortConversationSession(unknown): %v", err)
	}
}

// blockingStream blocks on Next until the context is cancelled, then returns
// the context error — the shape a real HTTP turn stream takes on abort.
type blockingStream struct{}

func (s *blockingStream) Next(ctx context.Context) (Event, bool, error) {
	<-ctx.Done()
	return nil, false, ctx.Err()
}
func (s *blockingStream) Close() error { return nil }

// TestChatTurnClientReplyPermissionReleasesAParkedCall replaces the assertion that
// this used to make — that a native Ask turn is "text-only" and permission
// approval is unsupported. That was true when the native path executed no tools;
// it runs bash and writes constantly now, so the old test asserted that a
// FEATURE WAS ABSENT, and it had to change with the feature rather than after it.
//
// What matters now is the contract: a decision releases the call that is parked
// on it, and a decision for an ask nobody is waiting on is inert rather than an
// error.
func TestChatTurnClientReplyPermissionReleasesAParkedCall(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "10m")
	b, bus, cancel := newConsentTestBridge(t)
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- b.awaitConsentPermission(context.Background(), bus,
			ToolCall{ToolCallID: "tc-1", Name: "bash", ArgsJSON: `{"command":"ls"}`}, `{"command":"ls"}`)
	}()
	id := waitForAsk(t, b)

	if err := b.ReplyPermission(context.Background(), "s", id); err != nil {
		t.Fatalf("ReplyPermission returned %v; it must release the parked call", err)
	}
	select {
	case d := <-done:
		if d != "once" {
			t.Fatalf("decision = %q, want \"once\"", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReplyPermission did not release the parked call")
	}

	// A decision for an ask nobody is waiting on: inert, not an error.
	if err := b.ReplyPermission(context.Background(), "s", "native-ask-nonexistent"); err != nil {
		t.Fatalf("ReplyPermission for an unknown ask returned %v, want nil", err)
	}
}

func TestChatTurnClientAttachmentsParity(t *testing.T) {
	// The native bridge implements the attachment-aware sender (parity
	// with the opencode adapter): images ride as data-URL Image parts,
	// UTF-8 text inlines fenced — never silently dropped.
	var _ scheduler.SendTurnMessageWithAttachments = (*NativeBridge)(nil)
	prov := &chatTestProvider{events: []Event{
		TextDelta{Text: "seen"},
		Finish{StopReason: StopStop},
	}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-att", "ask-orchicon:conv-att")

	bus, _ := b.Subscribe(ctx, "conv-att")
	err := b.SendTurnMessageWithAttachments(ctx, "conv-att", sid, "system", "orchicon/ollama/deepseek-v4-flash", "look", []scheduler.ChatAttachment{
		{Name: "chart.png", MimeType: "image/png", Data: []byte{1, 2, 3}},
		{Name: "notes.md", MimeType: "text/markdown", Data: []byte("# hi")},
	})
	if err != nil {
		t.Fatalf("SendTurnMessageWithAttachments: %v", err)
	}
	drainBus(t, bus)
	req := prov.lastRequest()
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 user message", len(req.Messages))
	}
	var sawText, sawImage, sawFenced bool
	for _, c := range req.Messages[0].Content {
		if c.Text != nil {
			sawText = true
			if strings.Contains(*c.Text, "notes.md") {
				sawFenced = true
			}
		}
		if c.Image != nil && strings.HasPrefix(*c.Image, "data:image/png;base64,") {
			sawImage = true
		}
	}
	if !sawText || !sawImage || !sawFenced {
		t.Fatalf("content = %+v, want text + image data URL + fenced file", req.Messages[0].Content)
	}
}

func TestChatTurnClientBinaryAttachmentFailsLoudly(t *testing.T) {
	// Non-image, non-UTF8 binaries have no native wire shape: loud,
	// actionable failure naming the file (never a silent drop).
	b := newChatBridge(t, &chatTestProvider{})
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-bin", "ask-orchicon:conv-bin")
	err := b.SendTurnMessageWithAttachments(ctx, "conv-bin", sid, "system", "orchicon/ollama/deepseek-v4-flash", "read", []scheduler.ChatAttachment{
		{Name: "doc.pdf", MimeType: "application/pdf", Data: []byte{0x25, 0x50, 0x44, 0x46, 0xff, 0xfe}},
	})
	if err == nil || !strings.Contains(err.Error(), "doc.pdf") {
		t.Fatalf("err = %v, want a loud failure naming the file", err)
	}
}

func TestChatTurnClientHistorySurvivesRestart(t *testing.T) {
	// A "restart" (fresh bridge over the same history dir) reseeds the
	// session from disk instead of starting over.
	dir := t.TempDir()
	mkBridge := func() *NativeBridge {
		prov := &chatTestProvider{events: []Event{
			TextDelta{Text: "reply"},
			Finish{StopReason: StopStop},
		}}
		resolver := ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
			return prov, nil
		})
		bb := NewBridge(resolver, "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
		bb.SetAskHistoryDir(dir)
		return bb
	}
	ctx := tenant.WithID(context.Background(), "tnt_test")
	b1 := mkBridge()
	sid, _ := b1.CreateConversationSession(ctx, "conv-restart", "ask-orchicon:conv-restart")
	bus, _ := b1.Subscribe(ctx, "conv-restart")
	if err := b1.SendTurnMessage(ctx, "conv-restart", sid, "system", "orchicon/ollama/deepseek-v4-flash", "q1"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	drainBus(t, bus)

	// Fresh bridge (restart), same dir: the follow-up must re-send the
	// prior user + assistant context.
	b2 := mkBridge()
	bus2, _ := b2.Subscribe(ctx, "conv-restart")
	// CreateConversationSession must not wipe the persisted file.
	if _, err := b2.CreateConversationSession(ctx, "conv-restart", "ask-orchicon:conv-restart"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if err := b2.SendTurnMessage(ctx, "conv-restart", sid, "system", "orchicon/ollama/deepseek-v4-flash", "q2"); err != nil {
		t.Fatalf("second send: %v", err)
	}
	drainBus(t, bus2)
	b2.mu.Lock()
	hist := append([]Message(nil), b2.chatHistory[sid]...)
	b2.mu.Unlock()
	if len(hist) != 4 {
		t.Fatalf("reseeded history has %d messages, want 4 (user, assistant, user, assistant)", len(hist))
	}
	if hist[0].Content[0].Text == nil || *hist[0].Content[0].Text != "q1" {
		t.Fatalf("history[0] = %+v, want the pre-restart question", hist[0])
	}
}

func TestChatTurnClientSessionEventMapping(t *testing.T) {
	prov := &chatTestProvider{events: []Event{
		ReasoningDelta{Text: "thinking..."},
		TextDelta{Text: "answer"},
		ToolCall{Index: 0, ToolCallID: "t1", Name: "bash", ArgsJSON: "{}"},
		StreamError{Err: errors.New("mid-stream boom")},
	}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-5", "ask-orchicon:conv-5")

	bus, _ := b.Subscribe(ctx, "conv-5")
	if err := b.SendTurnMessage(ctx, "conv-5", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainBus(t, bus)

	var sawReasoning, sawText, sawTool, sawError bool
	for _, e := range evts {
		switch e.Kind {
		case "delta":
			if e.IsReasoning {
				sawReasoning = true
			} else {
				sawText = true
			}
		case "tool_part":
			sawTool = true
		case "error":
			sawError = true
		}
	}
	if !sawReasoning {
		t.Fatal("no reasoning delta mapped")
	}
	if !sawText {
		t.Fatal("no text delta mapped")
	}
	if !sawTool {
		t.Fatal("no tool_part mapped")
	}
	if !sawError {
		t.Fatal("no error event mapped from StreamError")
	}
}

func TestChatTurnClientMissingTenant(t *testing.T) {
	b := newChatBridge(t, &chatTestProvider{})
	sid, _ := b.CreateConversationSession(context.Background(), "conv-6", "ask-orchicon:conv-6")
	// No tenant in context → actionable error, never a panic.
	err := b.SendTurnMessage(context.Background(), "conv-6", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi")
	if err == nil {
		t.Fatal("SendTurnMessage without tenant succeeded; want actionable error")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("error %q does not name the missing tenant", err)
	}
}

// fakeAskTools is a scripted AskToolProvider for the tool-loop tests.
type fakeAskTools struct {
	mu      sync.Mutex
	defs    []ToolDef
	results map[string]string
	errs    map[string]error
	calls   []string
}

func (f *fakeAskTools) AskToolDefs(context.Context) []ToolDef { return f.defs }

func (f *fakeAskTools) ExecuteAskTool(_ context.Context, name, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if err, ok := f.errs[name]; ok {
		return "", err
	}
	return f.results[name], nil
}

// TestChatTurnClientToolRoundTrip pins the agentic loop: round 1 streams
// text + a tool call, the call executes against the injected tools, round
// 2 streams the final answer. The bus must carry a tool_part plus both
// text parts, and the committed history must replay the full tool flow.
func TestChatTurnClientToolRoundTrip(t *testing.T) {
	prov := &chatTestProvider{rounds: [][]Event{
		{
			TextDelta{Text: "Checking "},
			ToolCall{Index: 0, ToolCallID: "call_1", Name: "list_projects", ArgsJSON: `{}`},
			Finish{StopReason: StopToolUse},
		},
		{
			TextDelta{Text: "Done: 3 projects."},
			Finish{StopReason: StopStop},
		},
	}}
	b := newChatBridge(t, prov)
	b.SetAskTools(&fakeAskTools{
		defs:    []ToolDef{{Name: "list_projects", Description: "List projects", ParamsJSON: `{"type":"object"}`}},
		results: map[string]string{"list_projects": `["a","b","c"]`},
	})
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-tools", "ask-orchicon:conv-tools")

	// The tool defs must ride the provider request.
	bus, _ := b.Subscribe(ctx, "conv-tools")
	if err := b.SendTurnMessage(ctx, "conv-tools", sid, "system", "orchicon/ollama/deepseek-v4-flash", "list my projects"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainBus(t, bus)

	var sawToolPart bool
	var partTexts []string
	var sawIdle bool
	for _, e := range evts {
		switch e.Kind {
		case "tool_part":
			sawToolPart = true
		case "part":
			if e.Type == "text" {
				partTexts = append(partTexts, e.Text)
			}
		case "idle":
			sawIdle = true
		}
	}
	if !sawToolPart {
		t.Fatal("no tool_part on the bus for the tool round")
	}
	if !sawIdle {
		t.Fatal("no idle at turn end")
	}
	// The reply is ONE consolidated part (both rounds' text) — never a
	// separate part per round, which would render as overlapping bubbles
	// and duplicate a repeated preamble.
	if len(partTexts) != 1 {
		t.Fatalf("parts = %q, want exactly ONE consolidated text part", partTexts)
	}
	joined := partTexts[0]
	if !strings.Contains(joined, "Checking") || !strings.Contains(joined, "Done: 3 projects.") {
		t.Fatalf("part text = %q, want both rounds' text", joined)
	}
	if prov.requestCount() != 2 {
		t.Fatalf("provider turns = %d, want 2 (tool round + final)", prov.requestCount())
	}
	// The committed history replays the full flow: user, assistant
	// (text + tool use), tool result, assistant (final text).
	b.mu.Lock()
	hist := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if len(hist) != 4 {
		t.Fatalf("history has %d messages, want 4 (user, assistant+tooluse, toolresult, assistant)", len(hist))
	}
	if hist[1].Role != RoleAssistant {
		t.Fatalf("history[1].Role = %q, want assistant", hist[1].Role)
	}
	hasToolUse := false
	for _, c := range hist[1].Content {
		if c.ToolUse != nil && c.ToolUse.Name == "list_projects" {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Fatalf("history[1] = %+v, want the tool use", hist[1])
	}
	if hist[2].Role != RoleTool {
		t.Fatalf("history[2].Role = %q, want tool", hist[2].Role)
	}
}

// TestChatTurnClientToolFailureIsAResult pins the recovery contract: a
// failing tool call is recorded as an error tool result and the turn
// continues (the model sees it) — never a turn failure.
func TestChatTurnClientToolFailureIsAResult(t *testing.T) {
	prov := &chatTestProvider{rounds: [][]Event{
		{
			ToolCall{Index: 0, ToolCallID: "call_9", Name: "boom", ArgsJSON: `{}`},
			Finish{StopReason: StopToolUse},
		},
		{
			TextDelta{Text: "It failed, sorry."},
			Finish{StopReason: StopStop},
		},
	}}
	b := newChatBridge(t, prov)
	b.SetAskTools(&fakeAskTools{
		defs: []ToolDef{{Name: "boom", ParamsJSON: `{"type":"object"}`}},
		errs: map[string]error{"boom": errors.New("kaboom")},
	})
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-toolerr", "ask-orchicon:conv-toolerr")

	bus, _ := b.Subscribe(ctx, "conv-toolerr")
	if err := b.SendTurnMessage(ctx, "conv-toolerr", sid, "system", "orchicon/ollama/deepseek-v4-flash", "go"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainBus(t, bus)
	var sawIdle, sawError bool
	for _, e := range evts {
		if e.Kind == "idle" {
			sawIdle = true
		}
		if e.Kind == "error" {
			sawError = true
		}
	}
	if !sawIdle || sawError {
		t.Fatalf("idle=%v error=%v, want a completed turn with no error event", sawIdle, sawError)
	}
	b.mu.Lock()
	hist := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if len(hist) != 4 || hist[2].Role != RoleTool {
		t.Fatalf("history = %+v, want user/assistant/tool/assistant", hist)
	}
	tr := hist[2].Content[0].ToolResult
	if tr == nil || !tr.IsError || !strings.Contains(tr.Content, "kaboom") {
		t.Fatalf("tool result = %+v, want the error recorded", hist[2])
	}
}

// TestChatTurnClientDuplicateCallIDsExecuteOnce pins the wire contract:
// two ToolCall events sharing one call_id in a round (provider/decoder
// echo) execute once and record one result — a second
// function_call_output for the id makes the wire reject the turn.
func TestChatTurnClientDuplicateCallIDsExecuteOnce(t *testing.T) {
	prov := &chatTestProvider{rounds: [][]Event{
		{
			ToolCall{Index: 0, ToolCallID: "call_dup", Name: "fn", ArgsJSON: `{}`},
			ToolCall{Index: 1, ToolCallID: "call_dup", Name: "fn", ArgsJSON: `{}`},
			Finish{StopReason: StopToolUse},
		},
		{
			TextDelta{Text: "done"},
			Finish{StopReason: StopStop},
		},
	}}
	tools := &fakeAskTools{
		defs:    []ToolDef{{Name: "fn", ParamsJSON: `{"type":"object"}`}},
		results: map[string]string{"fn": "ok"},
	}
	b := newChatBridge(t, prov)
	b.SetAskTools(tools)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-dupe", "ask-orchicon:conv-dupe")

	bus, _ := b.Subscribe(ctx, "conv-dupe")
	if err := b.SendTurnMessage(ctx, "conv-dupe", sid, "system", "orchicon/ollama/deepseek-v4-flash", "go"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	drainBus(t, bus)
	if len(tools.calls) != 1 {
		t.Fatalf("tool executions = %d, want 1", len(tools.calls))
	}
	b.mu.Lock()
	hist := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	outs := 0
	for _, m := range hist {
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID == "call_dup" {
				outs++
			}
		}
	}
	if outs != 1 {
		t.Fatalf("results for call_dup = %d, want exactly one", outs)
	}
}

// TestChatTurnClientManyToolRoundsUnbounded pins the removal of the
// 8-round cap: a turn whose fake provider issues >8 tool rounds then
// finishes must complete normally (no injected budget notice, no tools
// stripped) and commit the FULL working history — every round's tool use
// and tool result — so a follow-up re-sends complete context.
func TestChatTurnClientManyToolRoundsUnbounded(t *testing.T) {
	const nRounds = 12 // > the former 8-round cap
	rounds := make([][]Event, 0, nRounds+1)
	for i := 0; i < nRounds; i++ {
		rounds = append(rounds, []Event{
			TextDelta{Text: "step "},
			ToolCall{Index: 0, ToolCallID: "call_r" + string(rune('a'+i)), Name: "probe", ArgsJSON: `{}`},
			Finish{StopReason: StopToolUse},
		})
	}
	rounds = append(rounds, []Event{
		TextDelta{Text: "all done"},
		Finish{StopReason: StopStop},
	})
	prov := &chatTestProvider{rounds: rounds}
	b := newChatBridge(t, prov)
	b.SetAskTools(&fakeAskTools{
		defs:    []ToolDef{{Name: "probe", ParamsJSON: `{"type":"object"}`}},
		results: map[string]string{"probe": "ok"},
	})
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-many", "ask-orchicon:conv-many")

	bus, _ := b.Subscribe(ctx, "conv-many")
	if err := b.SendTurnMessage(ctx, "conv-many", sid, "system", "orchicon/ollama/deepseek-v4-flash", "go"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainBus(t, bus)

	// The turn completes normally: one consolidated text part, one idle,
	// and NO budget-exhaustion notice anywhere.
	var partText string
	var sawIdle bool
	for _, e := range evts {
		switch e.Kind {
		case "part":
			if e.Type == "text" {
				partText = e.Text
			}
		case "idle":
			sawIdle = true
		}
	}
	if !sawIdle {
		t.Fatal("no idle at turn end")
	}
	if strings.Contains(partText, "budget exhausted") {
		t.Fatalf("reply %q contains the removed budget notice", partText)
	}
	if !strings.Contains(partText, "all done") {
		t.Fatalf("reply %q missing the final answer", partText)
	}
	// The loop ran all rounds: nRounds tool rounds + 1 final text round.
	if prov.requestCount() != nRounds+1 {
		t.Fatalf("provider turns = %d, want %d (all tool rounds + final)", prov.requestCount(), nRounds+1)
	}
	// The committed history replays the full flow: user, then per round an
	// assistant (text + tool use) and a tool result, then the final
	// assistant text.
	b.mu.Lock()
	hist := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	want := 1 + 2*nRounds + 1 // user + (assistant+toolresult)*nRounds + final assistant
	if len(hist) != want {
		t.Fatalf("history has %d messages, want %d (full N-round replay)", len(hist), want)
	}
	// Every round's tool use and tool result must be present.
	for i := 0; i < nRounds; i++ {
		assistant := hist[1+2*i]
		if assistant.Role != RoleAssistant {
			t.Fatalf("history[%d].Role = %q, want assistant", 1+2*i, assistant.Role)
		}
		hasUse := false
		for _, c := range assistant.Content {
			if c.ToolUse != nil && c.ToolUse.Name == "probe" {
				hasUse = true
			}
		}
		if !hasUse {
			t.Fatalf("history[%d] = %+v, want the tool use", 1+2*i, assistant)
		}
		result := hist[2+2*i]
		if result.Role != RoleTool || result.Content[0].ToolResult == nil {
			t.Fatalf("history[%d] = %+v, want the tool result", 2+2*i, result)
		}
	}
}

// TestChatTurnClientNoWorkerBudgetImport guards the budgets-are-workers-only
// invariant by construction: the native Ask turn path must never import the
// worker-budget facade package (internal/opencode). It parses chatturn.go
// with go/parser and fails if internal/opencode appears in its imports.
func TestChatTurnClientNoWorkerBudgetImport(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "chatturn.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse chatturn.go: %v", err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "github.com/beardedparrott/orchicon/internal/opencode" {
			t.Fatalf("chatturn.go imports %q — the worker-budget facade must gate workers only, never native Ask turns", path)
		}
	}
}

// TestChatTurnClientEmitsReasoningAsACompletedPart is the adapter's half of the
// reasoning-durability fix, and the operator's own diagnosis as a test: "You had found
// an issue where reasoning was not reporting back like other message types."
//
// THE COLLECTOR PERSISTS PARTS, NOT DELTAS. Its durable reasoning slice is appended only by
// a completed reasoning part, and its live reasoning tail is RESET by every completed text
// part — so a reasoning channel that is only ever streamed as deltas has NO durable form at
// all. It existed solely in the live mirror, which is why a turn that died mid-thought left
// nothing behind: "anything you were currently typing (mostly in thought) goes away".
//
// This asserts the symmetry directly: the same accumulated reasoning the turn streamed as
// deltas is also published as a COMPLETED part, and it arrives BEFORE the text part because
// thinking precedes the answer and the collector appends both to ordered slices.
func TestChatTurnClientEmitsReasoningAsACompletedPart(t *testing.T) {
	prov := &chatTestProvider{events: []Event{
		ReasoningDelta{Text: "weighing "},
		ReasoningDelta{Text: "the options"},
		TextDelta{Text: "Here is the answer."},
		Finish{StopReason: StopStop},
	}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-r1", "ask-orchicon:conv-r1")

	bus, _ := b.Subscribe(ctx, "conv-r1")
	if err := b.SendTurnMessage(ctx, "conv-r1", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainBus(t, bus)

	var reasoningPart, textPart string
	var reasoningAt, textAt int
	for i, e := range evts {
		if e.Kind != "part" {
			continue
		}
		switch e.Type {
		case "reasoning":
			reasoningPart, reasoningAt = e.Text, i
		case "text":
			textPart, textAt = e.Text, i
		}
	}
	if reasoningPart == "" {
		t.Fatalf("no completed reasoning part was emitted — reasoning has no durable form "+
			"(events: %s)", describeEvents(evts))
	}
	if reasoningPart != "weighing the options" {
		t.Errorf("reasoning part = %q, want the whole streamed reasoning", reasoningPart)
	}
	// Reasoning before text, because the collector appends both to ordered slices.
	if textPart == "" {
		t.Fatalf("no completed text part was emitted (events: %s)", describeEvents(evts))
	}
	if reasoningAt > textAt {
		t.Errorf("the reasoning part arrived AFTER the text part (reasoning@%d, text@%d) — the "+
			"transcript would read as if the answer preceded the thinking", reasoningAt, textAt)
	}
}

// TestChatTurnClientAbortStillPublishesTheWork is the other half of the data-loss bug.
//
// The collector's STALL path aborts the serve session while the collector is still live, and
// this path used to `return` bare on abort — publishing nothing. So a stalled turn finalized
// with empty text and empty reasoning while the operator had watched both stream, and the
// work was gone. An aborted turn must still hand over what it produced; it just must not
// claim to have COMPLETED, which is what the idle event means.
func TestChatTurnClientAbortStillPublishesTheWork(t *testing.T) {
	// A stream that yields its events and then blocks until cancelled — the shape of a real
	// turn interrupted mid-generation.
	prov := &chatTestProvider{stream: &blockingAfterStream{events: []Event{
		ReasoningDelta{Text: "half a thought"},
		TextDelta{Text: "partial answer"},
	}}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-r2", "ask-orchicon:conv-r2")

	bus, _ := b.Subscribe(ctx, "conv-r2")
	if err := b.SendTurnMessage(ctx, "conv-r2", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	// Let the deltas land, then abort mid-turn.
	time.Sleep(150 * time.Millisecond)
	if err := b.AbortConversationSession(ctx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}
	evts := drainBus(t, bus)

	var reasoningPart, textPart string
	for _, e := range evts {
		if e.Kind == "idle" {
			t.Fatal("an aborted turn must not emit idle — it did not complete")
		}
		if e.Kind != "part" {
			continue
		}
		switch e.Type {
		case "reasoning":
			reasoningPart = e.Text
		case "text":
			textPart = e.Text
		}
	}
	if textPart == "" {
		t.Errorf("an aborted turn published no text part — the answer the operator watched "+
			"stream was discarded (events: %s)", describeEvents(evts))
	}
	if reasoningPart == "" {
		t.Errorf("an aborted turn published no reasoning part — the thinking the operator "+
			"watched stream was discarded (events: %s)", describeEvents(evts))
	}
}

// blockingAfterStream yields a scripted sequence, then blocks until the context is
// cancelled — a turn cut off mid-generation rather than one that ended.
type blockingAfterStream struct {
	events []Event
	i      int
}

func (s *blockingAfterStream) Next(ctx context.Context) (Event, bool, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	<-ctx.Done()
	return nil, false, ctx.Err()
}
func (s *blockingAfterStream) Close() error { return nil }

// describeEvents renders the event stream for a failure message, so a broken assertion
// shows what actually arrived instead of only what was missing.
func describeEvents(evts []scheduler.SessionEvent) string {
	parts := make([]string, 0, len(evts))
	for _, e := range evts {
		parts = append(parts, e.Kind+"/"+e.Type)
	}
	return strings.Join(parts, ", ")
}

// TestChatTurnClientAbortKeepsThePublishedReplyInHistory is the SECOND half of the publish
// fix above, and it is the bug the operator kept reporting as the model "losing its brain":
// "It doesn't know it's already done things and then tries to do them again."
//
// The publish made an aborted turn's work VISIBLE (durable, on every client's screen). But the
// abort path returned without committing, so the SESSION never learned about it — and the next
// turn re-sends b.chatHistory as full context. The model was therefore handed a history in which
// its own last reply had never happened, while the operator was reading that very reply in the
// transcript. It repeats itself, re-asks what it just asked, and re-does work it already did.
//
// Abort is not an edge case here: the collector aborts on a stall, on Stop, and on EVERY
// supersede — and interjecting is how the operator steers a running turn.
func TestChatTurnClientAbortKeepsThePublishedReplyInHistory(t *testing.T) {
	const partial = "the partial answer the operator watched land"
	prov := &chatTestProvider{stream: &blockingAfterStream{events: []Event{
		TextDelta{Text: partial},
	}}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-abort-hist", "ask-orchicon:conv-abort-hist")

	bus, _ := b.Subscribe(ctx, "conv-abort-hist")
	if err := b.SendTurnMessage(ctx, "conv-abort-hist", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "first"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := b.AbortConversationSession(ctx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}
	drainBus(t, bus)

	// The operator's transcript now holds `partial`. The next turn must re-send it, or the model
	// answers from a history that contradicts what the operator can read.
	if err := b.SendTurnMessage(ctx, "conv-abort-hist", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "second"); err != nil {
		t.Fatalf("SendTurnMessage (2): %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	replayed := requestText(prov.lastRequest())
	if !strings.Contains(replayed, partial) {
		t.Fatalf("the next turn replayed a history WITHOUT the aborted turn's own reply — the model "+
			"re-answers work it already did, while the operator reads that reply on screen.\nhistory replayed: %q", replayed)
	}
	if !strings.Contains(replayed, "second") {
		t.Fatalf("the next turn must still carry the new user message; history replayed: %q", replayed)
	}
}

// requestText flattens a turn request's replayed history into one string for assertions.
func requestText(req TurnRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Text != nil {
				b.WriteString(*c.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// --- the concurrent-commit data loss ---------------------------------------------------------

// textMsg builds a one-part message for the interleaving tests.
func textMsg(role Role, s string) Message {
	t := s
	return Message{Role: role, Content: []Content{{Text: &t}}}
}

// historyText flattens a session history into one string.
func historyText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Text != nil {
				b.WriteString(*c.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// TestConcurrentTurnCommitsDoNotEraseEachOther is the reported "Orchicon is not usable if you can't have a
// session that remembers what it's doing", reduced to its mechanism.
//
// commitChatHistory used to REPLACE the session history with the committing turn's own snapshot-plus-output.
// Two turns overlap whenever the operator interjects (which supersedes the running turn), so the last one to
// commit erased the other's messages. The signature in the operator's own conversation was five consecutive
// USER messages with no reply between them: a user message rides the NEXT turn's snapshot so it survives,
// while a reply exists only in the turn that produced it, so it does not.
//
// This drives the two commits in the order the production log shows them (the superseded turn commits first,
// the superseding turn second) and requires every message from both to survive.
func TestConcurrentTurnCommitsDoNotEraseEachOther(t *testing.T) {
	b := newChatBridge(t, &chatTestProvider{})
	const sid = "orchicon-ask:conv-interleave"
	prior := []Message{textMsg(RoleUser, "hello")}

	// Turn A dispatches: the map becomes prior + [userA], and A holds that snapshot.
	historyA := append(append([]Message(nil), prior...), textMsg(RoleUser, "question A"))
	b.chatHistory[sid] = append([]Message(nil), historyA...)

	// THE OPERATOR INTERJECTS. Turn B dispatches, snapshotting what the map holds NOW — which already
	// carries [userA] but not replyA (A has not produced it yet).
	historyB := append(append([]Message(nil), historyA...), textMsg(RoleUser, "question B"))
	b.chatHistory[sid] = append([]Message(nil), historyB...)

	// A is cancelled (supersede) and commits what it managed to produce.
	workingA := append(append([]Message(nil), historyA...), textMsg(RoleAssistant, "reply A"))
	b.commitChatHistory(sid, historyA, workingA)

	// B completes and commits.
	workingB := append(append([]Message(nil), historyB...), textMsg(RoleAssistant, "reply B"))
	b.commitChatHistory(sid, historyB, workingB)

	got := historyText(b.chatHistory[sid])
	for _, want := range []string{"hello", "question A", "reply A", "question B", "reply B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the session history lost %q — a concurrent turn's commit erased it, so the model "+
				"cannot see work that is on the operator's screen.\nsession history:\n%s", want, got)
		}
	}
	// And nothing was duplicated by the append.
	if n := strings.Count(got, "question A"); n != 1 {
		t.Fatalf("question A appears %d times, want exactly 1:\n%s", n, got)
	}
	if n := strings.Count(got, "hello"); n != 1 {
		t.Fatalf("the prior history appears %d times, want exactly 1:\n%s", n, got)
	}
}

// The same guarantee when the order is reversed: whichever turn commits LAST must not erase the other. This
// is the half a "make the abort path commit too" fix cannot give, because both orders are reachable —
// commit order depends on how fast each provider stream drains, not on which turn started first.
func TestConcurrentCommitOrderDoesNotMatter(t *testing.T) {
	b := newChatBridge(t, &chatTestProvider{})
	const sid = "orchicon-ask:conv-interleave-2"
	prior := []Message{textMsg(RoleUser, "hello")}

	historyA := append(append([]Message(nil), prior...), textMsg(RoleUser, "question A"))
	b.chatHistory[sid] = append([]Message(nil), historyA...)
	historyB := append(append([]Message(nil), historyA...), textMsg(RoleUser, "question B"))
	b.chatHistory[sid] = append([]Message(nil), historyB...)

	workingA := append(append([]Message(nil), historyA...), textMsg(RoleAssistant, "reply A"))
	workingB := append(append([]Message(nil), historyB...), textMsg(RoleAssistant, "reply B"))
	// B FIRST this time.
	b.commitChatHistory(sid, historyB, workingB)
	b.commitChatHistory(sid, historyA, workingA)

	got := historyText(b.chatHistory[sid])
	for _, want := range []string{"question A", "reply A", "question B", "reply B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("commit order decided which turn survived; %q was lost:\n%s", want, got)
		}
	}
}

// A single ordinary turn still accumulates exactly as before: the prior history plus this turn's own
// messages, with nothing duplicated and nothing dropped.
func TestCommitAppendsTheTurnsOwnMessagesToTheSession(t *testing.T) {
	b := newChatBridge(t, &chatTestProvider{})
	const sid = "orchicon-ask:conv-normal"
	prior := []Message{textMsg(RoleUser, "first"), textMsg(RoleAssistant, "first reply")}
	b.chatHistory[sid] = append([]Message(nil), prior...)

	history := append(append([]Message(nil), prior...), textMsg(RoleUser, "second"))
	// THE DISPATCH WRITES THE USER MESSAGE INTO THE SESSION before the turn runs (SendTurnMessage), so the
	// map already holds `history` by the time the turn commits — mirrored here, or the commit appends a tail
	// onto a map that never had this turn's own user message.
	b.chatHistory[sid] = append([]Message(nil), history...)
	// A WELL-FORMED tool round: the call and its result. A result with no matching call is the ORPHANED
	// shape sanitizeChatHistory deliberately drops (it is the mirror of a dangling call, and providers
	// reject it), so a fixture that paired them wrongly would fail for the wrong reason.
	working := append(append([]Message(nil), history...),
		Message{Role: RoleAssistant, Content: []Content{
			{Text: strptr("second reply")},
			{ToolUse: &ContentToolUse{ToolCallID: "c1", Name: "bash", ArgsJSON: "{}"}},
		}},
		Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "c1", Content: "ok"}}}},
	)
	b.commitChatHistory(sid, history, working)

	got := historyText(b.chatHistory[sid])
	for _, want := range []string{"first", "first reply", "second", "second reply"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing after a normal commit:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "first reply"); n != 1 {
		t.Fatalf("the prior history was duplicated (%d copies):\n%s", n, got)
	}
	// prior + the turn's user message + its assistant round + its tool result.
	if len(b.chatHistory[sid]) != len(prior)+3 {
		t.Fatalf("history length = %d, want %d", len(b.chatHistory[sid]), len(prior)+3)
	}
}

// --- the shrink guard: silent session loss must be impossible ---------------------------------

// logBridge builds a bridge whose logs the test can read (newChatBridge discards them, and the guard's
// whole job is what it SAYS).
func logBridge(t *testing.T) (*NativeBridge, *strings.Builder) {
	t.Helper()
	var sb strings.Builder
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return &chatTestProvider{}, nil
	}), "", slog.New(slog.NewTextHandler(&sb, nil)))
	return b, &sb
}

// TestTheGuardCatchesTheEqualLengthSwap is the regression detector for the bug that ran unseen for weeks.
//
// IT IS THE PRODUCTION SHAPE, AND THE LENGTH IS THE POINT. The replacing commit did not truncate the session,
// it SWAPPED a message: with the session holding H+[userA]+[userB], it wrote H+[userA]+replyA — the SAME
// message count, with the operator's own question B replaced by a reply. A guard built on length, or on
// per-role counts, sees nothing at all here; that is how this survived so long, and the first version of this
// guard was written that way and would NOT have caught the bug. The invariant a session actually needs is that
// no message DISAPPEARS, so this test asserts on that.
func TestTheGuardCatchesTheEqualLengthSwap(t *testing.T) {
	b, logs := logBridge(t)
	const sid = "orchicon-ask:conv-guard"
	prior := []Message{textMsg(RoleUser, "hello")}

	// A's turn dispatched.
	historyA := append(append([]Message(nil), prior...), textMsg(RoleUser, "question A"))
	b.chatHistory[sid] = append([]Message(nil), historyA...)
	b.persistAskHistoryLocked(sid)

	// The operator interjects: B's snapshot carries A's user message AND B's own.
	historyB := append(append([]Message(nil), historyA...), textMsg(RoleUser, "question B"))
	b.chatHistory[sid] = append([]Message(nil), historyB...)
	b.persistAskHistoryLocked(sid)
	beforeLen := len(b.chatHistory[sid])

	// WHAT THE OLD COMMIT WROTE: `cur = working`, i.e. turn A's own snapshot-plus-output. Same length,
	// different content — question B is gone, reply A is in its place.
	b.chatHistory[sid] = append(append([]Message(nil), historyA...), textMsg(RoleAssistant, "reply A"))
	b.persistAskHistoryLocked(sid)

	if len(b.chatHistory[sid]) != beforeLen {
		t.Fatalf("fixture no longer reproduces the swap: length went %d -> %d (it must stay EQUAL, or this "+
			"test would pass against a length-based guard that cannot catch the real bug)",
			beforeLen, len(b.chatHistory[sid]))
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "LOST MESSAGES WITHOUT AN INTENTIONAL REDUCTION") {
		t.Fatalf("a session that swapped a message away was not reported at error level; logs:\n%s", out)
	}
	if !strings.Contains(out, sid) {
		t.Errorf("the report must name the session; logs:\n%s", out)
	}
}

// The other half: a session that only GROWS is never reported. Without this the guard could pass its own
// regression test by shouting at everything, which would make it useless in the log.
func TestTheGuardIsSilentOnNormalGrowth(t *testing.T) {
	b, logs := logBridge(t)
	const sid = "orchicon-ask:conv-guard-grow"
	prior := []Message{textMsg(RoleUser, "hello")}
	b.chatHistory[sid] = append([]Message(nil), prior...)
	b.persistAskHistoryLocked(sid)

	for i, q := range []string{"second", "third", "fourth"} {
		history := append(append([]Message(nil), b.chatHistory[sid]...), textMsg(RoleUser, q))
		b.chatHistory[sid] = append([]Message(nil), history...)
		working := append(append([]Message(nil), history...), textMsg(RoleAssistant, "reply "+q))
		b.commitChatHistory(sid, history, working)
		if strings.Contains(logs.String(), "LOST MESSAGES") {
			t.Fatalf("growth was reported as a shrink on iteration %d; logs:\n%s", i, logs.String())
		}
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("an ordinary growing session logged an error; logs:\n%s", logs.String())
	}
}

// An INTENTIONAL reduction (compaction / context reduction) is reported as intended, not as data loss — the
// distinction is the whole point of the mark, and without it the guard would cry wolf on the one lossy path
// that is by design and already announced to the operator.
func TestAnIntentionalReductionIsReportedAsIntendedNotAsLoss(t *testing.T) {
	b, logs := logBridge(t)
	const sid = "orchicon-ask:conv-guard-compact"
	big := make([]Message, 40)
	for i := range big {
		big[i] = textMsg(RoleUser, "message")
	}
	b.chatHistory[sid] = big
	b.persistAskHistoryLocked(sid)

	// What reduceSessionHistory / CompactConversationSession do: shrink, declare it, then persist.
	b.chatHistory[sid] = big[:5]
	b.markHistoryReductionLocked(sid, "conversation compaction")
	b.persistAskHistoryLocked(sid)

	out := logs.String()
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("an intentional reduction was reported as data loss; logs:\n%s", out)
	}
	// slog QUOTES a value containing spaces (reason="conversation compaction"), so the words are asserted
	// separately from the key rather than as one literal.
	if !strings.Contains(out, "reduced as intended") || !strings.Contains(out, "reason=") ||
		!strings.Contains(out, "conversation compaction") {
		t.Fatalf("an intentional reduction must be reported WITH its reason; logs:\n%s", out)
	}
}

// The mark is consumed exactly once: the persistence after a compaction is expected to shrink, but if the
// history shrinks AGAIN later without a new mark, that is real loss and must not be excused by a stale mark.
func TestAStaleReductionMarkDoesNotExcuseLaterLoss(t *testing.T) {
	b, logs := logBridge(t)
	const sid = "orchicon-ask:conv-guard-stale"
	b.chatHistory[sid] = make([]Message, 20)
	b.persistAskHistoryLocked(sid)

	b.chatHistory[sid] = make([]Message, 10)
	b.markHistoryReductionLocked(sid, "conversation compaction")
	b.persistAskHistoryLocked(sid) // intended — consumes the mark

	logs.Reset()
	b.chatHistory[sid] = make([]Message, 4) // nothing declared this one
	b.persistAskHistoryLocked(sid)

	out := logs.String()
	if !strings.Contains(out, "LOST MESSAGES WITHOUT AN INTENTIONAL REDUCTION") {
		t.Fatalf("a LATER undeclared shrink was excused by a consumed mark; logs:\n%s", out)
	}
}

// A legitimate REPLAY REPAIR must not be reported as data loss. sanitizeChatHistory deliberately drops an
// orphaned tool result (a result whose call no preceding assistant message declares) because providers reject
// that shape — so a session that loses one of those has not lost anything the model could have used. Without
// the distinction a guard cries wolf on its own repair path, and a guard nobody trusts is not a guard.
func TestAReplayRepairIsNotReportedAsDataLoss(t *testing.T) {
	b, logs := logBridge(t)
	const sid = "orchicon-ask:conv-guard-repair"

	// A well-formed exchange, then an ORPHANED result appended the way a half-finished tool round leaves one.
	b.chatHistory[sid] = []Message{
		textMsg(RoleUser, "run it"),
		{Role: RoleAssistant, Content: []Content{
			{Text: strptr("running")},
			{ToolUse: &ContentToolUse{ToolCallID: "c1", Name: "bash", ArgsJSON: "{}"}},
		}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "c1", Content: "ok"}}}},
	}
	b.persistAskHistoryLocked(sid)

	// The turn PRODUCES an orphaned result — a result whose call nothing declared, the shape a half-finished
	// tool round leaves. It must therefore ride the commit's TAIL (what the turn contributed past its
	// snapshot), because that is the only part of `working` the commit folds into the session.
	history := append([]Message(nil), b.chatHistory[sid]...)
	orphan := Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "ghost", Content: "orphan"}}}}
	working := append(append(append([]Message(nil), history...), orphan), textMsg(RoleAssistant, "done"))
	b.commitChatHistory(sid, history, working)

	out := logs.String()
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("a legitimate replay repair was reported as data loss; logs:\n%s", out)
	}
	if !strings.Contains(out, "reduced as intended") || !strings.Contains(out, "replay repair removed") {
		t.Fatalf("the repair must still be REPORTED, as an intended reduction with its reason; logs:\n%s", out)
	}
	// And the orphan really was dropped — otherwise the test proves nothing about the repair path.
	for _, m := range b.chatHistory[sid] {
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID == "ghost" {
				t.Fatalf("the orphaned result survived, so this test never exercised the repair; history: %+v", b.chatHistory[sid])
			}
		}
	}
}

// --- the mid-loop cancellation that discarded a whole tool round ------------------------------

// cancelOnSecondRoundProvider serves a scripted FIRST round and then fails every later one with
// context.Canceled — the shape a supersede produces when it lands between tool rounds.
type cancelOnSecondRoundProvider struct {
	first []Event
	calls int
}

func (p *cancelOnSecondRoundProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	p.calls++
	if p.calls > 1 {
		return nil, context.Canceled
	}
	return &chatTestStream{events: p.first}, nil
}
func (p *cancelOnSecondRoundProvider) ListModels(context.Context) ([]ModelInfo, error) {
	return nil, nil
}
func (p *cancelOnSecondRoundProvider) Capabilities() Capabilities {
	return Capabilities{Streaming: true}
}

// TestATurnCancelledBetweenToolRoundsKeepsItsWork is the SECOND data-loss mechanism, found live on the
// operator's own conversation after the first was fixed.
//
// An interjection cancels the running turn mid-loop, and that cancellation surfaces at the NEXT round's
// StreamTurn as context.Canceled — NOT through drainOneRound's aborted flag. That path returned bare, so a
// turn that had produced text and RUN TOOLS lost the whole round: the session ended up holding two
// consecutive user messages with no trace of the turn between them, while the work sat in the transcript and
// on the operator's screen.
//
// The loss guard cannot see this class at all — it compares the session against what it last held, so it only
// catches a message that DISAPPEARS, and this one never arrived. Which is why the fingerprint (two
// consecutive user messages), not the guard, is what caught it.
func TestATurnCancelledBetweenToolRoundsKeepsItsWork(t *testing.T) {
	const partial = "I will start by reading the code"
	prov := &cancelOnSecondRoundProvider{first: []Event{
		TextDelta{Text: partial},
		ToolCall{Index: 0, ToolCallID: "tc-1", Name: "bash", ArgsJSON: `{"command":"ls"}`},
		Finish{},
	}}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-cancel-round", "ask-orchicon:conv-cancel-round")

	bus, _ := b.Subscribe(ctx, "conv-cancel-round")
	if err := b.SendTurnMessage(ctx, "conv-cancel-round", sid, "system", "orchicon/ollama-cloud/deepseek-v4-flash:0731", "do the thing"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	drainBus(t, bus)

	var got strings.Builder
	for _, m := range b.chatHistory[sid] {
		for _, c := range m.Content {
			if c.Text != nil {
				got.WriteString(*c.Text)
				got.WriteString("\n")
			}
		}
	}
	history := got.String()
	if !strings.Contains(history, partial) {
		t.Fatalf("the cancellation between tool rounds DISCARDED the round's text: the session holds no "+
			"trace of a turn whose work is on the operator's screen.\nsession history:\n%s", history)
	}
	// The tool call and its result are what `working` had accumulated; the model must be able to see that it
	// already ran that command, or it runs it again.
	if !strings.Contains(history, "tc-1") && !historyHasToolUse(b.chatHistory[sid], "tc-1") {
		t.Fatalf("the round's tool call was discarded with the text:\n%s", history)
	}
	if !strings.Contains(history, "do the thing") {
		t.Fatalf("the operator's own message must survive:\n%s", history)
	}
}

// historyHasToolUse reports whether a session history carries a tool-call id.
func historyHasToolUse(msgs []Message, id string) bool {
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == id {
				return true
			}
			if c.ToolResult != nil && c.ToolResult.ToolCallID == id {
				return true
			}
		}
	}
	return false
}
