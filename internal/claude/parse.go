package claude

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// This file is the claude → scheduler.ExecutionCallbacks normalization
// layer. Claude Code streams structured tool_use / tool_result blocks over
// the stream-json protocol (its stdout), which is a different transport
// shape from opencode's SSE part stream. The Mapper is what makes every
// downstream surface — the execution view, the todo panel, file diffs, the
// file-edit ledger and telemetry — behave IDENTICALLY for both adapters:
// it canonicalizes claude's tool vocabulary and emits the exact same
// callback sequence plus the exact same durable `{"part": …}` transcript
// envelope opencode emits.
//
// Scope note: this is the WORKER-EXECUTION path only. Nothing here touches
// scheduler.ChatTurnClient — Ask chat on claude is out of scope.

// MapperDeps is everything the mapper needs that the owning session already
// holds. Every hook is optional (nil = that side effect is skipped), which
// keeps the mapper unit-testable with zero I/O.
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

// Mapper owns the whole stream-event → ExecutionCallbacks fan-out for one
// claude session, plus the durable transcript + ledger side effects. It is
// driven from a single goroutine (the session's run loop), so its internal
// state needs no cross-goroutine coordination beyond the small mutex that
// guards Output for the terminal read.
type Mapper struct {
	execID string
	cbs    scheduler.ExecutionCallbacks
	deps   MapperDeps

	mu        sync.Mutex
	sessionID string
	tools     map[string]toolCall // tool_use id → what that call was
	fileSet   map[string]bool
	out       strings.Builder // accumulated text for OnResult's output
	textBuf   strings.Builder // per-turn text, coalesced into ONE part

	stall *stallMonitor
}

// toolCall remembers what one in-flight `tool_use` block called, so the
// matching `tool_result` can drive the side effects that need POST-execution
// ground truth. The file-edit ledger is the one that matters: its observer
// diffs the file's on-disk state, so it can only run once the CLI has
// actually performed the write.
type toolCall struct {
	canonical string
	input     map[string]any
}

// NewMapper builds a Mapper for one execution.
func NewMapper(execID string, cbs scheduler.ExecutionCallbacks, deps MapperDeps) *Mapper {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &Mapper{
		execID:  execID,
		cbs:     cbs,
		deps:    deps,
		tools:   make(map[string]toolCall),
		fileSet: make(map[string]bool),
		stall:   newStallMonitor(deps.Manifest, time.Now()),
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

// recordToolUse persists one durable tool_use part in opencode's exact
// envelope: {"part": <part>, "error": <err>}. This is what makes
// execution.todos.go's todoItemsFromPayload parse claude todos with zero
// adapter-specific branch.
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

// addFiles emits OnWrittenFiles for the fresh paths (deduped across the
// session, exactly like opencode's incremental file_diff telemetry) and
// clears an outstanding advisory stall.
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
		m.cbs.OnRecovered(ctx, m.execID, reasonNoFileProgress)
	}
	if len(fresh) > 0 {
		m.cbs.OnWrittenFiles(ctx, m.execID, fresh)
	}
}

// callFileEditHook drives the shared diff-pipeline ledger. It is called with
// the CANONICAL tool name and an opencode-shaped input ({filePath}) so
// server/fileedit_hook.go's built-in observer path actually fires — claude's
// raw "Write"/"file_path" vocabulary is silently dropped there.
func (m *Mapper) callFileEditHook(ctx context.Context, canonical string, input map[string]any) {
	if m.deps.FileEdits == nil {
		return
	}
	m.deps.FileEdits(ctx, m.execID, m.deps.TenantID, m.deps.ExecDir, canonical, m.ledgerInput(input), "")
}

// ledgerInput reshapes a claude tool input into the key the shared file-edit
// ledger hook reads (`filePath`, then `path`) with the path made
// execution-dir-relative.
func (m *Mapper) ledgerInput(input map[string]any) map[string]any {
	return map[string]any{"filePath": m.ledgerPath(toolInputPath(input))}
}

// ledgerPath makes claude's file path execution-dir-relative. Claude Code
// reports ABSOLUTE paths; the ledger's observer JOINS the path onto the
// execution dir, so an absolute path is looked up at execDir+"/<abs>", the
// read fails, and ObserveAfter returns an empty Entry — the row is silently
// dropped and the diff pane stays empty. Relative paths pass through
// untouched, and a path OUTSIDE the execution dir is left alone so the
// observer's own containment check (not this helper) decides to drop it.
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

