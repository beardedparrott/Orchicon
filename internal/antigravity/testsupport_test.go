package antigravity

// testsupport_test.go — A FAKE `agy acp` CHILD, and the harness every bridge test drives.
//
// A REPLAY HARNESS, NOT A MODEL DOUBLE. It speaks the real ACP wire (initialize, authenticate,
// session/new, session/set_model, session/prompt, session/cancel, session/resume, session/close) and it emits
// the update shapes child 1 RECORDED from agy 1.5.13 — the very payloads in
// internal/antigravity/testdata/acp. So a bridge test proves the mapper against the recorded contract
// without spawning agy or spending a token, and an assertion that passes here is an assertion about the
// transcript agy really produces.
//
// WHY THE PROCESS IS DRIVEN RATHER THAN SCRIPTED BY LINE: the client's writes and the child's replies are
// causally linked (a reply must follow the request that asked for it, on the request's id), and a test that
// emits a reply before the request races the client's own goroutine. The driver reads each request, replies
// in order, and for a prompt it emits that turn's recorded updates first.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// harnessModel is the model agy's catalogue offers and every harness execution asks for.
const harnessModel = "gemini-3.8-flash-low"

// harnessSessionID is the agy conversation id the fake allocates for `session/new`.
const harnessSessionID = "e71c8e2b-b1b9-46af-aba3-bbbf4e59159e"

// script is one fake child's behaviour.
type script struct {
	// updates is emitted for the FIRST prompt (one recorded turn), in order.
	updates []string
	// nextUpdates is emitted for each SUBSEQUENT prompt (index 1, 2, …); empty means "echo one message".
	nextUpdates [][]string
	// usage is the turn result's accounting. Zero means the recorded file_edit_diff figure.
	usage *acp.Usage
	// stopReason overrides the prompt result's stop reason.
	stopReason string
	// hang makes a prompt block until `session/cancel` (or the client closing), which is what the abort and
	// mid-run-injection tests need.
	hang bool
	// rejectModel makes `session/set_model` fail with agy's -32602, as unknown_model.jsonl recorded.
	rejectModel bool
	// resumeError makes `session/resume` fail, so the session/load fallback is exercised.
	resumeError bool
	// authError makes `authenticate` fail with agy's own missing-key text (auth_required.jsonl), which matches
	// none of the shared auth signals — the exact case adapter.AntigravityAuthFailure cannot catch on its own.
	authError bool
	// onPrompt, when set, runs before the prompt's updates are emitted — the hook a test uses to trigger an
	// injection, an abort or a compaction while a turn is genuinely in flight.
	onPrompt func(promptIndex int)
	// afterUpdates, when set, runs after the updates are emitted but before the result — the hook for an
	// assertion that must observe the stream mid-turn.
	afterUpdates func(promptIndex int)
}

// defaultTurnUsage is the recorded file_edit_diff turn's accounting: it exercises all four buckets.
func defaultTurnUsage() acp.Usage {
	return acp.Usage{
		InputTokens:       15726,
		OutputTokens:      163,
		CachedReadTokens:  21689,
		CachedWriteTokens: 0,
		ThoughtTokens:     0,
		TotalTokens:       37578,
	}
}

// promptRecord is one prompt the fake received.
type promptRecord struct {
	Text  string
	Index int
}

// fakeACP is a Proc speaking ACP.
type fakeACP struct {
	t *testing.T

	argv []string
	env  []string
	cwd  string

	lines  chan []byte
	stderr chan []byte
	reqs   chan []byte

	exitOnce sync.Once
	// closeMu serialises a send on lines/stderr against shutdown's close of those channels.
	//
	// WITHOUT IT THE FAKE PANICS INTERMITTENTLY. shutdown() closes stop AND lines; emit() then selects
	// between "send on lines" and "receive from stop", and Go picks uniformly among READY cases — but a
	// send on a CLOSED channel is ready-to-panic, not skipped. So half the time the fake chose the panic.
	// The prompt goroutine (go p.handlePrompt) reaches its final emit exactly while AbortExecution tears
	// the child down, which is why only the abort tests caught it. shutdown takes the write lock, so the
	// channels cannot close while an emit is mid-flight.
	closeMu sync.RWMutex
	exited  chan struct{}
	closed  chan struct{}

	mu       sync.Mutex
	script   script
	prompts  []promptRecord
	cancels  int
	resumed  []string
	newCalls int
	closedIn bool
	// modelSets records every session/set_model modelId, in order.
	modelSets []string
	// newParams records every session/new params object verbatim, for the MCP assertion.
	newParams []json.RawMessage
	// permRequests counts permission requests the fake ASKED.
	permAsked []acp.ToolCallInfo
	// pendingRelease unblocks a hanging prompt. It is closed by session/cancel or by release().
	pendingRelease chan struct{}
	// cancelSeen records that a session/cancel arrived, which decides the stopReason of a hanging turn: agy
	// ends it `cancelled` only when it was actually cancelled (cancel_midturn.jsonl), and `end_turn` otherwise.
	cancelSeen bool
	// stop cancels the driver goroutine.
	stop chan struct{}
}

