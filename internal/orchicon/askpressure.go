package orchicon

// askpressure.go — the proactive context-pressure gate for native Ask turns.
//
// The reactive path (askreduce.go) rescues a turn that is ALREADY failing. This
// is the path that avoids reaching that state: before dispatching a turn, compare
// the conversation's measured prompt size against the model's REAL context
// window and compact when it crosses a threshold.
//
// Both halves of that comparison are deliberately live, never guessed:
//
//   - The NUMERATOR is a real measurement. It comes from the provider's own
//     reported usage on the previous turn (the Finish event's Usage, recorded
//     per round by askUsageSink in chatturn.go). Nothing is estimated from
//     character counts.
//   - The DENOMINATOR is a live window hint, resolved the same way executions
//     resolve theirs (orchicon/contextwindow.go): the bound provider's
//     ListModels, which merges probe-derived context, Ollama native /api/show
//     metadata and catalog enrichment.
//
// When EITHER half is missing the gate stays DISARMED and says so in the log.
// That is the platform's existing rule for context-window triggers
// (contextwindow.go: "compaction must never guess a window"), and it is why a
// model with no live hint simply never auto-compacts — operator-visible, never a
// silent wrong decision. Manual compaction is unaffected (the
// CompactConversation RPC and /compact always work).

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// defaultAskPressureFrac is the fraction of the model's context window at which
// a native Ask conversation is compacted before its next turn.
const defaultAskPressureFrac = 0.95

// askPressureEnv overrides defaultAskPressureFrac. An unparseable or
// out-of-range value falls back to the default rather than disarming silently.
const askPressureEnv = "ORCHICON_ASK_COMPACT_PRESSURE_FRAC"

// askPressureThreshold resolves the trigger fraction (>0 and <=1).
//
// NOTE: the worker compaction ladder reads its pressure fraction from the
// tenant/worker budget JSON (context_compaction.pressure_frac, default 0.8),
// which the Ask path has no equivalent of — a chat turn carries no work item and
// no merged budget. An env knob is the honest interim: it is explicit,
// operator-visible and globally consistent. Plumbing a tenant-level Ask
// compaction policy is a separate change, not a silent default here.
func askPressureThreshold() float64 {
	raw := strings.TrimSpace(os.Getenv(askPressureEnv))
	if raw == "" {
		return defaultAskPressureFrac
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 || v > 1 {
		return defaultAskPressureFrac
	}
	return v
}

// recordAskPromptTokens stores the newest MEASURED prompt size for a session.
// Called from the per-round usage sink, so the value always reflects a real
// provider report. Guarded by mu.
func (b *NativeBridge) recordAskPromptTokens(sessionID string, promptTokens int64) {
	if sessionID == "" || promptTokens <= 0 {
		return
	}
	b.mu.Lock()
	if b.askPromptTokens == nil {
		b.askPromptTokens = map[string]int64{}
	}
	b.askPromptTokens[sessionID] = promptTokens
	b.mu.Unlock()
}

// askLastPromptTokens returns the newest measured prompt size for a session, or
// 0 when nothing has been measured yet (a fresh session, or a server restart
// that lost the in-memory sample). 0 disarms the gate — never estimated.
func (b *NativeBridge) askLastPromptTokens(sessionID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.askPromptTokens[sessionID]
}

// resolveAskContextWindow resolves the conversation model's real context window
// from its provider, cached once per session (a ListModels call per turn would be
// wasteful, and the window does not change under a live session). 0 means "no
// live hint" and carries a reason for the log.
func (b *NativeBridge) resolveAskContextWindow(ctx context.Context, prov Provider, sessionID, model string) (int64, string) {
	b.mu.Lock()
	if w, ok := b.askWindowTokens[sessionID]; ok {
		b.mu.Unlock()
		return w, "cached"
	}
	b.mu.Unlock()
	if prov == nil {
		return 0, NoContextWindow + ":provider_unset"
	}
	models, err := prov.ListModels(ctx)
	if err != nil {
		// Do NOT cache a lookup failure: a transient provider error must not
		// disarm the gate for the rest of the session.
		return 0, NoContextWindow + ":list_models_error"
	}
	for i := range models {
		if models[i].ID != model {
			continue
		}
		w := models[i].Context
		b.mu.Lock()
		if b.askWindowTokens == nil {
			b.askWindowTokens = map[string]int64{}
		}
		b.askWindowTokens[sessionID] = w
		b.mu.Unlock()
		if w <= 0 {
			return 0, NoContextWindow + ":context_zero"
		}
		return w, "live"
	}
	return 0, NoContextWindow + ":model_not_found:" + model
}

// maybeCompactForPressure compacts a native Ask conversation BEFORE its next turn
// when its measured prompt size has crossed the configured fraction of the
// model's live context window.
//
// Best-effort by design: a compaction failure never blocks the turn (the turn
// still runs, and the reactive path in askreduce.go remains the backstop). Any
// decision NOT to compact is logged with its reason, so a disarmed gate is
// visible rather than mysterious.
func (b *NativeBridge) maybeCompactForPressure(ctx context.Context, prov Provider, conversationID, sessionID, modelRef, model string) {
	last := b.askLastPromptTokens(sessionID)
	if last <= 0 {
		return // nothing measured yet — the gate cannot fire on a guess
	}
	window, reason := b.resolveAskContextWindow(ctx, prov, sessionID, model)
	if window <= 0 {
		b.log.Info("orchicon: Ask pressure gate disarmed — no live context-window hint",
			"session", sessionID,
			"model", model,
			"prompt_tokens", last,
			"reason", reason)
		return
	}
	threshold := askPressureThreshold()
	frac := float64(last) / float64(window)
	if frac < threshold {
		return
	}
	b.log.Warn("orchicon: Ask context pressure over threshold — compacting before the turn",
		"session", sessionID,
		"model", model,
		"prompt_tokens", last,
		"context_window", window,
		"fraction", frac,
		"threshold", threshold)
	res, err := b.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: conversationID,
		SessionID:      sessionID,
		ModelRef:       modelRef,
		Reason:         "pressure",
	})
	if err != nil {
		b.log.Warn("orchicon: Ask pressure compaction failed — continuing on the existing history",
			"session", sessionID, "error", err)
		return
	}
	b.log.Warn("orchicon: Ask pressure compaction result",
		"session", sessionID, "compacted", res.Compacted, "detail", res.Detail)
	if res.Compacted {
		// The measurement describes the PRE-compaction history. Drop it so the
		// next turn cannot re-trigger on a stale number; a fresh measurement
		// arrives from that turn's own usage report.
		b.recordAskPromptTokens(sessionID, 0)
	}
}
