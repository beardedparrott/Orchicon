package antigravity

// mapper.go — the ACP `session/update` stream → scheduler.ExecutionCallbacks fan-out for one agy
// session, plus the durable transcript, the diff ledger and the usage recorder.
//
// THIS IS THE PORT OF internal/claude/parse.go, AND IT EXISTS AS ITS OWN TYPE FOR A REASON. The Ask
// bridge (child 6) drives the same ACP client and must render tool calls, text and usage exactly the
// same way; if the mapping lived inside the worker session, child 6 would either import a session it
// does not want or write a second mapper that drifts. So the mapper depends on NOTHING session-shaped:
// it takes callbacks, the resolution callbacks it needs as funcs, and it is driven update-by-update.
//
// WHERE agy DIFFERS FROM claude, AND WHAT THAT MEANS HERE:
//
//   - Reasoning text does not exist. agy 1.5.13 never emits `agent_thought_chunk`; only
//     `usage.thoughtTokens` (docs §3.1, thinking_turn.jsonl). The AgentThoughtChunk arm is still
//     implemented for a future agy, and the reasoning part / callback shape matches claude's so a GUI
//     that renders reasoning needs no adapter branch.
//   - A tool call's RESULT is a later `tool_call_update` with the same toolCallId, not a separate
//     `tool_result` block. So the terminal status arrives on the update, and the diff content lives
//     there too — which is why the ledger fires from the update, never from the first sighting.
//   - Usage arrives twice: as `usage_update{used,size}` (a CONTEXT GAUGE) during the turn and as
//     `usage{inputTokens,…}` on the prompt RESULT (the BILLABLE ACCOUNTING). They are different
//     things and are recorded differently — conflating them would either bill a context gauge or lose
//     the cache-read tokens.
//   - There is no `system/init`: the session id comes from `session/new`'s response and every update
//     carries it, so the mapper is told the id rather than discovering it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// FileEditHookFunc is the diff-pipeline ledger callback. It is the SAME concrete function type as
// claude.FileEditHookFunc and scheduler.DiffLedgerFunc (a defined type, so this alias is what lets the
// bridge satisfy DiffLedgerSink without importing claude's alias).
type FileEditHookFunc = func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string)

// MapperDeps is everything the mapper needs that the owning session already holds.
type MapperDeps struct {
	TenantID string
	// Model is the BARE model id the session is driving (used for the usage record's pricing key).
	Model string
	// ExecDir is the execution's working directory (worktree or project dir).
	ExecDir string
	// Manifest carries the attribution fields the usage record needs.
	Manifest scheduler.ExecutionManifest
	// FileEdits is the shared diff-pipeline ledger hook.
	FileEdits FileEditHookFunc
	// UsageRecorder receives each turn's token accounting.
	UsageRecorder scheduler.UsageRecorderFunc
	// AuthMethod is the NORMALIZED agy auth method this session authenticated with (authmethod.Methods()).
	// It decides the usage row's BILLING REGIME: an OAuth method draws on the operator's plan quota (no
	// per-token price exists), an API-key method is metered and the usage layer prices it from the
	// catalog. An empty value is normalized to authmethod.Default, so a caller that has nothing to say
	// gets the same regime agy itself would have used.
	AuthMethod string
	// BindSession claims the agy session id for this execution (the single-writer invariant). nil =
	// the id is not tracked.
	BindSession func(ctx context.Context, sid string)
	// Record persists one durable session-transcript part. nil = no transcript.
	Record func(ctx context.Context, kind string, payload map[string]any)
	// OnViolation reports a tool call that escaped the permission policy (agy may auto-approve in its
	// default mode when its own settings allow it). The session turns this into a cancel + failure.
	OnViolation func(tool, reason string)
	Log         *slog.Logger
}

// toolCallState remembers what one in-flight agy tool call was, so the terminal update can drive the
// side effects that need the call's own arguments.
type toolCallState struct {
	canonical string
	agyName   string
	kind      string
	input     map[string]any
}

