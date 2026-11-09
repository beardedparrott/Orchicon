package claude

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// This file is the claude → scheduler.ExecutionCallbacks normalization layer.

// MapperDeps is everything the mapper needs that the owning session already holds.
type MapperDeps struct {
	TenantID string
	Model    string
	// ExecDir is the execution's working directory (worktree or project dir).
	ExecDir  string
	Manifest scheduler.ExecutionManifest

	// FileEdits is the shared diff-pipeline ledger hook (the same concrete
	// function type the opencode adapter takes).
	FileEdits FileEditHookFunc
	// UsageRecorder receives the terminal result message's token/cost
	// telemetry.
	UsageRecorder scheduler.UsageRecorderFunc
	// BindSession claims a freshly-seen claude session id (the single-writer
	// invariant). nil = the id is not tracked.
	BindSession func(ctx context.Context, sid string)
	// Record persists one durable session-transcript part. nil = no
	// transcript.
	Record func(ctx context.Context, kind string, payload map[string]any)
	Log    *slog.Logger
}

// Mapper owns the whole stream-event → ExecutionCallbacks fan-out for one claude session, plus the
// durable transcript + ledger side effects.
type Mapper struct {
	execID string
	cbs    scheduler.ExecutionCallbacks
	deps   MapperDeps

	mu        sync.Mutex
	sessionID string
	tools     map[string]toolCall // tool_use id → what that call was
	fileSet   map[string]bool
	// tasks is the ACCUMULATING claude task list (TaskCreate/TaskUpdate), in creation order.
	tasks            []taskItem
	taskIndex        map[string]int
	taskKeyByToolUse map[string]string
	// recorded is the idempotence guard: a fingerprint of every usage event already recorded, so a
	// replayed/duplicated terminal result (a resume re-emit.
	recorded map[string]struct{}
	// usageAcc accumulates per-message usage samples (assistant / message_start / message_delta), keyed
	// by message id, as a FALLBACK for a terminal result that carries no aggregate usage.
	usageAcc map[string]Usage
	out      strings.Builder // accumulated text for OnResult's output
	textBuf  strings.Builder // per-turn text, coalesced into ONE part
	// reasonBuf is per-turn REASONING, coalesced into ONE reasoning part.
	reasonBuf strings.Builder

	stall *stallMonitor

	// turnUsage/turnCost/turnUsageOK expose the usage the LAST completed turn reported.
	turnUsage   Usage
	turnCost    float64
	turnUsageOK bool
}

// sessionIDFor reads the claude session id the stream reported ("" until
// the first system/init line lands).
func (m *Mapper) sessionIDFor() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionID
}

// setTurnUsage records the turn's usage for the budget gate.
func (m *Mapper) setTurnUsage(u Usage, costUSD float64, ok bool) {
	m.mu.Lock()
	m.turnUsage, m.turnCost, m.turnUsageOK = u, costUSD, ok
	m.mu.Unlock()
}

// turnUsage returns the usage + priced cost of the last completed turn.
func (m *Mapper) turnUsageInfo() (Usage, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnUsage, m.turnCost, m.turnUsageOK
}

// toolCall remembers what one in-flight `tool_use` block called, so the matching `tool_result` can
// drive the side effects that need POST-execution ground truth.
type toolCall struct {
	canonical string
	input     map[string]any
}

// taskItem is one entry of the accumulating claude task list.
type taskItem struct {
	key      string
	content  string
	status   string
	priority string
}

// NewMapper builds a Mapper for one execution.
func NewMapper(execID string, cbs scheduler.ExecutionCallbacks, deps MapperDeps) *Mapper {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &Mapper{
		execID:           execID,
		cbs:              cbs,
		deps:             deps,
		tools:            make(map[string]toolCall),
		fileSet:          make(map[string]bool),
		taskIndex:        make(map[string]int),
		taskKeyByToolUse: make(map[string]string),
		recorded:         make(map[string]struct{}),
		usageAcc:         make(map[string]Usage),
		stall:            newStallMonitor(deps.Manifest, time.Now()),
	}
}

// Output returns the text accumulated so far — the OnResult output payload.
func (m *Mapper) Output() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out.String()
}

