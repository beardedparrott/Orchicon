package chat

import (
	"context"
	"errors"
	"fmt"
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
	// gen identifies the stream this slot currently belongs to. It is bumped by every SEND, and each
	// stream carries the generation it started under, so a stream that has been SUPERSEDED cannot tear
	// down the slot its successor owns.
	//
	// WITHOUT IT, AN INTERJECTION KILLED ITS OWN TURN'S STATE. Interjecting supersedes the running turn,
	// which closes the OLD stream cleanly — that close arrives as StreamDoneMsg and cleared `streaming`
	// and `pendingReplyID` for the conversation, i.e. for the NEW turn the operator had just started. The
	// visible effects are exactly the reported ones: the thinking indicator and the watchdog vanished the
	// moment the interjection was accepted, the poll fell to the non-streaming REPLACE path and discarded
	// the live reply, and the transcript could keep a half-merged copy of both turns. A generation turns
	// "a stream ended" into "THIS stream ended", which is the fact the slot actually needs.
	gen uint64
	// optimisticUser is the not-yet-persisted sent text (rendered
	// immediately; replaced by the transcript row once ListMessages
	// returns it).
	optimisticUser string
	sentText       string
	// lastActivity is the UnixMilli of the most recent event from the stream — a text chunk, a
	// reasoning chunk, a tool event or a HEARTBEAT. The liveness watchdog reads it (see
	// runLivenessWatch); without it, a stream whose socket died silently is indistinguishable
	// from a turn that is simply thinking.
	lastActivity int64
}

// askStreamStallTimeout is how long a streaming turn may go with NO event at all before the
// stream is declared dead and re-dialled.
//
// THE SERVER SENDS A HEARTBEAT EVERY 15 SECONDS (askHeartbeatInterval), so a live stream is never
// quiet for anything close to this. Two intervals plus slack means one missed heartbeat does not
// trip it, while a genuinely dead socket is caught in well under a minute. Without this the TUI
// waited forever — see runLivenessWatch for the failure that produced it.
const askStreamStallTimeout = 40 * time.Second

// askLivenessCheckInterval is how often the watchdog looks. Small relative to the timeout so the
// re-dial happens promptly once the turn is stale, and cheap enough to be irrelevant (it reads one
// int64 under a mutex).
const askLivenessCheckInterval = 5 * time.Second

// askTurnPollInterval is how often a STREAMING turn re-reads its durable transcript.
//
// THIS IS THE ROBUST HALF OF LIVE STREAMING, and it exists because the live half cannot be trusted
// on its own. The server mirrors the running turn's collected text, reasoning and tool ledger into
// the ACKED assistant message every 250ms (askorchicon.upsertPartialMessage — the mechanism built so
// "a client that lost the live stream (refresh, another tab/device) polls ListMessages and watches
// the reply grow"). A poll DURING the turn therefore renders the same progress the socket would
// deliver, over a plain unary RPC that cannot be half-open, cannot be buffered mid-body by a proxy,
// and cannot go quiet without an error.
//
// The operator, on a stream that delivered heartbeats but no reply: "I don't even care if you have to
// find a brand new method." This is that method — and it is not a fallback bolted on beside the
// stream: both feed the SAME merge (mergeHistory), so whichever arrives first paints, and a turn
// where either path is broken still streams through the other.
//
// One second: the mirror flushes at 250ms, so this is four mirror generations per poll — the reply
// reads as live rather than as a slideshow. The cost is one bounded ListMessages a second while a
// turn is in flight, for one conversation.
const askTurnPollInterval = time.Second

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

	// pendingModel / pendingMode are the model + persona a NEW conversation
	// is created with (the Ask-model picker + /mode). The Ask API has no
	// update-model RPC, so a model change applies to the next new
	// conversation (model_ref on CreateConversation) — matching the GUI's
	// New chat, which also only sets model_ref at create time.
	pendingModel string
	pendingMode  apiv1.ConversationMode

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
	ModelRef  string
	Mode      apiv1.ConversationMode
	// ProjectID is the project this conversation belongs to, or "" when unassigned (the API's empty string, kept
	// as-is rather than normalized into a sentinel so the rail and the GUI agree on what "unassigned" looks like).
	//
	// It is what the rail GROUPS BY: the operator's "second higher level in organization for conversations ...
	// associated with Projects in a parent category". A plain field on the row rather than a map beside it,
	// because every site that renders a conversation needs it and a parallel map is one more thing that can
	// disagree with the list it describes.
	ProjectID string
	// PendingReplyID is the acked assistant message id of a turn the SERVER reports as still running, or "".
	//
	// IT IS WHAT LETS THIS CLIENT RE-ATTACH TO ITS OWN TURN, which is the reported bug: "When I leave an chat
	// and go back into it, it loses the 'orchicon is thinking...' and watchdog." The server-side turn registry
	// is the authority and it survives leaving the pane (and a refresh, and another device); without this field
	// the shell had nothing to restore FROM, so re-entering a conversation mid-turn showed an idle pane with a
	// reply that was quietly still being written. turn_in_flight alone is not enough — the Watch RPC needs the
	// assistant message id, which is what makes the re-attach address THIS turn and not a stale one.
	PendingReplyID string
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
	// Categories and Assignments are the CONVERSATION groupings the SAME response carried.
	//
	// They are here because the list is the freshest place the rail can learn its groupings from: the
	// shell's cache is loaded once at startup, so a grouping created in the GUI while the TUI runs never
	// reached the rail. Passing them through the message also keeps the read on the MAIN LOOP, where the
	// cache lives — the fetch itself runs on a goroutine.
	Categories  []*apiv1.Category
	Assignments []*apiv1.CategoryAssignment
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