// Mapper owns the whole update → callbacks fan-out for one agy session.
type Mapper struct {
	execID string
	cbs    scheduler.ExecutionCallbacks
	deps   MapperDeps

	mu        sync.Mutex
	sessionID string
	tools     map[string]toolCallState
	fileSet   map[string]bool
	// recorded is the idempotence guard for usage: a fingerprint of every turn already recorded, so a
	// replayed or duplicated result cannot double-charge the execution's budget (claude's D8 rule).
	recorded map[string]struct{}
	out      strings.Builder // accumulated assistant text, for OnResult's output
	textBuf  strings.Builder // per-turn text, coalesced into ONE durable part
	// reasonBuf is per-turn REASONING, coalesced into ONE part. Always empty against agy 1.5.13.
	reasonBuf strings.Builder

	// ctxUsed/ctxSize are the last context-window gauge agy reported.
	ctxUsed int64
	ctxSize int64
	// turn is the 1-based count of completed turns; it is the usage fingerprint's discriminator.
	turn int
	// boundOnce guards the session-id bind so a replayed session_info_update re-binds once only.
	boundOnce bool

	// violations counts tool calls that escaped the permission policy, for the session's backstop.
	violations int
	// lastViolation is the human-readable reason of the most recent one.
	lastViolation string
}

// NewMapper builds a Mapper for one execution.
func NewMapper(execID string, cbs scheduler.ExecutionCallbacks, deps MapperDeps) *Mapper {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &Mapper{
		execID:   execID,
		cbs:      cbs,
		deps:     deps,
		tools:    make(map[string]toolCallState),
		fileSet:  make(map[string]bool),
		recorded: make(map[string]struct{}),
	}
}

// Output returns the assistant text accumulated so far — the OnResult output payload.
func (m *Mapper) Output() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out.String()
}

// ContextGauge reports the most recent context-window gauge (used, size), both 0 before agy reports
// one. It is what the session's compaction ladder reads.
func (m *Mapper) ContextGauge() (used, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctxUsed, m.ctxSize
}

// Violations reports how many tool calls escaped the permission policy, and the last reason.
func (m *Mapper) Violations() (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.violations, m.lastViolation
}

// BindSessionID tells the mapper the agy session id the client allocated. agy sends the id on every
// update, so this is a shortcut for the session, not a substitute for the update-time bind.
func (m *Mapper) BindSessionID(ctx context.Context, sid string) {
	if strings.TrimSpace(sid) == "" {
		return
	}
	m.mu.Lock()
	m.sessionID = sid
	first := !m.boundOnce
	m.boundOnce = true
	m.mu.Unlock()
	if first && m.deps.BindSession != nil {
		m.deps.BindSession(ctx, sid)
	}
	m.recordPart(ctx, db.SessionPartSessionInfo, map[string]any{
		"session_id":   sid,
		"adapter_kind": adapter.KindAntigravity,
	})
}

// sessionIDFor reads the agy session id ("" until bound).
func (m *Mapper) sessionIDFor() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionID
}

// ---------------------------------------------------------------- side effects

func (m *Mapper) recordPart(ctx context.Context, kind string, payload map[string]any) {
	if m.deps.Record == nil {
		return
	}
	m.deps.Record(ctx, kind, payload)
}

// recordToolUse persists one durable tool_use part in opencode's envelope: {"part": <part>}.
func (m *Mapper) recordToolUse(ctx context.Context, canonical, agyName string, input map[string]any, status, output, errMsg string) {
	if canonical == "" {
		return
	}
	env := map[string]any{
		"part":  toolUsePart(canonical, agyName, input, status, output),
		"error": nil,
	}
	if errMsg != "" {
		env["error"] = errMsg
	}
	m.recordPart(ctx, db.SessionPartToolUse, env)
}

