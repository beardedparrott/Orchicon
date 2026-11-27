package antigravity

// ask_mapper.go — the ACP `session/update` stream → scheduler.SessionEvent, for an Ask conversation.
//
// THIS IS A SIBLING OF mapper.go, NOT A SECOND INTERPRETATION OF THE WIRE. It reuses the SAME package-level
// helpers the worker mapper uses — toolOutput, editedPaths, diffNewText, canonicalToolName, agyInput,
// isFileEditTool, toolKindWrites, ledgerPathFor — so the two transports cannot disagree about what a
// tool call's output was or which paths an edit touched. What differs is the DESTINATION: the worker's
// mapper fans out to scheduler.ExecutionCallbacks and a durable transcript; this one fans out to a
// scheduler.SessionBus that the Ask collector drains.
//
// THE EVENT VOCABULARY IS THE BUS CONTRACT, matched to what internal/claude/ask.go emits:
//
//	AgentMessageChunk        → delta{Type:"text"}            (streamed, per chunk)
//	                         → part{Type:"text"} at turn end (the completed block)
//	AgentThoughtChunk        → delta{Type:"reasoning",IsReasoning:true}
//	                         → part{Type:"reasoning"} at turn end
//	ToolCall                 → part{Type:"tool_use"}         (tool, callID, state.input)
//	ToolCallUpdate completed → tool_result{ToolCallID,ToolName,ArgsJSON,Output,IsError}
//	PromptResult             → idle (one per turn), or error{Text}
//
// REASONING TEXT DOES NOT EXIST ON agy 1.5.13. It never emits `agent_thought_chunk` — only
// `usage.thoughtTokens` (docs/adapters/antigravity-acp.md §3.1). The arm is implemented so a future agy
// needs no client change, and it is NEVER FABRICATED: a turn with no thought chunk produces no reasoning
// delta, and the reasoning tokens still reach the usage row. That is why "streamed reasoning" can only be
// demonstrated against a model that emits it, and the acceptance report says so rather than claiming
// otherwise.
//
// THE ASK DIFF LEDGER IS b.askFileEdits, NOT b.fileEdits. An Ask conversation is not an execution, and its
// edits must land under owner kind ask_conversation — feeding the execution ledger would attribute a
// conversation's writes to an execution id that does not exist.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// askToolCall remembers what a tool_use asked for, so its later terminal update can name the tool and carry
// the ORIGINAL arguments — the same bookkeeping the claude Ask transport keeps.
type askToolCall struct {
	name  string // the RAW agy name (what the operator sees, and what a card named)
	input map[string]any
	kind  string // agy's tool kind ("edit"/"execute"/"read"), for the writes fallback
}

// askMapper owns the update → bus fan-out for one Ask conversation.
type askMapper struct {
	b      *Bridge
	convID string

	// handled counts the updates this mapper has finished. It is the barrier's other half (see
	// askSession.settle): an empty events channel does not mean the last update has been MAPPED, only that
	// it has been taken.
	handledCount atomic.Int64

	mu       sync.Mutex
	tools    map[string]askToolCall
	textBuf  strings.Builder // per-turn text, flushed as ONE part at turn end
	reasonBuf strings.Builder // per-turn reasoning, flushed as ONE part at turn end
	// ctxUsed/ctxSize are the last context-window gauge agy reported, for the conversation's meter.
	ctxUsed int64
	ctxSize int64
}

// newAskMapper builds the mapper for one conversation.
func newAskMapper(b *Bridge, convID string) *askMapper {
	return &askMapper{b: b, convID: convID, tools: make(map[string]askToolCall)}
}

// handled reports how many updates have been fully mapped.
func (m *askMapper) handled() int64 { return m.handledCount.Load() }

// ContextGauge reports the most recent (used, size) agy reported, both 0 before it reports one.
func (m *askMapper) ContextGauge() (int64, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctxUsed, m.ctxSize
}

