package opencode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ContinueSessionOpts carries everything needed to run a one-shot
// follow-up question against a worker's session WITHOUT creating a new
// execution or work item.
type ContinueSessionOpts struct {
	ExecutionID  string
	TenantID     string
	SystemPrompt string // the worker's composed system prompt (per-message)
	ModelRef     string
	ProjectDir   string
	Message      string // the user's follow-up question
	// Context is the durable-transcript context used to seed a FRESH
	// session when the original serve/session is no longer reachable.
	Context string
	// Original session identity (from the transcript's session_info part);
	// re-attached when the serve is still reachable for real continuity.
	SessionID     string
	ServeURL      string
	ServePassword string
	// StartSeq is the next transcript seq to use (the last existing part's
	// seq + 1), so the follow-up entries append after the original run.
	StartSeq int64
}

// ContinueSession runs the follow-up. It returns the assistant's reply.
// The follow-up and the reply are recorded into the durable transcript so
// the session chat shows the exchange. No new execution/work item is ever
// created.
func (a *Adapter) ContinueSession(ctx context.Context, opts ContinueSessionOpts) (string, error) {
	client, sessionID, reuse := a.followUpSession(ctx, opts)
	if client == nil {
		return "", fmt.Errorf("no opencode serve available for the follow-up")
	}

	// Re-attached session: send only the question (the session already
	// carries the worker identity + full context). Fresh session: seed the
	// transcript context + the question as the goal.
	msg := opts.Message
	if !reuse {
		msg = opts.Context + "\n\n# Follow-up question\n\n" + opts.Message
	}

	// Record the user's question into the durable transcript UP FRONT so the
	// session chat shows the bubble immediately while the model works — the
	// reply is written separately once it lands. seq = StartSeq; a fresh
	// session also records its session_info right after, so the reply still
	// appends AFTER both.
	seq := opts.StartSeq
	if seq <= 0 {
		seq = 1
	}
	if a.sessionStore != nil {
		parts := []db.SessionPart{
			{
				ExecutionID: opts.ExecutionID,
				TenantID:    opts.TenantID,
				Seq:         seq,
				Kind:        db.SessionPartUserMessage,
				Payload:     db.MarshalPartPayload(map[string]any{"text": opts.Message, "source": "follow_up"}),
			},
		}
		seq++
		if !reuse {
			parts = append(parts, db.SessionPart{
				ExecutionID: opts.ExecutionID,
				TenantID:    opts.TenantID,
				Seq:         seq,
				Kind:        db.SessionPartSessionInfo,
				Payload:     db.MarshalPartPayload(map[string]any{"session_id": sessionID, "serve_url": client.BaseURL()}),
			})
			seq++
		}
		if err := a.sessionStore(ctx, opts.ExecutionID, opts.TenantID, parts); err != nil {
			a.log.Warn("follow-up transcript write failed", "execution", opts.ExecutionID, "error", err)
		}
	}

	reply, err := collectReply(ctx, client, sessionID, opts.SystemPrompt, opts.ModelRef, msg)
	if err != nil {
		return "", err
	}

	// Record the assistant's reply into the durable transcript (append after
	// the up-front user message / session_info).
	if a.sessionStore != nil && reply != "" {
		parts := []db.SessionPart{
			{
				ExecutionID: opts.ExecutionID,
				TenantID:    opts.TenantID,
				Seq:         seq,
				Kind:        db.SessionPartText,
				Payload:     db.MarshalPartPayload(map[string]any{"part": map[string]any{"type": "text", "text": reply}}),
			},
		}
		if err := a.sessionStore(ctx, opts.ExecutionID, opts.TenantID, parts); err != nil {
			a.log.Warn("follow-up transcript write failed", "execution", opts.ExecutionID, "error", err)
		}
	}
	return reply, nil
}

// followUpSession resolves the client + session for a follow-up: the
// original serve/session when still reachable (real continuity), else a
// fresh session on the host serve. Returns (client, sessionID, reused).
func (a *Adapter) followUpSession(ctx context.Context, opts ContinueSessionOpts) (*SessionClient, string, bool) {
	if opts.ServeURL != "" && opts.SessionID != "" {
		orig := NewSessionClient(opts.ServeURL, opts.ServePassword, opts.ProjectDir)
		if orig.Healthy(ctx) {
			return orig, opts.SessionID, true
		}
	}
	if a.host != nil {
		if client := a.host.Client(); client != nil {
			sid, err := client.CreateSession(ctx, opts.ExecutionID+"-followup")
			if err == nil && sid != "" {
				return client, sid, false
			}
			a.log.Warn("follow-up session create failed", "execution", opts.ExecutionID, "error", err)
		}
	}
	return nil, "", false
}

// collectReply subscribes to a session, sends a message, and waits for the
// reply (bounded). It returns the assistant's accumulated text. Used by
// the one-shot follow-up flow — the serve is warm, so a generous window
// (5m) covers a multi-tool answer.
func collectReply(ctx context.Context, client *SessionClient, sessionID, system, modelRef, message string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("no opencode serve available for the follow-up")
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sub, err := client.Subscribe(subCtx)
	if err != nil {
		return "", fmt.Errorf("follow-up subscribe: %w", err)
	}
	defer sub.Close()

	if err := client.SendMessage(subCtx, sessionID, system, modelRef, message); err != nil {
		return "", fmt.Errorf("follow-up send: %w", err)
	}

	var reply strings.Builder
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-subCtx.Done():
			return reply.String(), subCtx.Err()
		case <-deadline.C:
			return reply.String(), nil
		case evt, ok := <-sub.Events():
			if !ok {
				return reply.String(), nil
			}
			if sid, _ := evt.Properties["sessionID"].(string); sid != "" && sid != sessionID {
				continue
			}
			switch evt.Type {
			case "session.idle":
				return reply.String(), nil
			case "permission.asked":
				if pid, _ := evt.Properties["id"].(string); pid != "" {
					go func() { _ = client.ReplyPermission(subCtx, sessionID, pid) }()
				}
			default:
				if legacy, ok := legacyEventFromBus(evt); ok {
					if t, _ := legacy["type"].(string); t == "text" {
						if part, ok2 := legacy["part"].(map[string]any); ok2 {
							if text, ok3 := part["text"].(string); ok3 && text != "" {
								reply.WriteString(text)
								reply.WriteString("\n\n")
							}
						}
					}
				}
			}
		}
	}
}