// toolUsePart builds the opencode-shaped part map for one tool call. `agy_name` is carried alongside
// the canonical name so the RAW agy spelling survives in the transcript even though the UI, the ledger
// and the todo parser all read the canonical one.
func toolUsePart(canonical, agyName string, input map[string]any, status, output string) map[string]any {
	state := map[string]any{"status": status}
	if input != nil {
		state["input"] = input
	}
	if output != "" {
		state["output"] = output
	}
	part := map[string]any{
		"type":  "tool_use",
		"tool":  canonical,
		"state": state,
	}
	if agyName != "" && agyName != canonical {
		part["agy_name"] = agyName
	}
	return part
}

// emitText streams one assistant text fragment and accumulates it.
func (m *Mapper) emitText(ctx context.Context, t string) {
	if t == "" {
		return
	}
	m.mu.Lock()
	m.out.WriteString(t)
	m.textBuf.WriteString(t)
	m.mu.Unlock()
	m.cbs.OnText(ctx, m.execID, t)
}

// emitReasoning accumulates one reasoning fragment.
func (m *Mapper) emitReasoning(t string) {
	if t == "" {
		return
	}
	m.mu.Lock()
	m.reasonBuf.WriteString(t)
	m.mu.Unlock()
}

// flushReasoning persists the turn's reasoning as ONE durable part (claude's shape exactly).
func (m *Mapper) flushReasoning(ctx context.Context) {
	m.mu.Lock()
	body := m.reasonBuf.String()
	m.reasonBuf.Reset()
	m.mu.Unlock()
	if strings.TrimSpace(body) == "" {
		return
	}
	m.recordPart(ctx, db.SessionPartReasoning, map[string]any{
		"part": map[string]any{"type": "reasoning", "text": body},
	})
}

// flushText persists the turn's text as ONE durable part; a part per token would blow up the
// transcript.
func (m *Mapper) flushText(ctx context.Context) {
	m.mu.Lock()
	body := m.textBuf.String()
	m.textBuf.Reset()
	m.mu.Unlock()
	if strings.TrimSpace(body) == "" {
		return
	}
	m.recordPart(ctx, db.SessionPartText, map[string]any{
		"part": map[string]any{"type": "text", "text": body},
	})
}

// addFiles emits OnWrittenFiles for the fresh paths, deduped across the session exactly like
// opencode's incremental file_diff telemetry.
func (m *Mapper) addFiles(ctx context.Context, paths []string) {
	m.mu.Lock()
	var fresh []string
	for _, p := range paths {
		if p == "" || m.fileSet[p] {
			continue
		}
		m.fileSet[p] = true
		fresh = append(fresh, p)
	}
	m.mu.Unlock()
	if len(fresh) > 0 {
		m.cbs.OnWrittenFiles(ctx, m.execID, fresh)
	}
}

// callFileEditHook drives the shared diff-pipeline ledger for one completed edit.
//
// IT RUNS AT COMPLETION, NOT AT THE FIRST SIGHTING, because the hook's observer diffs the file's fresh
// on-disk state: firing on the `in_progress` tool_call would ledger a file the agent has not written
// yet.
func (m *Mapper) callFileEditHook(ctx context.Context, canonical, path string) {
	if m.deps.FileEdits == nil || path == "" {
		return
	}
	input := map[string]any{"filePath": ledgerPathFor(m.deps.ExecDir, path)}
	m.deps.FileEdits(ctx, m.execID, m.deps.TenantID, m.deps.ExecDir, canonical, input, "")
}

