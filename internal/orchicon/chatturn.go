package orchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the native bridge implements the Ask chat-session
// capability. It deliberately does NOT implement
// scheduler.SendTurnMessageWithAttachments: the askorchicon collector fails
// loudly when an adapter lacks the attachment capability (never silently
// degrading to text-only).
var _ scheduler.ChatTurnClient = (*NativeBridge)(nil)

// AskToolProvider supplies the Ask-time tool surface for native turns.
// It is implemented outside this package (the askorchicon tool registry
// owns the product tools; the server injects it via SetAskTools) so the
// provider substrate never imports the product layer.
type AskToolProvider interface {
	// AskToolDefs returns the tool definitions offered to the model.
	AskToolDefs() []ToolDef
	// ExecuteAskTool runs one tool call and returns its result text.
	ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error)
}

// askMaxToolRounds bounds the per-turn tool loop so a model that keeps
// calling tools cannot spin forever inside one user message.
const askMaxToolRounds = 8

// chatBus is the per-turn SessionBus the native bridge feeds from its drain
// goroutine. It is the adapter-neutral surface the askorchicon collector
// consumes; the bridge maps the provider's normalized TurnStream events onto
// the SessionEvent vocabulary (idle/error/delta/part).
type chatBus struct {
	events chan scheduler.SessionEvent
	done   chan struct{}
	once   sync.Once
}

func newChatBus() *chatBus {
	return &chatBus{events: make(chan scheduler.SessionEvent, 32), done: make(chan struct{})}
}

func (b *chatBus) Events() <-chan scheduler.SessionEvent { return b.events }
func (b *chatBus) Done() <-chan struct{}                 { return b.done }
func (b *chatBus) Close() {
	b.once.Do(func() {
		close(b.done)
		close(b.events)
	})
}

// emit pushes one event onto the bus, dropping it if the bus is already
// closed (the drain goroutine ended). Never blocks.
func (b *chatBus) emit(evt scheduler.SessionEvent) {
	select {
	case b.events <- evt:
	default:
	}
}

// SetAskTools injects the Ask-time tool surface for native turns (the
// askorchicon product tools). Nil (default) keeps the pre-tools behavior:
// the model answers from the system prompt with no tool calls.
func (b *NativeBridge) SetAskTools(p AskToolProvider) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askTools = p
}

// askToolsLocked returns the injected tool definitions (nil when no
// provider is set). Callers must hold b.mu.
func (b *NativeBridge) askToolsLocked() []ToolDef {
	if b.askTools == nil {
		return nil
	}
	return b.askTools.AskToolDefs()
}

// CreateConversationSession implements scheduler.ChatTurnClient. The native
// transports are sessionless, so this returns a synthetic session id and
// initializes an empty in-memory history under it. No server-side session
// exists; the history is re-sent as full context per turn (D2).
func (b *NativeBridge) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	if conversationID == "" {
		return "", errors.New("orchicon bridge: create conversation session requires a conversation id")
	}
	sid := "orchicon-ask:" + conversationID
	b.mu.Lock()
	if b.chatHistory == nil {
		b.chatHistory = map[string][]Message{}
	}
	if _, ok := b.chatHistory[sid]; !ok {
		b.chatHistory[sid] = nil
	}
	b.mu.Unlock()
	return sid, nil
}

// Subscribe implements scheduler.ChatTurnClient: it returns a fresh buffered
// SessionBus for the conversation's next turn and registers it under the
// conversation's session so SendTurnMessage feeds it. The bus is fed by the
// drain goroutine started in SendTurnMessage and closed when the turn ends.
func (b *NativeBridge) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	bus := newChatBus()
	b.mu.Lock()
	if b.chatBuses == nil {
		b.chatBuses = map[string]*chatBus{}
	}
	b.chatBuses[conversationID] = bus
	b.mu.Unlock()
	return bus, nil
}