// handleUpdate maps ONE decoded ACP update onto the conversation's bus.
func (s *askSession) handleUpdate(ctx context.Context, u acp.Update) {
	s.mu.Lock()
	m := s.mapper
	s.mu.Unlock()
	if m == nil {
		return
	}
	defer m.handledCount.Add(1)
	m.handle(ctx, s, u)
}

// Handle dispatches one update. It is exported through askSession.handleUpdate; keeping the switch here
// means the session file holds lifecycle and this one holds the vocabulary.
func (m *askMapper) handle(ctx context.Context, s *askSession, u acp.Update) {
	switch up := u.(type) {
	case acp.AgentMessageChunk:
		if up.Content.Type == "text" || up.Content.Type == "" {
			if t := up.Content.Text; t != "" {
				m.mu.Lock()
				m.textBuf.WriteString(t)
				m.mu.Unlock()
				s.busEmit(scheduler.SessionEvent{Kind: "delta", Type: "text", Text: t})
			}
		}

	case acp.AgentThoughtChunk:
		// agy 1.5.13 NEVER emits this (docs §3.1); a future agy might. Mapped, never fabricated.
		if up.Content.Type == "text" || up.Content.Type == "" {
			if t := up.Content.Text; t != "" {
				m.mu.Lock()
				m.reasonBuf.WriteString(t)
				m.mu.Unlock()
				s.busEmit(scheduler.SessionEvent{Kind: "delta", Type: "reasoning", IsReasoning: true, Text: t})
			}
		}

	case acp.UserMessageChunk:
		// A `session/load` replay of the original prompt. Forwarding it as output would attribute the user's
		// words to the model and duplicate the turn in the transcript.

	case acp.ToolCall:
		m.handleToolCall(s, up.ToolCallInfo)

	case acp.ToolCallUpdate:
		m.handleToolUpdate(ctx, s, up.ToolCallInfo)

	case acp.Plan:
		m.emitPlan(s, up.Entries)

	case acp.UsageUpdate:
		// THE CONTEXT GAUGE, not the billable accounting: agy reports `used`/`size` here and the tokens on
		// the prompt RESULT. Conflating them would either bill a gauge or lose the cache-read tokens.
		m.mu.Lock()
		if up.Size > 0 {
			m.ctxUsed, m.ctxSize = up.Used, up.Size
		}
		m.mu.Unlock()

	case acp.SessionInfoUpdate, acp.AvailableCommands, acp.CurrentModeUpdate, acp.ConfigOptionUpdate:
		// Display-only surfaces the Ask transport does not act on. Deliberately silent: several repeat every
		// turn and a log line per turn is noise.

	case acp.Unknown:
		// Forward compatibility: a future agy adding an update must not fail a turn.
	}
}

// handleToolCall streams the FIRST sighting of a tool call with its input, as the claude Ask transport's
// `part{tool_use}` does — the shape askorchicon's ledger and stall monitor both read.
func (m *askMapper) handleToolCall(s *askSession, info acp.ToolCallInfo) {
	raw := strings.TrimSpace(info.Name)
	if raw == "" {
		return
	}
	input := agyInput(info.RawInput, info.Args)
	m.mu.Lock()
	m.tools[info.ToolCallID] = askToolCall{name: raw, input: input, kind: info.Kind}
	m.mu.Unlock()

	s.busEmit(scheduler.SessionEvent{
		Kind: "part",
		Type: "tool_use",
		Part: map[string]any{
			// THE RAW AGY NAME, not the canonical one: the operator is watching agy run, and a card that said
			// "bash" for what the model called `run_command` would make the transcript disagree with itself.
			// (The worker mapper canonicalises because ITS consumers are all adapter-neutral; the Ask bus has
			// one consumer that renders what it is told.)
			"tool":   raw,
			"callID": info.ToolCallID,
			"state":  map[string]any{"input": input},
		},
	})
}