// ConversationCreatedMsg carries a freshly created conversation (the GUI's
// CreateConversation). ConvID is empty when Err is set.
type ConversationCreatedMsg struct {
	ConvID   string
	ModelRef string
	Err      string
}

// ConversationProjectSetMsg reports the outcome of a conversation's project change
// (SetConversationProject) so the shell can reconcile the rail's project grouping.
type ConversationProjectSetMsg struct {
	ID        string
	ProjectID string
	Err       string
}

// ConversationMutatedMsg reports the outcome of a conversation write
// (rename / delete / mode change) so the shell can reconcile the rail.
type ConversationMutatedMsg struct {
	Op  string // "rename" | "delete" | "mode" | "compact"
	ID  string
	Err string
	// Detail is the outcome text for ops that report one (compact): a one-line,
	// user-facing explanation shown verbatim, including WHY compaction declined
	// and the measured context size around it when the server reported one.
	Detail string
}

// StreamDoneMsg marks the goroutine forwarding loop ended (exported —
// the shell's dispatch consumes it). Gen is the generation of the stream that ended, so a stream
// superseded by an interjection cannot clear the slot of the turn that replaced it.
type StreamDoneMsg struct {
	ConvID string
	Gen    uint64
}

func (s StreamDoneMsg) isMsg() {}

// AbortTurnMsg reports the outcome of a Stop (the composer's ctrl+y) so the shell can settle the
// composer's affordance row and repaint the transcript. ConvID is echoed so a stale abort for a
// conversation the operator has since left cannot retitle the pane.
type AbortTurnMsg struct {
	ConvID string
	Err    string
}

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

// SetPendingModel records the model a NEW conversation is created with.
func (c *Controller) SetPendingModel(ref string) {
	c.mu.Lock()
	c.pendingModel = ref
	c.mu.Unlock()
}

// PendingModel returns the model new conversations are created with.
func (c *Controller) PendingModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingModel
}

// SetPendingMode records the persona a NEW conversation is created with.
func (c *Controller) SetPendingMode(mode apiv1.ConversationMode) {
	c.mu.Lock()
	c.pendingMode = mode
	c.mu.Unlock()
}

// PendingMode returns the persona new conversations are created with.
func (c *Controller) PendingMode() apiv1.ConversationMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingMode
}

// ModeNames lists the persona spellings the operator can type, in the order the
// help text shows them. ONE list, so the `/mode` help, the error message and the
// parser cannot drift apart — the failure mode this codebase keeps producing.
//
// The order is the escalation of agency: think it through, do it with me, or
// dispatch it.
func ModeNames() []string { return []string{"brainstorm", "iteration", "quick_work"} }

// ParseMode maps a user-typed persona to the proto enum (case-insensitive).
//
// "quick work" and "quick_work" both parse: the enum's own name is QUICK_WORK, the
// label is "Quick Work", and an operator typing it into a chat box will use
// whichever of those feels natural. Rejecting one of them for a spelling the UI
// itself uses would be a spelling test, which is exactly what the mode command is
// meant to avoid.
func ParseMode(s string) (apiv1.ConversationMode, bool) {
	norm := strings.ToLower(strings.TrimSpace(s))
	// Collapse the separators so "quick work", "quick_work", "quick-work" and
	// "quickwork" all reach the same mode.
	norm = strings.NewReplacer(" ", "_", "-", "_").Replace(norm)
	switch norm {
	case "brainstorm":
		return apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM, true
	case "iteration":
		return apiv1.ConversationMode_CONVERSATION_MODE_ITERATION, true
	case "quick_work", "quickwork":
		return apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK, true
	}
	return apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED, false
}

