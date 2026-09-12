package orchicon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the native bridge implements the Ask chat-session
// capability, including the attachment-aware sender (parity with the
// opencode adapter — attachments ride the turn inline, never silently
// dropped).
var _ scheduler.ChatTurnClient = (*NativeBridge)(nil)
var _ scheduler.SendTurnMessageWithAttachments = (*NativeBridge)(nil)
var _ scheduler.ConversationHistoryPurger = (*NativeBridge)(nil)

// Attachment caps mirror the server-side turn validation
// (startConversationTurnOpts): the bridge enforces them too so direct
// interface callers get the same bounds.
const (
	askMaxAttachments           = 5
	askMaxAttachmentBytes       = 10 * 1024 * 1024
	askMaxAttachmentsTotalBytes = 20 * 1024 * 1024
)

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

// SetAskHistoryDir enables disk persistence for Ask session histories
// (one JSON file per session under dir). Empty disables persistence
// (memory-only). Wired from the server instance data dir.
func (b *NativeBridge) SetAskHistoryDir(dir string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askHistoryDir = dir
}

// askHistoryVersion versions the on-disk Ask history envelope.
const askHistoryVersion = 1

// askHistoryMaxBytes caps one persisted session file (image attachments
// are base64 data URLs — a long image-heavy conversation could otherwise
// grow the file without bound). Oversize histories stay memory-only.
const askHistoryMaxBytes = 16 * 1024 * 1024

// askHistoryFilename sanitizes a session id into a safe file stem.
func askHistoryFilename(sessionID string) string {
	var sb strings.Builder
	for _, r := range sessionID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "session"
	}
	return sb.String()
}

// persistAskHistoryLocked writes the session's history to disk (atomic
// tmp + rename). Callers must hold b.mu. Best-effort: every failure is a
// warn + return, never a turn failure.
func (b *NativeBridge) persistAskHistoryLocked(sessionID string) {
	dir := b.askHistoryDir
	if dir == "" {
		return
	}
	history := b.chatHistory[sessionID]
	env := map[string]any{"version": askHistoryVersion, "messages": history}
	raw, err := json.Marshal(env)
	if err != nil {
		b.log.Warn("orchicon: ask history marshal failed", "session", sessionID, "error", err)
		return
	}
	if len(raw) > askHistoryMaxBytes {
		b.log.Warn("orchicon: ask history oversize — staying memory-only", "session", sessionID, "bytes", len(raw))
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.log.Warn("orchicon: ask history dir create failed", "dir", dir, "error", err)
		return
	}
	path := filepath.Join(dir, askHistoryFilename(sessionID)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		b.log.Warn("orchicon: ask history write failed", "session", sessionID, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		b.log.Warn("orchicon: ask history rename failed", "session", sessionID, "error", err)
	}
}

// loadAskHistoryLocked reads a persisted session history from disk (nil on
// a miss or any failure — a new conversation looks exactly like a lost
// file, and both correctly start empty). Callers must hold b.mu.
func (b *NativeBridge) loadAskHistoryLocked(sessionID string) []Message {
	dir := b.askHistoryDir
	if dir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, askHistoryFilename(sessionID)+".json"))
	if err != nil {
		return nil
	}
	var env struct {
		Version  int       `json:"version"`
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != askHistoryVersion {
		b.log.Warn("orchicon: ask history unreadable — starting empty", "session", sessionID, "error", err)
		return nil
	}
	return env.Messages
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

// PurgeConversationHistory implements scheduler.ConversationHistoryPurger:
// it discards a deleted conversation's durable Ask history — the in-memory
// map entry AND the persisted JSON file — because nothing else reclaims them.
// The native adapter is sessionless, so the history file is the only on-disk
// artifact of a conversation; without this, every deleted conversation leaked
// its file forever (observed: 19 conversation files totalling 19MB, several of
// them multi-MB).
//
// Idempotent: a missing map entry or missing file is a successful no-op. The
// caller treats any error as advisory (logged, never failing the delete RPC),
// since the durable DB record is already gone by the time this runs.
func (b *NativeBridge) PurgeConversationHistory(_ context.Context, conversationID, sessionID string) error {
	sid := sessionID
	if sid == "" && conversationID != "" {
		// Sessionless adapters mint their own synthetic id; resolve it so a
		// conversation row without a persisted session id still purges.
		sid = scheduler.NativeSessionIDPrefix + conversationID
	}
	if sid == "" {
		return nil
	}
	b.mu.Lock()
	delete(b.chatHistory, sid)
	dir := b.askHistoryDir
	b.mu.Unlock()
	if dir == "" {
		return nil // memory-only history: nothing persisted to reclaim
	}
	path := filepath.Join(dir, askHistoryFilename(sid)+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("orchicon bridge: purge ask history %s: %w", path, err)
	}
	// Sweep the atomic-write temp file too: a crash mid-persist can leave it
	// behind, and a deleted conversation must leave nothing on disk.
	_ = os.Remove(path + ".tmp")
	return nil
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
	return b.dispatchTurnMessage(ctx, conversationID, sessionID, system, modelRef, text, nil)
}

// SendTurnMessageWithAttachments implements
// scheduler.SendTurnMessageWithAttachments (parity with the opencode
// adapter): images ride as image data-URL parts (every native wire
// consumes data URLs) and UTF-8 text files inline as fenced text parts —
// one POST, no upload-path dependency. Non-image, non-UTF8 binaries have
// no native wire shape and fail loudly naming the file (never silently
// dropped); those need the opencode adapter's document handling.
func (b *NativeBridge) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	return b.dispatchTurnMessage(ctx, conversationID, sessionID, system, modelRef, text, attachments)
}

