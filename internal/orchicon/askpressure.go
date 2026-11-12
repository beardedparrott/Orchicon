package orchicon

// askpressure.go — the proactive context-pressure gate for native Ask turns.

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

// promptOccupancy is the FULL input side of one provider round: the fresh, cache-read and
// cache-write buckets summed.
func promptOccupancy(u Usage) int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// recordAskPromptTokens stores the newest MEASURED prompt size for a session.
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

// askLastPromptTokens returns the newest measured prompt size for a session, or 0 when nothing has
// been measured yet (a fresh session, or a server restart that lost the in-memory sample).
func (b *NativeBridge) askLastPromptTokens(sessionID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.askPromptTokens[sessionID]
}

// resolveAskContextWindow resolves the conversation model's real context window from its provider,
// cached once per session (a ListModels call per turn would be wasteful.
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

// resolveAskPricing resolves the conversation model's PRICING, cached once per session through the
// same provider ListModels the context window uses.
func (b *NativeBridge) resolveAskPricing(ctx context.Context, prov Provider, sessionID, model string) *Pricing {
	b.mu.Lock()
	if p, ok := b.askPricing[sessionID]; ok {
		b.mu.Unlock()
		return p
	}
	b.mu.Unlock()
	if prov == nil {
		return nil
	}
	models, err := prov.ListModels(ctx)
	if err != nil {
		return nil
	}
	for i := range models {
		if models[i].ID != model {
			continue
		}
		p := models[i].Pricing
		b.mu.Lock()
		if b.askPricing == nil {
			b.askPricing = map[string]*Pricing{}
		}
		b.askPricing[sessionID] = p
		b.mu.Unlock()
		return p
	}
	// The model is not in the provider's own list. Resolved-and-absent is cached
	// too: a model the provider does not describe will not appear mid-conversation.
	b.mu.Lock()
	if b.askPricing == nil {
		b.askPricing = map[string]*Pricing{}
	}
	b.askPricing[sessionID] = nil
	b.mu.Unlock()
	return nil
}

// maybeCompactForPressure compacts a native Ask conversation BEFORE its next turn when its measured
// prompt size has crossed the configured fraction of the model's live context window.
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
		// The measurement describes the PRE-compaction history.
		b.recordAskPromptTokens(sessionID, 0)
		// AND TELL THE TRANSCRIPT.
		b.reportAskCompaction(ctx, conversationID, sessionID, "pressure", res)
	}
}