// SendTurnMessage implements scheduler.ChatTurnClient. It appends the user
// message to the session's in-memory history, resolves the provider from the
// model_ref through the registry credential path, and streams ONE turn via
// Provider.StreamTurn. The stream is started SYNCHRONOUSLY (a pre-stream
// failure — auth/connect — returns the error so the collector fails the turn
// as a send-accept failure); the drain goroutine then maps events onto the
// bus and returns nil (accepted). This ordering guarantees the collector's
// `sent` guard never drops the first delta (D4).
func (b *NativeBridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	if sessionID == "" {
		return errors.New("orchicon bridge: send turn requires a session id (create the conversation session first)")
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return errors.New("orchicon bridge: no tenant in context — cannot resolve the Ask provider")
	}
	providerID, model, ok := adapter.SplitForServe(modelRef)
	if !ok || providerID == "" || model == "" {
		return fmt.Errorf("orchicon bridge: Ask model ref %q has no provider/model", modelRef)
	}
	if b.resolver == nil {
		return errors.New("orchicon bridge: no provider resolver for the Ask turn")
	}
	prov, err := b.resolver.Get(ctx, tenantID, providerID)
	if err != nil {
		return fmt.Errorf("orchicon bridge: resolve Ask provider: %w", err)
	}

	// Append the user message to the session's replayable history (committed
	// under the lock so a concurrent turn never double-appends).
	userMsg := text
	b.mu.Lock()
	history := append([]Message(nil), b.chatHistory[sessionID]...)
	history = append(history, Message{Role: RoleUser, Content: []Content{{Text: &userMsg}}})
	b.chatHistory[sessionID] = history
	b.mu.Unlock()

	// Build the turn request: the accumulated history re-sent as full context
	// (the sessionless emulation), the system prompt as a cacheable block,
	// and the Ask tool surface when one is injected (SetAskTools). Without
	// tools the model can only answer from the system prompt's project
	// context — with them it can query, read, and act like the host-serve
	// path.
	b.mu.Lock()
	tools := b.askToolsLocked()
	b.mu.Unlock()
	req := TurnRequest{
		Model: model,
		System: []SystemBlock{
			{Text: system, Cache: true},
		},
		Messages:    history,
		Tools:       tools,
		MaxTokens:   maxOutputTokens(),
		CacheControl: CacheControlSystemAndTools,
		// Stable per-conversation session id for OpenCode Zen/Go (D1): the
		// provider requires x-opencode-session per conversation.
		SessionID: conversationID,
	}
	// The turn context is derived from the request ctx so
	// AbortConversationSession can cancel it mid-flight (D7) — including
	// the provider HTTP calls on every tool round.
	turnCtx, cancel := context.WithCancel(ctx)
	// Start the stream SYNCHRONOUSLY so a pre-stream failure surfaces as a
	// send-accept failure (the collector fails the turn) rather than a
	// dropped first delta (D4).
	stream, err := prov.StreamTurn(turnCtx, req)
	if err != nil {
		cancel()
		return fmt.Errorf("orchicon bridge: start Ask turn: %w", err)
	}

	// Drain the stream on a goroutine, mapping events onto the bus.
	b.mu.Lock()
	if b.chatTurns == nil {
		b.chatTurns = map[string]context.CancelFunc{}
	}
	b.chatTurns[sessionID] = cancel
	bus := b.chatBuses[conversationID]
	if bus == nil {
		bus = newChatBus()
	}
	b.mu.Unlock()

	go b.drainChatTurn(turnCtx, prov, bus, stream, req, sessionID, history)

	// Return nil (accepted) BEFORE the drain goroutine emits, so the
	// collector observes every event with sent == true (D4).
	return nil
}