// CreateConversation creates a conversation with the given model + persona
// (the GUI's New chat: model_ref + mode are create-time fields).
//
// projectID places it in a project from birth — the TUI's "create in this project folder". An empty id creates
// an unassigned conversation, which is what every other caller wants.
func (c *Controller) CreateConversation(modelRef string, mode apiv1.ConversationMode, initialMessage, projectID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
			ModelRef:       modelRef,
			InitialMessage: initialMessage,
			Mode:           mode,
			ProjectId:      projectID,
		}))
		if err != nil {
			return ConversationCreatedMsg{Err: err.Error()}
		}
		cv := resp.Msg.GetConversation()
		return ConversationCreatedMsg{ConvID: cv.GetId(), ModelRef: cv.GetModelRef()}
	}
}

// SetConversationProject moves a conversation into a project (or unassigns it with an
// empty id) — SetConversationProject. It is what the rail's /project command and its
// create-in-folder gesture both resolve to, so the TUI cannot disagree with the GUI
// about what "belongs to a project" means.
func (c *Controller) SetConversationProject(id, projectID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.SetConversationProject(ctx, connect.NewRequest(&apiv1.SetConversationProjectRequest{
			Id:        id,
			ProjectId: projectID,
		}))
		if err != nil {
			return ConversationProjectSetMsg{ID: id, Err: err.Error()}
		}
		return ConversationProjectSetMsg{ID: id, ProjectID: resp.Msg.GetConversation().GetProjectId()}
	}
}

// RenameConversation persists a conversation title (UpdateConversationTitle).
func (c *Controller) RenameConversation(id, title string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := c.cl.Ask.UpdateConversationTitle(ctx, connect.NewRequest(&apiv1.UpdateConversationTitleRequest{
			Id:    id,
			Title: title,
		}))
		if err != nil {
			return ConversationMutatedMsg{Op: "rename", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{Op: "rename", ID: id}
	}
}

// CanCompact reports whether the conversation may be compacted right now.
// Empty string means yes; otherwise it returns the operator-facing reason it
// must not.
//
// The one refusal: a turn is in flight. Compaction rewrites the very history the
// running turn is generating from, so compacting underneath it would corrupt an
// answer in progress. This lives on the controller (not the slash command) so the
// policy is testable and any future caller inherits it.
func (c *Controller) CanCompact(convID string) string {
	if convID == "" {
		return "no conversation open — /new or pick one from the rail"
	}
	if c.IsStreaming(convID) {
		return "a turn is in flight — stop it (esc) before /compact (compaction rewrites the history the turn is using)"
	}
	return ""
}

// CompactConversation compacts the conversation's accumulated context
// (CompactConversation) so a long session can continue inside the model's
// context window. The server owns the policy: it declines (compacted=false with
// a reason in Detail) when the conversation is too short to be worth a lossy
// collapse, and returns the MEASURED context sizes when it did compact. This is
// a synchronous RPC — compaction is one bounded operation, so there is no stream
// and no turn is dispatched.
func (c *Controller) CompactConversation(id string) tea.Cmd {
	return func() tea.Msg {
		// Compaction may run a summarize model turn, so the deadline is
		// deliberately generous (the same 2-minute class as a summarize call)
		// rather than the 20s used by the pure-CRUD conversation writes.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		resp, err := c.cl.Ask.CompactConversation(ctx, connect.NewRequest(&apiv1.CompactConversationRequest{
			ConversationId: id,
			Reason:         "manual",
		}))
		if err != nil {
			return ConversationMutatedMsg{Op: "compact", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{
			Op:     "compact",
			ID:     id,
			Detail: compactionDetail(resp.Msg),
		}
	}
}

// compactionDetail renders a CompactConversationResponse for the dock notice:
// the server's own detail line, plus the measured context sizes when it reported
// them (0 means "unknown" and is omitted rather than shown as zero — the server
// never estimates, so a zero here would read as a real measurement of nothing).
func compactionDetail(r *apiv1.CompactConversationResponse) string {
	if r == nil {
		return ""
	}
	detail := r.GetDetail()
	if detail == "" {
		if r.GetCompacted() {
			detail = "conversation compacted"
		} else {
			detail = "nothing to compact"
		}
	}
	if r.GetContextTokensBefore() > 0 {
		detail += fmt.Sprintf(" (%d → %d tokens)", r.GetContextTokensBefore(), r.GetContextTokensAfter())
	}
	return detail
}

// DeleteConversation deletes a conversation and all its messages.
func (c *Controller) DeleteConversation(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := c.cl.Ask.DeleteConversation(ctx, connect.NewRequest(&apiv1.DeleteConversationRequest{Id: id}))
		if err != nil {
			return ConversationMutatedMsg{Op: "delete", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{Op: "delete", ID: id}
	}
}

// SetConversationMode switches the conversation's persona (SetConversationMode).
// The change applies from the NEXT message on — no session change.
func (c *Controller) SetConversationMode(id string, mode apiv1.ConversationMode) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := c.cl.Ask.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
			Id:   id,
			Mode: mode,
		}))
		if err != nil {
			return ConversationMutatedMsg{Op: "mode", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{Op: "mode", ID: id}
	}
}

