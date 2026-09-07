package opencode

import (
	"context"
	"errors"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ChatTurnClient is implemented on *Adapter (see adapter.go) by delegating
// to the host serve's SessionClient. This file holds the Subscribe event
// adapter that maps the raw opencode bus vocabulary onto the
// scheduler-neutral SessionEvent surface the Ask drain loop consumes.

// hostServeClient returns the always-on host serve's session client, or nil
// when the serve transport is unavailable.
func (a *Adapter) hostServeClient() *SessionClient {
	if a.host == nil {
		return nil
	}
	return a.host.Client()
}

// CreateConversationSession implements scheduler.ChatTurnClient, creating a
// fresh session on the host serve. An Ask turn has no project directory, so
// the request goes out unscoped (mirrors the historical chat behavior).
func (a *Adapter) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	c := a.hostServeClient()
	if c == nil {
		return "", errors.New("host opencode serve unavailable — Ask chat transport is disabled")
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
	c := a.hostServeClient()
	if c == nil {
		return errors.New("host opencode serve unavailable — Ask chat transport is disabled")
	}
	var parts []AttachmentPart
	if len(attachments) > 0 {
		parts = make([]AttachmentPart, 0, len(attachments))
		for _, at := range attachments {
			parts = append(parts, AttachmentPart{Name: at.Name, MimeType: at.MimeType, Data: at.Data})
		}
	}
	err := c.SendMessageWithAttachments(ctx, sessionID, system, modelRef, text, parts)
	if err != nil && errors.Is(err, ErrSessionNotFound) {
		return scheduler.ErrSessionNotFound
	}
	return err
}

// AbortConversationSession implements scheduler.ChatTurnClient, stopping a
// live turn on a session so the model stops generating now.
func (a *Adapter) AbortConversationSession(ctx context.Context, sessionID string) error {
	c := a.hostServeClient()
	if c == nil {
		return nil
	}
	return c.Abort(ctx, sessionID)
}

// ReplyPermission implements scheduler.ChatTurnClient, auto-approving a
// permission.asked signal.
func (a *Adapter) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	c := a.hostServeClient()
	if c == nil {
		return errors.New("host opencode serve unavailable — Ask chat transport is disabled")
	}
	return c.ReplyPermission(ctx, sessionID, permissionID)
}

// Subscribe implements scheduler.ChatTurnClient. It opens the host serve's
// /event SSE stream (multiplexing ALL sessions) and adapts each raw BusEvent
// onto the scheduler-neutral SessionEvent surface, applying the mid-generation
// event classification. The Ask drain loop consumes only SessionEvents — it
// never sees the raw opencode bus type.
func (a *Adapter) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	c := a.hostServeClient()
	if c == nil {
		return nil, errors.New("host opencode serve unavailable — Ask chat transport is disabled")
	}
	sub, err := c.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	ad := &sessionEventAdapter{sub: sub, events: make(chan scheduler.SessionEvent, 32)}
	ad.start()
	return ad, nil
}

// sessionEventAdapter adapts an opencode BusSub onto the scheduler.SessionBus
// surface, classifying events into SessionEvent kinds. Events() returns the
// SAME channel on every call (the drain loop re-reads it each select
// iteration), so the translating goroutine is started once at construction.
type sessionEventAdapter struct {
	sub    BusSub
	events chan scheduler.SessionEvent
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
				if se := classifyBusEvent(evt); se != nil {
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

func (a *ClientSessionAdapter) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	sub, err := a.sc.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	ad := &sessionEventAdapter{sub: sub, events: make(chan scheduler.SessionEvent, 32)}
	ad.start()
	return ad, nil
}

// Compile-time assertions for the plain-client adapter.
var _ scheduler.ChatTurnClient = (*ClientSessionAdapter)(nil)
var _ scheduler.SendTurnMessageWithAttachments = (*ClientSessionAdapter)(nil)

// NewSessionBusFromSub wraps an opencode BusSub as a scheduler.SessionBus,
// classifying every raw bus event onto the scheduler-neutral SessionEvent
// surface. Used by askorchicon tests (a fake opencode.BusSub feeding raw
// events) and the adapter/plain-client Subscribe paths.
func NewSessionBusFromSub(sub BusSub) scheduler.SessionBus {
	ad := &sessionEventAdapter{sub: sub, events: make(chan scheduler.SessionEvent, 32)}
	ad.start()
	return ad
}

// classifyBusEvent maps one raw opencode bus event onto a SessionEvent, or
// nil when the event carries no turn-visible signal.
func classifyBusEvent(evt BusEvent) *scheduler.SessionEvent {
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
