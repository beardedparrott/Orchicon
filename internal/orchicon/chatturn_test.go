package orchicon

import (
	"context"
	"errors"
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

func TestChatTurnClientPreStreamFailure(t *testing.T) {	prov := &chatTestProvider{preErr: errors.New("auth failed")}
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

func TestChatTurnClientReplyPermissionErrors(t *testing.T) {
	b := newChatBridge(t, &chatTestProvider{})
	err := b.ReplyPermission(context.Background(), "s", "p")
	if err == nil {
		t.Fatal("ReplyPermission succeeded; want actionable text-only error")
	}
	if !strings.Contains(err.Error(), "text-only") {
		t.Fatalf("ReplyPermission error %q does not explain the text-only limitation", err)
	}
}

func TestChatTurnClientNoAttachmentsCapability(t *testing.T) {
	// The native bridge must NOT implement SendTurnMessageWithAttachments so
	// the askorchicon collector fails loudly on attachment turns (never
	// silently degrading to text-only).
	var _ scheduler.ChatTurnClient = (*NativeBridge)(nil)
	if _, ok := any((*NativeBridge)(nil)).(scheduler.SendTurnMessageWithAttachments); ok {
		t.Fatal("NativeBridge unexpectedly implements SendTurnMessageWithAttachments")
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

func (f *fakeAskTools) AskToolDefs() []ToolDef { return f.defs }

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
	joined := strings.Join(partTexts, "\n")
	if !strings.Contains(joined, "Checking") || !strings.Contains(joined, "Done: 3 projects.") {
		t.Fatalf("part texts = %q, want both rounds' text", joined)
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
