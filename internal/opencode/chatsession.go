package opencode

import (
	"context"
	"errors"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"sync"
)

// ChatTurnClient is implemented on *Adapter (see adapter.go) by delegating
// to the host serve's SessionClient. This file holds the Subscribe event
// adapter that maps the raw opencode bus vocabulary onto the
// scheduler-neutral SessionEvent surface the Ask drain loop consumes.

// hostServeClient returns the host serve's session client, or nil when the
// serve transport is unavailable. It never STARTS the serve — callers that
// may need the serve started go through ensureHostServeClient.
func (a *Adapter) hostServeClient() *SessionClient {
	h := chatHost(a.askHost, a.host)
	if h == nil {
		return nil
	}
	return h.Client()
}

// chatHost selects the serve an Ask conversation turn runs on. Ask has its own
// serve (see NewAskHostServe) because opencode's permission config is
// per-PROCESS: the worker serve carries the worker sandbox, the Ask serve
// carries the interactive profile. This pure selector is the ONE place the Ask
// surface resolves a serve — the worker paths (sessionClientFor, follow-ups)
// keep a.host.
func chatHost(ask, exec *HostServe) *HostServe {
	if ask != nil {
		return ask
	}
	return exec
}

// ensureHostServeClient returns the host serve's session client, starting
// the serve on FIRST demand (AC 2). Every Ask turn whose transport resolves
// to the opencode adapter comes through here, so an Ask session on an
// opencode-resolved transport is itself a demand trigger — while a plane
// whose Ask default resolves natively never starts one.
//
// The error is the start failure verbatim (disabled kill-switch, missing
// binary, serve never ready): the Ask turn fails LOUDLY with the reason
// instead of silently degrading (AC 4).
// askServeFor picks the serve an Ask turn runs on, resolving the conversation's MCP SCOPE first.
//
// THIS CLOSES A GAP THAT EXISTED ON THIS ADAPTER ALONE. claude and the native bridge both resolved
// "project-owned ∪ conversation-owned" for an Ask turn; opencode selected a single pre-built serve
// whose config carried only the built-in Orchicon server, so an MCP server the operator added to a
// PROJECT or to the CONVERSATION was invisible to it — and there was no symptom beyond tools that
// simply were not there. Reported as "Scoped MCP servers added by the user in Conversations should
// work for all adapters".
//
// THE SCOPE IS ALREADY ON THE CONTEXT, put there by the Ask collector beside the mode and the
// project (askmode.ConversationScope, plus the tenant for the resolver). So this needs no new
// parameter through the scheduler's chat interface, and no second source of truth: one turn, one
// scope, read where it is needed.
//
// EMPTY SET -> THE DEFAULT ASK SERVE. A conversation with no project and no servers of its own must
// behave exactly as it did before, which is what ServeFor already guarantees (the empty set maps to
// the default serve). A resolution FAILURE also falls back rather than failing the turn, and says so
// in the log: a conversation that cannot read its MCP configuration should still be able to talk,
// unlike a dispatched execution where a missing server set means the work cannot be done.
//
// THE COST OF PER-SET SERVES, stated rather than discovered later: a pooled serve owns its own data
// dir, so an opencode SESSION lives on exactly one of them. If a conversation's MCP set changes
// mid-conversation — the operator adds a server to the project, or to the conversation — the next
// turn lands on a DIFFERENT serve and the old session id is not there. That is already a handled
// case rather than a new failure: SendTurnMessage maps the 404 onto scheduler.ErrSessionNotFound and
// the Ask collector recreates and re-seeds the session from Orchicon's own history, so the
// conversation continues with its transcript intact and the model's opencode-side session context
// restarts. There is no alternative that keeps both properties — one shared serve cannot take a
// per-conversation set without leaking it to every other conversation, which is the exact thing the
// pool exists to prevent.
func (a *Adapter) askServeFor(ctx context.Context) *HostServe {
	fallback := chatHost(a.askHost, a.host)
	if a.askPool == nil || a.hostScope == nil {
		return fallback
	}
	scope := askmode.ConversationScopeFromContext(ctx)
	set := a.resolveAskMCPSet(ctx, scope)
	pooled, err := a.askPool.ServeFor(ctx, set)
	if err != nil {
		a.log.Warn("ask session: host serve pool has no serve for this MCP set — using the default Ask serve",
			"conversation", scope.ConversationID, "project", scope.ProjectID,
			"servers", mcpclient.ProvenanceString(set.Servers), "error", err)
		return fallback
	}
	return pooled
}

