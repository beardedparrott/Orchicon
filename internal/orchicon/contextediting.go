package orchicon

// contextediting.go — CLEARING, not compacting.
//
// The separation matters, because they do different jobs and the native loop previously had only the
// second one available:
//
//	compaction     operates on MEANING  — summarizes the conversation, costs a model call, rewrites the
//	                                     model's own reasoning and decisions, and is lossy in ways that are
//	                                     hard to predict.
//	clearing       operates on BYTES    — replaces an already-consumed tool payload with a short
//	  (this file)                        placeholder, costs nothing, is deterministic, and never touches
//	                                     what the model decided.
//
// WHY CLEARING IS SAFE AT ALL. A tool result has done its work the moment the model has read it and
// produced the next assistant turn. From then on it is dead weight that is re-sent on EVERY subsequent
// turn — and the platform owns the window, so it can stop re-sending it while keeping the full payload in
// the durable transcript (execution_session_parts + the JSONL transcript). Nothing is destroyed; only the
// resident copy is dropped.
//
// THE EVIDENCE. Anthropic shipped this as a harness feature (context editing / tool result clearing,
// clear_tool_uses_20250919) and measured, on a 100-turn agentic search evaluation, an 84% reduction in
// token consumption AND a 29% improvement in task performance from clearing alone — the quality gain being
// the more interesting half, because it means blanking consumed payloads is not a cost-for-capability
// trade. The mechanism they describe is what this file implements: clear the OLDEST tool results first,
// replace with a placeholder, and clear at least a threshold's worth so the work is worth doing.
//
// THE CACHE CAVEAT, WHICH IS WHY THIS IS BOUNDED BY BANDS RATHER THAN RUN PER TURN. Clearing changes the
// bytes of the prompt, which invalidates a cached prefix (the same caveat Anthropic documents). Clearing
// on every turn would therefore trade a token saving for a cache write every turn. So a clear only
// happens when it crosses into a new pressure band, which makes each individual clear large and rare.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// contextEditPolicy is the operator-tunable clearing policy, parsed from the same merged budget JSON
// transport as context_compaction (so it needs no new settings column and no new API surface).
//
// A ZERO-VALUED POLICY DISABLES CLEARING ENTIRELY, which is the safe default for an existing deployment:
// turning it on is an explicit operator decision, and turning it off is setting `enabled: false`.
type contextEditPolicy struct {
	// Enabled turns clearing on.
	Enabled bool
	// TriggerFrac is the context-window occupancy (0..1] at which clearing begins.
	TriggerFrac float64
	// KeepRecent is how many of the MOST RECENT tool results are never cleared. This is the single most
	// important safety number in the file: the newest results are the ones the model is actually
	// reasoning about, and a model that cannot see the output it just asked for will re-run the command.
	KeepRecent int
	// MinBytes is the `clear_at_least` equivalent: a clear that would reclaim less than this is skipped,
	// because invalidating a cached prefix to save a few hundred bytes is a net loss.
	MinBytes int
	// BandWidth is how much additional window occupancy (as a fraction) earns a NEW clear after the
	// trigger has been passed. It is the cache-invalidation governor: one clear per band, so a session
	// pays a cache write once per BandWidth of growth instead of on every turn.
	//
	// IT MUST BE A SUBDIVISION ABOVE THE TRIGGER, not a divisor of the whole occupancy. Deriving the band
	// as int(frac/TriggerFrac) collapses to 1 for every frac in [trigger, 1.0) — i.e. exactly one clear
	// for the entire session — which is a bug a test caught rather than a reviewer.
	BandWidth float64
	// Placeholder is written in place of a cleared payload. It must tell the model BOTH that the payload
	// was removed and that no action is required — otherwise a conscientious worker re-runs the command
	// to recover evidence it believes it lost, which costs more than the clearing saved.
	Placeholder string
}