// askUserContent builds the user message content for a turn: the text plus
// one content element per attachment (image → Image data URL, UTF-8 text →
// fenced Text). Caps mirror the server-side validation.
func askUserContent(text string, attachments []scheduler.ChatAttachment) ([]Content, error) {
	var content []Content
	if text != "" {
		t := text
		content = append(content, Content{Text: &t})
	}
	if len(attachments) > askMaxAttachments {
		return nil, fmt.Errorf("orchicon bridge: too many attachments (%d, max %d)", len(attachments), askMaxAttachments)
	}
	total := 0
	for _, a := range attachments {
		if len(a.Data) == 0 {
			continue
		}
		if len(a.Data) > askMaxAttachmentBytes {
			return nil, fmt.Errorf("orchicon bridge: attachment %q too large (max 10MB)", a.Name)
		}
		total += len(a.Data)
		if total > askMaxAttachmentsTotalBytes {
			return nil, fmt.Errorf("orchicon bridge: attachments too large (max 20MB total)")
		}
		mime := a.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		if strings.HasPrefix(mime, "image/") {
			u := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
			content = append(content, Content{Image: &u})
			continue
		}
		if utf8.Valid(a.Data) {
			name := a.Name
			if name == "" {
				name = "attachment"
			}
			fenced := "--- attachment: " + name + " (" + mime + ") ---\n" + string(a.Data)
			content = append(content, Content{Text: &fenced})
			continue
		}
		return nil, fmt.Errorf("orchicon bridge: attachment %q (%s) is a binary document the native wires cannot carry — use an opencode-adapter model for binary documents", a.Name, mime)
	}
	if len(content) == 0 {
		t := text
		content = append(content, Content{Text: &t})
	}
	return content, nil
}