// newFakeACP builds and starts a fake child.
func newFakeACP(t *testing.T, sc script) *fakeACP {
	t.Helper()
	p := &fakeACP{
		t:      t,
		lines:  make(chan []byte, 4096),
		stderr: make(chan []byte, 64),
		reqs:   make(chan []byte, 1024),
		exited: make(chan struct{}),
		closed: make(chan struct{}),
		script: sc,
		stop:   make(chan struct{}),
	}
	go p.drive()
	return p
}

// --- acp.Proc -------------------------------------------------------------------------------------

func (p *fakeACP) WriteLine(b []byte) error {
	select {
	case p.reqs <- append([]byte(nil), b...):
	case <-p.exited:
	}
	return nil
}

func (p *fakeACP) Lines() <-chan []byte  { return p.lines }
func (p *fakeACP) Stderr() <-chan []byte { return p.stderr }

func (p *fakeACP) Wait() (int, error) {
	<-p.exited
	return 0, nil
}

func (p *fakeACP) CloseInput() error {
	p.mu.Lock()
	p.closedIn = true
	p.mu.Unlock()
	return nil
}

func (p *fakeACP) Kill() error  { p.shutdown(); return nil }
func (p *fakeACP) Close() error { p.shutdown(); return nil }

// shutdown ends the fake child: stdout closes and Wait unblocks.
func (p *fakeACP) shutdown() {
	p.exitOnce.Do(func() {
		// The write lock is what makes the closes safe against a concurrent emit: no send can be
		// in flight, and every later emit sees the closed stop and returns on its first check.
		p.closeMu.Lock()
		defer p.closeMu.Unlock()
		close(p.stop)
		close(p.lines)
		close(p.stderr)
		close(p.exited)
	})
}

// --- the driver -----------------------------------------------------------------------------------

// c2s is a client-written document.
type c2s struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

// drive reads requests and answers them in order.
func (p *fakeACP) drive() {
	defer func() {
		// A panic in a test hook must not silently end the child: surface it.
		if r := recover(); r != nil {
			p.t.Errorf("fake agy panicked: %v", r)
		}
	}()
	for {
		select {
		case <-p.stop:
			return
		case raw, ok := <-p.reqs:
			if !ok {
				return
			}
			var doc c2s
			if err := json.Unmarshal(raw, &doc); err != nil {
				continue
			}
			if doc.Method == "" {
				// The CLIENT answering a permission request the fake asked: record and carry on.
				continue
			}
			p.handle(doc)
		}
	}
}

