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
	"github.com/beardedparrott/orchicon/internal/toolclass"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// controller.go — the TUI port of the GUI's Ask Orchicon streaming client
// (frontend/src/routes/ask-orchicon.tsx runStream/runWatch + frontend/src/api/askOrchicon.ts).

// IsAuthExpired reports whether err is a Connect UNAUTHENTICATED (401 — the re-auth prompt shape
// every dock RPC failure funnels through).
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
	// gen identifies the stream this slot currently belongs to.
	gen uint64
	// optimisticUser is the not-yet-persisted sent text (rendered immediately; replaced by the
	// transcript row once ListMessages returns it).
	optimisticUser string
	sentText       string
	// attempt is the turn the slot's CURRENT stream attempt was opened for.
	attempt streamAttempt
	// authDead records that this conversation's last transcript read was rejected as UNAUTHENTICATED.
	authDead bool
	// lastActivity is the UnixMilli of the most recent event from the stream — a text chunk, a
	// reasoning chunk, a tool event or a HEARTBEAT.
	lastActivity int64
	// serverTimeMs and serverRecvMonoMs are the SERVER's clock, and the moment THIS client heard it,
	// captured together from Heartbeat.server_time_unix_ms.
	serverTimeMs     int64
	serverRecvMonoMs int64
}

// askStreamStallTimeout is how long a streaming turn may go with NO event at all before the stream
// is declared dead and re-dialled.
const askStreamStallTimeout = 40 * time.Second

// askLivenessCheckInterval is how often the watchdog looks.
const askLivenessCheckInterval = 5 * time.Second

// askTurnPollInterval is how often a STREAMING turn re-reads its durable transcript.
const askTurnPollInterval = time.Second

// Controller owns Ask Orchicon conversation state + streaming for the TUI.
type Controller struct {
	cl  *client.Clients
	reg Registrar // nilable; subscription re-arm hook

	mu     sync.Mutex
	convs  []Conversation
	state  map[string]*convState // per active-conversation slot
	active string
	err    string // last dock-visible error ("" = none)

	// pendingModel / pendingMode are the model + persona a NEW conversation is created with (the
	// Ask-model picker + /mode).
	pendingModel string
	pendingMode  apiv1.ConversationMode

	seq int // chunk key sequence (st-/sr- keys like the GUI)

	// store receives live chunks (the chat view); cmds receives follow-up tea.Cmds produced by
	// goroutines (watch re-dials).
	store EventStore
	cmds  chan tea.Cmd
}

// Bind wires the controller to the view's event store and the shell's cmd sink.
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
	// Fullsend is whether this conversation is in FULLSEND: the operator has waived the permission
	// PROMPT for it.
	Fullsend bool
	// ProjectID is the project this conversation belongs to, or "" when unassigned (the API's empty
	// string, kept as-is rather than normalized into a sentinel so the rail and the GUI agree on what.
	ProjectID string
	// SkillFiles is the conversation's skill_files path list (Conversation.skill_files) — the
	// conversation-level half of the skills feature.
	SkillFiles []string
	// PendingReplyID is the acked assistant message id of a turn the SERVER reports as still running,
	// or "".
	PendingReplyID string
}

// Registrar is the minimal subscription hook the controller needs (*subs.Registry satisfies it) —
// kept as an interface so controller tests need no registry.
type Registrar interface{}

// NewController builds the chat controller.
func NewController(cl *client.Clients) *Controller {
	return &Controller{cl: cl, state: map[string]*convState{}}
}

// --- tea.Msgs the controller emits (shell dispatches them back) --------

// TurnAckedMsg reports that a dispatched turn has been ACCEPTED by the server — the stream acked it
// with TurnStarted, so the send has resolved and the turn is genuinely under way.
type TurnAckedMsg struct {
	ConvID string
}

// ConversationsMsg carries the loaded conversation list.
type ConversationsMsg struct {
	Convs []Conversation
	Err   string
	// Categories and Assignments are the CONVERSATION groupings the SAME response carried.
	Categories  []*apiv1.Category
	Assignments []*apiv1.CategoryAssignment
}

// TranscriptMsg carries the durable transcript of the opened
// conversation (already phase-grouped).
type TranscriptMsg struct {
	ConvID string
	Items  []ChatItem
	// ToolCalls are the durable tool-call rows of the page just loaded, in ledger order, for the
	// activity line's rolling counter (internal/toolclass.SummarizeCalls).
	ToolCalls []toolclass.Call
	// SettledAsks are the asks this page records an OUTCOME for: the durable `permission.<outcome>`
	// rows, paired with the ask id each one settles.
	SettledAsks []AskOutcome
	Err         string
}