// resolveAskMCPSet resolves the conversation's MCP set: project-owned ∪ conversation-owned rows, the
// same ScopeConversation the native and claude Ask paths use, so all three agree by construction.
func (a *Adapter) resolveAskMCPSet(ctx context.Context, scope askmode.ConversationScope) mcpclient.Resolution {
	if scope.ConversationID == "" {
		return mcpclient.Resolution{} // no scope stamped: the default serve, unchanged
	}
	res, err := a.hostScope.ResolveScope(ctx, mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeConversation,
		ProjectID:      scope.ProjectID,
		ConversationID: scope.ConversationID,
	})
	if err != nil {
		a.log.Warn("ask session: MCP resolution failed — using the default Ask serve",
			"conversation", scope.ConversationID, "project", scope.ProjectID, "error", err)
		return mcpclient.Resolution{}
	}
	return res
}

func (a *Adapter) ensureHostServeClient(ctx context.Context) (*SessionClient, error) {
	h := a.askServeFor(ctx)
	if err := h.EnsureStarted(ctx); err != nil {
		return nil, fmt.Errorf("host opencode serve unavailable — Ask chat transport is disabled: %w", err)
	}
	c := h.Client()
	if c == nil {
		return nil, errors.New("host opencode serve unavailable — Ask chat transport is disabled")
	}
	return c, nil
}

// SessionOwnerKind implements scheduler.SessionOwnerKind: sessions created
// by this adapter belong to the opencode kind. Ask Orchicon uses it to
// enforce adapter-scoped session identity — a conversation whose persisted
// session came from another adapter (e.g. the native synthetic
// "orchicon-ask:" id) must recreate first and never dispatch that foreign id
// to the opencode serve (which 500s on an unknown session).
func (a *Adapter) SessionOwnerKind() string { return "opencode" }

// CreateConversationSession implements scheduler.ChatTurnClient, creating a
// fresh session on the host serve. An Ask turn has no project directory, so
// the request goes out unscoped (mirrors the historical chat behavior).
func (a *Adapter) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return "", err
	}
	sid, err := c.CreateSession(ctx, title)
	if err != nil {
		return "", err
	}
	return sid, nil
}

// SendTurnMessage implements scheduler.ChatTurnClient, appending a user
// message to a conversation session. A 404 (session no longer on the serve)
// is mapped onto scheduler.ErrSessionNotFound so the Ask collector can
// recreate + re-seed it without depending on the opencode type.
func (a *Adapter) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return a.SendTurnMessageWithAttachments(ctx, conversationID, sessionID, system, modelRef, text, nil)
}

// SendTurnMessageWithAttachments implements
// scheduler.SendTurnMessageWithAttachments, the optional attachment-aware
// sender for Ask turns. Attachments go out as inline data: URLs (no
// UploadAttachment/BlobStore dependency — mirroring the host serve client).
func (a *Adapter) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return err
	}
	var parts []AttachmentPart
	if len(attachments) > 0 {
		parts = make([]AttachmentPart, 0, len(attachments))
		for _, at := range attachments {
			parts = append(parts, AttachmentPart{Name: at.Name, MimeType: at.MimeType, Data: at.Data})
		}
	}
	// ASK: the prompt is delivered on the turn that follows CREATION, and only there. This client
	// marked the session when it created it (promptPending), which is the one signal that separates
	// the seeded first prompt from the non-empty reuse prompt sent on every later turn.
	err = c.sendMessage(ctx, sessionID, system, modelRef, text, parts, c.consumePromptPending(sessionID))
	if err != nil && errors.Is(err, ErrSessionNotFound) {
		return scheduler.ErrSessionNotFound
	}
	return err
}