func (p *fakeACP) handle(doc c2s) {
	switch doc.Method {
	case "initialize":
		p.emit(resultDoc(doc.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession":        true,
				"promptCapabilities": map[string]any{"image": true, "audio": false, "embeddedContext": true},
				"sessionCapabilities": map[string]any{
					"resume": map[string]any{}, "close": map[string]any{}, "load": map[string]any{},
					"list": map[string]any{}, "additionalDirectories": map[string]any{},
				},
			},
			"agentInfo": map[string]any{"name": "Antigravity ACP Server", "version": "1.5.13"},
			"authMethods": []map[string]any{
				{"id": "oauth-personal", "name": "Log in with Google"},
				{"id": "gemini-api-key", "name": "Gemini API key"},
				{"id": "oauth-business", "name": "Log in with Gemini Enterprise"},
				{"id": "agent-platform", "name": "Gemini Enterprise Agent Platform"},
			},
		}))

	case "authenticate":
		if p.script.authError {
			p.emit(errorDoc(doc.ID, acp.CodeInvalidParams,
				"GEMINI_API_KEY is not set; pass an API key in _meta.apiKey or set the GEMINI_API_KEY environment variable"))
			return
		}
		p.emit(resultDoc(doc.ID, map[string]any{}))

	case "session/new":
		p.mu.Lock()
		p.newCalls++
		p.newParams = append(p.newParams, append(json.RawMessage(nil), doc.Params...))
		p.mu.Unlock()
		p.emit(resultDoc(doc.ID, p.sessionResult(harnessSessionID)))

	case "session/resume", "session/load":
		var prm struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(doc.Params, &prm)
		if p.script.resumeError && doc.Method == "session/resume" {
			p.emit(errorDoc(doc.ID, acp.CodeInvalidRequest, "session/resume is not supported by this build"))
			return
		}
		p.mu.Lock()
		p.resumed = append(p.resumed, prm.SessionID)
		p.mu.Unlock()
		p.emit(resultDoc(doc.ID, p.sessionResult(prm.SessionID)))

	case "session/set_model":
		var prm struct {
			ModelID string `json:"modelId"`
		}
		_ = json.Unmarshal(doc.Params, &prm)
		if p.script.rejectModel {
			p.emit(errorDoc(doc.ID, acp.CodeInvalidParams, "Unknown model ID: "+prm.ModelID))
			return
		}
		p.mu.Lock()
		p.modelSets = append(p.modelSets, prm.ModelID)
		p.mu.Unlock()
		p.emit(resultDoc(doc.ID, map[string]any{}))

	case "session/set_mode":
		p.emit(resultDoc(doc.ID, map[string]any{}))

	case "session/cancel":
		p.mu.Lock()
		p.cancels++
		p.cancelSeen = true
		p.mu.Unlock()
		// `session/cancel` is a NOTIFICATION: agy answers the in-flight prompt, not the cancel.
		p.mu.Lock()
		release := p.pendingRelease
		p.pendingRelease = nil
		p.mu.Unlock()
		if release != nil {
			close(release)
		}

	case "session/close":
		p.emit(resultDoc(doc.ID, map[string]any{}))

	case "session/prompt":
		// ON ITS OWN GOROUTINE, and that is not tidiness. A prompt that hangs (script.hang) blocks until
		// `session/cancel` arrives — and if it blocked the DRIVER, the driver could never read that cancel. The
		// real child keeps reading its stdin while a turn runs, so the fake must too.
		go p.handlePrompt(doc)

	default:
		p.emit(errorDoc(doc.ID, acp.CodeMethodNotFound, "method not found: "+doc.Method))
	}
}

func (p *fakeACP) handlePrompt(doc c2s) {
	var prm struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(doc.Params, &prm)
	var text strings.Builder
	for _, b := range prm.Prompt {
		text.WriteString(b.Text)
	}

	p.mu.Lock()
	idx := len(p.prompts)
	p.prompts = append(p.prompts, promptRecord{Text: text.String(), Index: idx})
	hook := p.script.onPrompt
	after := p.script.afterUpdates
	release := make(chan struct{})
	if p.script.hang {
		p.pendingRelease = release
	}
	p.mu.Unlock()

	if hook != nil {
		hook(idx)
	}

	updates := p.updatesFor(idx)
	for _, u := range updates {
		// Via emit, never an inline select: the same closed-channel hazard applies to these sends.
		select {
		case <-p.stop:
			return
		default:
		}
		p.emit(u)
	}
	if after != nil {
		after(idx)
	}

	if p.script.hang {
		// Block until session/cancel (or the client tearing the process down). This is what makes the abort
		// and injection tests exercise a GENUINELY in-flight turn.
		select {
		case <-release:
		case <-p.stop:
			return
		}
		p.mu.Lock()
		cancelled := p.cancelSeen
		p.mu.Unlock()
		reason := acp.StopReasonEndTurn
		if cancelled {
			reason = acp.StopReasonCancelled
		}
		p.emit(resultDoc(doc.ID, map[string]any{
			"stopReason": reason,
			"usage":      acp.Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105},
		}))
		return
	}

	reason := p.script.stopReason
	if reason == "" {
		reason = acp.StopReasonEndTurn
	}
	usage := defaultTurnUsage()
	if p.script.usage != nil {
		usage = *p.script.usage
	}
	p.emit(resultDoc(doc.ID, map[string]any{"stopReason": reason, "usage": usage}))
}

