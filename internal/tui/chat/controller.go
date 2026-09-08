package chat

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// controller.go — the TUI port of the GUI's Ask Orchicon streaming
// client (frontend/src/routes/ask-orchicon.tsx runStream/runWatch +
// frontend/src/api/askOrchicon.ts). Semantics carried over:
//
//   - ChatStream acks with TurnStarted{assistant_message_id}; the reply
//     is collected server-side on a request-independent context, so a
//     dropped socket (network blip, server restart) must NOT tear down
//     the turn: the slot goes reconnecting and WatchTurnStream re-dials
//     the broadcast hub (no dispatch), while a ListMessages poll stays
//     the completion authority.
//   - InterjectConversationTurn supersedes an in-flight turn.
//   - The live stream oneof carries textChunk | reasoning | turnStarted
//     | heartbeat | error (current impl emits TurnStarted + live chunks
//     + heartbeat; ListMessages remains authoritative for final text).

// IsAuthExpired reports whether err is a Connect UNAUTHENTICATED (401 —
// the re-auth prompt shape every dock RPC failure funnels through).
// Non-connect errors carrying the marker in their text classify too
// (wrapped errors lose the connect type).
func IsAuthExpired(err error) bool {
	if err == nil {
		return false
	}
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "unauthenticated")
}

// convState is one conversation's stream slot (ConvStream in the GUI).
type convState struct {
	streaming      bool
	reconnecting   bool
	pendingReplyID string
	// optimisticUser is the not-yet-persisted sent text (rendered
	// immediately; replaced by the transcript row once ListMessages
	// returns it).
	optimisticUser string
	sentText       string
}

// Controller owns Ask Orchicon conversation state + streaming for the
// TUI. It is a plain struct with tea.Cmd factories — the shell owns
// dispatching its messages back in.
type Controller struct {
	cl  *client.Clients
	reg Registrar // nilable; subscription re-arm hook

	mu     sync.Mutex
	convs  []Conversation
	state  map[string]*convState // per active-conversation slot
	active string
	err    string // last dock-visible error ("" = none)

	seq int // chunk key sequence (st-/sr- keys like the GUI)

	// store receives live chunks (the chat view); cmds receives follow-up
	// tea.Cmds produced by goroutines (watch re-dials). Both are set via
	// Bind by the shell — the package cannot import bubbletea's program
	// channel directly.
	store EventStore
	cmds  chan tea.Cmd
}

// Bind wires the controller to the view's event store and the shell's
// cmd sink. Goroutine-produced follow-ups (watch re-dials) are pushed
// into cmds; the shell drains them via NextBoundCmd messages.
func (c *Controller) Bind(store EventStore, cmds chan tea.Cmd) {
	c.mu.Lock()
	c.store, c.cmds = store, cmds
	c.mu.Unlock()
}

// Conversation is one Ask Orchicon conversation row (list rail).
type Conversation struct {
	ID        string
	Title     string
	TurnInFly bool
	MessageN  int32
}

// Registrar is the minimal subscription hook the controller needs
// (*subs.Registry satisfies it) — kept as an interface so controller
// tests need no registry.
type Registrar interface{}

// NewController builds the chat controller.
func NewController(cl *client.Clients) *Controller {
	return &Controller{cl: cl, state: map[string]*convState{}}
}

// --- tea.Msgs the controller emits (shell dispatches them back) --------

// ConversationsMsg carries the loaded conversation list.
type ConversationsMsg struct {
	Convs []Conversation
	Err   string
}

// TranscriptMsg carries the durable transcript of the opened
// conversation (already phase-grouped).
type TranscriptMsg struct {
	ConvID string
	Items  []ChatItem
	Err    string
}

// chatEventMsg forwards one ChatStreamResponse oneof event. ConvID tags
// the producing conversation so a stale stream can never write into
// another conversation's slot.
type chatEventMsg struct {
	ConvID string
	Ev     *apiv1.ChatStreamResponse
	// done closes the stream consumer loop (stream end / error).
	done bool
	err  error
}

// TurnResolvedMsg fires when the ListMessages poll observes the acked
// reply row while the turn is no longer in flight — the completion
// authority (GUI completion effect).
type TurnResolvedMsg struct {
	ConvID  string
	Failed  bool
	ErrText string
}