// AskOutcome is one ask the durable transcript has ALREADY settled: the ask's id, and the outcome
// the server recorded for it.
type AskOutcome struct {
	AskID   string
	Outcome string
}

// chatEventMsg forwards one ChatStreamResponse oneof event.
type chatEventMsg struct {
	ConvID string
	Ev     *apiv1.ChatStreamResponse
	// done closes the stream consumer loop (stream end / error).
	done bool
	err  error
}

// TurnResolvedMsg fires when the ListMessages poll observes the acked reply row while the turn is
// no longer in flight — the completion authority (GUI completion effect).
type TurnResolvedMsg struct {
	ConvID  string
	Failed  bool
	ErrText string
}

// ErrMsg carries a dock-visible failure (send/interject/watch/list).
type ErrMsg struct {
	Where  string
	Err    error
	ConvID string
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
	// Detail is the outcome text for ops that report one (compact): a one-line, user-facing explanation
	// shown verbatim, including WHY compaction declined and the measured context size around it.
	Detail string
}

// StreamDoneMsg marks the goroutine forwarding loop ended (exported — the shell's dispatch consumes
// it).
type StreamDoneMsg struct {
	ConvID string
	Gen    uint64
}

func (s StreamDoneMsg) isMsg() {}

// AbortTurnMsg reports the outcome of a Stop (the composer's ctrl+y) so the shell can settle the
// composer's affordance row and repaint the transcript.
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

// ModeNames lists the persona spellings the operator can type, in the order the help text shows
// them.
func ModeNames() []string { return []string{"brainstorm", "iteration", "quick_work"} }

// ParseMode maps a user-typed persona to the proto enum (case-insensitive).
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

// CreateConversation creates a conversation with the given model + persona (the GUI's New chat:
// model_ref + mode are create-time fields).
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

// SetConversationProject moves a conversation into a project (or unassigns it with an empty id) —
// SetConversationProject.
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

