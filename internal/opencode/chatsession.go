package opencode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/scheduler"
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
func (a *Adapter) ensureHostServeClient(ctx context.Context) (*SessionClient, error) {
	h := chatHost(a.askHost, a.host)
	if h == nil {
		return nil, errors.New("host opencode serve unavailable — Ask chat transport is disabled")
	}
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
	err = c.SendMessageWithAttachments(ctx, sessionID, system, modelRef, text, parts)
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
	ad := newSessionEventAdapter(sub)
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
	err := a.sc.SendMessageWithAttachments(ctx, sessionID, system, modelRef, text, parts)
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
	ad := &sessionEventAdapter{
		sub:    sub,
		events: make(chan scheduler.SessionEvent, 32),
		calls:  newToolCallIndex(),
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
}

func newToolCallIndex() *toolCallIndex {
	return &toolCallIndex{args: make(map[string]map[string]any)}
}

// observe records the args of a `message.part.updated` tool part, if that is
// what the event is. Nil-receiver safe: an adapter built without correlation
// passes nil and simply records nothing.
//
// Entries MERGE rather than replace: the serve streams a tool part's input
// progressively (a later update carries the rest of the args), and an early
// update can carry an empty input. Later keys win.
func (x *toolCallIndex) observe(evt BusEvent) {
	if x == nil || evt.Type != "message.part.updated" {
		return
	}
	part, _ := evt.Properties["part"].(map[string]any)
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
	sid, _ := evt.Properties["sessionID"].(string)
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
	case "permission.asked":
		pid, _ := evt.Properties["id"].(string)
		e := base("permission", "")
		e.PermissionID = pid
		// Carry the raw ask through verbatim (opencode emits `permission`/
		// `title`, `patterns`/`pattern`, `metadata` and `callID` alongside
		// `id`). The consent layer answers the ask from THIS detail, so
		// dropping it here would turn a real ask back into a blind
		// auto-approve.
		e.Detail = evt.Properties
		// An MCP/host-suite ask carries NO detail of its own (patterns ["*"],
		// metadata {}), so the args of the call it gates are the only thing to
		// key and show. Attach them when the call was observed.
		if args := calls.argsForPermission(evt.Properties); args != nil {
			e.Detail = permissionDetailWithToolInput(evt.Properties, args)
		}
		return e
	case "session.error":
		msg := "opencode session error"
		if errObj, ok := evt.Properties["error"].(map[string]any); ok {
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