// DefaultContextEditPolicy is the built-in clearing policy. Trigger 0.5 and keep-8 are deliberately
// conservative: clearing starts comfortably before the window is full (so there is room for the next
// few rounds) and the most recent 8 results — typically the last several rounds of work — are untouchable.
func DefaultContextEditPolicy() contextEditPolicy {
	return contextEditPolicy{
		Enabled:     true,
		TriggerFrac: 0.5,
		KeepRecent:  8,
		MinBytes:    16 * 1024,
		BandWidth:   0.1,
		Placeholder: defaultClearPlaceholder,
	}
}

// defaultClearPlaceholder is what a cleared tool result becomes.
//
// IT MENTIONS THE OFFLOAD PATH AND EXPLICITLY FORBIDS RE-RUNNING. A placeholder that only said "removed"
// invites the model to regenerate it — Anthropic's framing of the same hazard. The disk reference is
// appended per result by the caller when an offload exists.
const defaultClearPlaceholder = "[orchicon: this tool output was cleared from context to reclaim window space after it had been read. " +
	"Do NOT re-run the command to recover it. The full output is retained in the durable transcript and, when it was oversized, on disk at the path given below if one is shown.]"

// contextEditState latches clearing progress per session.
type contextEditState struct {
	mu sync.Mutex
	// cleared counts payloads cleared this execution (telemetry + transcript parts).
	cleared int
	// reclaimedBytes is the total resident bytes dropped.
	reclaimedBytes int
	// lastBand is the highest pressure band already cleared at, so a clear happens once per band rather
	// than every turn (the cache-invalidation bound).
	lastBand int
}

// ContextEditingSettingsDoc is the operator-facing description of the `context_editing` block. It lives
// next to the parser so the two cannot drift, and it is what the settings tool's schema refers to.
//
//	{
//	  "context_editing": {
//	    "enabled": true,        // OFF unless set — enabling changes what every worker sees
//	    "trigger_frac": 0.5,    // start clearing at 50% of the resolved context window
//	    "keep_recent": 8,       // the newest 8 tool results are NEVER cleared
//	    "min_bytes": 16384,     // skip a clear that reclaims less than this (cache write must be worth it)
//	    "band_width": 0.1,      // one clear per further 10% of window occupancy
//	    "placeholder": "..."    // text written in place of a cleared payload
//	  }
//	}
const ContextEditingSettingsDoc = "`context_editing` (nested in the merged budget JSON, beside " +
	"`context_compaction`): enabled (default false), trigger_frac (default 0.5), keep_recent (default 8), " +
	"min_bytes (default 16384), band_width (default 0.1), placeholder."