// SetConversationModel retargets an OPEN conversation's model_ref
// (SetConversationModel). The change applies from the NEXT message; an empty
// ref CLEARS the per-conversation override so the tenant default applies.
// This is the /models write path — it is what lets the operator retarget a
// chat in place instead of abandoning it for a new one.
func (c *Controller) SetConversationModel(id, modelRef string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := c.cl.Ask.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{
			Id:       id,
			ModelRef: modelRef,
		}))
		if err != nil {
			return ConversationMutatedMsg{Op: "model", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{Op: "model", ID: id}
	}
}

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
				ModelRef:  cv.GetModelRef(),
				Mode:      cv.GetMode(),
				ProjectID: cv.GetProjectId(),
				// Read at list time, so a conversation the server reports as mid-turn is recognisable as such the
				// moment the rail loads — which is what the re-attach on open needs.
				PendingReplyID: cv.GetPendingAssistantMessageId(),
			})
		}
		return ConversationsMsg{Convs: convs, Categories: resp.Msg.GetCategories(), Assignments: resp.Msg.GetAssignments()}
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
		// The page arrives NEWEST-first (see conversationItems) — it is reversed
		// there, together with the millisecond timestamps.
		return TranscriptMsg{ConvID: id, Items: GroupByPhase(conversationItems(resp.Msg.GetMessages()))}
	}
}

// conversationItems converts a ListMessages page into transcript items in
// CHRONOLOGICAL (oldest-first) order.
//
// The server returns messages NEWEST-first — db.ListMessages orders by
// created_at DESC and askorchicon.Service passes those rows straight through —
// so the page MUST be reversed here. The GUI does exactly this
// (frontend/src/api/askOrchicon.ts: "(res.messages ?? []).reverse()"); the TUI
// skipped it and rendered the whole transcript INVERTED, putting the model's
// reply above the operator's message.
//
// Timestamps are taken at MILLISECOND precision. created_at is truncated to
// whole seconds by GetSeconds(), which tied messages written in the same second
// together and left a stable sort unable to separate them — so reversal here is
// load-bearing, not merely cosmetic.
//
// REASONING IS CARRIED HERE TOO, and its absence was the operator's "No reasoning
// block" report — which I initially and wrongly told them was not happening.
// ChatMessage.reasoning is a `repeated string` the proto documents as "rendered by
// the frontend as thinking bubbles", and the GUI reads it; this function read only
// GetContent(), so EVERY durable reasoning part was dropped the moment the
// transcript loaded from history. The live stream chunks were the only path that
// ever produced a reasoning item, and the completion poll then REPLACES the live
// buffer with this durable list (chatStore.replace) — so the reasoning the operator
// had watched arrive disappeared the instant the turn finished. That is exactly "All
// I am seeing is 'Orchicon is thinking...'": the thinking NOTICE is live and
// survives, while the reasoning body does not.
//
// ONE ITEM PER PART, the shape the proto preserves ("one entry per reasoning part
// received that turn, boundaries preserved") and the shape the live path already
// produces, so GroupByPhase coalesces them into one bubble per phase exactly as it
// does for a streamed turn. They are emitted BEFORE the message's text because
// thinking precedes the words it produced, and they share the message's timestamp:
// SortChronologically is a STABLE sort, so same-instant order is preserved.
//
// Keys come from the message id and the part index, NOT from a counter: a fold's
// state is held per item key (chatStore foldedReasoning), so a key that changed on
// every load would spring a collapsed reasoning block open on every refresh.
func conversationItems(msgs []*apiv1.ChatMessage) []ChatItem {
	items := make([]ChatItem, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		kind := KindText
		switch strings.ToLower(m.GetRole()) {
		case "user":
			kind = KindUser
		case "error":
			kind = KindError
		}
		at := m.GetCreatedAt().AsTime().UnixMilli()
		for j, part := range m.GetReasoning() {
			// A blank part is not a reasoning block, and rendering it would put an empty
			// bubble in the transcript for a turn that had nothing to say there.
			if strings.TrimSpace(part) == "" {
				continue
			}
			items = append(items, ChatItem{
				Kind: KindReasoning,
				Text: part,
				At:   at,
				Key:  "m-" + m.GetId() + "-r" + itoa(int64(j)),
			})
		}
		items = append(items, ChatItem{
			Kind: kind,
			Text: m.GetContent(),
			At:   at,
			Key:  "m-" + m.GetId(),
		})
	}
	return items
}