// updatesFor returns the update lines for one prompt.
func (p *fakeACP) updatesFor(idx int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx == 0 {
		return p.script.updates
	}
	i := idx - 1
	if i < len(p.script.nextUpdates) {
		return p.script.nextUpdates[i]
	}
	return []string{updateMessage("ok")}
}

// sessionResult renders a session/new|resume|load result with the harness model catalogue.
func (p *fakeACP) sessionResult(sid string) map[string]any {
	return map[string]any{
		"sessionId": sid,
		"models": map[string]any{
			"availableModels": []map[string]any{
				{"modelId": "gemini-4-neon-high", "name": "Gemini 4 Neon (High)"},
				{"modelId": harnessModel, "name": "Gemini 3.8 Flash (Low)"},
				{"modelId": "claude-opus-5-5-low", "name": "Claude Opus 5.5 (Low)"},
			},
			"currentModelId": "gemini-4-neon-high",
		},
		"modes": map[string]any{"currentModeId": "default"},
		"_meta": map[string]any{"antigravity": map[string]any{"tier": map[string]any{"name": "Google AI Pro"}}},
	}
}

// emit writes one line to stdout.
func (p *fakeACP) emit(line string) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	// Non-blocking check FIRST: if the fake is already down, leave without ever entering the select
	// below, where a send on a closed channel would be chosen at random.
	select {
	case <-p.stop:
		return
	default:
	}
	select {
	case p.lines <- []byte(line):
	case <-p.stop:
	}
}

// resultDoc renders a JSON-RPC result.
func resultDoc(id json.RawMessage, result any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	return string(b)
}

// errorDoc renders a JSON-RPC error.
func errorDoc(id json.RawMessage, code int, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": code, "message": msg},
	})
	return string(b)
}

// --- recorded update shapes ------------------------------------------------------------------------

// updateMessage is the recorded agent_message_chunk payload.
func updateMessage(text string) string {
	return updateDoc(map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

// updateThought is the agent_thought_chunk payload. agy 1.5.13 NEVER emits it (docs §3.1); the bridge still
// handles it, and this is what AC1's reasoning assertion is driven with.
func updateThought(text string) string {
	return updateDoc(map[string]any{
		"sessionUpdate": "agent_thought_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

// updateUsage is the context-window gauge (NOT the billable accounting).
func updateUsage(used, size int64) string {
	return updateDoc(map[string]any{"sessionUpdate": "usage_update", "used": used, "size": size})
}

// updateToolCall is a `tool_call` with the recorded Capitalised args + rawInput.
func updateToolCall(id, name, kind, title, status string, args map[string]any, locations ...string) string {
	u := map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    id,
		"name":          name,
		"kind":          kind,
		"title":         title,
		"status":        status,
		"args":          args,
		"rawInput":      args,
	}
	if len(locations) > 0 {
		locs := make([]map[string]any, 0, len(locations))
		for _, l := range locations {
			locs = append(locs, map[string]any{"path": l})
		}
		u["locations"] = locs
	}
	return updateDoc(u)
}

// updateToolCallCompleted is a terminal `tool_call_update`, the shape agy records for a finished call.
// `content` mirrors the recording: a `content` item whose nested text is the command output, and diff items
// for an edit.
func updateToolCallCompleted(id, name, kind string, args map[string]any, content []map[string]any, rawOutput any, locations ...string) string {
	u := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    id,
		"name":          name,
		"kind":          kind,
		"status":        "completed",
		"args":          args,
		"rawInput":      args,
		"content":       content,
	}
	if rawOutput != nil {
		u["rawOutput"] = rawOutput
	}
	if len(locations) > 0 {
		locs := make([]map[string]any, 0, len(locations))
		for _, l := range locations {
			locs = append(locs, map[string]any{"path": l})
		}
		u["locations"] = locs
	}
	return updateDoc(u)
}

// updateToolCallFailed is a terminal `tool_call_update` with status failed (a refused permission).
func updateToolCallFailed(id, name, kind string, args map[string]any, locations ...string) string {
	u := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    id,
		"name":          name,
		"kind":          kind,
		"status":        "failed",
		"args":          args,
		"rawInput":      args,
		"rawOutput":     map[string]any{"output": ""},
		"content":       []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": ""}}},
	}
	if len(locations) > 0 {
		locs := make([]map[string]any, 0, len(locations))
		for _, l := range locations {
			locs = append(locs, map[string]any{"path": l})
		}
		u["locations"] = locs
	}
	return updateDoc(u)
}

// updatePlan is a `plan` update mapped onto the todowrite envelope.
func updatePlan(entries ...map[string]any) string {
	return updateDoc(map[string]any{"sessionUpdate": "plan", "entries": entries})
}

// updateDoc wraps an update in the recorded `session/update` notification envelope.
func updateDoc(update map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": harnessSessionID,
			"update":    update,
		},
	})
	return string(b)
}