// recordUsage forwards one turn's token accounting to the shared recorder.
//
// THE FIELD MAPPING IS DIRECT, NOT VIA aigateway.UsageFromAnthropic, and that is deliberate: agy
// reports `inputTokens` ALREADY EXCLUDING the cached reads (docs §3.4 — totalTokens is
// input+output+cachedRead+cachedWrite), which is NOT Anthropic's semantics (where input excludes
// cache-read AND cache-creation separately, and the sum is derived). Running agy's numbers through the
// Anthropic transform would subtract the cache a second time and under-report the prompt.
func (m *Mapper) recordUsage(ctx context.Context, res *acp.PromptResult) {
	if m.deps.UsageRecorder == nil || res == nil {
		return
	}
	u := res.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedReadTokens == 0 &&
		u.CachedWriteTokens == 0 && u.ThoughtTokens == 0 {
		// A cancelled turn can carry an all-zero usage object. Recording it would insert a usage row
		// that says the turn was free, which is a different claim from "we have no figure".
		m.deps.Log.Debug("antigravity: turn reported no token usage; nothing recorded", "execution", m.execID)
		return
	}

	m.mu.Lock()
	// THE FINGERPRINT IS THE WHOLE ACCOUNTING TUPLE, NOT A TURN COUNTER, and that is deliberate. agy reports
	// no turn number on the prompt result, so a re-delivered result (a reconnect, a duplicated answer) is
	// indistinguishable from the original except by its numbers. Keying on them drops the duplicate and
	// charges the turn once.
	//
	// The trade is that two CONSECUTIVE byte-identical turns are treated as one re-delivery. That cannot
	// happen in practice — a second turn's input tokens always include the first turn's output, so the
	// tuple grows — and the error on the wrong side of the trade (double-charging a budget) is the worse one.
	fp := fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d|%d", m.execID, m.sessionID,
		u.InputTokens, u.OutputTokens, u.CachedReadTokens, u.CachedWriteTokens, u.ThoughtTokens, u.TotalTokens)
	_, dup := m.recorded[fp]
	if !dup {
		m.recorded[fp] = struct{}{}
		m.turn++
	}
	turn := m.turn
	m.mu.Unlock()
	if dup {
		m.deps.Log.Warn("antigravity: duplicate usage event dropped", "execution", m.execID, "turn", turn)
		return
	}

	rec := scheduler.UsageRecord{
		TenantID:         m.deps.TenantID,
		ProjectID:        m.deps.Manifest.ProjectID,
		TaskID:           m.deps.Manifest.TaskID,
		ExecutionID:      m.execID,
		WorkerID:         m.deps.Manifest.WorkerID,
		Provider:         "google",
		Model:            m.providerModel(),
		PromptTokens:     u.InputTokens,
		CacheReadTokens:  u.CachedReadTokens,
		CacheWriteTokens: u.CachedWriteTokens,
		CompletionTokens: u.OutputTokens,
		ReasoningTokens:  u.ThoughtTokens,
		AdapterKind:      adapter.KindAntigravity,
		WorkflowRunID:    m.deps.Manifest.RuntimeWorkflowID,
		// agy reports NO money figure (docs §8 row 8), so cost is left for the usage layer to price
		// from the tokens. Reporting 0 here is honest; the recorder prices it.
		//
		// THE REGIME DECIDES WHETHER THERE IS ANYTHING TO PRICE AT ALL. On plan quota the turn is
		// already paid for by the operator's subscription and no per-token charge exists — the row is
		// marked db.BillingPlanQuota so the cost explorer says "included in plan" instead of pricing a
		// zero as if it were free. On an API-key method the row is metered (Billing "") and the recorder
		// prices it from the catalog.
		CostUSD: 0,
		Billing: m.billingRegime(),
	}
	if err := m.deps.UsageRecorder(ctx, rec); err != nil {
		m.deps.Log.Warn("antigravity: record usage failed", "execution", m.execID, "error", err)
	}
	// The context gauge travels in the step_finish part: scheduler.UsageRecord has no window field and
	// inventing a DB column for it is not this adapter's call.
	used, size := m.ContextGauge()
	m.recordPart(ctx, db.SessionPartStepFinish, map[string]any{
		"usage": map[string]any{
			"input":          u.InputTokens,
			"output":         u.OutputTokens,
			"cached_read":    u.CachedReadTokens,
			"cached_write":   u.CachedWriteTokens,
			"reasoning":      u.ThoughtTokens,
			"total":          u.TotalTokens,
			"stop_reason":    res.StopReason,
			"context_window": map[string]any{"used": used, "size": size},
		},
	})
}