// ErrMsg carries a dock-visible failure (send/interject/watch/list).
type ErrMsg struct {
	Where string
	Err   error
}

// StreamDoneMsg marks the goroutine forwarding loop ended (exported —
// the shell's dispatch consumes it).
type StreamDoneMsg struct{ ConvID string }

func (s StreamDoneMsg) isMsg() {}

// --- accessors -----------------------------------------------------------

// Active returns the active conversation id ("" = none).
func (c *Controller) Active() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// SetActive pins the active conversation (dock sends target it).
func (c *Controller) SetActive(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = id
}

// Conversations returns a copy of the cached list.
func (c *Controller) Conversations() []Conversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Conversation, len(c.convs))
	copy(out, c.convs)
	return out
}

// IsStreaming reports whether the conversation has a live turn slot.
func (c *Controller) IsStreaming(convID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamingLocked(convID)
}

func (c *Controller) streamingLocked(convID string) bool {
	st, ok := c.state[convID]
	return ok && st != nil && st.streaming
}

// LastError returns (and clears) the sticky error string.
func (c *Controller) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.err
	c.err = ""
	return e
}

func (c *Controller) setErr(where string, err error) {
	c.mu.Lock()
	msg := err.Error()
	c.err = msg
	c.mu.Unlock()
}

// --- tea.Cmd factories ----------------------------------------------------

// LoadConversations fetches the conversation rail.
func (c *Controller) LoadConversations() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.ListConversations(ctx, connect.NewRequest(&apiv1.ListConversationsRequest{PageSize: 100}))
		if err != nil {
			return ConversationsMsg{Err: err.Error()}
		}
		convs := make([]Conversation, 0, len(resp.Msg.GetConversations()))
		for _, cv := range resp.Msg.GetConversations() {
			convs = append(convs, Conversation{
				ID:        cv.GetId(),
				Title:     cv.GetTitle(),
				TurnInFly: cv.GetTurnInFlight(),
				MessageN:  cv.GetMessageCount(),
			})
		}
		return ConversationsMsg{Convs: convs}
	}
}

// OpenConversation loads the durable transcript (ListMessages, reversed
// into ascending order like the GUI hook) and phase-groups it.
func (c *Controller) OpenConversation(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.ListMessages(ctx, connect.NewRequest(&apiv1.ListMessagesRequest{
			ConversationId: id,
			PageSize:       200,
		}))
		if err != nil {
			return TranscriptMsg{ConvID: id, Err: err.Error()}
		}
		// ListMessages returns oldest-first already (created_at asc); the
		// GUI hook reverses defensively — keep a stable same-order read.
		items := make([]ChatItem, 0, len(resp.Msg.GetMessages()))
		for _, m := range resp.Msg.GetMessages() {
			kind := KindText
			switch strings.ToLower(m.GetRole()) {
			case "user":
				kind = KindUser
			case "error":
				kind = KindError
			}
			at := m.GetCreatedAt().GetSeconds() * 1000
			items = append(items, ChatItem{Kind: kind, Text: m.GetContent(), At: at, Key: "m-" + m.GetId()})
		}
		return TranscriptMsg{ConvID: id, Items: GroupByPhase(items)}
	}
}

// Send dispatches a turn: InterjectConversationTurn when the
// conversation already has an in-flight turn (supersede), ChatStream
// otherwise — exactly the GUI's mode selection. The returned Cmd starts
// the event-forwarding goroutine. contextPreamble is the bracketed
// context line prepended to the message (the only context shape the
// API accepts).
func (c *Controller) Send(convID, text, contextPreamble string) tea.Cmd {
	full := text
	if contextPreamble != "" {
		full = contextPreamble + "\n" + text
	}
	c.mu.Lock()
	st := c.state[convID]
	if st == nil {
		st = &convState{}
		c.state[convID] = st
	}
	interject := st.streaming
	st.streaming = true
	st.reconnecting = false
	st.sentText = text
	st.optimisticUser = text
	if interject {
		// Superseding turn: clear the old slot's reply pointer (the new
		// turn acks a fresh assistant_message_id).
		st.pendingReplyID = ""
	}
	c.mu.Unlock()

	call := "send"
	if interject {
		call = "interject"
	}
	return c.startStream(convID, full, call)
}