// SetConversationSkillFiles REPLACES the conversation's skill_files path list
// (SetConversationSkillFiles.
func (c *Controller) SetConversationSkillFiles(id string, files []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := c.cl.Ask.SetConversationSkillFiles(ctx, connect.NewRequest(&apiv1.SetConversationSkillFilesRequest{
			Id:    id,
			Files: files,
		})); err != nil {
			return ConversationMutatedMsg{Op: "skill_files", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{Op: "skill_files", ID: id}
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
func (c *Controller) CanCompact(convID string) string {
	if convID == "" {
		return "no conversation open — /new or pick one from the rail"
	}
	if c.IsStreaming(convID) {
		return "a turn is in flight — stop it (esc) before /compact (compaction rewrites the history the turn is using)"
	}
	return ""
}

// CompactConversation compacts the conversation's accumulated context (CompactConversation) so a
// long session can continue inside the model's context window.
func (c *Controller) CompactConversation(id string) tea.Cmd {
	return func() tea.Msg {
		// Compaction may run a summarize model turn, so the deadline is deliberately generous (the same
		// 2-minute class as a summarize call) rather than the 20s used by the pure-CRUD conversation.
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

// MessageDeleteMode mirrors the proto enum for callers that must not import the generated package's
// constants by number.
const (
	// MessageDeleteOnly removes ONE message. It is the operator's "undo": the reply it produced stays, so
	// forgetting the question need not lose the answer.
	MessageDeleteOnly = apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_ONLY
	// MessageDeleteAndFollowing removes that message and every message after it. The operator's "revert to
	// here".
	MessageDeleteAndFollowing = apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_AND_FOLLOWING
)

// DeleteMessage removes one message, or that message and everything after it.
func (c *Controller) DeleteMessage(conversationID, messageID string, mode apiv1.MessageDeleteMode) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.DeleteConversationMessage(ctx, connect.NewRequest(&apiv1.DeleteConversationMessageRequest{
			ConversationId: conversationID,
			MessageId:      messageID,
			Mode:           mode,
		}))
		if err != nil {
			return ConversationMutatedMsg{Op: "remove", ID: conversationID, Err: err.Error()}
		}
		detail := "message removed"
		if resp.Msg.GetDeletedCount() > 1 {
			detail = fmt.Sprintf("%d messages removed", resp.Msg.GetDeletedCount())
		}
		if resp.Msg.GetSessionReset() {
			detail += " — the model's context was reset for the next turn"
		}
		return ConversationMutatedMsg{Op: "remove", ID: conversationID, Detail: detail}
	}
}

// compactionDetail renders a CompactConversationResponse for the dock notice.
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

// SetConversationModel retargets an OPEN conversation's model_ref (SetConversationModel).
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

// SetConversationFullsend turns FULLSEND on or off for one conversation — the TUI's `/fullsend`
// write path.
func (c *Controller) SetConversationFullsend(id string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := c.cl.Ask.SetConversationFullsend(ctx, connect.NewRequest(&apiv1.SetConversationFullsendRequest{
			Id:      id,
			Enabled: enabled,
		}))
		if err != nil {
			return ConversationMutatedMsg{Op: "fullsend", ID: id, Err: err.Error()}
		}
		return ConversationMutatedMsg{Op: "fullsend", ID: id}
	}
}

// convRailPageSize is the page the rail asks for.
const convRailPageSize = 100

// convRailMaxPages bounds the paging loop.
const convRailMaxPages = 20

// LoadConversations fetches the conversation rail.
func (c *Controller) LoadConversations() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		convs := make([]Conversation, 0, convRailPageSize)
		var categories []*apiv1.Category
		var assignments []*apiv1.CategoryAssignment
		token := ""
		for page := 0; page < convRailMaxPages; page++ {
			resp, err := c.cl.Ask.ListConversations(ctx, connect.NewRequest(&apiv1.ListConversationsRequest{
				PageSize:  convRailPageSize,
				PageToken: token,
			}))
			if err != nil {
				return ConversationsMsg{Err: err.Error()}
			}
			for _, cv := range resp.Msg.GetConversations() {
				convs = append(convs, Conversation{
					ID:         cv.GetId(),
					Title:      cv.GetTitle(),
					TurnInFly:  cv.GetTurnInFlight(),
					MessageN:   cv.GetMessageCount(),
					ModelRef:   cv.GetModelRef(),
					Mode:       cv.GetMode(),
					Fullsend:   cv.GetFullsend(),
					ProjectID:  cv.GetProjectId(),
					SkillFiles: cv.GetSkillFiles(),
					// Read at list time, so a conversation the server reports as mid-turn is recognisable as
					// such the moment the rail loads — which is what the re-attach on open needs.
					PendingReplyID: cv.GetPendingAssistantMessageId(),
				})
			}
			// The grouping set is the TENANT's and is identical on every page, so it is taken once from the
			// first — a later page cannot legitimately differ.
			if page == 0 {
				categories = resp.Msg.GetCategories()
				assignments = resp.Msg.GetAssignments()
			}
			if token = resp.Msg.GetNextPageToken(); token == "" {
				break
			}
		}
		return ConversationsMsg{Convs: convs, Categories: categories, Assignments: assignments}
	}
}

// ConsentRepliedMsg reports the SERVER's answer to a permission decision.
type ConsentRepliedMsg struct {
	ConvID string
	// AskID names the ask the verdict is about, so the shell can settle the ONE card that asked it.
	AskID   string
	Applied bool
	Expired bool
	Detail  string
	Err     string
}

// PermissionGrantsMsg carries the conversation's ACTIVE session grants, as the server holds them.
type PermissionGrantsMsg struct {
	ConvID string
	Grants []SessionGrant
	Err    string
	Notice string
}

// LoadPermissionGrants fetches the conversation's active session grants.
func (c *Controller) LoadPermissionGrants(convID string) tea.Cmd {
	if convID == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.ListPermissionGrants(ctx, connect.NewRequest(&apiv1.ListPermissionGrantsRequest{
			ConversationId: convID,
		}))
		if err != nil {
			return PermissionGrantsMsg{ConvID: convID, Err: err.Error()}
		}
		return PermissionGrantsMsg{ConvID: convID, Grants: sessionGrantsFromProto(resp.Msg.GetGrants())}
	}
}