// AbortConversationSession implements scheduler.ChatTurnClient, stopping a
// live turn on a session so the model stops generating now.
func (a *Adapter) AbortConversationSession(ctx context.Context, sessionID string) error {
	// Abort is BEST-EFFORT: a turn can only be live on a serve that was
	// already started, so a serve that cannot be reached here has nothing to
	// abort. EnsureStarted keeps the lazy path uniform (a live turn implies a
	// live serve → the fast path), and the error is not surfaced because
	// there is no turn to fail.
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return nil
	}
	return c.Abort(ctx, sessionID)
}

// ReplyPermission implements scheduler.ChatTurnClient, auto-approving a
// permission.asked signal.
func (a *Adapter) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return err
	}
	return c.ReplyPermission(ctx, sessionID, permissionID)
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient with an explicit
// serve response value.
func (a *Adapter) ReplyPermissionDecision(ctx context.Context, sessionID, permissionID, decision string) error {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return err
	}
	return c.ReplyPermissionDecision(ctx, sessionID, permissionID, decision)
}

// Subscribe implements scheduler.ChatTurnClient. It opens the host serve's
// /event SSE stream (multiplexing ALL sessions) and adapts each raw BusEvent
// onto the scheduler-neutral SessionEvent surface, applying the mid-generation
// event classification. The Ask drain loop consumes only SessionEvents — it
// never sees the raw opencode bus type.
func (a *Adapter) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return nil, err
	}
	sub, err := c.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	// The conversation's directory, so the ledger observes the files THIS session
	// touched — resolved by the SERVER's installed resolver, not by reaching for
	// the process's working directory. That reach is the defect that made every
	// Ask diff pane blank (see internal/api's askBaseDir note), and an adapter
	// cannot do better on its own: it has no pool, and reading a project row here
	// would be a layering violation.
	//
	// A nil resolver (or one that errors) yields "" — the ledger then observes
	// nothing rather than observing the WRONG tree, which is the safer failure.
	dir := ""
	if a.askDirResolver != nil {
		dir = a.askDirResolver(ctx, conversationID)
	}
	ad := newSessionEventAdapterWithLedger(sub, conversationID, dir, tenant.FromContext(ctx), a.askFileEdits)
	return ad, nil
}

// sessionEventAdapter adapts an opencode BusSub onto the scheduler.SessionBus
// surface, classifying events into SessionEvent kinds. Events() returns the
// SAME channel on every call (the drain loop re-reads it each select
// iteration), so the translating goroutine is started once at construction.
type sessionEventAdapter struct {
	sub    BusSub
	events chan scheduler.SessionEvent
	// calls correlates an in-flight tool call's args with the permission.asked
	// raised for it (see toolCallIndex).
	calls *toolCallIndex
	// The diff-ledger context: the conversation this adapter is serving, the
	// session's own directory, and the hook. Carried HERE rather than resolved
	// inside the pure classifier, because the classifier maps one event to one
	// SessionEvent and must stay free of side effects — and because the ledger
	// needs the conversation id and directory, which are properties of the
	// SUBSCRIPTION rather than of the event.
	convID   string
	baseDir  string
	tenantID string
	ledger   FileEditHookFunc
}

func (a *sessionEventAdapter) start() {
	go func() {
		defer close(a.events)
		raw := a.sub.Events()
		for {
			select {
			case evt, ok := <-raw:
				if !ok {
					return
				}
				a.ledgerToolCall(evt)
				if se := classifyBusEventIndexed(evt, a.calls); se != nil {
					select {
					case a.events <- *se:
					case <-a.sub.Done():
						return
					}
				}
			case <-a.sub.Done():
				return
			}
		}
	}()
}