// drainChatTurn reads the provider's TurnStream(s) and maps events onto the
// SessionBus vocabulary (D3): TextDelta → delta(text) + accumulate into the
// part buffer; ReasoningDelta → delta(reasoning); ToolCall → tool_part +
// execute against the injected Ask tools and continue the turn with the
// results (agentic loop, bounded by askMaxToolRounds); StreamError → error;
// Finish → emit one part(text) with the accumulated reply, then idle. On
// completion the full working history (assistant texts, tool uses and tool
// results — not just the final text) replaces the session's history so a
// follow-up re-sends the complete context.
func (b *NativeBridge) drainChatTurn(ctx context.Context, prov Provider, bus *chatBus, stream TurnStream, req TurnRequest, sessionID string, history []Message) {
	defer bus.Close()
	defer func() {
		b.mu.Lock()
		delete(b.chatTurns, sessionID)
		b.mu.Unlock()
	}()

	// working is the turn's replayable history: it starts as the snapshot
	// committed in SendTurnMessage (prior turns + this user message) and
	// grows with every assistant message and tool result until commit.
	working := append([]Message(nil), history...)
	var reply strings.Builder
	var reasoning strings.Builder

	// finishTurn emits the accumulated reply as ONE completed text part,
	// then idle, and commits the working history. The collector builds the
	// persisted reply ONLY from part/text events, so this part is what
	// lands in the DB.
	finishTurn := func() {
		if t := strings.TrimSpace(reply.String()); t != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
		}
		bus.emit(scheduler.SessionEvent{Kind: "idle"})
		b.commitChatHistory(sessionID, history, working)
	}

	for round := 0; ; round++ {
		roundDone, calls, aborted := b.drainOneRound(ctx, bus, stream, &reply, &reasoning)
		_ = stream.Close()
		if aborted {
			// Abort (D7): the turn was cancelled — finalize without
			// committing. The collector's Stop path already cancelled its
			// own context, so the turn finalizes cleanly.
			return
		}
		if !roundDone {
			// Stream ended without a Finish event — close out the turn so
			// the collector persists what arrived (pre-existing behavior
			// for a provider that ends without a terminal signal).
			b.appendAssistantText(&working, reply.String())
			finishTurn()
			return
		}
		if len(calls) == 0 {
			b.appendAssistantText(&working, reply.String())
			finishTurn()
			return
		}
		// Tool round: record the assistant's text (if any) plus the tool
		// uses, emit the round's text as its own part (the collector
		// appends every part, so multi-round replies persist in full),
		// execute every call, and continue the turn with the results.
		// Tool results are part of the replayable history (unlike the
		// pre-tools turn, which committed text only).
		b.appendAssistantTurn(&working, reply.String(), calls)
		if t := strings.TrimSpace(reply.String()); t != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
		}
		reply.Reset()
		if round+1 >= askMaxToolRounds {
			// Budget exhausted: append the notice as a tool result and
			// take ONE final text-only turn (tools stripped so the model
			// must answer from the results so far instead of calling
			// again). The final turn below is drained by the next loop
			// iteration like any other round.
			working = append(working, Message{Role: RoleTool, Content: []Content{{
				ToolResult: &ContentToolResult{ToolCallID: calls[0].ToolCallID, Content: "Tool round budget exhausted — answer from the results so far.", IsError: true},
			}}})
			req.Messages = append([]Message(nil), working...)
			req.Tools = nil
		} else {
			b.executeToolCalls(ctx, &working, calls)
			req.Messages = append([]Message(nil), working...)
		}
		next, err := prov.StreamTurn(ctx, req)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			return
		}
		stream = next
	}
}

// drainOneRound consumes one provider stream until its Finish event (or an
// error/early end), live-emitting deltas onto the bus and accumulating the
// reply text. It returns roundDone (a Finish event arrived), the complete
// tool calls issued this round, and aborted (the turn context was
// cancelled).
func (b *NativeBridge) drainOneRound(ctx context.Context, bus *chatBus, stream TurnStream, reply, reasoning *strings.Builder) (roundDone bool, calls []ToolCall, aborted bool) {
	for {
		evt, ok, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, nil, true
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			return false, nil, false
		}
		if !ok {
			return false, nil, false
		}
		switch e := evt.(type) {
		case TextDelta:
			reply.WriteString(e.Text)
			bus.emit(scheduler.SessionEvent{Kind: "delta", Type: "text", Text: e.Text})
		case ReasoningDelta:
			reasoning.WriteString(e.Text)
			bus.emit(scheduler.SessionEvent{Kind: "delta", Type: "reasoning", Text: e.Text, IsReasoning: true})
		case ToolCall:
			bus.emit(scheduler.SessionEvent{Kind: "tool_part", Type: "tool", Text: e.Name})
			calls = append(calls, e)
		case StreamError:
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: e.Err.Error()})
			return false, nil, false
		case Finish:
			return true, calls, false
		}
	}
}

