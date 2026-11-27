package antigravity

// askmapper.go — THE ASK EVENT MAPPER: agy's `session/update` stream, rendered as the SessionEvents an Ask
// conversation's collector already understands.
//
// WHY THIS IS A SEPARATE MAPPER FROM mapper.go. `Mapper` is welded to ExecutionCallbacks: it reports starts,
// results, health, violations and artifacts to an execution's supervisor, and it writes the EXECUTION diff
// ledger. An Ask conversation has none of that — it has a bus, a conversation, and the ask ledger. Reusing
// Mapper would have meant inventing an execution for every conversation.
//
// THE EVENT SHAPES ARE COPIED FROM THE CLAUDE ASK PATH, NOT INVENTED (internal/claude/ask.go handleLine):
//
//	agent_message_chunk → {delta, text}
//	agent_thought_chunk → {delta, reasoning}
//	tool_call           → {part, tool_use, Part{tool, callID, state.input}}
//	tool_call_update    → {tool_result, …} on completion, carrying output + the ORIGINAL arguments
//	turn end            → {part, step_finish, Part{tokens}} then {idle}
//
// THE TOOL NAME ON THE WIRE IS THE CANONICAL ONE (`bash`/`write`/`edit`/`read`). The Ask ledger, the stall
// monitor and internal/toolclass all key on those names, and the consent layer's own vocabulary is
// claude's — so `write_to_file` is emitted as `write` while its permission CARD still names `write_to_file`
// (asked before the call exists). The two are different questions: the card is "what is being asked", the
// part is "what ran".

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// askMapper maps one conversation's updates onto its bus.
type askMapper struct {
	s *askSession
	// usage is the last usage_update gauge agy reported, carried onto the turn's step_finish so the
	// conversation's context meter moves. A zero gauge is not emitted on its own.
	usageUsed, usageSize int64
	// tools remembers what each tool_use asked for, so its terminal update can name the tool and carry the
	// original arguments (agy's terminal update often repeats neither).
	tools map[string]askToolCallInfo
}

func newAskMapper(s *askSession) *askMapper {
	return &askMapper{s: s, tools: make(map[string]askToolCallInfo)}
}

// Handle maps one update.
func (m *askMapper) Handle(ctx context.Context, u acp.Update) {
	switch upd := u.(type) {
	case acp.AgentMessageChunk:
		if t := strings.TrimSpace(upd.Content.Text); t != "" {
			m.s.busEmit(scheduler.SessionEvent{Kind: "delta", Type: "text", Text: upd.Content.Text})
		}

	case acp.AgentThoughtChunk:
		// REASONING, ON ITS OWN TYPE — the same shape claude's `thinking_delta` produces, so the TUI's
		// reasoning fold (internal/tui/reasoning_fold.go) and the GUI fold render it with no client change.
		//
		// agy 1.5.13 NEVER EMITS THIS (docs/adapters/antigravity-acp.md §3.1: only `thoughtTokens` is
		// reported). The arm exists so a future agy that starts sending reasoning needs no client change —
		// and so a synthetic update in the tests proves the mapping is right today.
		if t := strings.TrimSpace(upd.Content.Text); t != "" {
			m.s.busEmit(scheduler.SessionEvent{Kind: "delta", Type: "reasoning", IsReasoning: true, Text: upd.Content.Text})
		}

	case acp.ToolCall:
		m.toolCall(ctx, upd.ToolCallInfo)

	case acp.ToolCallUpdate:
		m.toolCallUpdate(ctx, upd.ToolCallInfo)

	case acp.UsageUpdate:
		// THE GAUGE, REMEMBERED NOT EMITTED: an update carries only "used of size", and a step_finish with
		// no token counts would be a usage row of zeroes (askorchicon's recordTurnUsage ignores it). It
		// travels with the turn's real usage below instead.
		m.usageUsed, m.usageSize = upd.Used, upd.Size

	default:
		// user_message_chunk (a replay), plan, available_commands_update, current_mode_update,
		// config_option_update, session_info_update, Unknown: nothing a conversation renders. Dropping them
		// here is what keeps the bus free of display-only noise.
	}
}

// toolCall renders the first sighting of a tool call as the `tool_use` part the shared consumers read.
func (m *askMapper) toolCall(ctx context.Context, info acp.ToolCallInfo) {
	canonical := canonicalToolName(info.Name)
	input := agyInput(info.RawInput, info.Args)
	if id := strings.TrimSpace(info.ToolCallID); id != "" {
		m.tools[id] = askToolCallInfo{canonical: canonical, raw: info.Name, input: input}
	}
	m.s.busEmit(scheduler.SessionEvent{
		Kind: "part",
		Type: "tool_use",
		Part: map[string]any{
			"tool":   canonical,
			"callID": info.ToolCallID,
			"state":  map[string]any{"input": input},
		},
	})
	_ = ctx
}