// ledgerToolCall records the file edits of a COMPLETED tool_use part.
//
// It fires only for a completed call, for the same reason the execution path
// does: a running call has not written anything yet, so an observer would diff
// the file against itself and drop the row. It also fires BEFORE the classifier,
// because the classifier's output is capped for the UI while the ledger needs
// the RAW output — a huge batch_write carries its file_edits payload at the END
// of the JSON, which a cap would splice away.
func (a *sessionEventAdapter) ledgerToolCall(evt BusEvent) {
	if a.ledger == nil || a.convID == "" {
		return
	}
	legacy, ok := LegacyEventFromBus(evt)
	if !ok {
		return
	}
	if etype, _ := legacy["type"].(string); etype != "tool_use" {
		return
	}
	part, _ := legacy["part"].(map[string]any)
	if part == nil {
		return
	}
	state, _ := part["state"].(map[string]any)
	if state == nil {
		return
	}
	// Only a finished call has ground truth on disk.
	if st, _ := state["status"].(string); st != "" && st != "completed" {
		return
	}
	toolName, _ := part["tool"].(string)
	if toolName == "" {
		return
	}
	input, _ := state["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	output, _ := state["output"].(string)
	a.ledger(context.Background(), a.convID, a.tenantID, a.baseDir, toolName, input, output)
}

func (a *sessionEventAdapter) Events() <-chan scheduler.SessionEvent { return a.events }
func (a *sessionEventAdapter) Done() <-chan struct{}                 { return a.sub.Done() }
func (a *sessionEventAdapter) Close()                                { a.sub.Close() }

// ClientSessionAdapter adapts a *SessionClient onto the
// scheduler.ChatTurnClient surface, so a plain host serve client (no
// *Adapter in the loop) can drive Ask conversations. It is the nil-dispatcher
// fallback askorchicon uses when no adapter namespace is configured.
type ClientSessionAdapter struct {
	sc *SessionClient
}

// NewClientSessionAdapter wraps a SessionClient as a scheduler.ChatTurnClient.
func NewClientSessionAdapter(sc *SessionClient) *ClientSessionAdapter {
	return &ClientSessionAdapter{sc: sc}
}

func (a *ClientSessionAdapter) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	return a.sc.CreateSession(ctx, title)
}

// QueuesPrompts implements scheduler.SessionQueuesPrompts and answers TRUE — the same answer as the primary
// Adapter, because it is the same serve underneath: SendTurnMessage here goes through SessionClient.SendMessage
// to prompt_async, which the serve serialises per session ("a message sent while a turn is in flight is queued
// and answered at the next turn boundary").
//
// IT WAS UNDECLARED, which is why this file's other adapter declared the capability and this one silently did
// not: an undeclared adapter falls to "does not queue", so interjections on a conversation served through THIS
// path were held until the turn ended rather than delivered to the boundary.
func (a *ClientSessionAdapter) QueuesPrompts() bool { return true }

func (a *ClientSessionAdapter) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	err := a.sc.SendMessage(ctx, sessionID, system, modelRef, text)
	if err != nil && errors.Is(err, ErrSessionNotFound) {
		return scheduler.ErrSessionNotFound
	}
	return err
}

func (a *ClientSessionAdapter) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	var parts []AttachmentPart
	if len(attachments) > 0 {
		parts = make([]AttachmentPart, 0, len(attachments))
		for _, at := range attachments {
			parts = append(parts, AttachmentPart{Name: at.Name, MimeType: at.MimeType, Data: at.Data})
		}
	}
	err := a.sc.sendMessage(ctx, sessionID, system, modelRef, text, parts, a.sc.consumePromptPending(sessionID))
	if err != nil && errors.Is(err, ErrSessionNotFound) {
		return scheduler.ErrSessionNotFound
	}
	return err
}

func (a *ClientSessionAdapter) AbortConversationSession(ctx context.Context, sessionID string) error {
	return a.sc.Abort(ctx, sessionID)
}

func (a *ClientSessionAdapter) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return a.sc.ReplyPermission(ctx, sessionID, permissionID)
}

func (a *ClientSessionAdapter) ReplyPermissionDecision(ctx context.Context, sessionID, permissionID, decision string) error {
	return a.sc.ReplyPermissionDecision(ctx, sessionID, permissionID, decision)
}

func (a *ClientSessionAdapter) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	sub, err := a.sc.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	ad := newSessionEventAdapter(sub)
	return ad, nil
}