// executeToolCalls runs one round's tool calls against the injected Ask
// tools and appends one tool-result message per call to working. Calls are
// never dropped: with no tool provider injected the result records the
// misconfiguration as an error so the model can explain instead of
// hanging. A tool execution failure is recorded as an error result (the
// model sees it and can recover), never as a turn failure.
func (b *NativeBridge) executeToolCalls(ctx context.Context, working *[]Message, calls []ToolCall) {
	b.mu.Lock()
	tools := b.askTools
	b.mu.Unlock()
	for _, c := range calls {
		args := c.ArgsJSON
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		var out string
		var toolErr error
		if tools == nil {
			toolErr = errors.New("orchicon bridge: no Ask tool provider injected — cannot execute tool " + c.Name)
		} else {
			out, toolErr = tools.ExecuteAskTool(ctx, c.Name, args)
		}
		content := out
		isErr := false
		if toolErr != nil {
			content = toolErr.Error()
			isErr = true
		}
		*working = append(*working, Message{Role: RoleTool, Content: []Content{{
			ToolResult: &ContentToolResult{ToolCallID: c.ToolCallID, Content: content, IsError: isErr},
		}}})
	}
}

// appendAssistantText appends one assistant text message to working when
// text is non-blank.
func (b *NativeBridge) appendAssistantText(working *[]Message, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	t := text
	*working = append(*working, Message{Role: RoleAssistant, Content: []Content{{Text: &t}}})
}

// appendAssistantTurn appends one assistant message carrying the round's
// text (when non-blank) plus its tool uses, then clears the consumed text
// from reply via the caller's Reset.
func (b *NativeBridge) appendAssistantTurn(working *[]Message, text string, calls []ToolCall) {
	var content []Content
	if strings.TrimSpace(text) != "" {
		t := text
		content = append(content, Content{Text: &t})
	}
	for _, c := range calls {
		args := c.ArgsJSON
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		content = append(content, Content{ToolUse: &ContentToolUse{ToolCallID: c.ToolCallID, Name: c.Name, ArgsJSON: args}})
	}
	if len(content) == 0 {
		return
	}
	*working = append(*working, Message{Role: RoleAssistant, Content: content})
}

// commitChatHistory replaces the session's in-memory history with the
// turn's full working history (user message, assistant texts, tool uses
// and tool results) so a follow-up re-sends the complete context.
// Best-effort: when the turn produced no new messages the stored history
// is left untouched; when the stored entry was reset mid-turn (session
// recreated) the turn's own snapshot seeds it.
func (b *NativeBridge) commitChatHistory(sessionID string, history, working []Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(working) == 0 {
		return
	}
	cur := b.chatHistory[sessionID]
	if len(cur) == 0 && len(history) > 0 {
		// The entry was reset mid-turn — seed from the turn snapshot so
		// the user message is not lost, then prefer the working tail.
		if len(working) >= len(history) {
			cur = append([]Message(nil), working...)
		} else {
			cur = append([]Message(nil), history...)
			cur = append(cur, working...)
		}
	} else {
		cur = append([]Message(nil), working...)
	}
	b.chatHistory[sessionID] = cur
}

// AbortConversationSession implements scheduler.ChatTurnClient: it cancels
// the in-flight turn's context so the model stops generating now (D7). Safe
// no-op for unknown sessions.
func (b *NativeBridge) AbortConversationSession(ctx context.Context, sessionID string) error {
	b.mu.Lock()
	cancel, ok := b.chatTurns[sessionID]
	b.mu.Unlock()
	if !ok {
		return nil // safe no-op for unknown/finished sessions
	}
	cancel()
	return nil
}

// ReplyPermission implements scheduler.ChatTurnClient. Native Ask turns are
// text-only (no tools, no permission.asked), so this is never invoked in
// practice; it returns an actionable error rather than silently swallowing an
// approval (D6).
func (b *NativeBridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return errors.New("orchicon native Ask turns are text-only — permission approval is not supported")
}