// --- the harness -----------------------------------------------------------------------------------

// recordedCapturer is an ExecutionCallbacks sink that records everything, so a test can assert the exact
// sequence the bridge emitted.
type recordedCapturer struct {
	mu        sync.Mutex
	started   []string
	text      []string
	textAll   strings.Builder
	toolCalls []recordedToolCall
	files     [][]string
	artifacts []recordedArtifact
	health    []string
	results   []recordedResult
	done      chan struct{}
}

// recordedToolCall is one OnToolCall.
type recordedToolCall struct {
	Name   string
	Input  []byte
	Output []byte
}

// recordedArtifact is one OnArtifact.
type recordedArtifact struct {
	Name string
	Type string
	Body string
}

// recordedResult is one OnResult.
type recordedResult struct {
	ExecID string
	OK     bool
	Output string
	ErrMsg string
}

func newCapturer() *recordedCapturer {
	return &recordedCapturer{done: make(chan struct{}, 8)}
}

func (c *recordedCapturer) OnStarted(_ context.Context, execID string) {
	c.mu.Lock()
	c.started = append(c.started, execID)
	c.mu.Unlock()
}

func (c *recordedCapturer) OnText(_ context.Context, _, text string) {
	c.mu.Lock()
	c.text = append(c.text, text)
	c.textAll.WriteString(text)
	c.mu.Unlock()
}

func (c *recordedCapturer) OnWrittenFiles(_ context.Context, _ string, files []string) {
	c.mu.Lock()
	c.files = append(c.files, append([]string(nil), files...))
	c.mu.Unlock()
}

func (c *recordedCapturer) OnHealth(_ context.Context, _, state string) {
	c.mu.Lock()
	c.health = append(c.health, state)
	c.mu.Unlock()
}

func (c *recordedCapturer) OnStall(context.Context, string, string, bool) {}
func (c *recordedCapturer) OnRecovered(context.Context, string, string)   {}

func (c *recordedCapturer) OnToolCall(_ context.Context, _, name string, input, output []byte) {
	c.mu.Lock()
	c.toolCalls = append(c.toolCalls, recordedToolCall{
		Name: name, Input: append([]byte(nil), input...), Output: append([]byte(nil), output...),
	})
	c.mu.Unlock()
}

func (c *recordedCapturer) OnArtifact(_ context.Context, _, name, artifactType, content string) {
	c.mu.Lock()
	c.artifacts = append(c.artifacts, recordedArtifact{Name: name, Type: artifactType, Body: content})
	c.mu.Unlock()
}

func (c *recordedCapturer) OnResult(_ context.Context, execID string, ok bool, output, errMsg string) {
	c.mu.Lock()
	c.results = append(c.results, recordedResult{ExecID: execID, OK: ok, Output: output, ErrMsg: errMsg})
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
}

// harness is everything one bridge test needs.
type harness struct {
	t        *testing.T
	b        *Bridge
	manifest scheduler.ExecutionManifest
	cbs      *recordedCapturer
	project  string
	worktree string

	// captured per-execution state.
	mu     sync.Mutex
	procs  []*fakeACP
	usage  []scheduler.UsageRecord
	parts  []db.SessionPart
	ledger []ledgerCall
}

// ledgerCall is one diff-ledger invocation.
type ledgerCall struct {
	ExecID   string
	TenantID string
	ExecDir  string
	Tool     string
	Input    map[string]any
}