// ---------------------------------------------------------------- side effects

func (m *Mapper) recordPart(ctx context.Context, kind string, payload map[string]any) {
	if m.deps.Record == nil {
		return
	}
	m.deps.Record(ctx, kind, payload)
}

// recordToolUse persists one durable tool_use part in opencode's exact envelope: {"part": <part>,
// "error": <err>}.
func (m *Mapper) recordToolUse(ctx context.Context, canonical string, input map[string]any, status, output, errMsg string) {
	if canonical == "" {
		return
	}
	env := map[string]any{
		"part":  toolUsePart(canonical, input, status, output),
		"error": nil,
	}
	if errMsg != "" {
		env["error"] = errMsg
	}
	m.recordPart(ctx, db.SessionPartToolUse, env)
}

// toolUsePart builds the opencode-shaped part map for one tool call.
func toolUsePart(canonical string, input map[string]any, status, output string) map[string]any {
	state := map[string]any{"status": status}
	if input != nil {
		state["input"] = input
	}
	if output != "" {
		state["output"] = output
	}
	return map[string]any{
		"type":  "tool_use",
		"tool":  canonical,
		"state": state,
	}
}

// emitText streams one text fragment and accumulates it.
func (m *Mapper) emitText(ctx context.Context, t string) {
	m.mu.Lock()
	m.out.WriteString(t)
	m.textBuf.WriteString(t)
	m.mu.Unlock()
	m.stall.sawOutput(time.Now())
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
	m.stall.sawOutput(time.Now())
}

// flushReasoning persists the turn's reasoning as ONE durable part, mirroring flushText's shape
// with db.SessionPartReasoning.
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

// flushText persists the turn's text as ONE durable part (opencode coalesces
// per-turn too — a part per token would blow up the transcript).
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

// addFiles emits OnWrittenFiles for the fresh paths (deduped across the session, exactly like
// opencode's incremental file_diff telemetry) and clears an outstanding advisory stall.
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
	if m.stall.sawFileWrite(time.Now()) {
		// The recovered label is the RECOVERY signal, not the stall reason: opencode's monitor reports
		// "recovered:no_file_progress" for the same cleared advisory trip.
		m.cbs.OnRecovered(ctx, m.execID, recoveredNoFilePrefix)
	}
	if len(fresh) > 0 {
		m.cbs.OnWrittenFiles(ctx, m.execID, fresh)
	}
}

// callFileEditHook drives the shared diff-pipeline ledger.
func (m *Mapper) callFileEditHook(ctx context.Context, canonical string, input map[string]any) {
	if m.deps.FileEdits == nil {
		return
	}
	m.deps.FileEdits(ctx, m.execID, m.deps.TenantID, m.deps.ExecDir, canonical, m.ledgerInput(input), "")
}

// ledgerInput reshapes a claude tool input into the key the shared file-edit ledger hook reads
// (`filePath`, then `path`) with the path made execution-dir-relative.
func (m *Mapper) ledgerInput(input map[string]any) map[string]any {
	return map[string]any{"filePath": m.ledgerPath(toolInputPath(input))}
}