// startStream opens the ChatStream/InterjectConversationTurn and returns
// a Cmd whose first message is a synthetic turnStarted placeholder (the
// actual ack arrives via the forwarding goroutine).
func (c *Controller) startStream(convID, full, call string) tea.Cmd {
	ctx := context.Background()
	return func() tea.Msg {
		var (
			stream *connect.ServerStreamForClient[apiv1.ChatStreamResponse]
			err    error
		)
		if call == "interject" {
			stream, err = c.cl.Ask.InterjectConversationTurn(ctx, connect.NewRequest(&apiv1.InterjectConversationTurnRequest{
				ConversationId: convID,
				Message:        full,
			}))
		} else {
			stream, err = c.cl.Ask.ChatStream(ctx, connect.NewRequest(&apiv1.ChatStreamRequest{
				ConversationId: convID,
				Message:        full,
			}))
		}
		if err != nil {
			c.failStream(convID, err)
			return ErrMsg{Where: call, Err: err}
		}
		go c.consume(convID, stream)
		return nil
	}
}

// consume forwards stream events into the tea channel via the returned
// messages (bubbletea re-dispatches what the Cmd returns; extra events
// ride the channel the program gave us at NewController time).
func (c *Controller) consume(convID string, stream *connect.ServerStreamForClient[apiv1.ChatStreamResponse]) {
	for stream.Receive() {
		c.handleEvent(convID, stream.Msg())
	}
	if err := stream.Err(); err != nil && err != io.EOF {
		c.dropStream(convID, err)
		return
	}
	// graceful server close: slot clears on the next poll resolution
	if c.cmds != nil {
		c.cmds <- func() tea.Msg { return StreamDoneMsg{ConvID: convID} }
	}
}

// Watch re-attaches to an ACKED turn's hub after a socket drop (GUI
// runWatch): replays subsequent chunks without dispatching.
func (c *Controller) Watch(convID, assistantMessageID string) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.cl.Ask.WatchTurnStream(context.Background(), connect.NewRequest(&apiv1.WatchTurnStreamRequest{
			ConversationId:     convID,
			AssistantMessageId: assistantMessageID,
		}))
		if err != nil {
			// NotFound (turn over) → poll resolves completion; not fatal.
			return nil
		}
		go c.consume(convID, stream)
		return nil
	}
}

// pollTranscript re-fetches the durable transcript (completion
// authority + reconnect refresh).
func (c *Controller) pollTranscript(convID string) tea.Cmd {
	return c.OpenConversation(convID)
}

// Poll is the exported poll hook (shell-side turn resolution).
func (c *Controller) Poll(convID string) tea.Cmd { return c.pollTranscript(convID) }

// EndStream clears a conversation's stream slot when the server closed
// the stream cleanly (consume's EOF path pushed StreamDoneMsg). The
// ListMessages poll that follows is the completion authority.
func (c *Controller) EndStream(convID string) {
	c.mu.Lock()
	if st := c.state[convID]; st != nil {
		st.streaming = false
		st.reconnecting = false
		st.pendingReplyID = ""
	}
	c.mu.Unlock()
}

// --- stream event handling -----------------------------------------------

// EventStore is where the controller appends live chunks so the view can
// render them. The shell's chat view implements it; controller tests use
// an in-memory recorder.
type EventStore interface {
	// AppendLiveItem adds one live ChatItem for the conversation (in
	// arrival order; the view applies phase grouping).
	AppendLiveItem(convID string, item ChatItem)
	// SetReconnecting flips the conversation's reconnecting banner.
	SetReconnecting(convID string, on bool)
}