func (b *NativeBridge) dispatchTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
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
	// under the lock so a concurrent turn never double-appends). The
	// content carries the text plus any attachments (images as data URLs,
	// text files fenced) so follow-ups and tool rounds replay them.
	userContent, err := askUserContent(text, attachments)
	if err != nil {
		return err
	}
	b.mu.Lock()
	history := append([]Message(nil), b.chatHistory[sessionID]...)
	if len(history) == 0 {
		// Memory has nothing (fresh session or a server restart wiped
		// it) — reseed from the persisted file when present so the turn
		// re-sends the full context instead of starting over.
		history = append([]Message(nil), b.loadAskHistoryLocked(sessionID)...)
	}
	// REPLAY BOUNDARY: regardless of where the history came from (this
	// process, the persisted file, an interrupted prior turn), what is sent to
	// the provider is well-formed — and the repair is persisted, so a session
	// poisoned by a dangling tool call heals permanently instead of 400ing on
	// every subsequent turn.
	history = sanitizeChatHistory(history)
	history = append(history, Message{Role: RoleUser, Content: userContent})
	b.chatHistory[sessionID] = history
	b.persistAskHistoryLocked(sessionID)
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
		Messages:     history,
		Tools:        tools,
		MaxTokens:    maxOutputTokens(),
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
	// dropped first delta (D4). A provider context-window overflow is the one
	// failure with a deterministic remedy: reduce the replayed history and
	// retry (askreduce.go). Without that, a long conversation is permanently
	// wedged — every subsequent turn re-sends the same oversized history and
	// 400s (observed live on a 1574-message Ask session at ~1.02M tokens).
	stream, err := b.startTurnWithContextRecovery(turnCtx, prov, &req, sessionID)
	if err != nil {
		cancel()
		return fmt.Errorf("orchicon bridge: start Ask turn: %w", err)
	}
	// A reduction (when one happened) replaced the replayed history. Drain
	// against what the provider actually ACCEPTED, so the session's working
	// context matches the prompt it saw.
	history = req.Messages

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

	go b.drainChatTurn(turnCtx, prov, bus, stream, req, sessionID, history, b.askUsageSink(tenantID, conversationID, modelRef, providerID, model))

	// Return nil (accepted) BEFORE the drain goroutine emits, so the
	// collector observes every event with sent == true (D4).
	return nil
}

