package orchicon

import (
	"context"
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
// scheduler.SendTurnMessageWithAttachments: native Ask turns are text-only,
// and the askorchicon collector fails loudly when an adapter lacks the
// attachment capability (never silently degrading to text-only).
var _ scheduler.ChatTurnClient = (*NativeBridge)(nil)

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
	// (the sessionless emulation), the system prompt as a cacheable block.
	req := TurnRequest{
		Model: model,
		System: []SystemBlock{
			{Text: system, Cache: true},
		},
		Messages:    history,
		MaxTokens:   maxOutputTokens(),
		CacheControl: CacheControlSystemAndTools,
		// Stable per-conversation session id for OpenCode Zen/Go (D1): the
		// provider requires x-opencode-session per conversation.
		SessionID: conversationID,
	}
	// Start the stream SYNCHRONOUSLY so a pre-stream failure surfaces as a
	// send-accept failure (the collector fails the turn) rather than a
	// dropped first delta (D4).
	stream, err := prov.StreamTurn(ctx, req)
	if err != nil {
		return fmt.Errorf("orchicon bridge: start Ask turn: %w", err)
	}

	// Drain the stream on a goroutine, mapping events onto the bus. The turn
	// context is derived from the request ctx so AbortConversationSession can
	// cancel it mid-flight (D7).
	turnCtx, cancel := context.WithCancel(ctx)
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

	go b.drainChatTurn(turnCtx, bus, stream, sessionID, history)

	// Return nil (accepted) BEFORE the drain goroutine emits, so the
	// collector observes every event with sent == true (D4).
	return nil
}

// drainChatTurn reads the provider's TurnStream and maps events onto the
// SessionBus vocabulary (D3): TextDelta → delta(text) + accumulate into the
// part buffer; ReasoningDelta → delta(reasoning) + accumulate; ToolCall →
// tool_part; StreamError → error; Finish → emit one part(text) with the
// accumulated reply, then idle. On completion the assistant reply is appended
// to the session's history so a follow-up re-sends it as context.
func (b *NativeBridge) drainChatTurn(ctx context.Context, bus *chatBus, stream TurnStream, sessionID string, history []Message) {
	defer stream.Close()
	defer bus.Close()
	defer func() {
		b.mu.Lock()
		delete(b.chatTurns, sessionID)
		b.mu.Unlock()
	}()

	var reply strings.Builder
	var reasoning strings.Builder
	for {
		evt, ok, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				// Abort (D7): the turn was cancelled — finalize without a
				// reply. The collector's Stop path already cancelled its own
				// context, so the turn finalizes cleanly.
				return
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			return
		}
		if !ok {
			// Stream ended without a Finish event — emit the accumulated
			// reply as a part then idle so the collector persists it.
			if t := strings.TrimSpace(reply.String()); t != "" {
				bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
			}
			bus.emit(scheduler.SessionEvent{Kind: "idle"})
			b.commitChatHistory(sessionID, history, reply.String())
			return
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
		case StreamError:
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: e.Err.Error()})
			return
		case Finish:
			// Emit the accumulated reply as ONE completed text part, then
			// idle. The collector builds the persisted reply ONLY from
			// part/text events, so this part is what lands in the DB.
			if t := strings.TrimSpace(reply.String()); t != "" {
				bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
			}
			bus.emit(scheduler.SessionEvent{Kind: "idle"})
			b.commitChatHistory(sessionID, history, reply.String())
			return
		}
	}
}

// commitChatHistory appends the assistant reply to the session's in-memory
// history so a follow-up re-sends it as context. Best-effort: a missing
// history entry (session recreated / server restart) is a no-op.
func (b *NativeBridge) commitChatHistory(sessionID string, history []Message, reply string) {
	if strings.TrimSpace(reply) == "" {
		return
	}
	assistant := reply
	b.mu.Lock()
	defer b.mu.Unlock()
	// The user message was already committed to the session's history in
	// SendTurnMessage; append the assistant reply to it. If the history was
	// reset (session recreated) in the meantime, seed from the turn's own
	// history so the reply still lands.
	cur := b.chatHistory[sessionID]
	if len(cur) == 0 {
		cur = append([]Message(nil), history...)
	}
	cur = append(cur, Message{Role: RoleAssistant, Content: []Content{{Text: &assistant}}})
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
