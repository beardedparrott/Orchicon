package orchicon

// askcompact.go — context compaction for the NATIVE Ask transport
// (scheduler.ChatCompactor). Not to be confused with compaction.go, which is
// the WORKER loop's budget-ladder gate.
//
// WHY this exists: the native transports are SESSIONLESS — every turn re-sends
// the whole accumulated history as context (see dispatchTurnMessage). That
// makes an over-limit conversation unrecoverable: once the history exceeds the
// model's window, EVERY send 400s and the conversation is permanently wedged.
// opencode never reaches that state because it compacts proactively and can
// summarize its own server-side session. A sessionless adapter has no server
// session to summarize, so it must rewrite the history it owns.
//
// The two-stage shape is what makes an ALREADY-over-limit session recoverable:
//
//  1. REDUCE (deterministic, no model call, cannot fail). Measured on a live
//     wedged conversation: 46% of bytes were images, 37% tool results, and
//     only 3% actual text. Dropping images to markers and tool payloads to
//     one-liners brings that transcript back inside ANY window, so stage 2 is
//     a single bounded request instead of an impossible one.
//  2. SUMMARIZE (one model call, on the conversation's own model). The reduced
//     transcript becomes a narrative summary, and the history becomes
//     [summary] + [most recent turns].
//
// Stage 1 is the whole reason this can run AFTER the limit: opencode summarizes
// before the window fills, so its summarize request always fits. We must shrink
// first, because we may be called precisely because it no longer fits.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the native bridge can compact Ask conversations.
var _ scheduler.ChatCompactor = (*NativeBridge)(nil)

const (
	// askCompactTailMessages is how many of the most recent messages are kept
	// VERBATIM after the summary, so the immediate conversational thread (and
	// any in-flight work) survives the collapse. Mirrors the tenant
	// context_recent_turns default (6) used by the worker compaction policy.
	askCompactTailMessages = 6
	// askCompactMinMessages declines compaction on a short conversation: a
	// lossy collapse of a handful of messages costs detail and saves nothing.
	askCompactMinMessages = 12
	// askCompactBytesPerToken is the coarse chars->tokens divisor used ONLY to
	// report a rough size in detail. It never populates the measured token
	// fields and never arms a gate.
	askCompactBytesPerToken = 4
)

// askCompactSummaryInstruction is the summarization prompt. It is written to
// preserve the WORKING STATE (decisions, identifiers, next steps) rather than
// to produce prose, because the summary replaces real history and is the only
// surviving record of everything before it.
const askCompactSummaryInstruction = `You are compacting a long-running conversation so it can continue in a smaller context.

Produce a summary that lets the conversation resume with no loss of working state. Cover, in this order:

1. What the user is trying to achieve — the goal, in their terms.
2. What has been decided — decisions, constraints, and preferences the user stated.
3. What has been done — completed work, with concrete identifiers (file paths, branch names, commit ids, entity ids).
4. What is in flight — anything unfinished, and the exact next step.
5. Open questions or blockers — anything unresolved that needs the user.

Rules:
- Be specific and concrete. Preserve identifiers verbatim (paths, ids, names, numbers).
- Do NOT invent anything that is not in the transcript.
- Do NOT add pleasantries or meta-commentary about summarizing.
- Image content and tool output were dropped BEFORE you saw this transcript. If something important is clearly missing, note it under open questions rather than guessing.

Write the summary now.`