// Send dispatches a turn: InterjectConversationTurn when the
// conversation already has an in-flight turn (supersede), ChatStream
// otherwise — exactly the GUI's mode selection. The returned Cmd starts
// the event-forwarding goroutine. contextPreamble is the bracketed
// context line prepended to the message (the only context shape the
// API accepts).
func (c *Controller) Send(convID, text, contextPreamble string) tea.Cmd {
	return c.SendWithAttachments(convID, text, contextPreamble, nil)
}

// SendWithAttachments is Send with file/image attachments for the turn.
//
// An attachment is BYTES on the wire (AttachmentInput.data), and it is converted to the request message
// HERE — the same place every other field of the turn is built — so the two send paths (a fresh send and
// an interject/supersede) cannot disagree about how a turn is shaped. The server does the rest: it
// validates the caps, persists them with the message, describes them in the system prompt, and forwards
// each as an OpenCode FilePartInput (a data: URL), which is how a screenshot becomes a vision part.
func (c *Controller) SendWithAttachments(convID, text, contextPreamble string, files []*apiv1.AttachmentInput) tea.Cmd {
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
	// ARM THE WATCHDOG AND THE DURABLE POLL HERE — NOT INSIDE THE STREAM'S COMMAND.
	//
	// THIS IS THE FIX, and it explains why the poll did not help when it was first added. Both
	// goroutines used to be armed inside startStream's command, AFTER `ChatStream` returned — so they
	// started only if the streaming call actually OPENED. And that call is precisely what fails: it
	// blocks until the server's response headers arrive, and if a proxy buffers the streaming response
	// those headers never come, so it blocks forever. The result was the operator's exact report:
	// `streaming` is set true above and stays true, so the pane shows the thinking notice and its
	// watchdog age and NOTHING ELSE, while `consume` never runs and NO mechanism that could save it —
	// the poll included — was ever started. Every safety net was gated behind the failure it existed to
	// cover.
	//
	// Armed here, at the moment the operator SENT, both run regardless of the transport. The poll then
	// drives rendering from the durable store — the reply still grows on screen even if the stream
	// never opens a socket at all — and the watchdog's clock is stamped BY THE SEND, so a handshake
	// that never completes is caught as silence rather than waiting forever on a stream that will never
	// start.
	//
	// (Watch re-arms both, for the same reason: a re-dial is a NEW attempt whose liveness must be
	// watched from the re-dial, not from the original send.)
	c.mu.Lock()
	if st := c.state[convID]; st != nil {
		st.lastActivity = now()
		// A NEW GENERATION FOR EVERY SEND: the stream this command is about to open owns the slot from
		// here, so any stream already running belongs to the previous generation and its end must be
		// ignored (see convState.gen).
		st.gen++
	}
	gen := c.state[convID].gen
	c.mu.Unlock()
	go c.runLivenessWatch(convID, gen)
	go c.runTurnPoll(convID)
	return c.startStream(convID, full, call, files, gen)
}

// startStream opens the ChatStream/InterjectConversationTurn and returns
// a Cmd whose first message is a synthetic turnStarted placeholder (the
// actual ack arrives via the forwarding goroutine). — gen is the slot generation this stream owns.
func (c *Controller) startStream(convID, full, call string, files []*apiv1.AttachmentInput, gen uint64) tea.Cmd {
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
				Attachments:    files,
			}))
		} else {
			stream, err = c.cl.Ask.ChatStream(ctx, connect.NewRequest(&apiv1.ChatStreamRequest{
				ConversationId: convID,
				Message:        full,
				Attachments:    files,
			}))
		}
		if err != nil {
			c.failStream(convID, err)
			return ErrMsg{Where: call, Err: err}
		}
		go c.consume(convID, gen, stream)
		// RE-ARM BOTH, and HERE it is correct to arm after the call returns: this is a RE-DIAL of a turn
		// that was already acknowledged, so a failed re-dial is already covered by the poll running since
		// the original send (see SendWithAttachments). Arming on success keeps each liveness watch tied
		// to an attempt that actually started.
		c.mu.Lock()
		if st := c.state[convID]; st != nil {
			st.lastActivity = now()
		}
		c.mu.Unlock()
		go c.runLivenessWatch(convID, gen)
		go c.runTurnPoll(convID)
		return nil
	}
}