// recordUsage forwards the terminal result message's token + cost telemetry.
func (m *Mapper) recordUsage(ctx context.Context, ev StreamEvent) {
	if m.deps.UsageRecorder == nil {
		return
	}
	in := scheduler.UsageRecord{
		TenantID:         m.deps.TenantID,
		ProjectID:        m.deps.Manifest.ProjectID,
		TaskID:           m.deps.Manifest.TaskID,
		ExecutionID:      m.execID,
		WorkerID:         m.deps.Manifest.WorkerID,
		Model:            m.deps.Model,
		PromptTokens:     ev.Usage.InputTokens,
		CacheReadTokens:  ev.Usage.CacheReadTokens,
		CacheWriteTokens: ev.Usage.CacheCreationTokens,
		CompletionTokens: ev.Usage.OutputTokens,
		CostUSD:          ev.TotalCostUSD,
		AdapterKind:      "claude",
		WorkflowRunID:    m.deps.Manifest.RuntimeWorkflowID,
	}
	if err := m.deps.UsageRecorder(ctx, in); err != nil {
		m.deps.Log.Warn("claude: record usage failed", "execution", m.execID, "error", err)
	}
}

// ------------------------------------------------------------------ handling

// Handle dispatches ONE decoded stream event. It returns true at the turn
// boundary (a terminal `result`), which the caller uses to decide whether
// the session stays alive for a queued turn — the mapper never finishes the
// execution itself.
func (m *Mapper) Handle(ctx context.Context, ev StreamEvent) bool {
	switch {
	case ev.Type == "system" && ev.SessionID != "":
		m.handleSystem(ctx, ev.SessionID)
	case ev.Type == "text_delta":
		if ev.Text != "" {
			m.emitText(ctx, ev.Text)
		}
	case ev.Type == "assistant":
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
		m.flushText(ctx)
		m.recordUsage(ctx, ev)
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

// handleToolUse maps one assistant tool_use block onto the tool-call callback
// plus the durable part, and fans the file-writing tools out to
// OnWrittenFiles, the ledger, the artifact channel and the todo snapshot.
func (m *Mapper) handleToolUse(ctx context.Context, tu ToolUse) {
	canon := canonicalToolName(tu.Name)
	if tu.ID != "" {
		m.mu.Lock()
		m.tools[tu.ID] = toolCall{canonical: canon, input: tu.Input}
		m.mu.Unlock()
	}
	m.stall.sawOutput(time.Now())

	input := tu.Input
	if canon == "todowrite" {
		if items := normalizeTodoItems(tu.Input); len(items) > 0 {
			// The durable part must carry the NORMALIZED list so the
			// envelope `{"part":…}` todos parser reads the same
			// {content,status,priority} shape opencode writes.
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
	// NOTE: the file-edit ledger is deliberately NOT driven from here. At
	// tool_use time the CLI has not performed the write yet, so the ledger's
	// observer would diff the file against itself and DROP the row (a live
	// claude run then produces an empty file_edit_ledger and the diff pane
	// stays blank). It fires from handleToolResult, where the file is on disk
	// — the same point opencode fires it (its tool_use part is already
	// `completed`).
	// A `write` is also an inline artifact (mirrors opencode/adapter.go's
	// built-in write → artifact routing).
	if canon == "write" {
		if content := strField(tu.Input, "content"); content != "" {
			m.cbs.OnArtifact(ctx, m.execID, paths[0], artifactTypeFromPath(paths[0]), content)
		}
	}
}

// handleToolResult maps one user tool_result block onto OnToolCall (the
// resolved output) plus a durable part carrying the terminal status. There is
// no OnError on ExecutionCallbacks, so an is_error result surfaces through
// the tool-call output and state.status="error" + a top-level "error" — the
// exact shape opencode writes.
func (m *Mapper) handleToolResult(ctx context.Context, tr ToolResult) {
	m.mu.Lock()
	call := m.tools[tr.ToolUseID]
	delete(m.tools, tr.ToolUseID)
	m.mu.Unlock()
	name := call.canonical
	m.stall.sawOutput(time.Now())

	m.cbs.OnToolCall(ctx, m.execID, name, nil, []byte(tr.Content))
	status := "completed"
	errMsg := ""
	if tr.IsError {
		status = "error"
		errMsg = tr.Content
	}
	m.recordToolUse(ctx, name, nil, status, tr.Content, errMsg)

	// The diff pipeline's ledger runs HERE, at completion: the hook's
	// observer diffs the file's fresh on-disk state, so it must run after the
	// CLI has performed the write. Only a COMPLETED call ledgers — a failed
	// edit carries no ground truth and must never create a phantom row
	// (mirrors opencode/adapter.go's `toolStatus == "completed"` gate).
	if !tr.IsError && ledgersFileEdit(name) && toolInputPath(call.input) != "" {
		m.callFileEditHook(ctx, name, call.input)
	}
}

// ledgersFileEdit reports whether a canonical tool name is one of the
// file-writing built-ins whose edit the shared diff pipeline must ledger.
func ledgersFileEdit(canonical string) bool { return canonical == "write" || canonical == "edit" }

// Tick evaluates the stall monitor and raises OnStall. It returns the reason
// of a FATAL stall (the caller hard-kills the child and fails the execution),
// or "" when nothing tripped or the trip was advisory.
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

// canonicalToolName normalizes claude's built-in tool vocabulary onto the
// names opencode uses, so the execution view, the ledger hook and the todo
// parser are adapter-neutral. Unknown tools pass through verbatim (Task /
// Agent keep their own identity).
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

// normalizeTodoItems reads a claude TodoWrite input into the canonical todo
// shape. It is defensive about claude's repaired input key names — the
// streamed input is the RAW shape, so `id`/`task_id` and `active_form`/
// `activeForm` may both appear; neither is part of the {content,status,
// priority} surface, so both are tolerated (ignored) rather than assumed
// absent. An unknown `status` string is preserved verbatim (the RPC layer
// maps unknown → UNSPECIFIED, never an error).
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

// toolInputPath is the path a file-writing tool touched (claude's Write/Edit
// carry `file_path`, NotebookEdit `notebook_path`).
func toolInputPath(input map[string]any) string {
	if p := strField(input, "file_path"); p != "" {
		return p
	}
	return strField(input, "notebook_path")
}

// artifactTypeFromPath tags an artifact by its extension (mirrors
// opencode/adapter.go's artifactTypeFromPath so both adapters label the same
// file identically).
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

// ------------------------------------------------------------- stall monitor

const (
	reasonNoProgress     = "stalled:no_progress"
	reasonNoFileProgress = "stalled:no_file_progress"

	// Adapter defaults, used when the tenant left the dimension blank
	// (manifest pointer nil). An explicit 0 DISABLES the dimension.
	defaultStallNoProgressWindow = 300 * time.Second
	defaultStallNoFileDiffWindow = 120 * time.Second
)

// stallMonitor is the pure progress watchdog: total silence past the
// no-progress window is FATAL (the caller hard-kills the child), while
// ongoing activity with no file write past the no-file window is ADVISORY
// (the subprocess keeps running; a later file write clears it).
type stallMonitor struct {
	started       time.Time
	lastOutput    time.Time
	lastFileWrite time.Time

	noProgressWindow time.Duration
	noFileWindow     time.Duration

	progressed    bool
	advisoryFired bool
	fatalFired    bool
}

// newStallMonitor resolves the windows from the manifest. nil pointer = the
// tenant has no opinion → the adapter default; non-nil = explicit, and 0
// disables that dimension.
func newStallMonitor(m scheduler.ExecutionManifest, now time.Time) *stallMonitor {
	return &stallMonitor{
		started:          now,
		lastOutput:       now,
		lastFileWrite:    now,
		noProgressWindow: resolveWindow(m.StallNoProgressWindowSeconds, defaultStallNoProgressWindow),
		noFileWindow:     resolveWindow(m.StallNoFileDiffWindowSeconds, defaultStallNoFileDiffWindow),
	}
}

func resolveWindow(v *int64, def time.Duration) time.Duration {
	if v == nil {
		return def
	}
	if *v <= 0 {
		return 0
	}
	return time.Duration(*v) * time.Second
}

// sawOutput records progress (any text or tool activity).
func (s *stallMonitor) sawOutput(now time.Time) {
	s.lastOutput = now
	s.progressed = true
}

// sawFileWrite records a file write and reports whether it cleared an
// outstanding ADVISORY stall (which the caller surfaces via OnRecovered).
func (s *stallMonitor) sawFileWrite(now time.Time) bool {
	s.lastFileWrite = now
	if s.advisoryFired {
		s.advisoryFired = false
		return true
	}
	return false
}

// Evaluate returns the signal to raise ("" = none) and whether it is fatal.
func (s *stallMonitor) Evaluate(now time.Time) (string, bool) {
	if s.noProgressWindow > 0 && !s.fatalFired && now.Sub(s.lastOutput) > s.noProgressWindow {
		s.fatalFired = true
		return reasonNoProgress, true
	}
	if s.noFileWindow > 0 && !s.advisoryFired && !s.fatalFired && s.progressed &&
		now.Sub(s.lastFileWrite) > s.noFileWindow {
		s.advisoryFired = true
		return reasonNoFileProgress, false
	}
	return "", false
}
