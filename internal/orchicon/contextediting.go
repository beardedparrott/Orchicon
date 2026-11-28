package orchicon

// contextediting.go — keep the last N tool calls, summarize the rest.
//
// THE MODEL IS DELIBERATELY TRIVIAL, and that is the point. An earlier version of this file gated clearing
// on a fraction of the model's context window, with separate keep-recent / minimum-bytes / pressure-band
// knobs. Three things were wrong with that:
//
//  1. A FRACTION-OF-WINDOW TRIGGER IS A LIMIT CONTROL, NOT A COST CONTROL. Cost is driven by the ABSOLUTE
//     number of tokens re-sent every turn. A 200k context costs the same on a 1M window as on a 256k one,
//     so "clear at 50% of the window" never fires on a large-window model even while it is re-sending
//     100k tokens a turn. The knob did not mean what a cost knob has to mean.
//  2. OCCUPANCY WAS MEASURED AS BYTES/4 OF THE TRANSCRIPT. Most of what a warm session re-sends is CACHED
//     tokens, and the byte estimate is blind to that split (and off by whatever the real text-to-token
//     ratio is for code and tool output). Compaction next door measures occupancy correctly, as
//     InputTokens + CacheReadTokens; two mechanisms measuring the same thing different ways is a bug.
//  3. FOUR KNOBS, NO PREDICTABLE ANSWER. "When will it clear?" could not be answered from the settings,
//     which makes it impossible to reason about or to verify.
//
// THE RULE NOW: `keep_tool_calls` (default 20). Only the most recent N tool results stay verbatim; every
// older one is replaced with a short stub that keeps its IDENTITY — which tool ran, on what, and a note
// that the output was removed. The agent therefore still knows a call happened and what it was, so its
// plan and its reasoning stay intact, while the bulk of the bytes stop being re-sent.
//
// It is checked after every tool round, so the steady state is "the last N calls, plus whatever model
// text and tool calls have not resolved yet". No pressure gate, no bands, no minimum reclaim.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// contextEditPolicy is the whole policy: how many tool results stay verbatim.
//
// ZERO OR ABSENT DISABLES CLEARING, which keeps an unconfigured deployment's behaviour unchanged.
type contextEditPolicy struct {
	// KeepToolCalls is how many of the most recent tool results are never touched. 0 disables.
	KeepToolCalls int
}

// DefaultContextEditPolicy is the built-in policy. 20 is chosen to comfortably cover "the last several
// rounds of work" — a typical round is 1-4 tool calls — so the agent keeps everything it is actively
// reasoning about and loses only what it has already moved on from.
func DefaultContextEditPolicy() contextEditPolicy {
	return contextEditPolicy{KeepToolCalls: 20}
}

// contextEditState latches what clearing has done for this session (telemetry + transcript).
type contextEditState struct {
	cleared        int
	reclaimedBytes int
}

// contextEditPolicyFromSettings parses the `context_editing` block out of the merged budget JSON, beside
// `context_compaction` — both are context hygiene, so they live together in one document.
//
//	"context_editing": {"keep_tool_calls": 20}     // 0 or absent = disabled
func contextEditPolicyFromSettings(budgetJSON []byte) contextEditPolicy {
	pol := DefaultContextEditPolicy()
	pol.KeepToolCalls = 0 // DEFAULTS OFF: enabling this changes what every worker sees.
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
		KeepToolCalls *int `json:"keep_tool_calls"`
	}
	if err := json.Unmarshal(ceRaw, &ce); err != nil {
		return pol
	}
	if ce.KeepToolCalls != nil && *ce.KeepToolCalls > 0 {
		pol.KeepToolCalls = *ce.KeepToolCalls
	}
	return pol
}

// ContextEditingSettingsDoc is the operator-facing description of the block, kept beside the parser so
// the two cannot drift.
const ContextEditingSettingsDoc = "`context_editing` (nested in the merged budget JSON, beside " +
	"`context_compaction`): keep_tool_calls (default unset = off). Only the most recent N tool results stay " +
	"verbatim; older ones are replaced with a stub naming the tool and its arguments."

// clearedStubPrefix marks a result this file already cleared, so a later pass never double-clears and a
// test can assert the state exactly.
const clearedStubPrefix = "[orchicon: cleared"