// contextEditPolicyFromSettings parses the policy from the merged budget JSON's `context_editing` key.
//
// It is nested beside `context_compaction` on purpose: both are context-hygiene policy, both ride the
// existing budget transport, and an operator reading the settings JSON finds them together.
func contextEditPolicyFromSettings(budgetJSON []byte) contextEditPolicy {
	pol := DefaultContextEditPolicy()
	// DEFAULT OFF for an existing tenant: an unconfigured deployment keeps its current behaviour, and
	// clearing is switched on explicitly. (DefaultContextEditPolicy describes the ON shape.)
	pol.Enabled = false
	if len(budgetJSON) == 0 {
		return pol
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(budgetJSON, &raw); err != nil {
		return pol
	}
	ceRaw, ok := raw["context_editing"]
	if !ok {
		return pol
	}
	var ce struct {
		Enabled         *bool    `json:"enabled"`
		TriggerFrac     *float64 `json:"trigger_frac"`
		KeepRecent      *int     `json:"keep_recent"`
		MinBytes        *int     `json:"min_bytes"`
		BandWidth       *float64 `json:"band_width"`
		Placeholder     *string  `json:"placeholder"`
	}
	if err := json.Unmarshal(ceRaw, &ce); err != nil {
		return pol
	}
	if ce.Enabled != nil {
		pol.Enabled = *ce.Enabled
	}
	if ce.TriggerFrac != nil && *ce.TriggerFrac > 0 && *ce.TriggerFrac <= 1 {
		pol.TriggerFrac = *ce.TriggerFrac
	}
	if ce.KeepRecent != nil && *ce.KeepRecent >= 0 {
		pol.KeepRecent = *ce.KeepRecent
	}
	if ce.MinBytes != nil && *ce.MinBytes >= 0 {
		pol.MinBytes = *ce.MinBytes
	}
	if ce.BandWidth != nil && *ce.BandWidth > 0 && *ce.BandWidth <= 1 {
		pol.BandWidth = *ce.BandWidth
	}
	if ce.Placeholder != nil && strings.TrimSpace(*ce.Placeholder) != "" {
		pol.Placeholder = *ce.Placeholder
	}
	return pol
}

// toolResultClearing is the pure decision + rewrite core, kept free of session state so it can be tested
// exactly. It walks history OLDEST-FIRST, skips the most recent keepRecent tool results, and replaces
// each eligible result's content with the placeholder.
//
// `occupancy` and `windowTokens` decide whether to run at all; passing a zero window means "no hint", in
// which case the caller must decide (the loop treats a missing hint as "do not clear", because without a
// window there is no pressure to respond to and no way to size the reclaim).
func toolResultClearing(history []Message, pol contextEditPolicy, occupancy, windowTokens, lastBand int) (cleared int, reclaimedBytes int, band int, changed bool) {
	if !pol.Enabled || windowTokens <= 0 || occupancy <= 0 {
		return 0, 0, lastBand, false
	}
	frac := float64(occupancy) / float64(windowTokens)
	if frac < pol.TriggerFrac {
		return 0, 0, lastBand, false
	}
	// Band = how many BandWidth-wide steps of pressure exist ABOVE the trigger. Band 1 is "just crossed
	// the trigger"; each further BandWidth of growth is a new band, and only a new band earns a clear.
	//
	// Deliberately NOT int(frac/TriggerFrac): that yields 1 for every frac in [trigger, 1.0), so a
	// session would clear once and then never again no matter how much context it accumulated.
	width := pol.BandWidth
	if width <= 0 {
		width = 0.1
	}
	band = int((frac-pol.TriggerFrac)/width) + 1
	if band <= lastBand {
		return 0, 0, lastBand, false
	}

	// Collect the indices of every tool-result message, oldest first.
	var resultIdx []struct{ msg, content int }
	for mi := range history {
		for ci := range history[mi].Content {
			if history[mi].Content[ci].ToolResult != nil {
				resultIdx = append(resultIdx, struct{ msg, content int }{mi, ci})
			}
		}
	}
	// Keep the newest keepRecent untouched — those are what the model is still reasoning about.
	eligible := len(resultIdx) - pol.KeepRecent
	if eligible <= 0 {
		return 0, 0, lastBand, false
	}

	for i := 0; i < eligible; i++ {
		ref := resultIdx[i]
		tr := history[ref.msg].Content[ref.content].ToolResult
		if tr == nil {
			continue
		}
		// Already cleared (idempotent under repeated calls).
		if strings.HasPrefix(tr.Content, clearedPrefix) {
			continue
		}
		// Never clear an ERROR result on the same schedule: an error is the reason the worker changed
		// approach, and blanking it is how a session starts repeating a failure it already learned from.
		if tr.IsError {
			continue
		}
		reclaimedBytes += len(tr.Content)
		tr.Content = tr.Content[:0] + clearedContentFor(tr.Content, pol.Placeholder)
		history[ref.msg].Content[ref.content].ToolResult = tr
		cleared++
	}
	if cleared == 0 {
		return 0, 0, lastBand, false
	}
	// A clear that reclaims almost nothing is not worth the cache write it costs.
	if reclaimedBytes < pol.MinBytes {
		return 0, 0, lastBand, false
	}
	return cleared, reclaimedBytes, band, true
}

// clearedPrefix marks a payload this file has already cleared, so a later pass never double-clears and a
// test can assert the state unambiguously.
const clearedPrefix = "[orchicon: this tool output was cleared"

// clearedContentFor builds the replacement text, preserving any disk reference the payload already
// carried so the model keeps its handle on the full output.
func clearedContentFor(original, placeholder string) string {
	if !strings.HasPrefix(placeholder, clearedPrefix) {
		placeholder = clearedPrefix + " from context to reclaim window space after it had been read. " +
			"Do NOT re-run the command to recover it. The full output is retained in the durable transcript.]"
	}
	// Carry forward the offload path if the original payload named one — that reference is the model's
	// cheap handle on the full data, and clearing must not take it away.
	if idx := strings.Index(original, "full output offloaded to "); idx >= 0 {
		rest := original[idx:]
		if end := strings.IndexAny(rest, ")\n"); end > 0 {
			return placeholder + "\n[cleared payload is on disk: " + strings.TrimPrefix(rest[:end], "full output offloaded to ") + "]"
		}
	}
	return placeholder
}

// clearConsumedToolResults applies the policy to the live history at a turn boundary and records what it
// did in the durable transcript.
func (s *Session) clearConsumedToolResults(ctx context.Context) {
	pol := s.cep
	if !pol.Enabled {
		return
	}
	hint := s.resolveContextWindow(ctx)
	if !hint.Ok || hint.Tokens <= 0 {
		// No window hint: there is no pressure signal to respond to and no way to size the reclaim.
		return
	}
	// Occupancy is measured from the LIVE history rather than the last turn's provider-reported count, so
	// the decision does not lag a turn behind the growth it is meant to police.
	occupancy := estimateHistoryTokens(s.history)

	s.ces.mu.Lock()
	lastBand := s.ces.lastBand
	s.ces.mu.Unlock()

	cleared, reclaimed, band, changed := toolResultClearing(s.history, pol, occupancy, int(hint.Tokens), lastBand)
	if !changed {
		return
	}
	s.ces.mu.Lock()
	s.ces.cleared += cleared
	s.ces.reclaimedBytes += reclaimed
	s.ces.lastBand = band
	total := s.ces.cleared
	totalReclaimed := s.ces.reclaimedBytes
	s.ces.mu.Unlock()

	s.log.Info("context editing: cleared consumed tool results",
		"execution", s.id, "cleared", cleared, "reclaimed_bytes", reclaimed,
		"occupancy_tokens", occupancy, "window_tokens", hint.Tokens, "band", band,
		"total_cleared", total, "total_reclaimed_bytes", totalReclaimed)
	// Record it in the durable transcript so a reader (and the recovery seed) can see that payloads were
	// dropped from context deliberately rather than lost.
	_ = s.transcript.Append(TransContextEdit, map[string]any{
		"cleared": cleared, "reclaimed_bytes": reclaimed,
		"occupancy_tokens": occupancy, "window_tokens": hint.Tokens, "band": band,
	})
}

// estimateHistoryTokens approximates the token cost of the live history.
//
// ~4 bytes per token is the standard rough ratio for English + code, and it is deliberately used rather
// than a tokenizer: this is a PRESSURE estimate whose only job is deciding when to clear, and being
// approximately right at the band boundary is sufficient. An exact count would cost more than the
// clearing saves. The loop's real accounting (provider-reported usage) still governs budgets.
func estimateHistoryTokens(history []Message) int {
	bytes := 0
	for _, m := range history {
		for _, c := range m.Content {
			switch {
			case c.Text != nil:
				bytes += len(*c.Text)
			case c.ToolResult != nil:
				bytes += len(c.ToolResult.Content)
			case c.ToolUse != nil:
				bytes += len(c.ToolUse.ArgsJSON)
			case c.Reasoning != nil:
				bytes += len(*c.Reasoning)
			}
		}
	}
	return bytes / 4
}

// contextEditSummary renders a one-line description for a log/trace line.
func contextEditSummary(cleared, reclaimedBytes int) string {
	if cleared == 0 {
		return "no tool results cleared"
	}
	return fmt.Sprintf("cleared %d tool result(s), reclaimed %d bytes", cleared, reclaimedBytes)
}