// ledgerPath makes claude's file path execution-dir-relative.
func (m *Mapper) ledgerPath(p string) string {
	if p == "" || m.deps.ExecDir == "" || !filepath.IsAbs(p) {
		return p
	}
	base := filepath.Clean(m.deps.ExecDir)
	rel, err := filepath.Rel(base, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}

// recordUsage forwards one usage sample (the terminal turn aggregate, or the accumulated
// per-message fallback) to the shared recorder, attributed to THIS execution.
func (m *Mapper) recordUsage(ctx context.Context, usage Usage, costUSD float64, numTurns int) {
	if m.deps.UsageRecorder == nil {
		return
	}

	// Idempotence (D8): a replayed/duplicated usage event (a resume re-emit, a double delivery) must
	// not double-charge the execution's budget.
	m.mu.Lock()
	fp := usageFingerprint(m.execID, m.sessionID, numTurns, usage, costUSD)
	_, dup := m.recorded[fp]
	if !dup {
		m.recorded[fp] = struct{}{}
	}
	m.mu.Unlock()
	if dup {
		m.deps.Log.Warn("claude: duplicate usage event dropped", "execution", m.execID, "num_turns", numTurns)
		return
	}

	provider, model := m.providerModel()

	// Route the four wire buckets through the shared transform. Build the base
	// identity once; the transform only fills the canonical buckets.
	base := aigateway.UsageInput{
		TenantID:      m.deps.TenantID,
		ProjectID:     m.deps.Manifest.ProjectID,
		TaskID:        m.deps.Manifest.TaskID,
		ExecutionID:   m.execID,
		WorkerID:      m.deps.Manifest.WorkerID,
		Provider:      provider,
		Model:         model,
		AdapterKind:   adapterKindClaude,
		CostUSD:       costUSD,
		WorkflowRunID: m.deps.Manifest.RuntimeWorkflowID,
	}
	canon := aigateway.UsageFromAnthropic(aigateway.AnthropicUsage{
		InputTokens:              usage.InputTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		CacheCreationInputTokens: usage.CacheCreationTokens,
		OutputTokens:             usage.OutputTokens,
	}, base)

	in := scheduler.UsageRecord{
		TenantID:         canon.TenantID,
		ProjectID:        canon.ProjectID,
		TaskID:           canon.TaskID,
		ExecutionID:      canon.ExecutionID,
		WorkerID:         canon.WorkerID,
		Provider:         canon.Provider,
		Model:            canon.Model,
		PromptTokens:     canon.PromptTokens,
		CacheReadTokens:  canon.CacheReadTokens,
		CacheWriteTokens: canon.CacheWriteTokens,
		CompletionTokens: canon.CompletionTokens,
		ReasoningTokens:  canon.ReasoningTokens,
		CostUSD:          canon.CostUSD,
		AdapterKind:      canon.AdapterKind,
		WorkflowRunID:    canon.WorkflowRunID,
	}
	if err := m.deps.UsageRecorder(ctx, in); err != nil {
		m.deps.Log.Warn("claude: record usage failed", "execution", m.execID, "error", err)
	}
}

// adapterKindClaude tags claude usage rows/metrics for adapter parity.
const adapterKindClaude = "claude"

// providerModel resolves the pricing provider and the BARE model id for a claude usage record.
func (m *Mapper) providerModel() (string, string) {
	id := strings.TrimSpace(m.deps.Model)
	if id == "" {
		if parsed, err := parseModelRefLoose(m.deps.Manifest.ModelRef); err == nil {
			id = strings.TrimSpace(parsed)
		}
	}
	// Defensive: strip any adapter/provider prefix (take the last segment).
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return "anthropic", id
}

// usageFingerprint is the D8 idempotence key: the same terminal usage event
// replayed yields the same fingerprint and is dropped.
func usageFingerprint(execID, sessionID string, numTurns int, u Usage, costUSD float64) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d|%.6f",
		execID, sessionID, numTurns,
		u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens, u.OutputTokens,
		costUSD)
}

// accumulateUsage merges one per-message usage sample (keyed by message id) into the fallback
// accumulator, element-wise MAX so the repeated samples the partial-message stream emits for ONE.
func (m *Mapper) accumulateUsage(messageID string, u Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.usageAcc == nil {
		m.usageAcc = map[string]Usage{}
	}
	m.usageAcc[messageID] = mergeUsageMax(m.usageAcc[messageID], u)
}