// drainChatTurn reads the provider's TurnStream(s) and maps events onto the
// SessionBus vocabulary (D3): TextDelta → delta(text) + accumulate into the
// part buffer; ReasoningDelta → delta(reasoning); ToolCall → tool_part +
// execute against the injected Ask tools and continue the turn with the
// results (agentic loop, unbounded by round count); StreamError → error;
// Finish → emit one part(text) with the accumulated reply, then idle. On
// completion the full working history (assistant texts, tool uses and tool
// results — not just the final text) replaces the session's history so a
// follow-up re-sends the complete context.
func (b *NativeBridge) drainChatTurn(ctx context.Context, prov Provider, bus *chatBus, stream TurnStream, req TurnRequest, sessionID string, history []Message, usageSink func(context.Context, Usage)) {
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
	// reply accumulates EVERY round's assistant text into one consolidated
	// reply (the final part). Round text is never emitted as its own part:
	// a committed intermediate part would show up as a separate bubble that
	// overlaps the live delta stream (and, if the model re-states its
	// preamble, duplicates it).
	var reply strings.Builder
	// roundReply holds only the CURRENT round's text (for history replay,
	// which needs one assistant message per round).
	var roundReply strings.Builder
	var reasoning strings.Builder

	// finishTurn emits the consolidated reply as ONE completed text part,
	// then idle, and commits the working history. The collector builds the
	// persisted reply ONLY from part/text events, so this single part is
	// what lands in the DB.
	finishTurn := func() {
		if t := strings.TrimSpace(reply.String()); t != "" {
			bus.emit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: t})
		}
		bus.emit(scheduler.SessionEvent{Kind: "idle"})
		b.commitChatHistory(sessionID, history, working)
	}

	for round := 0; ; round++ {
		roundDone, calls, usage, aborted := b.drainOneRound(ctx, bus, stream, &roundReply, &reasoning)
		// Report the round's REAL usage (never estimated). Emitted per ROUND
		// because each round is one provider call — so the newest sample's prompt
		// size is exactly the context pressure a gate needs, and a multi-round
		// turn does not collapse into a single misleading total.
		if usageSink != nil {
			usageSink(ctx, usage)
		}
		_ = stream.Close()
		if aborted {
			// Abort (D7): the turn was cancelled — finalize without
			// committing. The collector's Stop path already cancelled its
			// own context, so the turn finalizes cleanly.
			return
		}
		roundText := roundReply.String()
		roundReply.Reset()
		if roundText != "" {
			if reply.Len() > 0 {
				reply.WriteString("\n\n")
			}
			reply.WriteString(roundText)
		}
		if !roundDone {
			// Stream ended without a Finish event — close out the turn so
			// the collector persists what arrived (pre-existing behavior
			// for a provider that ends without a terminal signal).
			b.appendAssistantText(&working, roundText)
			finishTurn()
			return
		}
		if len(calls) == 0 {
			b.appendAssistantText(&working, roundText)
			finishTurn()
			return
		}
		// Tool round: record the assistant's text (if any) plus the tool
		// uses, execute every call, and continue the turn with the results.
		// Tool results are part of the replayable history (unlike the
		// pre-tools turn, which committed text only).
		b.appendAssistantTurn(&working, roundText, calls)
		// The tool loop is unbounded by round count: it continues while the
		// model keeps issuing tool calls and terminates naturally when a
		// round returns none (the len(calls) == 0 → finishTurn() path above).
		// A pathological model that loops tool calls forever is bounded by
		// the time gates, not a round count: the reply window
		// (askReplyWindow(), default 30m, ORCHICON_ASK_REPLY_WINDOW), the
		// turn TTL sweeper (askTurnMaxAge(), default 31m,
		// ORCHICON_ASK_TURN_MAX_AGE — internal/askorchicon/chat.go), and the
		// stall monitor (tenant stall settings).
		b.executeToolCalls(ctx, &working, calls)
		req.Messages = append([]Message(nil), working...)
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
func (b *NativeBridge) drainOneRound(ctx context.Context, bus *chatBus, stream TurnStream, reply, reasoning *strings.Builder) (roundDone bool, calls []ToolCall, usage Usage, aborted bool) {
	for {
		evt, ok, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, nil, Usage{}, true
			}
			bus.emit(scheduler.SessionEvent{Kind: "error", Type: "error", Text: err.Error()})
			return false, nil, Usage{}, false
		}
		if !ok {
			return false, nil, Usage{}, false
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
			return false, nil, Usage{}, false
		case Finish:
			// The round's REAL provider usage rides the Finish event. Surfaced to
			// the caller so the Ask path can attribute it to the conversation
			// (never estimated — see askUsageSink).
			return true, calls, e.Usage, false
		}
	}
}