// handleToolUpdate applies a later state: the streamed output, the terminal status, and the ASK ledger.
func (m *askMapper) handleToolUpdate(ctx context.Context, s *askSession, info acp.ToolCallInfo) {
	m.mu.Lock()
	state, known := m.tools[info.ToolCallID]
	m.mu.Unlock()

	name := strings.TrimSpace(info.Name)
	if name == "" && known {
		name = state.name
	}
	if name == "" {
		return
	}
	input := agyInput(info.RawInput, info.Args)
	if input == nil && known {
		input = state.input
	}
	kind := info.Kind
	if kind == "" && known {
		kind = state.kind
	}

	switch info.Status {
	case acp.StatusCompleted, acp.StatusFailed:
		output := toolOutput(info)
		s.busEmit(scheduler.SessionEvent{
			Kind:       "tool_result",
			Type:       "tool_result",
			ToolCallID: info.ToolCallID,
			ToolName:   name,
			ArgsJSON:   jsonString(input),
			Output:     output,
			IsError:    info.Status == acp.StatusFailed,
		})
		m.mu.Lock()
		delete(m.tools, info.ToolCallID)
		m.mu.Unlock()
		if info.Status == acp.StatusFailed {
			// A FAILED edit wrote nothing, so it must never touch the ledger.
			return
		}
		m.ledgerEdit(ctx, s, name, kind, info, input)

	default:
		// pending / in_progress: stream the partial content so a long command is visible, but drive NO
		// terminal side effect.
		if output := toolOutput(info); output != "" {
			s.busEmit(scheduler.SessionEvent{
				Kind:       "tool_result",
				Type:       "tool_result",
				ToolCallID: info.ToolCallID,
				ToolName:   name,
				ArgsJSON:   jsonString(input),
				Output:     output,
			})
		}
	}
}

// ledgerEdit drives the ASK diff ledger for a completed file edit.
//
// IT FIRES AT COMPLETION, NOT AT THE FIRST SIGHTING, because the ledger's observer diffs the file's fresh
// on-disk state: an `in_progress` tool_call has not written anything yet, so firing there would ledger a
// file whose contents do not exist.
func (m *askMapper) ledgerEdit(ctx context.Context, s *askSession, rawName, kind string, info acp.ToolCallInfo, input map[string]any) {
	canonical := canonicalToolName(rawName)
	if !isFileEditTool(canonical) && !toolKindWrites(kind) {
		return
	}
	if m.b.askFileEdits == nil {
		return
	}
	dir := s.workDir()
	for _, p := range editedPaths(info, input) {
		if strings.TrimSpace(p) == "" {
			continue
		}
		m.b.askFileEdits(ctx, m.convID, s.tenantFor(), dir, canonical,
			map[string]any{"filePath": ledgerPathFor(dir, p)}, "")
	}
}

// emitPlan maps an ACP `plan` onto the same todowrite envelope the clients already render. agy 1.5.13 was
// not recorded sending one; it exists so a future agy needs no fourth parser.
func (m *askMapper) emitPlan(s *askSession, entries []acp.PlanEntry) {
	if len(entries) == 0 {
		return
	}
	type todo struct {
		Content  string `json:"content"`
		Status   string `json:"status"`
		Priority string `json:"priority,omitempty"`
	}
	items := make([]todo, 0, len(entries))
	for _, e := range entries {
		if c := strings.TrimSpace(e.Content); c != "" {
			st := strings.TrimSpace(e.Status)
			if st == "" {
				st = "pending"
			}
			items = append(items, todo{Content: c, Status: st, Priority: strings.TrimSpace(e.Priority)})
		}
	}
	if len(items) == 0 {
		return
	}
	s.busEmit(scheduler.SessionEvent{
		Kind: "part",
		Type: "tool_use",
		Part: map[string]any{
			"tool":   "todowrite",
			"callID": "plan",
			"state":  map[string]any{"input": map[string]any{"todos": items}},
		},
	})
}