// CompactConversationSession implements scheduler.ChatCompactor for a plain
// host-serve client (the nil-dispatcher fallback).
func (a *ClientSessionAdapter) CompactConversationSession(ctx context.Context, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	return compactOnServe(ctx, a.sc, opts)
}

// CompactConversationSession implements scheduler.ChatCompactor: the serve owns
// this conversation's session, so compaction is a summarize of that session in
// place — exactly the call the worker compaction gate makes (see
// session_run.doCompact). Unlike the native adapter there is no history to
// reduce first: opencode's own summarize handles the session it holds.
func (a *Adapter) CompactConversationSession(ctx context.Context, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return scheduler.ChatCompaction{}, err
	}
	return compactOnServe(ctx, c, opts)
}

// compactOnServe is the shared opencode compaction path.
func compactOnServe(ctx context.Context, c *SessionClient, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	if c == nil {
		return scheduler.ChatCompaction{}, errors.New("host opencode serve unavailable — Ask chat transport is disabled")
	}
	if opts.SessionID == "" {
		// opencode is session-FUL: without the session there is nothing to
		// summarize, and summarizing a fabricated id would be a silent no-op.
		return scheduler.ChatCompaction{}, errors.New("opencode: conversation compaction requires the conversation's session id")
	}
	providerID, model, ok := adapter.SplitForServe(opts.ModelRef)
	if !ok || providerID == "" || model == "" {
		return scheduler.ChatCompaction{}, fmt.Errorf("opencode: model ref %q has no provider/model to summarize with", opts.ModelRef)
	}
	if err := c.Compact(ctx, opts.SessionID, providerID, model); err != nil {
		return scheduler.ChatCompaction{}, err
	}
	// The serve reports no token counts back, so the measured token fields stay
	// 0 (unknown) rather than being filled with a guess.
	return scheduler.ChatCompaction{
		Compacted: true,
		Detail:    "summarized the session in place on the opencode serve",
	}, nil
}

// Compile-time assertions for the plain-client adapter.
var _ scheduler.ChatTurnClient = (*ClientSessionAdapter)(nil)
var _ scheduler.SendTurnMessageWithAttachments = (*ClientSessionAdapter)(nil)
var _ scheduler.ChatCompactor = (*ClientSessionAdapter)(nil)
var _ scheduler.ChatCompactor = (*Adapter)(nil)

// NewSessionBusFromSub wraps an opencode BusSub as a scheduler.SessionBus,
// classifying every raw bus event onto the scheduler-neutral SessionEvent
// surface. Used by askorchicon tests (a fake opencode.BusSub feeding raw
// events) and the adapter/plain-client Subscribe paths.
func NewSessionBusFromSub(sub BusSub) scheduler.SessionBus {
	return newSessionEventAdapter(sub)
}

// newSessionEventAdapter builds a started adapter carrying a fresh tool-call
// correlation index.
func newSessionEventAdapter(sub BusSub) *sessionEventAdapter {
	return newSessionEventAdapterWithLedger(sub, "", "", "", nil)
}

// newSessionEventAdapterWithLedger is newSessionEventAdapter for an Ask
// conversation, which additionally ledgers the file edits its tool calls make.
//
// WHY THE LEDGER IS FED FROM HERE. This adapter sees every raw bus event for the
// session, including the COMPLETED tool_use part carrying state.input and
// state.output — the same ground truth the execution path ledgers from. Without
// it an opencode Ask conversation recorded NOTHING and the diff pane showed no
// files, even though opencode is a first-class Ask adapter (it implements
// ChatTurnClient). Nothing about the adapter was wrong; the hook was simply
// never installed on it, which is the asymmetry the Dispatcher's central
// application point removes.
func newSessionEventAdapterWithLedger(sub BusSub, convID, baseDir, tenantID string, ledger FileEditHookFunc) *sessionEventAdapter {
	ad := &sessionEventAdapter{
		sub:      sub,
		events:   make(chan scheduler.SessionEvent, 32),
		calls:    newToolCallIndex(),
		convID:   convID,
		baseDir:  baseDir,
		tenantID: tenantID,
		ledger:   ledger,
	}
	ad.start()
	return ad
}