// askUsageSink builds the per-round usage reporter for one Ask turn, or nil
// when no recorder is wired (Ask then records no usage, matching the worker
// path's behaviour under a nil recorder).
//
// The sample is attributed to the CONVERSATION via SessionID rather than to an
// execution: a chat turn has no execution/task/project row (mirroring the
// opencode Ask path, askorchicon.recordTurnUsage). That attribution is also what
// makes the rows prunable — DeleteConversation de-links usage by conversation id
// (db.ClearUsageSessionIDs) instead of deleting it, because usage_records is the
// tenant's real spend ledger and Cost Explorer/Telemetry roll up from it.
//
// Before this, a native Ask turn recorded NOTHING: the native per-turn usage
// sink was wired only on the worker-execution path (bridge.go, emitTurnUsage),
// which is bound to an execution row. That left the Ask path with no measurable
// prompt size at all.
func (b *NativeBridge) askUsageSink(tenantID, conversationID, modelRef, provider, model string) func(context.Context, Usage) {
	if b.usageRecorder == nil {
		return nil
	}
	return func(ctx context.Context, u Usage) {
		// A genuinely empty round is dropped (parity with emitTurnUsage): a
		// provider that reported nothing is not a zero-cost sample.
		if u.InputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 &&
			u.OutputTokens == 0 && u.ReasoningTokens == 0 && u.CostUSD == 0 {
			return
		}
		// context.WithoutCancel: this is real usage that has already been paid
		// for, so a turn that ends (or is aborted) mid-record must not lose it.
		_ = b.usageRecorder(context.WithoutCancel(ctx), scheduler.UsageRecord{
			TenantID:         tenantID,
			Provider:         provider,
			Model:            model,
			PromptTokens:     u.InputTokens,
			CacheReadTokens:  u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
			CompletionTokens: u.OutputTokens,
			ReasoningTokens:  u.ReasoningTokens,
			CostUSD:          u.CostUSD,
			AdapterKind:      adapter.AdapterKind(modelRef),
			SessionID:        conversationID,
		})
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
	// seen guards against a provider/decoder echo issuing the same call
	// twice in one round: a second result for one call_id makes the wire
	// reject the turn as a duplicate function_call_output. First
	// occurrence wins.
	seen := map[string]bool{}
	for _, c := range calls {
		if c.ToolCallID != "" {
			if seen[c.ToolCallID] {
				continue
			}
			seen[c.ToolCallID] = true
		}
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
	seenUse := map[string]bool{}
	for _, c := range calls {
		if c.ToolCallID != "" {
			if seenUse[c.ToolCallID] {
				continue
			}
			seenUse[c.ToolCallID] = true
		}
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

// danglingToolResultOutput is the explicit tool result attached in place of a
// result that never arrived (the turn ended abnormally between the assistant's
// tool call and its result).
const danglingToolResultOutput = "tool call aborted — the turn ended before this tool returned a result"

// hasDanglingToolCalls reports whether a provider-bound history contains an
// assistant message with a tool use that no tool-role message answers. Such a
// history is exactly what providers reject with:
//
//	No tool output found for function call <id>.
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following
//	tool_calls message)
func hasDanglingToolCalls(messages []Message) bool {
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role != RoleTool {
			continue
		}
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID != "" {
				answered[c.ToolResult.ToolCallID] = true
			}
		}
	}
	for _, m := range messages {
		if m.Role != RoleAssistant {
			continue
		}
		for _, c := range m.Content {
			if c.ToolUse != nil && !answered[c.ToolUse.ToolCallID] {
				return true
			}
		}
	}
	return false
}

// sanitizeChatHistory returns a copy of the provider-bound history in which
// every assistant tool use is paired with a tool result: a call whose result is
// missing gets an explicit aborted result (so the model still learns the call
// happened and did not return), and a call with no id — which can never be
// matched to a result — is dropped, along with the assistant message when
// nothing else in it remains. This is the REPLAY-BOUNDARY invariant for the
// native Ask transport, whose history is re-sent in full on every turn (D2):
// whatever the session accumulated (an interrupted turn, a tool that never
// returned, a switch to a different model), what leaves for the provider is
// always well-formed.
func sanitizeChatHistory(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role != RoleTool {
			continue
		}
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID != "" {
				answered[c.ToolResult.ToolCallID] = true
			}
		}
	}

	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role != RoleAssistant {
			out = append(out, m)
			continue
		}
		var content []Content
		var missing []string
		seen := map[string]bool{}
		for _, c := range m.Content {
			if c.ToolUse == nil {
				content = append(content, c)
				continue
			}
			id := c.ToolUse.ToolCallID
			if id == "" {
				// Unaddressable call: replaying it is the bare tool_calls shape
				// the provider rejects, and no result could ever match it.
				continue
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			content = append(content, c)
			if !answered[id] {
				missing = append(missing, id)
			}
		}
		if len(content) == 0 {
			// Only unaddressable tool uses: the message itself goes away.
			continue
		}
		out = append(out, Message{Role: RoleAssistant, Content: content})
		for _, id := range missing {
			out = append(out, Message{Role: RoleTool, Content: []Content{{
				ToolResult: &ContentToolResult{ToolCallID: id, Content: danglingToolResultOutput, IsError: true},
			}}})
		}
	}
	return out
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
	// Never persist a dangling tool call: a turn that ended between a call and
	// its result must not poison the session for the next provider.
	cur = sanitizeChatHistory(cur)
	b.chatHistory[sessionID] = cur
	b.persistAskHistoryLocked(sessionID)
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