// toolResultStub is what an old tool result becomes: who it was, and what it did.
//
// IT KEEPS THE IDENTITY ON PURPOSE. A stub that said only "removed" would leave the agent unable to tell
// which call it was looking at, and it would re-run the work to recover its own reasoning. Naming the tool
// and its arguments means the transcript still reads as a record of what happened — the agent knows the
// call happened, what it asked for, and that the output was dropped to save room — so it can decide
// whether re-running is actually necessary instead of being forced to.
func toolResultStub(toolName, argsJSON string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		name = "tool"
	}
	summary := summarizeToolArgs(argsJSON)
	if summary == "" {
		return fmt.Sprintf("%s %s] output removed to save context — the call ran, its output is in the durable transcript.", clearedStubPrefix, name)
	}
	return fmt.Sprintf("%s %s %s] output removed to save context — the call ran, its output is in the durable transcript.", clearedStubPrefix, name, summary)
}

// summarizeToolArgs renders a SHORT identity for a tool call: the one argument that says what it operated
// on (a path, a command), truncated. A whole arg blob would defeat the purpose of the stub.
func summarizeToolArgs(argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		// Not an object (or not JSON at all): keep a short slice of it as the identity rather than dropping
		// it, so the stub still says what the call was given.
		if raw := truncateArg(argsJSON); raw != "" {
			return "(" + raw + ")"
		}
		return ""
	}
	// The most informative single argument, in preference order.
	for _, key := range []string{"filePath", "path", "command", "pattern", "query", "url", "id"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return fmt.Sprintf("(%s)", truncateArg(v))
		}
	}
	return ""
}

// truncateArg keeps an argument identity short enough to be cheap.
func truncateArg(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 120
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// keepRecentToolResults is the pure core: walk the history's tool results OLDEST-FIRST, leave the newest
// keep verbatim, replace every older one with a stub. Returns how many were cleared and the bytes those
// payloads occupied.
//
// The TOOL IDENTITY is read from the matching assistant tool_use in the same history (by call id), because
// a tool result carries only the id and its output — the name and arguments live on the request.
func keepRecentToolResults(history []Message, keep int) (cleared int, reclaimedBytes int) {
	if keep <= 0 {
		return 0, 0
	}
	// Index the tool_use side by call id so a result can be labelled with what it actually called.
	type callMeta struct{ name, args string }
	meta := map[string]callMeta{}
	for _, m := range history {
		for _, c := range m.Content {
			if c.ToolUse != nil {
				meta[c.ToolUse.ToolCallID] = callMeta{name: c.ToolUse.Name, args: c.ToolUse.ArgsJSON}
			}
		}
	}

	// Collect every tool-result location, oldest first.
	type ref struct{ msg, content int }
	var results []ref
	for mi := range history {
		for ci := range history[mi].Content {
			if history[mi].Content[ci].ToolResult != nil {
				results = append(results, ref{mi, ci})
			}
		}
	}
	if len(results) <= keep {
		return 0, 0
	}

	// Everything older than the newest `keep`.
	for _, r := range results[:len(results)-keep] {
		tr := history[r.msg].Content[r.content].ToolResult
		if tr == nil {
			continue
		}
		if strings.HasPrefix(tr.Content, clearedStubPrefix) {
			continue // idempotent
		}
		// An ERROR is never stubbed: it is the reason the worker changed approach, and an agent that cannot
		// see which call failed is one that repeats the failure.
		if tr.IsError {
			continue
		}
		m := meta[tr.ToolCallID]
		reclaimedBytes += len(tr.Content)
		tr.Content = toolResultStub(m.name, m.args)
		history[r.msg].Content[r.content].ToolResult = tr
		cleared++
	}
	return cleared, reclaimedBytes
}

// clearConsumedToolResults keeps the last N tool results and stubs the rest. Called after each tool round,
// so the window holds a bounded number of payloads rather than growing with the session.
func (s *Session) clearConsumedToolResults(ctx context.Context) {
	keep := s.cep.KeepToolCalls
	if keep <= 0 {
		return
	}
	cleared, reclaimed := keepRecentToolResults(s.history, keep)
	if cleared == 0 {
		return
	}
	s.ces.cleared += cleared
	s.ces.reclaimedBytes += reclaimed
	s.log.Info("context editing: stubbed old tool results",
		"execution", s.id, "cleared", cleared, "reclaimed_bytes", reclaimed,
		"keep", keep, "total_cleared", s.ces.cleared)
	// Durable record, so a reader (and the recovery seed) can tell deliberate stubbing from lost data.
	_ = s.transcript.Append(TransContextEdit, map[string]any{
		"cleared": cleared, "reclaimed_bytes": reclaimed, "keep": keep,
	})
}