// takeTurnUsage resolves the usage to record for a terminal turn: the authoritative `result`
// aggregate when present, else the accumulated per-message SUM.
func (m *Mapper) takeTurnUsage(ev StreamEvent) (Usage, bool) {
	if ev.UsagePresent {
		m.resetUsageAccum()
		return ev.Usage, true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() { m.usageAcc = map[string]Usage{} }()
	if len(m.usageAcc) == 0 {
		return Usage{}, false
	}
	var sum Usage
	for _, u := range m.usageAcc {
		sum.InputTokens += u.InputTokens
		sum.OutputTokens += u.OutputTokens
		sum.CacheReadTokens += u.CacheReadTokens
		sum.CacheCreationTokens += u.CacheCreationTokens
	}
	if sum == (Usage{}) {
		return Usage{}, false
	}
	return sum, true
}

// resetUsageAccum clears the per-turn per-message accumulator.
func (m *Mapper) resetUsageAccum() {
	m.mu.Lock()
	m.usageAcc = map[string]Usage{}
	m.mu.Unlock()
}

// mergeUsageMax merges two usage samples element-wise by MAX.
func mergeUsageMax(a, b Usage) Usage {
	return Usage{
		InputTokens:         maxInt64(a.InputTokens, b.InputTokens),
		OutputTokens:        maxInt64(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:     maxInt64(a.CacheReadTokens, b.CacheReadTokens),
		CacheCreationTokens: maxInt64(a.CacheCreationTokens, b.CacheCreationTokens),
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ------------------------------------------------------------------ handling

// Handle dispatches ONE decoded stream event.
func (m *Mapper) Handle(ctx context.Context, ev StreamEvent) bool {
	// They are NEVER summed into an aggregate result — that would double-count the turn.
	if ev.UsagePresent && !ev.UsageIsAggregate {
		m.accumulateUsage(ev.MessageID, ev.Usage)
	}
	switch {
	case ev.Type == "system" && ev.SessionID != "":
		m.handleSystem(ctx, ev.SessionID)
	case ev.Type == "text_delta":
		if ev.Text != "" {
			m.emitText(ctx, ev.Text)
		}
	case ev.Type == "thinking_delta":
		m.emitReasoning(ev.Text)
	case ev.Type == "assistant":
		// Reasoning FIRST, then text: a single assistant message can carry both, and thinking precedes the
		// answer it produced.
		if ev.Reasoning != "" {
			m.emitReasoning(ev.Reasoning)
		}
		if ev.Text != "" {
			m.emitText(ctx, ev.Text)
		}
		for _, tu := range ev.ToolUses {
			m.handleToolUse(ctx, tu)
		}
	case ev.Type == "user":
		for _, tr := range ev.ToolResults {
			m.handleToolResult(ctx, tr)
		}
	case ev.IsTerminalResult():
		// Reasoning first, then text — the order it was produced, so the
		// transcript's reasoning-before-text grouping holds after the flush.
		m.flushReasoning(ctx)
		m.flushText(ctx)
		usage, ok := m.takeTurnUsage(ev)
		if ok {
			m.recordUsage(ctx, usage, ev.TotalCostUSD, ev.NumTurns)
		}
		// Publish the turn's LIVE figures for the shared budget gate (the cache-aware cost the provider
		// priced, and the cache-read tokens) so the claude bridge charges the SAME numbers it reports.
		m.setTurnUsage(usage, ev.TotalCostUSD, ok)
		return true
	}
	return false
}

// handleSystem records the session identity once, binds it (single-writer),
// and emits the healthy health signal.
func (m *Mapper) handleSystem(ctx context.Context, sid string) {
	m.mu.Lock()
	first := m.sessionID != sid
	m.sessionID = sid
	m.mu.Unlock()
	m.recordPart(ctx, db.SessionPartSessionInfo, map[string]any{
		"session_id":   sid,
		"adapter_kind": "claude",
	})
	if !first {
		return
	}
	if m.deps.BindSession != nil {
		m.deps.BindSession(ctx, sid)
	}
	m.cbs.OnHealth(ctx, m.execID, domain.HealthHealthy)
}

// handleToolUse maps one assistant tool_use block onto the tool-call callback plus the durable
// part, and fans the file-writing tools out to OnWrittenFiles, the ledger.
func (m *Mapper) handleToolUse(ctx context.Context, tu ToolUse) {
	canon := canonicalToolName(tu.Name)
	if tu.ID != "" {
		m.mu.Lock()
		m.tools[tu.ID] = toolCall{canonical: canon, input: tu.Input}
		m.mu.Unlock()
	}
	m.stall.sawOutput(time.Now())

	// The claude task family (TaskCreate/TaskUpdate) is CUMULATIVE, not a whole-list replacement:
	// accumulate it and re-emit the full list onto the SAME todowrite envelope.
	if isTaskTool(tu.Name) {
		m.handleTaskTool(ctx, tu)
		return
	}

	input := tu.Input
	if canon == "todowrite" {
		if items := normalizeTodoItems(tu.Input); len(items) > 0 {
			// The durable part must carry the NORMALIZED list so the envelope `{"part":…}` todos parser reads
			// the same {content,status,priority} shape opencode writes.
			input = map[string]any{"todos": items}
			worktree.SaveTodoSnapshot(worktree.BaseFor(m.deps.ExecDir), items)
		}
	}

	m.cbs.OnToolCall(ctx, m.execID, canon, marshalAny(tu.Input), nil)
	m.recordToolUse(ctx, canon, input, "running", "", "")

	paths := writtenFilesFromTool(tu.Name, tu.Input)
	if len(paths) == 0 {
		return
	}
	m.addFiles(ctx, paths)
	// NOTE: the file-edit ledger is deliberately NOT driven from here.
	if canon == "write" {
		if content := strField(tu.Input, "content"); content != "" {
			m.cbs.OnArtifact(ctx, m.execID, paths[0], artifactTypeFromPath(paths[0]), content)
		}
	}
}

// handleToolResult maps one user tool_result block onto OnToolCall (the resolved output) plus a
// durable part carrying the terminal status.
func (m *Mapper) handleToolResult(ctx context.Context, tr ToolResult) {
	m.mu.Lock()
	call := m.tools[tr.ToolUseID]
	delete(m.tools, tr.ToolUseID)
	m.mu.Unlock()
	name := call.canonical
	m.stall.sawOutput(time.Now())
	// Feed the repetition signal (opencode parity): the signature is the
	// canonical tool name plus normalized args, tiered by result status.
	m.stall.sawToolCall(time.Now(), name, string(marshalAny(call.input)), tr.IsError)
	if tr.IsError {
		// A failed call is still activity: it must not look like silence.
		m.stall.sawOutput(time.Now())
	}

	// A TaskCreate/TaskUpdate resolution surfaces on the todowrite channel (the call already did, via
	// emitTodos) so it reads as ONE opencode-style tool call, and the TaskCreate's CLI-allocated id.
	if isTaskTool(name) {
		if !tr.IsError && name == "TaskCreate" {
			if id := taskIDFromResult(tr.Content); id != "" {
				m.rekeyCreatedTask(ctx, tr.ToolUseID, id)
			}
		}
		m.cbs.OnToolCall(ctx, m.execID, "todowrite", nil, []byte(tr.Content))
		return
	}

	m.cbs.OnToolCall(ctx, m.execID, name, nil, []byte(tr.Content))
	status := "completed"
	errMsg := ""
	if tr.IsError {
		status = "error"
		errMsg = tr.Content
	}
	m.recordToolUse(ctx, name, nil, status, tr.Content, errMsg)

	// The diff pipeline's ledger runs HERE, at completion: the hook's observer diffs the file's fresh
	// on-disk state, so it must run after the CLI has performed the write.
	if !tr.IsError && ledgersFileEdit(name) && toolInputPath(call.input) != "" {
		m.callFileEditHook(ctx, name, call.input)
	}
}

// ledgersFileEdit reports whether a canonical tool name is one of the
// file-writing built-ins whose edit the shared diff pipeline must ledger.
func ledgersFileEdit(canonical string) bool { return canonical == "write" || canonical == "edit" }

// Tick evaluates the stall monitor and raises OnStall.
func (m *Mapper) Tick(ctx context.Context, now time.Time) string {
	reason, fatal := m.stall.Evaluate(now)
	if reason == "" {
		return ""
	}
	m.cbs.OnStall(ctx, m.execID, reason, fatal)
	if fatal {
		return reason
	}
	return ""
}

// ------------------------------------------------------------------ helpers

// canonicalToolName normalizes claude's built-in tool vocabulary onto the names opencode uses, so
// the execution view, the ledger hook and the todo parser are adapter-neutral.
func canonicalToolName(name string) string {
	switch name {
	case "TodoWrite":
		return "todowrite"
	case "Write":
		return "write"
	case "Edit", "MultiEdit", "NotebookEdit", "ApplyPatch":
		return "edit"
	case "Read":
		return "read"
	case "Bash":
		return "bash"
	case "Grep":
		return "grep"
	case "Glob":
		return "glob"
	case "LS":
		return "ls"
	case "WebFetch":
		return "webfetch"
	case "WebSearch":
		return "websearch"
	}
	return name
}

// normalizeTodoItems reads a claude TodoWrite input into the canonical todo shape.
func normalizeTodoItems(input map[string]any) []worktree.TodoItem {
	if input == nil {
		return nil
	}
	raw, ok := input["todos"].([]any)
	if !ok {
		return nil
	}
	items := make([]worktree.TodoItem, 0, len(raw))
	for _, r := range raw {
		im, ok := r.(map[string]any)
		if !ok {
			continue
		}
		content := strField(im, "content")
		if content == "" {
			continue
		}
		status := strField(im, "status")
		if status == "" {
			status = "pending"
		}
		items = append(items, worktree.TodoItem{
			Content:  content,
			Status:   status,
			Priority: strField(im, "priority"),
		})
	}
	return items
}

// ------------------------------------------------------- claude task family
//
// Claude Code's task-tracking tools are CUMULATIVE and id-correlated, unlike
// TodoWrite's whole-list replacement: TaskCreate appends one item (whose
// CLI-allocated id arrives LATER, in the paired tool_result), TaskUpdate
// relabels or removes one item by id. The tracker below accumulates that list
// in the mapper and re-emits the WHOLE list onto the exact todowrite envelope
// internal/execution/todos.go parses — so the TUI, the GUI and the snapshot all
// read a claude task list through the SAME single parse site as opencode's
// todowrite, with no adapter-specific branch anywhere.

// isTaskTool reports whether a claude tool name is one of the cumulative
// task-tracking family.
func isTaskTool(name string) bool {
	return name == "TaskCreate" || name == "TaskUpdate"
}

// taskRemoveStatus reports whether a status value REMOVES a row rather than relabelling it.
func taskRemoveStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "deleted", "removed":
		return true
	}
	return false
}

// handleTaskTool accumulates ONE TaskCreate/TaskUpdate and re-emits the full
// list. It is reached only for the task family; TodoWrite keeps its own arm.
func (m *Mapper) handleTaskTool(ctx context.Context, tu ToolUse) {
	switch tu.Name {
	case "TaskCreate":
		content := taskSubject(tu.Input)
		if content == "" {
			content = "task"
		}
		item := taskItem{key: tu.ID, content: content, status: "pending", priority: strField(tu.Input, "priority")}
		m.mu.Lock()
		m.tasks = append(m.tasks, item)
		if item.key != "" {
			m.taskIndex[item.key] = len(m.tasks) - 1
			m.taskKeyByToolUse[tu.ID] = item.key
		}
		m.mu.Unlock()
	case "TaskUpdate":
		id := taskIDFromInput(tu.Input)
		m.mu.Lock()
		if idx, ok := m.taskIndex[id]; ok {
			if subject := taskSubject(tu.Input); subject != "" {
				m.tasks[idx].content = subject
			}
			if status := strField(tu.Input, "status"); status != "" {
				if taskRemoveStatus(status) {
					m.removeTaskLocked(id)
				} else {
					m.tasks[idx].status = status
				}
			}
			if p := strField(tu.Input, "priority"); p != "" {
				m.tasks[idx].priority = p
			}
		}
		m.mu.Unlock()
	}
	m.emitTodos(ctx)
}

// removeTaskLocked drops a row and re-indexes the survivor slots. The caller
// holds m.mu.
func (m *Mapper) removeTaskLocked(key string) {
	idx, ok := m.taskIndex[key]
	if !ok {
		return
	}
	m.tasks = append(m.tasks[:idx], m.tasks[idx+1:]...)
	delete(m.taskIndex, key)
	for k, i := range m.taskIndex {
		if i > idx {
			m.taskIndex[k] = i - 1
		}
	}
}

// rekeyCreatedTask correlates a TaskCreate to the id the CLI ALLOCATED, which arrives in the paired
// tool_result (never in the tool_use block).
func (m *Mapper) rekeyCreatedTask(ctx context.Context, toolUseID, allocated string) {
	if toolUseID == "" || allocated == "" {
		return
	}
	m.mu.Lock()
	old, ok := m.taskKeyByToolUse[toolUseID]
	if !ok || old == allocated {
		m.mu.Unlock()
		return
	}
	idx, ok := m.taskIndex[old]
	if !ok {
		m.mu.Unlock()
		return
	}
	m.tasks[idx].key = allocated
	delete(m.taskIndex, old)
	m.taskIndex[allocated] = idx
	delete(m.taskKeyByToolUse, toolUseID)
	m.mu.Unlock()
	m.emitTodos(ctx)
}

// emitTodos is the ONE place a claude task list leaves the mapper: it snapshots the sidecar file at
// the SAME site and with the SAME semantics as the TodoWrite arm.
func (m *Mapper) emitTodos(ctx context.Context) {
	m.mu.Lock()
	items := make([]worktree.TodoItem, 0, len(m.tasks))
	for _, t := range m.tasks {
		items = append(items, worktree.TodoItem{Content: t.content, Status: t.status, Priority: t.priority})
	}
	m.mu.Unlock()
	input := map[string]any{"todos": items}
	m.cbs.OnToolCall(ctx, m.execID, "todowrite", marshalAny(input), nil)
	worktree.SaveTodoSnapshot(worktree.BaseFor(m.deps.ExecDir), items)
	m.recordToolUse(ctx, "todowrite", input, "completed", "", "")
}

// taskSubject reads a task's human label defensively — the streamed input may
// spell it `subject` (TaskCreate), `content`, or `description`.
func taskSubject(input map[string]any) string {
	for _, k := range []string{"subject", "content", "description"} {
		if v := strField(input, k); v != "" {
			return v
		}
	}
	return ""
}

// taskIDFromInput reads a task's id defensively across claude's spellings (taskId / task_id / id)
// and both scalar shapes (string or number), stripping a leading '#' (claude renders ids as "#7").
func taskIDFromInput(input map[string]any) string {
	for _, k := range []string{"taskId", "task_id", "id"} {
		if v, ok := input[k]; ok {
			if s := scalarString(v); s != "" {
				return normalizeTaskID(s)
			}
		}
	}
	return ""
}

// scalarString renders a JSON scalar (string or number) as a trimmed string.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

// normalizeTaskID strips the '#' claude prefixes onto an allocated id.
func normalizeTaskID(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
}

var (
	// "Task #7 created successfully", "task 7", "task#7" …
	taskAllocIDRe = regexp.MustCompile(`(?i)task\s*#?\s*([0-9]+)`)
	// "#7" on its own.
	hashIDRe = regexp.MustCompile(`#\s*([0-9]+)`)
)

// taskIDFromResult extracts the id the CLI ALLOCATED for a TaskCreate from the paired tool_result
// text.
func taskIDFromResult(content string) string {
	if m := taskAllocIDRe.FindStringSubmatch(content); len(m) == 2 {
		return m[1]
	}
	if m := hashIDRe.FindStringSubmatch(content); len(m) == 2 {
		return m[1]
	}
	return ""
}

// toolInputPath is the path a file-writing tool touched (claude's Write/Edit
// carry `file_path`, NotebookEdit `notebook_path`).
func toolInputPath(input map[string]any) string {
	if p := strField(input, "file_path"); p != "" {
		return p
	}
	return strField(input, "notebook_path")
}

// artifactTypeFromPath tags an artifact by its extension (mirrors opencode/adapter.go's
// artifactTypeFromPath so both adapters label the same file identically).
func artifactTypeFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".md"), strings.HasSuffix(path, ".markdown"):
		return "markdown"
	case strings.HasSuffix(path, ".json"):
		return "json"
	case strings.HasSuffix(path, ".yaml"), strings.HasSuffix(path, ".yml"):
		return "yaml"
	case strings.HasSuffix(path, ".html"), strings.HasSuffix(path, ".htm"):
		return "html"
	case strings.HasSuffix(path, ".csv"):
		return "csv"
	case strings.HasSuffix(path, ".xml"):
		return "xml"
	case strings.HasSuffix(path, ".svg"):
		return "svg"
	default:
		return "text"
	}
}
