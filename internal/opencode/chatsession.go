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

// ChatTurnClient is implemented on *Adapter (see adapter.go) by delegating to the host serve's
// SessionClient.

// hostServeClient returns the host serve's session client, or nil when the serve transport is
// unavailable.
func (a *Adapter) hostServeClient() *SessionClient {
	h := chatHost(a.askHost, a.host)
	if h == nil {
		return nil
	}
	return h.Client()
}

// chatHost selects the serve an Ask conversation turn runs on.
func chatHost(ask, exec *HostServe) *HostServe {
	if ask != nil {
		return ask
	}
	return exec
}

// ensureHostServeClient returns the host serve's session client, starting the serve on FIRST demand
// (AC 2).
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

// SessionOwnerKind implements scheduler.SessionOwnerKind: sessions created by this adapter belong
// to the opencode kind.
func (a *Adapter) SessionOwnerKind() string { return "opencode" }

// CreateConversationSession implements scheduler.ChatTurnClient, creating a fresh session on the
// host serve.
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

// SendTurnMessage implements scheduler.ChatTurnClient, appending a user message to a conversation
// session.
func (a *Adapter) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return a.SendTurnMessageWithAttachments(ctx, conversationID, sessionID, system, modelRef, text, nil)
}

// SendTurnMessageWithAttachments implements scheduler.SendTurnMessageWithAttachments, the optional
// attachment-aware sender for Ask turns.
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
	// ASK: the prompt is delivered on the turn that follows CREATION, and only there.
	err = c.sendMessage(ctx, sessionID, system, modelRef, text, parts, c.consumePromptPending(sessionID))
	if err != nil && errors.Is(err, ErrSessionNotFound) {
		return scheduler.ErrSessionNotFound
	}
	return err
}

// AbortConversationSession implements scheduler.ChatTurnClient, stopping a
// live turn on a session so the model stops generating now.
func (a *Adapter) AbortConversationSession(ctx context.Context, sessionID string) error {
	// Abort is BEST-EFFORT: a turn can only be live on a serve that was already started, so a serve
	// that cannot be reached here has nothing to abort.
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

// Subscribe implements scheduler.ChatTurnClient.
func (a *Adapter) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	c, err := a.ensureHostServeClient(ctx)
	if err != nil {
		return nil, err
	}
	sub, err := c.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	// The conversation's directory, so the ledger observes the files THIS session touched — resolved by
	// the SERVER's installed resolver, not by reaching for the process's working directory.
	dir := ""
	if a.askDirResolver != nil {
		dir = a.askDirResolver(ctx, conversationID)
	}
	ad := newSessionEventAdapterWithLedger(sub, conversationID, dir, tenant.FromContext(ctx), a.askFileEdits)
	return ad, nil
}

// sessionEventAdapter adapts an opencode BusSub onto the scheduler.SessionBus surface, classifying
// events into SessionEvent kinds.
type sessionEventAdapter struct {
	sub    BusSub
	events chan scheduler.SessionEvent
	// calls correlates an in-flight tool call's args with the permission.asked
	// raised for it (see toolCallIndex).
	calls *toolCallIndex
	// The diff-ledger context: the conversation this adapter is serving, the session's own directory,
	// and the hook.
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

// ClientSessionAdapter adapts a *SessionClient onto the scheduler.ChatTurnClient surface, so a
// plain host serve client (no *Adapter in the loop) can drive Ask conversations.
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

// QueuesPrompts implements scheduler.SessionQueuesPrompts and answers TRUE — the same answer as the
// primary Adapter, because it is the same serve underneath.
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

// CompactConversationSession implements scheduler.ChatCompactor: the serve owns this conversation's
// session, so compaction is a summarize of that session in place.
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

// NewSessionBusFromSub wraps an opencode BusSub as a scheduler.SessionBus, classifying every raw
// bus event onto the scheduler-neutral SessionEvent surface.
func NewSessionBusFromSub(sub BusSub) scheduler.SessionBus {
	return newSessionEventAdapter(sub)
}

// newSessionEventAdapter builds a started adapter carrying a fresh tool-call
// correlation index.
func newSessionEventAdapter(sub BusSub) *sessionEventAdapter {
	return newSessionEventAdapterWithLedger(sub, "", "", "", nil)
}

// newSessionEventAdapterWithLedger is newSessionEventAdapter for an Ask conversation, which
// additionally ledgers the file edits its tool calls make.
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

// toolCallIndex correlates an in-flight tool call's ARGS with the permission.asked raised for that
// call.
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

// observe records the args of a `message.part.updated` tool part, if that is what the event is.
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

// FILLS MISSING FIELDS ONLY, so a v1 part that already carries both is untouched.
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

// stamp fills a RESOLVED tool part's missing identity from what the index recorded for the same
// call id: the tool NAME and the call's INPUT.
func (x *toolCallIndex) stamp(part map[string]any) {
	if x == nil || part == nil {
		return
	}
	cid, _ := part["callID"].(string)
	if cid == "" {
		return
	}
	// NEITHER ENTRY IS CONSUMED.
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

// argsForPermission resolves the args of the tool call a permission event's properties name
// (`tool.callID`), or nil when the ask carries no call id or the call was not observed (a
// subscription.
func (x *toolCallIndex) argsForPermission(props map[string]any) map[string]any {
	tool, _ := props["tool"].(map[string]any)
	if tool == nil {
		return nil
	}
	cid, _ := tool["callID"].(string)
	return x.get(cid)
}

// permissionDetailWithToolInput copies a permission event's properties with the correlated
// tool-call args attached as `toolInput`.
func permissionDetailWithToolInput(props, args map[string]any) map[string]any {
	out := make(map[string]any, len(props)+1)
	for k, v := range props {
		out[k] = v
	}
	out["toolInput"] = args
	return out
}

// busErrorMessage extracts the human-readable reason from an error-bearing bus event, whichever
// surface shaped it.
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

// classifyBusEvent maps one raw opencode bus event onto a SessionEvent, or nil when the event
// carries no turn-visible signal.
func classifyBusEvent(evt BusEvent) *scheduler.SessionEvent {
	return classifyBusEventIndexed(evt, nil)
}

// classifyBusEventIndexed is classifyBusEvent with the tool-call correlation index the adapter
// owns: a tool part's args are recorded.
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
		// A step failing is how v2 reports a provider/model error.
		e := base("error", "")
		e.Text = busErrorMessage(evt.Props())
		return e
	case "permission.asked":
		pid, _ := evt.Props()["id"].(string)
		e := base("permission", "")
		e.PermissionID = pid
		// Carry the raw ask through verbatim (opencode emits `permission`/ `title`, `patterns`/`pattern`,
		// `metadata` and `callID` alongside `id`).
		e.Detail = evt.Props()
		// An MCP/host-suite ask carries NO detail of its own (patterns ["*"], metadata {}), so the args of
		// the call it gates are the only thing to key and show.
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
		// Restore the call's identity that 2.x spread across separate events, so every consumer of this
		// part sees a real tool name and its args rather than an anonymous call (see toolCallIndex.stamp).
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

// QueuesPrompts implements scheduler.SessionQueuesPrompts: the opencode serve holds a prompt sent
// while a turn is in flight and answers it at that turn's next boundary (SendMessage's own.
func (a *Adapter) QueuesPrompts() bool { return true }