// RecordTurn closes one turn: flush the coalesced text and reasoning as ONE part each, record the usage
// under owner kind ask_conversation, and report the context gauge.
func (m *askMapper) RecordTurn(ctx context.Context, s *askSession, res *acp.PromptResult) {
	m.mu.Lock()
	text := m.textBuf.String()
	reason := m.reasonBuf.String()
	m.textBuf.Reset()
	m.reasonBuf.Reset()
	used, size := m.ctxUsed, m.ctxSize
	m.mu.Unlock()

	// REASONING FIRST: a single turn can carry both, and the thinking precedes the answer it produced.
	if strings.TrimSpace(reason) != "" {
		s.busEmit(scheduler.SessionEvent{Kind: "part", Type: "reasoning", IsReasoning: true, Text: reason})
	}
	if strings.TrimSpace(text) != "" {
		s.busEmit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: text})
	}
	if res != nil {
		m.recordUsage(ctx, s, res, used, size)
	}
}

// recordUsage forwards one turn's token accounting under owner kind ask_conversation.
//
// THE FIELD MAPPING IS DIRECT, exactly as the worker mapper does it, and for the same reason: agy reports
// `inputTokens` ALREADY EXCLUDING the cached reads (docs §3.4), which is NOT Anthropic's semantics, so
// running these numbers through the Anthropic transform would subtract the cache twice.
//
// THE ATTRIBUTION IS THE ASK ONE: ExecutionID, TaskID and WorkerID are all EMPTY because a conversation
// has none of them, and SessionID carries the conversation id — that is how askorchicon attributes the row
// (db.FileEditOwnerAskConversation's counterpart on the usage side).
func (m *askMapper) recordUsage(ctx context.Context, s *askSession, res *acp.PromptResult, used, size int64) {
	if m.b.usageRecorder == nil {
		return
	}
	u := res.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedReadTokens == 0 &&
		u.CachedWriteTokens == 0 && u.ThoughtTokens == 0 {
		// A cancelled turn can carry an all-zero usage object. Recording it would insert a row claiming the
		// turn was free, which is a different claim from "we have no figure".
		m.b.log.Debug("antigravity ask: turn reported no token usage; nothing recorded", "conversation", m.convID)
		return
	}
	s.mu.Lock()
	model := s.model
	pid := s.project
	s.mu.Unlock()
	if model == "" {
		model = "unknown"
	}
	rec := scheduler.UsageRecord{
		TenantID:         s.tenantFor(),
		ProjectID:        pid,
		ExecutionID:      "",
		WorkerID:         "",
		Provider:         "google",
		Model:            model,
		PromptTokens:     u.InputTokens,
		CacheReadTokens:  u.CachedReadTokens,
		CacheWriteTokens: u.CachedWriteTokens,
		CompletionTokens: u.OutputTokens,
		ReasoningTokens:  u.ThoughtTokens,
		AdapterKind:      adapter.KindAntigravity,
		// agy reports NO money figure (docs §8 row 8), so the cost is left for the usage layer to price from
		// the tokens. Reporting 0 here is honest; inventing a number would not be.
		CostUSD:   0,
		SessionID: m.convID,
	}
	if err := m.b.usageRecorder(ctx, rec); err != nil {
		m.b.log.Warn("antigravity ask: record usage failed", "conversation", m.convID, "error", err)
	}
	// THE CONTEXT METER rides the same gauge the worker transcript records in a step_finish part. The bus has
	// no gauge event, so the meter is fed from the usage row's project/conversation pairing plus this log —
	// the numbers are in the record above when the collector asks for them.
	m.b.log.Debug("antigravity ask: turn usage recorded",
		"conversation", m.convID, "input", u.InputTokens, "output", u.OutputTokens,
		"reasoning", u.ThoughtTokens, "context_used", used, "context_size", size)
}

// jsonString renders a map as its JSON text (the InputJSON the consent layer parses), returning "" for an
// empty map so an absent input is not "{}". It is the same function claude's Ask path uses, under a
// package-local name because that one is unexported.
func jsonString(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	out, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(out)
}

// sessionEvent is this package's local alias for the bus event, so a test can name the type without
// importing scheduler at every call site.
type sessionEvent = scheduler.SessionEvent