// SetAuthMethod records the NORMALIZED auth method the session authenticated with, which decides the
// usage rows' billing regime.
//
// IT IS A SETTER RATHER THAN A CONSTRUCTOR FIELD because of the ORDER the session runs in: the mapper is
// built with the session (`newSession`), and the auth method is only known once `run` has resolved tenant
// settings and built the spawn spec. Setting it at construction would mean resolving settings twice and
// risking the two answers disagreeing. Until it is set, an empty method normalizes to
// authmethod.Default — the same regime agy itself would have used.
func (m *Mapper) SetAuthMethod(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps.AuthMethod = method
}

// billingRegime maps the session's agy auth method onto the usage row's billing marker.
//
// oauth-personal and oauth-business draw on a Google subscription: there is no per-token price to record,
// so the row is db.BillingPlanQuota. gemini-api-key and agent-platform are metered, so the row is left
// metered ("") and the usage layer prices it from the catalog. An empty method normalizes to
// authmethod.Default (plan quota), matching what the spawn spec would have authenticated with.
func (m *Mapper) billingRegime() string {
	switch authmethod.Normalize(m.deps.AuthMethod) {
	case authmethod.MethodOAuthPersonal, authmethod.MethodOAuthBusiness:
		return db.BillingPlanQuota
	default:
		return ""
	}
}