// THE TUI ONLY EVER DROPPED IT LOCALLY, which is why revoking did nothing: the server's store still
// held the grant, so the very next tool call for that directory proceeded without asking.
func (c *Controller) RevokePermissionGrant(convID, directory string) tea.Cmd {
	if convID == "" || directory == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.RevokePermissionGrant(ctx, connect.NewRequest(&apiv1.RevokePermissionGrantRequest{
			ConversationId: convID,
			Directory:      directory,
		}))
		if err != nil {
			return PermissionGrantsMsg{ConvID: convID, Err: err.Error()}
		}
		notice := "revoked · " + directory
		if !resp.Msg.GetRemoved() {
			// NEVER A SILENT SUCCESS: the directory was not granted (already revoked, or never was), which is a
			// different fact from "revoked" and the operator is entitled to it.
			notice = "no session grant for " + directory + " — nothing was revoked"
		}
		return PermissionGrantsMsg{
			ConvID: convID,
			Grants: sessionGrantsFromProto(resp.Msg.GetGrants()),
			Notice: notice,
		}
	}
}

// sessionGrantsFromProto maps the wire's grant list onto the TUI's view type.
func sessionGrantsFromProto(gs []*apiv1.SessionPermissionGrant) []SessionGrant {
	out := make([]SessionGrant, 0, len(gs))
	for _, g := range gs {
		if g == nil || g.GetDirectory() == "" {
			continue
		}
		out = append(out, SessionGrant{Directory: g.GetDirectory(), GrantedAt: g.GetGrantedAtUnix()})
	}
	return out
}

// ReplyPermissionAsk answers a pending permission ask ON THE SERVER. THE TUI NEVER DID THIS, WHICH
// IS WHY APPROVING A CARD DID NOTHING.
func (c *Controller) ReplyPermissionAsk(convID, askID string, choice apiv1.PermissionChoice, answer string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, err := c.cl.Ask.ReplyPermissionAsk(ctx, connect.NewRequest(&apiv1.ReplyPermissionAskRequest{
			ConversationId: convID,
			AskId:          askID,
			Choice:         choice,
			Answer:         answer,
		}))
		if err != nil {
			return ConsentRepliedMsg{ConvID: convID, AskID: askID, Err: err.Error()}
		}
		return ConsentRepliedMsg{
			ConvID:  convID,
			AskID:   askID,
			Applied: resp.Msg.GetApplied(),
			Expired: resp.Msg.GetExpired(),
			Detail:  resp.Msg.GetDetail(),
		}
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
			// A REJECTED CREDENTIAL STOPS THE POLL.
			c.markAuthFailure(id, err)
			return TranscriptMsg{ConvID: id, Err: err.Error()}
		}
		c.markAuthOK(id)
		// The page arrives NEWEST-first (see conversationItems) — it is reversed
		// there, together with the millisecond timestamps.
		return TranscriptMsg{
			ConvID:      id,
			Items:       GroupByPhase(conversationItems(resp.Msg.GetMessages())),
			ToolCalls:   pageToolCalls(resp.Msg.GetMessages()),
			SettledAsks: settledAsksFromPage(resp.Msg.GetMessages()),
		}
	}
}

// pageToolCalls collects the page's tool calls for the activity line's rolling counter, as the
// shape the shared summarizer takes (toolclass.Call).
func pageToolCalls(msgs []*apiv1.ChatMessage) []toolclass.Call {
	var out []toolclass.Call
	for _, m := range msgs {
		for _, c := range m.GetToolCalls() {
			if c == nil {
				continue
			}
			out = append(out, toolclass.Call{ToolName: c.GetFunctionName(), AtMs: c.GetIssuedAtUnixMs()})
		}
	}
	return out
}

// askOutcomeFromLedgerRow maps one ledger row's function name to the outcome it settles, or ("",
// false) when the row is not a resolution at all.
func askOutcomeFromLedgerRow(functionName string) (string, bool) {
	switch functionName {
	// An approval.
	case "permission.user_PERMISSION_CHOICE_ALLOW_ONCE", "permission.fullsend_approved", "permission.fullsend", "permission.answered":
		return "allow_once", true
	case "permission.user_PERMISSION_CHOICE_ALLOW_SESSION":
		return "allow_session", true
	// A refusal: the operator's click, a bare deny, a never-allow binary class, and a protected path the
	// policy refuses. All four mean the call did not proceed.
	case "permission.user_PERMISSION_CHOICE_DENY", "permission.deny", "permission.never_allow", "permission.protected_path":
		return "deny", true
	// No decision was made: the ask expired at finalize, or the policy could not be read. Settled, but
	// NOT as a denial the operator never made (see consentDecisionFromOutcome).
	case "permission.expired", "permission.policy_error":
		return "expired", true
	}
	// NOT A RESOLUTION — including `permission.ask` and `permission.question`, which are the RAISE
	// records. Ignoring them is the fix.
	return "", false
}