// handleEvent applies one ChatStreamResponse to the conversation's slot
// and the event store. Mirrors the GUI's runStream for-await body.
func (c *Controller) handleEvent(convID string, ev *apiv1.ChatStreamResponse) {
	if ev == nil {
		return
	}
	switch e := ev.Event.(type) {
	case *apiv1.ChatStreamResponse_TurnStarted:
		c.mu.Lock()
		if st := c.state[convID]; st != nil {
			st.pendingReplyID = e.TurnStarted.GetAssistantMessageId()
			st.reconnecting = false
			st.optimisticUser = "" // the user row is now persisted
		}
		c.mu.Unlock()
	case *apiv1.ChatStreamResponse_TextChunk:
		if content := e.TextChunk.GetContent(); content != "" {
			c.appendChunk(convID, ChatItem{Kind: KindText, Text: content, At: now(), Live: true, Phase: "p-0"})
		}
	case *apiv1.ChatStreamResponse_Reasoning:
		if content := e.Reasoning.GetContent(); content != "" {
			c.appendChunk(convID, ChatItem{Kind: KindReasoning, Text: content, At: now(), Live: true, Phase: "p-0"})
		}
	case *apiv1.ChatStreamResponse_Heartbeat:
		c.mu.Lock()
		if st := c.state[convID]; st != nil && st.reconnecting {
			st.reconnecting = false
		}
		c.mu.Unlock()
	case *apiv1.ChatStreamResponse_Error:
		// reply failure: surfaced by the poll when the error row lands;
		// record for the dock notice.
		c.mu.Lock()
		c.err = "chat error: " + e.Error.GetMessage()
		c.mu.Unlock()
	case *apiv1.ChatStreamResponse_ToolCallStart:
		// retained-for-SSE surface; render as a tool row when a server
		// emits it.
		id := e.ToolCallStart.GetId()
		c.appendChunk(convID, ChatItem{Kind: KindTool, Tool: &ParsedTool{
			ID:       id,
			ToolName: e.ToolCallStart.GetFunctionName(),
			Input:    e.ToolCallStart.GetArguments(),
			At:       now(),
		}, Key: "tc-" + id})
	case *apiv1.ChatStreamResponse_ToolCallResult:
		id := e.ToolCallResult.GetToolCallId()
		c.appendChunk(convID, ChatItem{Kind: KindTool, Tool: &ParsedTool{
			ID:       id,
			ToolName: "tool",
			Output:   e.ToolCallResult.GetOutput(),
			At:       now(),
		}, Key: "tcr-" + id})
	case *apiv1.ChatStreamResponse_Done:
		// poll finalizes; nothing to append
	}
}

func (c *Controller) appendChunk(convID string, item ChatItem) {
	c.mu.Lock()
	c.seq++
	key := "st-" + itoa(int64(c.seq))
	if item.Kind == KindReasoning {
		key = "sr-" + itoa(int64(c.seq))
	}
	c.mu.Unlock()
	item.Key = key
	if c.store != nil {
		c.store.AppendLiveItem(convID, item)
	}
}

// failStream handles a pre-ack send/interject failure: when the turn was
// acked the slot goes reconnecting (the server-side collector still
// runs), otherwise it tears down and the caller's ErrMsg surfaces.
func (c *Controller) failStream(convID string, err error) {
	c.mu.Lock()
	st := c.state[convID]
	watch := ""
	if st != nil {
		if st.pendingReplyID != "" {
			st.reconnecting = true
			watch = st.pendingReplyID
		} else {
			st.streaming = false
			st.optimisticUser = ""
			st.sentText = ""
		}
	}
	c.err = err.Error()
	c.mu.Unlock()
	if watch != "" && c.cmds != nil {
		c.cmds <- c.Watch(convID, watch)
	}
}

// dropStream handles a socket drop mid-stream (stream.Err() after the
// receive loop): acked turns re-dial WatchTurnStream; the poll remains
// the completion authority either way.
func (c *Controller) dropStream(convID string, err error) {
	c.mu.Lock()
	st := c.state[convID]
	watch := ""
	if st != nil && st.streaming {
		if st.pendingReplyID != "" {
			// acked turn: the server-side collector keeps running — slot
			// stays streaming, goes reconnecting, watch re-dials the hub.
			st.reconnecting = true
			watch = st.pendingReplyID
		} else {
			// pre-ack failure: tear down (GUI fail() with no reply id).
			st.streaming = false
			st.optimisticUser = ""
			st.sentText = ""
		}
	}
	c.err = err.Error()
	c.mu.Unlock()
	if watch != "" && c.cmds != nil {
		c.cmds <- c.Watch(convID, watch)
	}
}

func now() int64 { return time.Now().UnixMilli() }
