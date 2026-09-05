package orchicon

// contextwindow.go resolves the selected model's TRUE context window (and
// pricing) for the native session — LIVE hints only (operator directive:
// compaction must never guess a window). Resolution order mirrors
// sourcing's probe-or-nothing stance:
//
//  1. The bound provider's ListModels result for this model — sourcing
//     already merges probe-derived context (provider /models
//     context_length / max_model_len), Ollama native /api/show true
//     metadata, catalog-ENRICHED ModelInfo.Context (the catalog is
//     metadata-enrichment, never a model-id source) and manual entries
//     into ModelInfo.Context. Context == 0 everywhere means NO live hint.
//
// When no hint resolves, the resolver returns ok=false with a reason and
// window-trigger compaction stays DISARMED (the budget gate remains).
// Pricing resolves from the SAME live ListModels result (catalog/probe-
// enriched ModelInfo.Pricing) and never synthesizes a figure.

import (
	"context"
	"fmt"
)

// ContextWindowHint is the result of a true-context-window resolution.
type ContextWindowHint struct {
	// Tokens is the model's real context window in tokens (0 = unknown).
	Tokens int64
	// Ok is true only when a live hint resolved.
	Ok bool
	// Reason explains a no-hint result (logged, never guessed over).
	Reason string
}

// NoContextWindow is the reason recorded when no live hint exists.
const NoContextWindow = "no_context_window"

// resolveModelInfo returns the session model's LIVE ModelInfo (context +
// pricing), resolving it once per session through the bound provider's
// ListModels (never per-turn probed). model may be nil when no live hint
// exists; hint carries the no-hint reason.
func (s *Session) resolveModelInfo(ctx context.Context) (*ModelInfo, ContextWindowHint) {
	s.windowMu.Lock()
	defer s.windowMu.Unlock()
	if s.windowResolved {
		return s.windowModel, s.windowHint
	}
	s.windowResolved = true
	s.windowModel = nil
	if s.provider != nil {
		models, err := s.provider.ListModels(ctx)
		if err != nil {
			s.windowHint = ContextWindowHint{Reason: fmt.Sprintf("%s:list_models_error:%v", NoContextWindow, err)}
			s.log.Warn("native session: context-window resolution failed — falling back to the work item's declared window (if any)",
				"execution", s.id, "reason", s.windowHint.Reason)
		} else {
			found := false
			for i := range models {
				if models[i].ID != s.identity.Model {
					continue
				}
				m := models[i]
				s.windowModel = &m
				found = true
				if m.Context > 0 {
					s.windowHint = ContextWindowHint{Tokens: m.Context, Ok: true, Reason: "live"}
				} else {
					s.windowHint = ContextWindowHint{Reason: NoContextWindow + ":context_zero"}
				}
				break
			}
			if !found {
				s.windowHint = ContextWindowHint{Reason: NoContextWindow + ":model_not_found"}
			}
			// FALLBACK (work-item parity): when the live resolution has no
			// usable window but the work item declared one, use it — an
			// operator-set window is better than a permanently disarmed
			// trigger. The reason string records the provenance.
			if !s.windowHint.Ok && s.contextWindowFallback > 0 {
				s.windowHint = ContextWindowHint{Tokens: s.contextWindowFallback, Ok: true, Reason: "manifest_fallback:" + s.windowHint.Reason}
				s.log.Info("native session: using work item's declared context window (live hint unavailable)",
					"execution", s.id, "window", s.contextWindowFallback, "live_reason", s.windowHint.Reason)
			}
		}
	} else {
		s.windowHint = ContextWindowHint{Reason: NoContextWindow + ":provider_unset"}
		// Same fallback applies when no provider is bound (tests).
		if !s.windowHint.Ok && s.contextWindowFallback > 0 {
			s.windowHint = ContextWindowHint{Tokens: s.contextWindowFallback, Ok: true, Reason: "manifest_fallback:" + s.windowHint.Reason}
		}
	}
	return s.windowModel, s.windowHint
}

// resolveContextWindow returns the live context-window hint (cached once
// per session).
func (s *Session) resolveContextWindow(ctx context.Context) ContextWindowHint {
	_, hint := s.resolveModelInfo(ctx)
	return hint
}

// priceUsage returns the LIVE provider-priced cost of one turn's usage via
// the session model's resolved Pricing (catalog/probe-enriched ModelInfo).
// Returns 0 when no pricing resolved — the budget cost dimension then never
// fires on this session (never a synthesized estimate).
func (s *Session) priceUsage(ctx context.Context, u Usage) float64 {
	m, _ := s.resolveModelInfo(ctx)
	if m == nil || m.Pricing == nil {
		return 0
	}
	return m.CostFor(u)
}