// providerModel resolves the BARE model id for the usage record's pricing key.
func (m *Mapper) providerModel() string {
	id := strings.TrimSpace(m.deps.Model)
	if id == "" {
		if parsed, err := parseModelRefLoose(m.deps.Manifest.ModelRef); err == nil {
			id = strings.TrimSpace(parsed)
		}
	}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// ---------------------------------------------------------------- handling

// Handle dispatches ONE decoded ACP update.
func (m *Mapper) Handle(ctx context.Context, u acp.Update) {
	switch up := u.(type) {
	case acp.AgentMessageChunk:
		if up.Content.Type == "text" || up.Content.Type == "" {
			m.emitText(ctx, up.Content.Text)
		}

	case acp.AgentThoughtChunk:
		// agy 1.5.13 never sends this (docs §3.1); a future agy might. There is no reasoning CALLBACK
		// in scheduler.ExecutionCallbacks — claude's reasoning takes the same path — so it becomes a
		// durable part and nothing else.
		if up.Content.Type == "text" || up.Content.Type == "" {
			m.emitReasoning(up.Content.Text)
		}

	case acp.UserMessageChunk:
		// A `session/load` replay of the original prompt. Forwarding it as output would attribute the
		// user's words to the model and duplicate the goal in the transcript.

	case acp.ToolCall:
		m.handleToolCall(ctx, up.ToolCallInfo)

	case acp.ToolCallUpdate:
		m.handleToolUpdate(ctx, up.ToolCallInfo)

	case acp.Plan:
		m.emitPlan(ctx, up.Entries)

	case acp.UsageUpdate:
		m.mu.Lock()
		if up.Size > 0 {
			m.ctxUsed, m.ctxSize = up.Used, up.Size
		}
		m.mu.Unlock()

	case acp.SessionInfoUpdate:
		// The title agy derived from the first prompt. It is display-only; the session_id part written
		// by BindSessionID is what recovery reads.

	case acp.AvailableCommands, acp.CurrentModeUpdate, acp.ConfigOptionUpdate:
		// Advertised surfaces the bridge does not act on. Deliberately silent: these repeat every turn
		// and a log line per turn is noise.

	case acp.Unknown:
		// Forward compatibility: a future agy adding an update must not fail a turn. The acp package
		// already logged it at debug level.
	}
}

// handleToolCall records the first sighting of a tool call and streams it to the UI with its input.
func (m *Mapper) handleToolCall(ctx context.Context, info acp.ToolCallInfo) {
	canonical := canonicalToolName(info.Name)
	if canonical == "" {
		return
	}
	input := agyInput(info.RawInput, info.Args)
	m.mu.Lock()
	m.tools[info.ToolCallID] = toolCallState{
		canonical: canonical,
		agyName:   info.Name,
		kind:      info.Kind,
		input:     input,
	}
	m.mu.Unlock()

	// THE POST-HOC BOUNDARY BACKSTOP. agy's DEFAULT mode can auto-approve an edit without ever sending a
	// permission request (docs §3.3 does not promise it will ask), so a call the policy never saw can
	// still reach the filesystem. We cannot prevent it from here — but we can refuse to let the run
	// continue as if nothing happened. This is a DETECTOR, and the limitation is documented in
	// docs/adapters/antigravity-acp.md §11.
	m.checkBoundary(info, input)

	m.cbs.OnToolCall(ctx, m.execID, canonical, marshalToolInput(input), nil)
	m.recordToolUse(ctx, canonical, info.Name, input, "running", "", "")
}

// handleToolUpdate applies a later state of a tool call: the streamed output, the terminal status, the
// written files and the diff ledger.
func (m *Mapper) handleToolUpdate(ctx context.Context, info acp.ToolCallInfo) {
	canonical := canonicalToolName(info.Name)
	if canonical == "" {
		return
	}
	m.mu.Lock()
	state, ok := m.tools[info.ToolCallID]
	m.mu.Unlock()
	if ok && state.canonical != "" {
		canonical = state.canonical
	}
	input := agyInput(info.RawInput, info.Args)
	if input == nil && ok {
		input = state.input
	}
	kind := info.Kind
	if kind == "" && ok {
		kind = state.kind
	}

	switch info.Status {
	case acp.StatusCompleted, acp.StatusFailed:
		// Terminal: emit the resolved output, close the durable part, then run the side effects.
		output := toolOutput(info)
		m.cbs.OnToolCall(ctx, m.execID, canonical, marshalToolInput(input), []byte(output))
		if info.Status == acp.StatusFailed {
			m.recordToolUse(ctx, canonical, info.Name, input, "error", output, output)
			m.mu.Lock()
			delete(m.tools, info.ToolCallID)
			m.mu.Unlock()
			// A FAILED edit wrote nothing, so it must never touch the ledger or OnWrittenFiles.
			return
		}
		m.recordToolUse(ctx, canonical, info.Name, input, "completed", output, "")
		m.mu.Lock()
		delete(m.tools, info.ToolCallID)
		m.mu.Unlock()

		if isFileEditTool(canonical) || toolKindWrites(kind) {
			paths := editedPaths(info, input)
			m.addFiles(ctx, paths)
			for _, p := range paths {
				m.callFileEditHook(ctx, canonical, p)
				if canonical == "write" {
					// An artifact is a whole file the worker produced, which is what the claude bridge
					// reports for its `write` tool. `edit` deliberately does not emit one: it produced a
					// modification, not a new artifact.
					if content := diffNewText(info); content != "" {
						m.cbs.OnArtifact(ctx, m.execID, p, artifactTypeFromPath(p), content)
					}
				}
			}
		}

	default:
		// pending / in_progress: the call is still running. Stream the partial content so a long command
		// that reports progress is visible, but drive NO terminal side effect.
		if output := toolOutput(info); output != "" {
			m.cbs.OnToolCall(ctx, m.execID, canonical, marshalToolInput(input), []byte(output))
		}
	}
}

// emitPlan maps an ACP `plan` onto the SAME todowrite envelope the GUI, the TUI and the todo snapshot
// read for opencode and claude (claude's emitTodos, parse.go).
//
// agy 1.5.13 was not recorded sending a `plan` update, so this is protocol-shaped rather than
// recorded; it exists because a future agy that does send one must not need a fourth parser.
func (m *Mapper) emitPlan(ctx context.Context, entries []acp.PlanEntry) {
	if len(entries) == 0 {
		return
	}
	items := make([]worktree.TodoItem, 0, len(entries))
	for _, e := range entries {
		content := strings.TrimSpace(e.Content)
		if content == "" {
			continue
		}
		status := strings.TrimSpace(e.Status)
		if status == "" {
			status = "pending"
		}
		items = append(items, worktree.TodoItem{
			Content:  content,
			Status:   status,
			Priority: strings.TrimSpace(e.Priority),
		})
	}
	if len(items) == 0 {
		return
	}
	input := map[string]any{"todos": items}
	m.cbs.OnToolCall(ctx, m.execID, "todowrite", marshalToolInput(input), nil)
	if m.deps.ExecDir != "" {
		worktree.SaveTodoSnapshot(worktree.BaseFor(m.deps.ExecDir), items)
	}
	m.recordToolUse(ctx, "todowrite", "plan", input, "completed", "", "")
}

// RecordTurn closes a turn: flush the coalesced parts, record the usage, emit the health signal for a
// live session, and (for the mapper's own record) count the turn.
//
// It is called by the session when `session/prompt` returns, NOT per update: the prompt result is agy's
// only turn terminator.
func (m *Mapper) RecordTurn(ctx context.Context, res *acp.PromptResult) {
	m.flushReasoning(ctx)
	m.flushText(ctx)
	if res != nil {
		m.recordUsage(ctx, res)
	}
	m.cbs.OnHealth(ctx, m.execID, domain.HealthHealthy)
}

// checkBoundary is the post-hoc detector: an UNGATED edit or a never-allow command that reached the
// agent is recorded as a violation, which the session turns into a cancellation.
func (m *Mapper) checkBoundary(info acp.ToolCallInfo, input map[string]any) {
	canonical := canonicalToolName(info.Name)
	writes := isFileEditTool(canonical) || toolKindWrites(info.Kind)
	reason := ""
	if writes {
		if p := toolInputPath(input); p != "" {
			if allowed, _ := workerOutsideProject(p, m.deps.Manifest.ProjectDir); !allowed {
				reason = fmt.Sprintf("%s targeted %s, which is outside the project and outside every carve-out, "+
					"without a permission request", info.Name, p)
			}
		}
	}
	if reason == "" && canonical == "bash" {
		if cmd := toolInputCommand(input); cmd != "" {
			if rule, denied := workerMatchCommand(cmd); denied {
				reason = fmt.Sprintf("%s ran %q, which matches the worker deny rule %q, without a permission request",
					info.Name, cmd, rule)
			}
		}
	}
	if reason == "" {
		return
	}
	m.mu.Lock()
	m.violations++
	m.lastViolation = reason
	m.mu.Unlock()
	m.deps.Log.Error("antigravity: a tool call escaped the permission policy", "execution", m.execID, "reason", reason)
	if m.deps.OnViolation != nil {
		m.deps.OnViolation(info.Name, reason)
	}
}

// ------------------------------------------------------------------ helpers

// toolOutput renders a tool call's output text from the shapes agy uses: `content[]` items of type
// "content" (whose nested text is the command output) or a plain string, and `rawOutput.output`.
func toolOutput(info acp.ToolCallInfo) string {
	var b strings.Builder
	for _, c := range info.Content {
		if c.Type == "content" {
			if t := c.Text(); t != "" {
				b.WriteString(t)
			}
			continue
		}
		if c.Type == "text" {
			if t := c.Text(); t != "" {
				b.WriteString(t)
			}
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	// rawOutput.output is what run_command puts the command's own output in, and it is present even when
	// content[] is empty.
	if len(info.RawOutput) > 0 {
		var raw struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(info.RawOutput, &raw); err == nil {
			return raw.Output
		}
		var s string
		if err := json.Unmarshal(info.RawOutput, &s); err == nil {
			return s
		}
	}
	return ""
}

// editedPaths is every path an edit-class tool call touched: the diff content's paths, the call's
// locations, then the input's own path. All three are deduped by the caller.
func editedPaths(info acp.ToolCallInfo, input map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, c := range info.Content {
		if c.Type == "diff" {
			add(c.Path)
		}
	}
	for _, l := range info.Locations {
		add(l.Path)
	}
	add(toolInputPath(input))
	return out
}

// diffNewText is the fresh text of the call's first diff item — what a whole-file write produced.
func diffNewText(info acp.ToolCallInfo) string {
	for _, c := range info.Content {
		if c.Type == "diff" && c.NewText != "" {
			return c.NewText
		}
	}
	return ""
}