// consume forwards stream events into the tea channel via the returned
// messages (bubbletea re-dispatches what the Cmd returns; extra events
// ride the channel the program gave us at NewController time).
//
// gen is the slot generation this stream belongs to, and it is carried into every terminal outcome
// below so a stream that was SUPERSEDED mid-flight (an interjection) cannot clear or tear down the
// slot the replacement turn owns.
func (c *Controller) consume(convID string, gen uint64, stream *connect.ServerStreamForClient[apiv1.ChatStreamResponse]) {
	for stream.Receive() {
		c.handleEvent(convID, stream.Msg())
	}
	if err := stream.Err(); err != nil && err != io.EOF {
		c.dropStream(convID, gen, err)
		return
	}
	// graceful server close: the slot clears on the next poll resolution — unless this stream has
	// already been superseded, in which case the slot belongs to someone else and must be left alone.
	if c.cmds != nil {
		c.cmds <- func() tea.Msg { return StreamDoneMsg{ConvID: convID, Gen: gen} }
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
		go c.consume(convID, c.CurrentGen(convID), stream)
		// The watchdog and the durable poll are NOT armed here — see SendWithAttachments. Arming them at
		// this point would gate them behind this call returning, which is the failure they exist to cover.
		return nil
	}
}

// currentGen reads the slot's generation without bumping it. A RE-DIAL (Watch) belongs to the turn it
// is re-attaching to, so it must adopt that generation rather than mint a new one — minting one would
// make the live turn's own stream look superseded.
//
// Exported because the SHELL needs it too: EndStream is generation-guarded, so any caller outside this
// package that legitimately ends the current turn (a Stop, or a test driving this path) has to name the
// generation it means rather than leaving it implicit.
func (c *Controller) CurrentGen(convID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.state[convID]; st != nil {
		return st.gen
	}
	return 0
}