// toolCallIndex correlates an in-flight tool call's ARGS with the
// permission.asked raised for that call.
//
// It exists because the ask itself can carry no detail at all: opencode gates
// an MCP (and host-suite) tool call by its tool KEY, emitting
// `permission=<tool key>, patterns=["*"], metadata={}`. Without the args there
// is nothing to key the decision on and nothing to show a user — the consent
// layer keyed such an ask on its scope directory and approved it silently (the
// QA finding on this work item: a sibling-path write through an `orchicon_*`
// tool raised no ask). The args ARE on the bus, in the tool part that precedes
// the ask, so they are correlated by callID here and attached to the
// permission event's Detail as `toolInput`.
type toolCallIndex struct {
	mu   sync.Mutex
	args map[string]map[string]any
	// names maps a call id to the tool NAME, which on 2.x arrives on an event of
	// its own (`session.tool.input.started`) rather than with the call.
	names map[string]string
}

func newToolCallIndex() *toolCallIndex {
	return &toolCallIndex{args: make(map[string]map[string]any), names: make(map[string]string)}
}

// observe records the args of a `message.part.updated` tool part, if that is
// what the event is. Nil-receiver safe: an adapter built without correlation
// passes nil and simply records nothing.
//
// Entries MERGE rather than replace: the serve streams a tool part's input
// progressively (a later update carries the rest of the args), and an early
// update can carry an empty input. Later keys win.
func (x *toolCallIndex) observe(evt BusEvent) {
	if x == nil {
		return
	}
	// ---- v2: a call's identity arrives ACROSS SEPARATE EVENTS ----------------
	//
	// Verified against opencode 2.x's own schema: `session.tool.input.started`
	// carries the NAME, `session.tool.called` the INPUT, and the resolution
	// (`session.tool.success`/`.failed`) ONLY the id — ToolBase is
	// {sessionID, assistantMessageID, id} with no name. So neither half can be
	// read off the resolved part, and both must be recorded to reconstruct the
	// call at all. Without this the index never populated on 2.x (the gate below
	// is v1-only), every resolved call was identity-less, and the stall monitor
	// read them all as one signature — `8 repeats of |null` killing a live turn.
	switch evt.Type {
	case "session.tool.input.started":
		props := evt.Props()
		id, _ := props["id"].(string)
		name, _ := props["name"].(string)
		if id != "" && name != "" {
			x.mu.Lock()
			x.names[id] = name
			x.mu.Unlock()
		}
		return
	case "session.tool.called":
		props := evt.Props()
		id, _ := props["id"].(string)
		input, _ := props["input"].(map[string]any)
		if id != "" && len(input) > 0 {
			x.mu.Lock()
			dst := x.args[id]
			if dst == nil {
				dst = make(map[string]any, len(input))
				x.args[id] = dst
			}
			for k, v := range input {
				dst[k] = v
			}
			x.mu.Unlock()
		}
		return
	}
	if evt.Type != "message.part.updated" {
		return
	}
	part, _ := evt.Props()["part"].(map[string]any)
	if part == nil {
		return
	}
	if ptype, _ := part["type"].(string); ptype != "tool" {
		return
	}
	cid, _ := part["callID"].(string)
	if cid == "" {
		return
	}
	state, _ := part["state"].(map[string]any)
	if state == nil {
		return
	}
	input, _ := state["input"].(map[string]any)
	if len(input) == 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	dst := x.args[cid]
	if dst == nil {
		dst = make(map[string]any, len(input))
		x.args[cid] = dst
	}
	for k, v := range input {
		dst[k] = v
	}
}