// newHarness builds a bridge wired to a fake process, a capturing callbacks sink, a recording usage sink and a
// recording session store — the same setters the server injects.
func newHarness(t *testing.T) *harness {
	t.Helper()
	project := t.TempDir()
	worktree := project + "/.orchicon-worktrees/run_1"
	if err := mkdirAll(worktree); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	h := &harness{
		t:        t,
		b:        New(quiet()),
		cbs:      newCapturer(),
		project:  project,
		worktree: worktree,
	}
	h.manifest = scheduler.ExecutionManifest{
		ExecutionID:        "exe_agy_test",
		ProjectID:          "prj_1",
		TaskID:             "tsk_1",
		WorkerID:           "wrk_1",
		ModelRef:           "antigravity/google/" + harnessModel,
		Goal:               "create hello.txt containing hi and run `cat hello.txt`",
		SystemPrompt:       "You are the Orchicon worker. Project context and skills are appended here.",
		AcceptanceCriteria: "hello.txt contains hi",
		ProjectDir:         project,
		WorktreePath:       worktree,
	}
	// A permissive-but-real policy path: an absent file means "no operator denies", which is the same
	// degrade claude gets on a plane with no policy file.
	h.b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "oauth-personal", ShareTelemetry: false}, "", nil
	})
	h.b.SetBinaryOverride(func() (string, error) { return "/fake/agy", nil })
	h.b.SetUsageRecorder(func(_ context.Context, rec scheduler.UsageRecord) error {
		h.mu.Lock()
		h.usage = append(h.usage, rec)
		h.mu.Unlock()
		return nil
	})
	h.b.SetSessionStore(func(_ context.Context, execID, tenantID string, parts []db.SessionPart) error {
		h.mu.Lock()
		h.parts = append(h.parts, parts...)
		h.mu.Unlock()
		return nil
	})
	h.b.SetDiffLedger(func(_ context.Context, execID, tenantID, execDir, toolName string, input map[string]any, _ string) {
		h.mu.Lock()
		h.ledger = append(h.ledger, ledgerCall{
			ExecID: execID, TenantID: tenantID, ExecDir: execDir, Tool: toolName, Input: input,
		})
		h.mu.Unlock()
	}, nil)
	return h
}

// serve installs a script: every spawned fake child runs it.
func (h *harness) serve(sc script) {
	h.b.SetSpawnOverride(func(_ context.Context, spec acp.ProcSpec) (acp.Proc, error) {
		fp := newFakeACP(h.t, sc)
		fp.argv = append([]string(nil), spec.Argv...)
		fp.env = append([]string(nil), spec.Env...)
		fp.cwd = spec.Cwd
		h.mu.Lock()
		h.procs = append(h.procs, fp)
		h.mu.Unlock()
		h.t.Cleanup(fp.shutdown)
		return fp, nil
	})
}

// serveHanging installs a script whose first prompt never completes, for the abort/injection tests.
func (h *harness) serveHanging() {
	sc := script{hang: true}
	sc.updates = []string{updateUsage(0, 1048576), updateMessage("working on it")}
	h.serve(sc)
}

// proc returns the first fake child spun up (fails when there is none yet).
func (h *harness) proc() *fakeACP {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) == 0 {
		h.t.Fatal("no agy process was spawned")
	}
	return h.procs[0]
}

// procAt returns the i-th fake child.
func (h *harness) procAt(i int) *fakeACP {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.procs) {
		h.t.Fatalf("no agy process at index %d (have %d)", i, len(h.procs))
	}
	return h.procs[i]
}

// exec builds the execution row Start takes.
func (h *harness) exec() db.ExecutionRow {
	return db.ExecutionRow{ID: h.manifest.ExecutionID, TenantID: "tnt_test", WorkerID: h.manifest.WorkerID}
}

// start runs Start in the background with a bounded context.
func (h *harness) start() context.CancelFunc {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = h.b.Start(ctx, h.exec(), h.manifest, h.cbs)
	}()
	h.t.Cleanup(cancel)
	return cancel
}

// startAndWait runs Start to completion, failing the test if it does not finish.
func (h *harness) startAndWait() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(ctx, h.exec(), h.manifest, h.cbs)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		h.t.Fatal("Start never returned (the session did not reach a terminal turn)")
	}
}

// awaitResult blocks until OnResult has fired.
func (h *harness) awaitResult() recordedResult {
	h.t.Helper()
	select {
	case <-h.cbs.done:
	case <-time.After(20 * time.Second):
		h.t.Fatal("OnResult never fired")
	}
	h.cbs.mu.Lock()
	defer h.cbs.mu.Unlock()
	if len(h.cbs.results) == 0 {
		h.t.Fatal("OnResult fired but recorded nothing")
	}
	return h.cbs.results[len(h.cbs.results)-1]
}