// Reattach restores the stream slot for a turn the SERVER reports as running, when this client has no live
// stream for it. It returns the Watch command (nil when there was nothing to re-attach to).
//
// THE OPERATOR: "When I leave an chat and go back into it, it loses the 'orchicon is thinking...' and
// watchdog." Leaving the pane does not stop the turn — the collector runs on the server and mirrors its
// progress into the acked assistant row every 250ms — but the CLIENT's slot is what drives the thinking
// notice, the watchdog age and the completion poll, and nothing restored it. Returning to the conversation
// therefore looked like an idle chat whose reply happened to appear later.
//
// A LIVE LOCAL SLOT ALWAYS WINS. Server state only fills a gap, exactly as the GUI's re-attach effect does: a
// turn started in this client keeps its own stream until it completes, and re-attaching would replace a
// working stream with a second one.
//
// gen is NOT bumped. This is not a new send — it adopts the generation of the turn it is re-attaching to, so
// the live turn's own stream (if any arrives later) is not treated as superseded.
func (c *Controller) Reattach(convID, pendingReplyID string) tea.Cmd {
	if convID == "" || pendingReplyID == "" {
		return nil
	}
	c.mu.Lock()
	st := c.state[convID]
	if st == nil {
		st = &convState{}
		c.state[convID] = st
	}
	if st.streaming {
		c.mu.Unlock()
		return nil // a live local stream is authoritative over the server's flag
	}
	st.streaming = true
	st.reconnecting = true
	st.pendingReplyID = pendingReplyID
	st.lastActivity = now()
	gen := st.gen
	c.mu.Unlock()
	// ARM BOTH, for the same reason the send path does: the thinking notice and the stall detection have to
	// run for a turn this client did not start, and the poll is what actually paints the reply — the Watch
	// stream is the bonus.
	go c.runLivenessWatch(convID, gen)
	go c.runTurnPoll(convID)
	return c.Watch(convID, pendingReplyID)
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
//
// gen MUST MATCH THE SLOT'S. This is the guard that makes interjection work: the superseded turn's
// stream also closes cleanly, and without the check that close would clear the slot belonging to the
// turn that replaced it — killing the thinking indicator, the watchdog and the pending-reply id the
// moment the operator interjected.
func (c *Controller) EndStream(convID string, gen uint64) {
	c.mu.Lock()
	if st := c.state[convID]; st != nil && st.gen == gen {
		st.streaming = false
		st.reconnecting = false
		st.pendingReplyID = ""
	}
	c.mu.Unlock()
}

// AbortTurn stops the in-flight turn on a conversation: the TUI's Stop control (the composer's ctrl+y),
// the counterpart of the GUI's Ask-page Stop button, which calls this same RPC
// (ask-orchicon.tsx: handleStopStreaming -> useAbortConversationTurn -> abortTurn).
//
// It does the two things the GUI's handler does, and both matter:
//
//  1. ABORT THE SERVER-SIDE TURN. AbortConversationTurn cancels the registered session; it is
//     IDEMPOTENT on the server (a second call against an already-stopped turn succeeds — see the audit
//     service's own test), so a double-press cannot raise a spurious error.
//  2. CLEAR THE LOCAL TURN SLOT IMMEDIATELY. This is what makes Stop feel instant: the UI recovers at
//     once instead of waiting for the socket to notice the cancellation. The live partial reply is not
//     lost — the durable transcript is the completion authority, and the stream's own end drives the
//     poll that reconciles it (onStreamDone), exactly as the GUI's conversations-list invalidate does.
//
// The optimistic user echo is deliberately NOT cleared (the GUI clears its copy because it drops the
// whole local transcript). Here the echo is load-bearing: mergeHistory matches on it so the durable poll
// cannot render the operator's own message twice — and the message WAS persisted, because the turn had
// started.
func (c *Controller) AbortTurn(convID string) tea.Cmd {
	if convID == "" {
		return nil
	}
	return func() tea.Msg {
		_, err := c.cl.Ask.AbortConversationTurn(context.Background(),
			connect.NewRequest(&apiv1.AbortConversationTurnRequest{ConversationId: convID}))
		if err != nil {
			return AbortTurnMsg{ConvID: convID, Err: err.Error()}
		}
		c.EndStream(convID, c.CurrentGen(convID))
		c.clearReconnecting(convID)
		return AbortTurnMsg{ConvID: convID}
	}
}

// clearReconnecting drops the EVENT STORE's reconnect banner for a conversation. EndStream clears the
// controller's own flag; the store's is a separate surface (the chat view's banner), and a stop that
// left the banner up would report a connection loss that is no longer true.
func (c *Controller) clearReconnecting(convID string) {
	c.mu.Lock()
	store := c.store
	c.mu.Unlock()
	if store != nil {
		store.SetReconnecting(convID, false)
	}
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
	// EVERY EVENT STAMPS THE CLOCK, including the ones that carry no content. This is the input the
	// liveness watchdog reads, and a heartbeat is exactly as much evidence of a live stream as a
	// text chunk is — arguably more, since it is the only event a silently-thinking model emits.
	c.mu.Lock()
	if st := c.state[convID]; st != nil {
		st.lastActivity = now()
	}
	c.mu.Unlock()
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
//
// gen GUARDS THE TEARDOWN. A drop from a stream that has already been superseded must not touch the
// slot: the pre-ack branch below CLEARS `streaming`, which for a superseding interjection would end
// the turn the operator had just started. The acked branch is guarded too, for the same reason — its
// Watch would re-attach to the OLD turn's assistant id, which no longer exists.
func (c *Controller) dropStream(convID string, gen uint64, err error) {
	c.mu.Lock()
	st := c.state[convID]
	if st == nil || st.gen != gen {
		c.mu.Unlock()
		return // a superseded stream's failure is not this slot's business
	}
	watch := ""
	if st.streaming {
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

// runTurnPoll keeps a streaming turn's transcript fresh from the DURABLE store, on a timer, until
// the turn ends.
//
// WHY THIS EXISTS RATHER THAN TRUSTING THE STREAM: the operator's evidence was a pane that showed
// heartbeats — the watchdog's age kept resetting — but never the reply, because the socket was
// delivering only the keepalives. Nothing about that is diagnosable from the client side, and it does
// not need to be: the server mirrors the partial reply into the assistant message every 250ms, so a
// unary poll renders the same content. Independence from the stream IS the fix.
//
// IT PUSHES A COMMAND rather than emitting a message directly, because the shell owns the tea loop:
// pollTranscript returns a tea.Cmd and the shell runs it exactly as it runs the completion poll. The
// non-blocking send matches the wake channel's contract — a full channel means the shell is busy, and
// the next tick tries again.
//
// It stops as soon as the slot is no longer streaming, so a finished turn costs nothing and the
// goroutine cannot outlive the turn it serves. Dropping a POLL is harmless in a way dropping a chunk
// is not: the next one returns a superset of this one.
func (c *Controller) runTurnPoll(convID string) {
	ticker := time.NewTicker(askTurnPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		st := c.state[convID]
		streaming := st != nil && st.streaming
		c.mu.Unlock()
		if !streaming || c.cmds == nil {
			return
		}
		select {
		case c.cmds <- c.pollTranscript(convID):
		default:
		}
	}
}

// runLivenessWatch is the CLIENT-SIDE stream watchdog, and its absence was the streaming report.
//
// THE OPERATOR'S EVIDENCE, side by side: the GUI showed the reply streaming, a reasoning block, and
// the banner "Connection interrupted — still working… Output continues below." with "Last activity 1s
// ago" beneath it. The TUI, in the same turn, showed nothing but "Orchicon is thinking…" — until the
// conversation was re-entered, which runs a fresh ListMessages and paints the durable reply.
//
// WHAT WAS HAPPENING. A connection through the container network can die HALF-OPEN: no FIN reaches
// the client, so `stream.Receive()` blocks indefinitely. There is no error and no EOF, so consume()
// never returns, dropStream never runs, and the slot stays `streaming` forever. Nothing retries —
// the re-dial path exists (dropStream -> Watch) but only a RETURNING stream could reach it. The turn
// itself completes server-side, so the durable reply is there for the next load, which is exactly
// why re-entering the pane "fixed" it.
//
// THE SIGNAL WAS ALREADY ARRIVING AND SIMPLY UNUSED: the server sends a Heartbeat every 15 seconds,
// and the TUI RECEIVED them — handleEvent's Heartbeat case only cleared a flag. A heartbeat IS the
// server saying "this stream is alive", so its absence is the death signal. This watchdog turns that
// into the re-dial the GUI's own footer performs.
//
// It runs as a goroutine rather than a tea.Cmd chain on purpose: it must be checking even while the
// shell is idle waiting on the stream, which is precisely the state being detected — a timer that
// had to be re-armed by messages would be re-armed by the very messages that are not arriving.
//
// SILENCE IS ONLY A FAULT WHILE STREAMING, and the check stops as soon as the slot is not. That is
// also why it cannot re-dial a COMPLETED turn: completion clears `streaming` (EndStream, on the poll
// resolution) before the timeout could fire.
//
// gen IS THE GENERATION IT WATCHES. The watch belongs to ONE send, so it stops when the slot has moved
// on instead of reporting on whatever turn happens to be current. Without that, a watch armed for a turn
// that was superseded (an interjection) would eventually judge the NEW turn's slot stale and tear it
// down — the watchdog causing the very failure it exists to prevent.
func (c *Controller) runLivenessWatch(convID string, gen uint64) {
	ticker := time.NewTicker(askLivenessCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		stop, stale := c.livenessCheckFor(convID, gen)
		if stop {
			return
		}
		if stale {
			c.dropStream(convID, gen, errStreamStalled)
			return
		}
	}
}

// livenessCheck reports (stop, stale) for one tick: stop when there is nothing left to watch,
// stale when the stream has gone quiet past the timeout.
//
// SEPARATE FROM THE LOOP so the DECISION is testable without waiting on real timers — 40 seconds
// per case is not a test, and the decision is the whole behaviour.
func (c *Controller) livenessCheck(convID string) (stop, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return livenessVerdict(c.state[convID])
}

// livenessCheckFor is livenessCheck scoped to the generation that ARMED the watch: a slot that has moved
// on belongs to a newer turn and this watch has nothing left to say about it.
func (c *Controller) livenessCheckFor(convID string, gen uint64) (stop, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state[convID]
	if st != nil && st.gen != gen {
		return true, false // superseded: this watch's turn is over
	}
	return livenessVerdict(st)
}

// livenessVerdict is the shared judgement, so the generation-aware and -agnostic paths cannot disagree.
func livenessVerdict(st *convState) (stop, stale bool) {
	if st == nil || !st.streaming {
		return true, false // the turn is over, stopped, or the slot is gone
	}
	// lastActivity == 0 means the stream has not delivered its first event yet — that is "not
	// started", not "silent", so it is not given the watchdog's verdict. A turn torn down during
	// its own handshake would be the watchdog causing the very failure it exists to fix.
	if st.lastActivity == 0 {
		return false, false
	}
	return false, now()-st.lastActivity > askStreamStallTimeout.Milliseconds()
}

// streamIsStale answers the watchdog's question in one call, for tests that want only the verdict.
func (c *Controller) streamIsStale(convID string) bool {
	_, stale := c.livenessCheck(convID)
	return stale
}

// SilenceSince reports how long the conversation's stream has been quiet — the gap since the most
// recent event from the server (a text chunk, a reasoning chunk, a tool event or a HEARTBEAT).
//
// It returns 0 when there is nothing to report: no slot, not streaming, or no event yet. Zero means
// "no information", never "silent forever", so a caller cannot mistake an unstarted stream for a
// stalled one — the same distinction livenessCheck makes.
//
// THIS IS THE READ SIDE OF THE WATCHDOG. The watchdog acts on silence; this reports it, so the pane
// can say that a turn is still alive and how fresh its last event was. Without it the operator has
// two indistinguishable states — working and wedged — which is the whole difficulty of a streaming
// failure from the outside.
func (c *Controller) SilenceSince(convID string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state[convID]
	if st == nil || !st.streaming || st.lastActivity == 0 {
		return 0
	}
	d := now() - st.lastActivity
	if d < 0 {
		return 0 // a clock that stepped backwards is not a silence
	}
	return time.Duration(d) * time.Millisecond
}

// errStreamStalled reports a stream that stopped sending anything. It is not a failure the operator
// sees as one: dropStream treats an acked turn as RECONNECTING (the server-side collector is still
// running), so the pane shows the connection banner and re-attaches — the same outcome as a socket
// that broke loudly.
var errStreamStalled = errors.New("stream stalled: no event within the liveness timeout")

func now() int64 { return time.Now().UnixMilli() }