// stamp fills a RESOLVED tool part's missing identity from what the index
// recorded for the same call id: the tool NAME and the call's INPUT.
//
// WHY IT IS NEEDED. 2.x announces a call across three events and the resolution
// carries only the id, so a resolved part arrives with no tool and no args. Every
// consumer keying on the call's identity then sees all such calls as one thing —
// the stall monitor read a turn's distinct tool calls as `8 repeats of |null` and
// killed it. Filling the part is what makes the identity usable again, and it
// mirrors the execution path, which correlates the same three events by id
// (session_run.rememberV2Tool/fillV2Tool).
//
// FILLS MISSING FIELDS ONLY, so a v1 part that already carries both is untouched.
// The NAME entry is consumed (id → name is one-shot, and consuming it keeps the
// map from growing for the session's life); the args entry is left in place
// because the consent path reads it to annotate a permission ask by call id.
// get returns a copy of the args recorded for a tool call id, or nil when the
// call was never observed.
func (x *toolCallIndex) get(callID string) map[string]any {
	if x == nil || callID == "" {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	src := x.args[callID]
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// stamp fills a RESOLVED tool part's missing identity from what the index
// recorded for the same call id: the tool NAME and the call's INPUT.
//
// It mirrors the execution path, which correlates the same three 2.x events by
// id (session_run.rememberV2Tool/fillV2Tool) — the Ask path had no equivalent,
// so every resolved part arrived with neither field and every consumer keying
// on the call's identity collapsed them into one.
func (x *toolCallIndex) stamp(part map[string]any) {
	if x == nil || part == nil {
		return
	}
	cid, _ := part["callID"].(string)
	if cid == "" {
		return
	}
	// NEITHER ENTRY IS CONSUMED. A call can resolve more than once (a retry, or a
	// duplicated event), and consuming the name would leave the SECOND resolution
	// identity-less — which is precisely the state that produced the `|null` false
	// kill. Retaining is also consistent with the args entry, which the consent
	// path reads by id after a call has resolved; an id→name string is the same
	// order of memory as the args already held.
	x.mu.Lock()
	name := x.names[cid]
	args := x.args[cid]
	x.mu.Unlock()
	if _, has := part["tool"]; !has && name != "" {
		part["tool"] = name
	}
	if state, ok := part["state"].(map[string]any); ok {
		if _, has := state["input"]; !has && args != nil {
			state["input"] = args
		}
	}
}

// argsForPermission resolves the args of the tool call a permission event's
// properties name (`tool.callID`), or nil when the ask carries no call id or
// the call was not observed (a subscription that attached mid-call).
func (x *toolCallIndex) argsForPermission(props map[string]any) map[string]any {
	tool, _ := props["tool"].(map[string]any)
	if tool == nil {
		return nil
	}
	cid, _ := tool["callID"].(string)
	return x.get(cid)
}

// permissionDetailWithToolInput copies a permission event's properties with the
// correlated tool-call args attached as `toolInput`. The raw properties are
// never mutated.
func permissionDetailWithToolInput(props, args map[string]any) map[string]any {
	out := make(map[string]any, len(props)+1)
	for k, v := range props {
		out[k] = v
	}
	out["toolInput"] = args
	return out
}

// busErrorMessage extracts the human-readable reason from an error-bearing bus event, whichever
// surface shaped it. v1 put a string under `error`; v2 nests an object with `type` and `message`
// (measured: {"type":"provider.invalid-request","message":"…insufficient credits…"}). Falls back to a
// named default so a caller never renders an empty reason.
func busErrorMessage(props map[string]any) string {
	switch e := props["error"].(type) {
	case map[string]any:
		if m, _ := e["message"].(string); m != "" {
			return m
		}
	case string:
		if e != "" {
			return e
		}
	}
	return "opencode session error"
}

// classifyBusEvent maps one raw opencode bus event onto a SessionEvent, or
// nil when the event carries no turn-visible signal. No tool-call correlation
// (see classifyBusEventIndexed for the adapter's stateful entry point).
func classifyBusEvent(evt BusEvent) *scheduler.SessionEvent {
	return classifyBusEventIndexed(evt, nil)
}

// classifyBusEventIndexed is classifyBusEvent with the tool-call correlation
// index the adapter owns: a tool part's args are recorded, and a permission
// ask is annotated with the args of the call it belongs to.
func classifyBusEventIndexed(evt BusEvent, calls *toolCallIndex) *scheduler.SessionEvent {
	calls.observe(evt)
	sid, _ := evt.Props()["sessionID"].(string)
	base := func(kind, typ string) *scheduler.SessionEvent {
		if sid == "" {
			// Events without a session id are global (permission, error)
			// and flow to every conversation — the drain filters by session.
			sid = ""
		}
		return &scheduler.SessionEvent{Kind: kind, Type: typ, SessionID: sid}
	}
	switch evt.Type {
	case "session.idle":
		return base("idle", "")
	// ---- v2 terminal events -------------------------------------------------
	//
	// MEASURED against 2.0.26: a v1 stream ends with `session.idle` and reports failures with
	// `session.error`; a v2 stream ends with `session.execution.succeeded` / `.failed` and reports a
	// failed step with `session.step.failed`. Without these arms a v2 turn's completion and its
	// failure both classified as NOTHING, so the collector never learned the turn had ended — it sat
	// until its window expired while the model's reply sat in the session, which is the reported hang.
	case "session.execution.succeeded":
		// The turn's terminal event on v2 — the same signal `session.idle` is on v1.
		return base("idle", "")
	case "session.execution.failed":
		e := base("error", "")
		e.Text = busErrorMessage(evt.Props())
		return e
	case "session.step.failed":
		// A step failing is how v2 reports a provider/model error. The execution-level failure that
		// follows carries the same error; classifying the STEP one as well means the turn sees it
		// even if the execution event is missed (a late subscription, a dropped frame).
		e := base("error", "")
		e.Text = busErrorMessage(evt.Props())
		return e
	case "permission.asked":
		pid, _ := evt.Props()["id"].(string)
		e := base("permission", "")
		e.PermissionID = pid
		// Carry the raw ask through verbatim (opencode emits `permission`/
		// `title`, `patterns`/`pattern`, `metadata` and `callID` alongside
		// `id`). The consent layer answers the ask from THIS detail, so
		// dropping it here would turn a real ask back into a blind
		// auto-approve.
		e.Detail = evt.Props()
		// An MCP/host-suite ask carries NO detail of its own (patterns ["*"],
		// metadata {}), so the args of the call it gates are the only thing to
		// key and show. Attach them when the call was observed.
		if args := calls.argsForPermission(evt.Props()); args != nil {
			e.Detail = permissionDetailWithToolInput(evt.Props(), args)
		}
		return e
	case "session.error":
		msg := "opencode session error"
		if errObj, ok := evt.Props()["error"].(map[string]any); ok {
			if m, ok2 := errObj["message"].(string); ok2 && m != "" {
				msg = m
			}
		}
		e := base("error", "")
		e.Text = msg
		return e
	}
	// A tool call ISSUED but not yet resolved (MCP wedge).
	if tool, isStart := ToolStartFromBus(evt); isStart {
		e := base("tool_part", "tool")
		e.Text = tool
		return e
	}
	// Mid-generation token delta (liveness + live mirror).
	if delta, kind, ok := TokenDeltaInfoFromBus(evt); ok {
		e := base("delta", kind)
		e.Text = delta
		e.IsReasoning = kind == "reasoning"
		return e
	}
	// Completed telemetry part.
	if legacy, ok := LegacyEventFromBus(evt); ok {
		etype, _ := legacy["type"].(string)
		part, _ := legacy["part"].(map[string]any)
		e := base("part", etype)
		e.Part = part
		// Restore the call's identity that 2.x spread across separate events,
		// so every consumer of this part sees a real tool name and its args
		// rather than an anonymous call (see toolCallIndex.stamp).
		if etype == "tool_use" {
			calls.stamp(part)
		}
		if etype == "reasoning" {
			e.IsReasoning = true
			if t, ok2 := part["text"].(string); ok2 {
				e.Text = t
			}
		} else if etype == "text" {
			if t, ok2 := part["text"].(string); ok2 {
				e.Text = t
			}
		}
		return e
	}
	return nil
}

// QueuesPrompts implements scheduler.SessionQueuesPrompts: the opencode serve holds a prompt sent while a turn
// is in flight and answers it at that turn's next boundary (SendMessage's own contract), so an interjection is
// simply a send.
func (a *Adapter) QueuesPrompts() bool { return true }