// toolCallUpdate emits the terminal result of a tool call, and fires the ASK diff ledger for a successful
// write/edit.
//
// ONLY TERMINAL UPDATES ARE EMITTED, matching the claude Ask path: an in-progress update's partial output is
// superseded by the completed one, and emitting both would double the transcript.
func (m *askMapper) toolCallUpdate(ctx context.Context, info acp.ToolCallInfo) {
	status := strings.TrimSpace(info.Status)
	if status != acp.StatusCompleted && status != acp.StatusFailed {
		return
	}
	remembered, ok := m.tools[info.ToolCallID]
	if ok {
		delete(m.tools, info.ToolCallID)
	}
	canonical := remembered.canonical
	if canonical == "" {
		canonical = canonicalToolName(info.Name)
	}
	input := remembered.input
	if len(info.RawInput) > 0 || len(info.Args) > 0 {
		// The terminal update sometimes repeats the arguments (and sometimes changes them — agy fills in a
		// resolved path). The LAST word wins, but only when it actually carried something.
		if further := agyInput(info.RawInput, info.Args); len(further) > 0 {
			input = further
		}
	}
	failed := status == acp.StatusFailed

	m.s.busEmit(scheduler.SessionEvent{
		Kind:       "tool_result",
		Type:       "tool_result",
		ToolCallID: info.ToolCallID,
		ToolName:   canonical,
		ArgsJSON:   askInputJSON(input),
		Output:     toolOutput(info),
		IsError:    failed,
	})

	// THE ASK DIFF LEDGER, FIRED WHERE THE FILE IS ON DISK — the same point claude's ask path fires it
	// (`internal/claude/ask.go`: on a successful tool_result). It goes to `askFileEdits`, NEVER to the
	// execution ledger: an Ask conversation's edits belong to the conversation, not to a worker run.
	if failed || !isFileEditTool(canonical) || m.s.b.askFileEdits == nil {
		return
	}
	dir, targets := m.s.ledgerScope(input, info)
	if len(targets) == 0 {
		return
	}
	newText := diffNewText(info)
	for _, p := range targets {
		rel := ledgerPathFor(dir, p)
		if strings.TrimSpace(rel) == "" {
			continue
		}
		m.s.b.askFileEdits(ctx, m.s.convID, m.s.tenantIDSnapshot(), dir, canonical,
			map[string]any{"filePath": rel}, newText)
	}
}

// ledgerScope resolves the directory the ledger should record paths RELATIVE TO, and the paths the call
// touched.
//
// THE PROJECT WHEN THE PATH IS INSIDE IT, the ask directory otherwise: a conversation runs with its own cwd
// and the project as an additional readable root, so a path in the project must not be recorded as
// `../../..`-relative to the ask directory — the ledger would then disagree with the diff view about which
// file changed.
func (s *askSession) ledgerScope(input map[string]any, info acp.ToolCallInfo) (string, []string) {
	s.mu.Lock()
	askDir, projectDir := s.askDir, s.projectDir
	s.mu.Unlock()
	if strings.TrimSpace(projectDir) == "" {
		projectDir = askDir
	}
	paths := editedPaths(info, input)
	if len(paths) == 0 {
		return projectDir, nil
	}
	dir := askDir
	for _, p := range paths {
		if pathWithin(projectDir, p) {
			dir = projectDir
			break
		}
	}
	return dir, paths
}

// tenantIDSnapshot reads the tenant under the lock.
func (s *askSession) tenantIDSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenantID
}

// pathWithin reports whether p is inside dir (after cleaning), which decides which directory a ledger entry
// is relative to.
func pathWithin(dir, p string) bool {
	d := strings.TrimSpace(dir)
	t := strings.TrimSpace(p)
	if d == "" || t == "" {
		return false
	}
	cleanD := strings.TrimSuffix(d, "/")
	cleanT := strings.TrimSuffix(t, "/")
	if !strings.HasPrefix(cleanT, "/") {
		// A RELATIVE PATH IS INSIDE BY DEFINITION: it is relative to the child's cwd, and the ledger's
		// directory is the one that cwd belongs to.
		return true
	}
	return cleanT == cleanD || strings.HasPrefix(cleanT, cleanD+"/")
}

// turnEnd closes a turn: the usage part (with the context gauge), then idle or the error.
//
// THE ORDER MATTERS. `step_finish` carries the turn's accounting and the conversation's context meter reads
// it, so it must precede the terminal event — a collector that stops draining on `idle` would otherwise
// never see the usage.
func (m *askMapper) turnEnd(res *acp.PromptResult) {
	if part := m.stepFinishPart(res); part != nil {
		m.s.busEmit(scheduler.SessionEvent{Kind: "part", Type: "step_finish", Part: part})
	}
}

// stepFinishPart builds the turn's usage part, or nil when there is nothing to report.
//
// THE FIELD MAPPING IS DIRECT. agy's `usage.inputTokens` already EXCLUDES the cache buckets (the recorded
// file_edit_diff turn reports 15726 input beside 21689 cachedRead), so unlike the anthropic transport there
// is no transform to apply — running one would invent tokens the model never used.
func (m *askMapper) stepFinishPart(res *acp.PromptResult) map[string]any {
	tokens := map[string]any{}
	if res != nil {
		if res.Usage.InputTokens > 0 {
			tokens["input"] = res.Usage.InputTokens
		}
		if res.Usage.OutputTokens > 0 {
			tokens["output"] = res.Usage.OutputTokens
		}
		if res.Usage.ThoughtTokens > 0 {
			tokens["reasoning"] = res.Usage.ThoughtTokens
		}
		cache := map[string]any{}
		if res.Usage.CachedReadTokens > 0 {
			cache["read"] = res.Usage.CachedReadTokens
		}
		if res.Usage.CachedWriteTokens > 0 {
			cache["write"] = res.Usage.CachedWriteTokens
		}
		if len(cache) > 0 {
			tokens["cache"] = cache
		}
	}
	// THE CONTEXT GAUGE rides on the same part, as its own two fields: askorchicon's recordTurnUsage reads
	// `tokens` for the usage row, and the conversation's meter reads these. A turn that reported no usage at
	// all but did move the gauge is still worth emitting.
	if m.usageUsed > 0 || m.usageSize > 0 {
		tokens["context_used"] = m.usageUsed
		tokens["context_size"] = m.usageSize
	}
	if len(tokens) == 0 {
		return nil
	}
	return map[string]any{"tokens": tokens}
}

// jsonRawString renders a raw JSON fragment as text, for logs. "" for an absent one.
func jsonRawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}