// settledAsksFromPage reads the asks the page has already RESOLVED, from its ledger rows — the
// TUI's half of the rule the GUI's lib/ask-consent.ts settleFromLedger applies to the same page.
func settledAsksFromPage(msgs []*apiv1.ChatMessage) []AskOutcome {
	var out []AskOutcome
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.GetToolCalls() {
			if c == nil {
				continue
			}
			outcome, ok := askOutcomeFromLedgerRow(c.GetFunctionName())
			if !ok {
				continue
			}
			id := c.GetId()
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, AskOutcome{AskID: id, Outcome: outcome})
		}
	}
	return out
}

// The server returns messages NEWEST-first — db.ListMessages orders by created_at DESC and
// askorchicon.Service passes those rows straight through — so the page MUST be reversed here.
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
		case "system":
			// A NOTICE, not the model's words.
			kind = KindNotice
		}
		// THE METADATA ERROR IS CONSULTED BEFORE THE ASSISTANT DEFAULT, and the ORDER is the fix — the SAME
		// order as the GUI's bubbleKindFor (frontend/src/lib/ask-bubble.ts).
		text := m.GetContent()
		at := m.GetCreatedAt().AsTime().UnixMilli()
		// THE PROSE IS THE MODEL'S WORDS, so it rides the model's band (KindText) exactly as an ordinary
		// reply does; the error row then carries ONLY the failure and the model that refused.
		if kind != KindUser && kind != KindNotice {
			if errText := m.GetMetadata().GetError(); errText != "" {
				if partial := strings.TrimSpace(m.GetContent()); partial != "" {
					items = append(items, ChatItem{
						Kind: KindText,
						Text: partial,
						At:   at,
						Key:  "m-" + m.GetId() + "-partial",
					})
				}
				kind = KindError
				text = FailedTurnText(errText, m.GetMetadata().GetModelRef())
			}
		}
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
		// A recorded ask_user call renders as a clarifying-question card.
		items = append(items, ChatItem{
			Kind:      kind,
			Text:      text,
			At:        at,
			Key:       "m-" + m.GetId(),
			MessageID: m.GetId(),
		})
		if ask := parseAskUserCall(m.GetToolCalls(), m.GetToolResults()); ask != nil {
			items = append(items, ChatItem{
				Kind: KindAsk,
				Ask:  ask,
				At:   at,
				Key:  "m-" + m.GetId() + "-ask",
			})
		}
	}
	return items
}

// AnswerQuestion answers a recorded clarifying question.
//
// IT IS A REPLY, NOT A MESSAGE — and that was the defect.
//
// The recorded (MCP-served) ask carries the awaiting marker as its tool RESULT so the card can stay
// open, and ReplyPermissionAsk is the ONE thing that replaces that marker with the operator's words.
// This function used to send the label as an ordinary next message instead, which looks equivalent
// from the terminal — a user message appears and the model answers it — and is not:
//
//	the transcript still holds the AWAITING MARKER, so the card is still "open" to every client
//	and to a reload; the answer exists only as a message beside it
//	findPromotedAsk/recordPromotedAnswer never run, so nothing durable records what was answered
//
// That is what "answers the question but it comes back" is: the marker is the card's state, and
// nothing had ever cleared it. On the MCP adapters (claude, opencode) EVERY answer took this path,
// so a recorded card could never be settled by the keyboard or a click — only by the GUI's own
// reply RPC, which the TUI has had since permission cards needed it.
//
// askID is the recorded call's id, read from the card (ParsedAsk.ID). When it is EMPTY the card came
// from a row recorded before this field existed, and the old send-the-label path is kept as a
// fallback: a degraded answer is better than a dead card.
func (c *Controller) AnswerQuestion(convID, askID, label string) tea.Cmd {
	if strings.TrimSpace(askID) == "" {
		return c.Send(convID, label, "")
	}
	// UNSPECIFIED is the choice for a QUESTION: its outcome is the operator's words, not a grant
	// decision.
	return c.ReplyPermissionAsk(convID, askID, apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, label)
}

// Send dispatches a turn: InterjectConversationTurn when the conversation already has an in-flight
// turn (supersede), ChatStream otherwise — exactly the GUI's mode selection.
func (c *Controller) Send(convID, text, contextPreamble string) tea.Cmd {
	return c.SendWithAttachments(convID, text, contextPreamble, nil)
}