// CompactConversationSession implements scheduler.ChatCompactor for the native
// (sessionless) transport: reduce, summarize, then replace the history this
// adapter re-sends every turn.
func (b *NativeBridge) CompactConversationSession(ctx context.Context, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	sid := opts.SessionID
	if sid == "" && opts.ConversationID != "" {
		sid = scheduler.NativeSessionIDPrefix + opts.ConversationID
	}
	if sid == "" {
		return scheduler.ChatCompaction{}, errors.New("orchicon bridge: compaction requires a conversation or session id")
	}

	// Snapshot the history using the same load path a turn uses, so compaction
	// never operates on a different view than the one that gets re-sent.
	b.mu.Lock()
	history := append([]Message(nil), b.chatHistory[sid]...)
	if len(history) == 0 {
		history = append([]Message(nil), b.loadAskHistoryLocked(sid)...)
	}
	b.mu.Unlock()
	history = sanitizeChatHistory(history)

	if len(history) == 0 {
		return scheduler.ChatCompaction{Detail: "nothing to compact — this conversation has no history yet"}, nil
	}
	if len(history) < askCompactMinMessages {
		return scheduler.ChatCompaction{
			Detail: fmt.Sprintf("nothing to compact — only %d messages so far", len(history)),
		}, nil
	}

	// Stage 1: REDUCE. Deterministic, model-free, cannot fail.
	transcript, bytesBefore, bytesAfter := reduceAskHistory(history)
	if strings.TrimSpace(transcript) == "" {
		return scheduler.ChatCompaction{Detail: "nothing to compact — the history carries no readable text"}, nil
	}

	// Stage 2: SUMMARIZE on the conversation's own model.
	summary, err := b.summarizeTranscript(ctx, opts, transcript)
	if err != nil {
		return scheduler.ChatCompaction{}, fmt.Errorf("orchicon bridge: summarize conversation: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		// Leave the history untouched rather than replacing it with nothing.
		return scheduler.ChatCompaction{}, errors.New("orchicon bridge: the model returned an empty summary — history left untouched")
	}

	tail := history
	if len(tail) > askCompactTailMessages {
		tail = tail[len(tail)-askCompactTailMessages:]
	}
	next := make([]Message, 0, len(tail)+1)
	next = append(next, Message{Role: RoleAssistant, Content: []Content{{Text: compactedHistoryMarker(summary)}}})
	next = append(next, tail...)

	b.mu.Lock()
	// Archive the pre-collapse history beside the live file: the summary is
	// lossy by design, so this keeps the detail recoverable by hand.
	b.archiveAskHistoryLocked(sid)
	b.chatHistory[sid] = next
	b.persistAskHistoryLocked(sid)
	b.mu.Unlock()

	return scheduler.ChatCompaction{
		Compacted: true,
		Detail: fmt.Sprintf(
			"compacted %d messages into 1 summary + %d recent messages (transcript reduced from %s to %s before summarizing)",
			len(history), len(tail), humanBytes(bytesBefore), humanBytes(bytesAfter)),
		Summary: summary,
	}, nil
}

// summarizeTranscript runs ONE model turn over the reduced transcript and
// returns its text. Deliberately no tools and no history: a summarize turn must
// not start doing work. The turn is cancellable through ctx, and a pre-stream
// failure (auth/connect/limits) is returned so the caller reports a real error
// instead of silently leaving the conversation wedged.
func (b *NativeBridge) summarizeTranscript(ctx context.Context, opts scheduler.CompactConversationOpts, transcript string) (string, error) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return "", errors.New("no tenant in context — cannot resolve the summarize provider")
	}
	providerID, model, ok := adapter.SplitForServe(opts.ModelRef)
	if !ok || providerID == "" || model == "" {
		return "", fmt.Errorf("model ref %q has no provider/model to summarize with", opts.ModelRef)
	}
	if b.resolver == nil {
		return "", errors.New("no provider resolver for the summarize turn")
	}
	prov, err := b.resolver.Get(ctx, tenantID, providerID)
	if err != nil {
		return "", fmt.Errorf("resolve provider: %w", err)
	}

	userText := "Here is the conversation transcript to summarize:\n\n" + transcript
	req := TurnRequest{
		Model:        model,
		System:       []SystemBlock{{Text: askCompactSummaryInstruction, Cache: true}},
		Messages:     []Message{{Role: RoleUser, Content: []Content{{Text: &userText}}}},
		MaxTokens:    maxOutputTokens(),
		CacheControl: CacheControlSystemAndTools,
		// Same stable per-conversation id the turns use (providers that key
		// off x-opencode-session require it).
		SessionID: opts.ConversationID,
	}
	stream, err := prov.StreamTurn(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var out strings.Builder
	for {
		evt, ok, err := stream.Next(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		if delta, isText := evt.(TextDelta); isText {
			out.WriteString(delta.Text)
		}
	}
	return out.String(), nil
}

// reduceAskHistory flattens a full history into a text-only transcript for
// summarization, returning it plus the before/after byte sizes. Deterministic
// and model-free — which is the point: it cannot fail, and it is the only stage
// that can run when the history already exceeds the window.
//
// Text is preserved in full; images and tool payloads are replaced by explicit
// markers (never silently dropped, so the summarizer can say what is missing).
func reduceAskHistory(history []Message) (string, int, int) {
	var before, after int
	var b strings.Builder
	for _, m := range history {
		role := "USER"
		switch m.Role {
		case RoleAssistant:
			role = "ASSISTANT"
		case RoleTool:
			role = "TOOL"
		}
		for _, c := range m.Content {
			switch {
			case c.Text != nil:
				before += len(*c.Text)
				t := strings.TrimSpace(*c.Text)
				if t == "" {
					continue
				}
				b.WriteString(role + ": " + t + "\n")
				after += len(t)
			case c.Image != nil:
				before += len(*c.Image)
				line := role + ": [an image was shared or shown here — image content dropped during compaction]"
				b.WriteString(line + "\n")
				after += len(line)
			case c.ToolUse != nil:
				before += len(c.ToolUse.ArgsJSON)
				line := role + ": [called the tool " + c.ToolUse.Name + " — arguments dropped during compaction]"
				b.WriteString(line + "\n")
				after += len(line)
			case c.ToolResult != nil:
				before += len(c.ToolResult.Content)
				status := "returned output"
				if c.ToolResult.IsError {
					status = "FAILED"
				}
				line := role + ": [a tool call " + status + " — output dropped during compaction]"
				b.WriteString(line + "\n")
				after += len(line)
			}
		}
	}
	return b.String(), before, after
}

// compactedHistoryMarker frames a summary as the single assistant message that
// replaces the collapsed history. The framing matters: the model must read it
// as context it already established, not as a fresh instruction.
func compactedHistoryMarker(summary string) *string {
	s := "[Earlier conversation compacted to save context. Summary of everything before this point:]\n\n" +
		summary +
		"\n\n[End of compacted summary. The messages after this one are the most recent turns, verbatim.]"
	return &s
}

// archiveAskHistoryLocked copies the live history file to a timestamped sibling
// before compaction replaces it. Callers must hold b.mu. Best-effort: the
// summary is lossy by design, so the pre-collapse transcript is kept so an
// operator can recover detail the summary dropped.
func (b *NativeBridge) archiveAskHistoryLocked(sessionID string) {
	dir := b.askHistoryDir
	if dir == "" {
		return
	}
	base := filepath.Join(dir, askHistoryFilename(sessionID)+".json")
	raw, err := os.ReadFile(base)
	if err != nil {
		return // no live file yet — nothing to archive
	}
	dst := fmt.Sprintf("%s.compacted-%s.bak", base, time.Now().UTC().Format("20060102T150405"))
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		b.log.Warn("orchicon: ask history archive failed", "session", sessionID, "error", err)
	}
}

// humanBytes renders a byte count for a user-facing notice.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
