package orchicon

// contextwindow.go resolves the selected model's TRUE context window for
// the native session — LIVE hints only (operator directive: compaction
// must never guess a window). Resolution order mirrors sourcing's
// probe-or-nothing stance:
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

// resolveContextWindow asks the bound provider for its live model list
// and finds this session's model. The result is cached on the session
// (resolved once per session — no per-turn probing).
func (s *Session) resolveContextWindow(ctx context.Context) ContextWindowHint {
	s.windowMu.Lock()
	defer s.windowMu.Unlock()
	if s.windowResolved {
		return s.windowHint
	}
	s.windowResolved = true
	if s.provider == nil {
		s.windowHint = ContextWindowHint{Reason: NoContextWindow + ":provider_unset"}
		return s.windowHint
	}
	models, err := s.provider.ListModels(ctx)
	if err != nil {
		s.windowHint = ContextWindowHint{Reason: fmt.Sprintf("%s:list_models_error:%v", NoContextWindow, err)}
		return s.windowHint
	}
	for _, m := range models {
		if m.ID == s.identity.Model {
			if m.Context > 0 {
				s.windowHint = ContextWindowHint{Tokens: m.Context, Ok: true, Reason: "live"}
			} else {
				s.windowHint = ContextWindowHint{Reason: NoContextWindow + ":context_zero"}
			}
			return s.windowHint
		}
	}
	s.windowHint = ContextWindowHint{Reason: NoContextWindow + ":model_not_found"}
	return s.windowHint
}