// SendWithAttachments is Send with file/image attachments for the turn.
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
	// THE TURN IS REMEMBERED FOR THE ONE RETRY THAT IS POSSIBLE (see dropStream): a stream the server
	// refuses because a reply is already running is re-issued on the supersede path.
	st.attempt = streamAttempt{full: full, files: files}
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
	c.mu.Lock()
	if st := c.state[convID]; st != nil {
		st.lastActivity = now()
		// A NEW GENERATION FOR EVERY SEND: the stream this command is about to open owns the slot from
		// here.
		st.gen++
	}
	gen := c.state[convID].gen
	c.mu.Unlock()
	go c.runLivenessWatch(convID, gen)
	go c.runTurnPoll(convID)
	return c.startStream(convID, full, call, files, gen)
}

// startStream opens the ChatStream/InterjectConversationTurn and returns a Cmd whose first message
// is a synthetic turnStarted placeholder (the actual ack arrives via the forwarding goroutine).
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
		// A REFUSAL OF THIS SEND IS NOT HANDLED HERE — DELIBERATELY.
		if err != nil {
			c.failStream(convID, err)
			return ErrMsg{Where: call, Err: err, ConvID: convID}
		}
		go c.consume(convID, gen, stream)
		// RE-ARM BOTH, and HERE it is correct to arm after the call returns: this is a RE-DIAL of a turn
		// that was already acknowledged.
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

// consume forwards stream events into the tea channel via the returned messages (bubbletea
// re-dispatches what the Cmd returns.
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

// currentGen reads the slot's generation without bumping it.
func (c *Controller) CurrentGen(convID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.state[convID]; st != nil {
		return st.gen
	}
	return 0
}

// Reattach restores the stream slot for a turn the SERVER reports as running, when this client has
// no live stream for it.
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
	// ARM BOTH, for the same reason the send path does: the thinking notice and the stall detection
	// have to run for a turn this client did not start, and the poll is what actually paints the reply.
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

// DiscoverPendingAsks asks the SERVER which asks are still open for a conversation and raises a
// card for each.
func (c *Controller) DiscoverPendingAsks(convID string) tea.Cmd {
	if convID == "" || c.cl == nil || c.cl.Ask == nil {
		return nil
	}
	cl, store := c.cl, c.store
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := cl.Ask.ListPendingAsks(ctx, connect.NewRequest(&apiv1.ListPendingAsksRequest{
			ConversationId: convID,
		}))
		if err != nil || resp == nil {
			return nil
		}
		// Straight to the store, NOT the command channel: the store is an in-process sink whose delivery
		// cannot fail.
		if store != nil {
			// ONE STORE CALL, ONE REPAINT.
			asks := make([]PermissionAsk, 0, len(resp.Msg.GetAsks()))
			for _, p := range resp.Msg.GetAsks() {
				ask := PermissionAskFromProto(p)
				if ask.ConvID == "" {
					ask.ConvID = convID
				}
				asks = append(asks, ask)
			}
			store.ShowDiscoveredAsks(convID, asks)
		}
		return nil
	}
}

// EndStream clears a conversation's stream slot when the server closed the stream cleanly
// (consume's EOF path pushed StreamDoneMsg).
func (c *Controller) EndStream(convID string, gen uint64) bool {
	ended := false
	c.mu.Lock()
	if st := c.state[convID]; st != nil && st.gen == gen {
		st.streaming = false
		st.reconnecting = false
		st.pendingReplyID = ""
		ended = true
	}
	c.mu.Unlock()
	// IT REPORTS WHETHER IT ENDED THE SLOT so a caller can act on "this turn is over" without
	// re-deriving it.
	return ended
}

// AbortTurn stops the in-flight turn on a conversation.
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

// clearReconnecting drops the EVENT STORE's reconnect banner for a conversation.
func (c *Controller) clearReconnecting(convID string) {
	c.mu.Lock()
	store := c.store
	c.mu.Unlock()
	if store != nil {
		store.SetReconnecting(convID, false)
	}
}

// --- stream event handling -----------------------------------------------

// EventStore is where the controller appends live chunks so the view can render them.
type EventStore interface {
	// AppendLiveItem adds one live ChatItem for the conversation (in
	// arrival order; the view applies phase grouping).
	AppendLiveItem(convID string, item ChatItem)
	// SetReconnecting flips the conversation's reconnecting banner.
	SetReconnecting(convID string, on bool)
	// ShowConsentAsk surfaces a pending permission ask (or a clarifying question) as a card.
	ShowConsentAsk(ask PermissionAsk)
	// SettleConsentAsk records an ask that was settled SOMEWHERE ELSE — the other client, or the
	// collector expiring it — so this client's copy of the card stops being a choice.
	SettleConsentAsk(convID, askID, outcome, answer string)
	// ShowDiscoveredAsks draws the open asks the server reported for a conversation. IT ONLY ADDS — it
	// never settles a card, whatever the list does or does not contain.
	ShowDiscoveredAsks(convID string, asks []PermissionAsk)
}