// result reads the last recorded OnResult without waiting.
func (h *harness) result() recordedResult {
	h.cbs.mu.Lock()
	defer h.cbs.mu.Unlock()
	if len(h.cbs.results) == 0 {
		h.t.Fatal("no OnResult was recorded")
	}
	return h.cbs.results[len(h.cbs.results)-1]
}

// usageRecords returns the recorded usage samples.
func (h *harness) usageRecords() []scheduler.UsageRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]scheduler.UsageRecord(nil), h.usage...)
}

// sessionParts returns the recorded transcript parts.
func (h *harness) sessionParts() []db.SessionPart {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]db.SessionPart(nil), h.parts...)
}

// partsOfKind returns the recorded parts of one kind.
func (h *harness) partsOfKind(kind string) []db.SessionPart {
	var out []db.SessionPart
	for _, p := range h.sessionParts() {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

// ledgerCalls returns the recorded diff-ledger invocations.
func (h *harness) ledgerCalls() []ledgerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ledgerCall(nil), h.ledger...)
}

// toolCalls returns the recorded OnToolCall entries.
func (h *harness) toolCalls() []recordedToolCall {
	h.cbs.mu.Lock()
	defer h.cbs.mu.Unlock()
	return append([]recordedToolCall(nil), h.cbs.toolCalls...)
}

// findToolCall returns the LAST OnToolCall with the given canonical name.
func (h *harness) findToolCall(name string) (recordedToolCall, bool) {
	calls := h.toolCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name == name {
			return calls[i], true
		}
	}
	return recordedToolCall{}, false
}

// outputText returns the accumulated assistant text the callbacks received.
func (h *harness) outputText() string {
	h.cbs.mu.Lock()
	defer h.cbs.mu.Unlock()
	return h.cbs.textAll.String()
}

// writtenFiles returns every path OnWrittenFiles reported, flattened.
func (h *harness) writtenFiles() []string {
	h.cbs.mu.Lock()
	defer h.cbs.mu.Unlock()
	var out []string
	for _, batch := range h.cbs.files {
		out = append(out, batch...)
	}
	return out
}

// waitFor polls cond until it is true or the deadline expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// mkdirAll makes a directory tree.
func mkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

// release ends a hanging turn WITHOUT a cancel — what agy does when the model simply finishes. It is how a
// test exercises the delivery of a queued turn without also cancelling the session.
func (p *fakeACP) release() {
	p.mu.Lock()
	release := p.pendingRelease
	p.pendingRelease = nil
	p.mu.Unlock()
	if release != nil {
		close(release)
	}
}

// modelIdSets returns every modelId the fake received on `session/set_model`, in order.
func (p *fakeACP) modelIdSets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.modelSets...)
}

// promptsSeen returns every prompt the fake received, in order.
func (p *fakeACP) promptsSeen() []promptRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]promptRecord(nil), p.prompts...)
}

// cancelCount returns how many `session/cancel` notifications the fake received.
func (p *fakeACP) cancelCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancels
}

// resumedIDs returns every session id the fake re-attached to.
func (p *fakeACP) resumedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.resumed...)
}

// sessionNewParams returns every session/new params object, verbatim.
func (p *fakeACP) sessionNewParams() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]json.RawMessage(nil), p.newParams...)
}

// procOrNil returns the first fake child, or nil before one is spawned — the form a waitFor predicate needs,
// because proc() fails the test and a predicate must be allowed to be false.
func (h *harness) procOrNil() *fakeACP {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) == 0 {
		return nil
	}
	return h.procs[0]
}

// procAtOrNil returns the i-th fake child, or nil when it does not exist yet.
func (h *harness) procAtOrNil(i int) *fakeACP {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.procs) {
		return nil
	}
	return h.procs[i]
}

// awaitProc blocks until at least n fake children have been spawned and returns the n-th (1-based n is the
// index n-1).
func (h *harness) awaitProc(t *testing.T, n int) *fakeACP {
	t.Helper()
	waitFor(t, "the agy child to be spawned", func() bool { return h.procAtOrNil(n-1) != nil })
	return h.procAt(n - 1)
}

// awaitPrompts blocks until at least n prompts have been written to the child at index i.
func (h *harness) awaitPrompts(t *testing.T, i, n int) []promptRecord {
	t.Helper()
	waitFor(t, "the prompts to be written", func() bool {
		p := h.procAtOrNil(i)
		return p != nil && len(p.promptsSeen()) >= n
	})
	return h.procAt(i).promptsSeen()
}