// handleEvent applies one ChatStreamResponse to the conversation's slot
// and the event store. Mirrors the GUI's runStream for-await body.
func (c *Controller) handleEvent(convID string, ev *apiv1.ChatStreamResponse) {
	if ev == nil {
		return
	}
	// EVERY EVENT STAMPS THE CLOCK, including the ones that carry no content.
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
		// THE ACK IS THE ONE EVENT THAT SAYS THE SEND RESOLVED, so the shell is told about it — this runs
		// on the stream's own goroutine.
		if c.cmds != nil {
			select {
			case c.cmds <- func() tea.Msg { return TurnAckedMsg{ConvID: convID} }:
			default:
			}
		}
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
		if st := c.state[convID]; st != nil {
			if st.reconnecting {
				st.reconnecting = false
			}
			// THE SERVER'S CLOCK, RECORDED WITH THE INSTANT WE HEARD IT.
			if ts := e.Heartbeat.GetServerTimeUnixMs(); ts > 0 {
				st.serverTimeMs = ts
				st.serverRecvMonoMs = now()
			}
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
	case *apiv1.ChatStreamResponse_PermissionAskResolved:
		// THE OTHER CLIENT DECIDED.
		if r := e.PermissionAskResolved; r != nil && r.GetAskId() != "" && c.store != nil {
			// STRAIGHT TO THE STORE, not through the command channel.
			c.store.SettleConsentAsk(convID, r.GetAskId(), r.GetOutcome(), r.GetAnswer())
		}
	case *apiv1.ChatStreamResponse_PermissionAsk:
		// THE PERMISSION CARD'S WIRE ARM.
		ask := PermissionAskFromProto(e.PermissionAsk)
		// The ask names its OWN conversation, so the shell appends the card to the conversation that asked
		// rather than to whichever one is on screen (see ShowConsentAsk).
		if ask.ConvID == "" {
			ask.ConvID = convID
		}
		// DELIVERED TO THE STORE, NOT THE COMMAND CHANNEL — this was the silent drop.
		if c.store != nil {
			c.store.ShowConsentAsk(ask)
		}
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

// streamAttempt is the turn a slot's current stream attempt carries.
type streamAttempt struct {
	full    string // the message exactly as it goes on the wire (context preamble included)
	files   []*apiv1.AttachmentInput
	retried bool
}

// isTurnGateRefusal reports whether err is the server's ONE-TURN-PER-CONVERSATION refusal.
func isTurnGateRefusal(err error) bool {
	return err != nil && connect.CodeOf(err) == connect.CodeFailedPrecondition
}

// failStream handles a pre-ack send/interject failure: when the turn was acked the slot goes
// reconnecting (the server-side collector still runs).
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

// dropStream handles a socket drop mid-stream (stream.Err() after the receive loop): acked turns
// re-dial WatchTurnStream; the poll remains the completion authority either way.
func (c *Controller) dropStream(convID string, gen uint64, err error) {
	c.mu.Lock()
	st := c.state[convID]
	if st == nil || st.gen != gen {
		c.mu.Unlock()
		return // a superseded stream's failure is not this slot's business
	}
	var (
		watch  string
		retry  tea.Cmd
		report tea.Cmd
	)
	if st.streaming {
		switch {
		case st.pendingReplyID != "":
			// acked turn: the server-side collector keeps running — slot
			// stays streaming, goes reconnecting, watch re-dials the hub.
			st.reconnecting = true
			watch = st.pendingReplyID
		case isTurnGateRefusal(err) && st.attempt.full != "" && !st.attempt.retried:
			// PATH INSTEAD OF BOUNCED.
			st.attempt.retried = true
			st.gen++
			st.reconnecting = false
			st.lastActivity = now()
			gen = st.gen
			retry = c.startStream(convID, st.attempt.full, "interject", st.attempt.files, gen)
		default:
			// pre-ack failure: tear down (GUI fail() with no reply id).
			if st.attempt.full != "" {
				report = func() tea.Msg { return ErrMsg{Where: "send", Err: err, ConvID: convID} }
			}
			st.streaming = false
			st.optimisticUser = ""
			st.sentText = ""
			st.attempt = streamAttempt{}
		}
	}
	c.err = err.Error()
	c.mu.Unlock()
	if retry != nil {
		// A NEW ATTEMPT OWNS THE SLOT: the shell runs this exactly like the send's own command.
		if c.cmds != nil {
			c.cmds <- retry
		}
		return
	}
	if watch != "" && c.cmds != nil {
		c.cmds <- c.Watch(convID, watch)
		return
	}
	if report != nil && c.cmds != nil {
		// LAST, and only ever one of these: the shell dispatches it like any other chat message, which is
		// how a failed send reaches the dock.
		c.cmds <- report
	}
}

// runTurnPoll keeps a streaming turn's transcript fresh from the DURABLE store, on a timer, until
// the turn ends.
func (c *Controller) runTurnPoll(convID string) {
	ticker := time.NewTicker(askTurnPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		st := c.state[convID]
		streaming := st != nil && st.streaming
		dead := st != nil && st.authDead
		c.mu.Unlock()
		// A DEAD CREDENTIAL ENDS THE POLL, not just this iteration.
		if !streaming || dead || c.cmds == nil {
			return
		}
		select {
		case c.cmds <- c.pollTranscript(convID):
		default:
		}
	}
}

// runLivenessWatch is the CLIENT-SIDE stream watchdog, and its absence was the streaming report.
// SILENCE IS ONLY A FAULT WHILE STREAMING, and the check stops as soon as the slot is not.
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

// livenessCheck reports (stop, stale) for one tick: stop when there is nothing left to watch, stale
// when the stream has gone quiet past the timeout.
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
	// lastActivity == 0 means the stream has not delivered its first event yet — that is "not started",
	// not "silent", so it is not given the watchdog's verdict.
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

// SetServerTimeForTest plants the server clock the way a Heartbeat does, so a test can assert the
// word the activity line draws for a KNOWN server time without standing up a stream.
func (c *Controller) SetServerTimeForTest(convID string, serverTimeUnixMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if serverTimeUnixMs <= 0 {
		return // a zero stamp is not a stamp (see handleEvent's Heartbeat arm)
	}
	st := c.seedTurnForTest(convID)
	st.serverTimeMs = serverTimeUnixMs
	st.serverRecvMonoMs = now()
}

// seedTurnForTest materialises the slot a SEND would have made for this conversation — streaming,
// and with every field the two readers below touch — and returns it with the lock held.
func (c *Controller) seedTurnForTest(convID string) *convState {
	st := c.state[convID]
	if st == nil {
		st = &convState{}
		c.state[convID] = st
	}
	st.streaming = true
	st.reconnecting = false
	if st.lastActivity == 0 {
		st.lastActivity = now()
	}
	return st
}

// SetSilenceForTest back-dates the watchdog's clock by d, which is the ONLY way to drive the
// activity line's silence bands without sleeping 35 seconds.
func (c *Controller) SetSilenceForTest(convID string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.seedTurnForTest(convID)
	st.lastActivity = now() - d.Milliseconds()
}

// ServerTimeSince reports the SERVER's clock, extrapolated to right now — the value the activity
// verb rotation indexes on.
func (c *Controller) ServerTimeSince(convID string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state[convID]
	if st == nil || st.serverTimeMs <= 0 || st.serverRecvMonoMs <= 0 {
		return 0, false
	}
	delta := now() - st.serverRecvMonoMs
	if delta < 0 {
		delta = 0 // a clock that stepped backwards is not a negative age
	}
	return st.serverTimeMs + delta, true
}

// errStreamStalled reports a stream that stopped sending anything.
var errStreamStalled = errors.New("stream stalled: no event within the liveness timeout")

func now() int64 { return time.Now().UnixMilli() }

// markAuthFailure records that a transcript read was rejected as unauthenticated, which stops the
// turn poll until the credential is replaced. ONLY 401 STOPS IT.
func (c *Controller) markAuthFailure(convID string, err error) {
	if !IsAuthExpired(err) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.state[convID]; st != nil {
		st.authDead = true
	}
}

// markAuthOK clears the flag after a read that SUCCEEDED, so a refreshed credential (or a re-connect) resumes
// polling rather than staying parked for the rest of the process's life.
func (c *Controller) markAuthOK(convID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.state[convID]; st != nil && st.authDead {
		st.authDead = false
	}
}
