// Package opencode implements the OpenCode adapter bridge — a CLI
// subprocess wrapper that translates OpenCode's stdout JSON into
// execution telemetry events (docs/04_Runtime_Adapter_SDK.md §6).
//
// v0.1 transport strategy (docs/04 §6.0): the adapter spawns OpenCode
// as a subprocess, drives it via CLI flags, and parses JSON from stdout.
// This is the only stable surface available today and is sufficient to
// validate the orchestration model end-to-end. When OpenCode ships a
// stable IPC API, the adapter swaps its internals to an IPC client; the
// gRPC contract and control plane are unaffected.
//
// The adapter MUST NOT advertise capabilities the CLI surface cannot
// honestly deliver (docs/04 §6.2). v0.1 advertises a reduced
// capability set.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Adapter is the OpenCode CLI adapter bridge. It implements
// scheduler.AdapterBridge by spawning `opencode` as a subprocess and
// parsing stdout JSON lines into telemetry events.
type Adapter struct {
	log    *slog.Logger
	mu     sync.Mutex
	active map[string]*exec.Cmd // execution_id → running subprocess

	// usageRecorder records LLM usage (Postgres dual-write + OTel
	// metrics) on each step_finish event carrying tokens + cost
	// (docs/04 §6.1, docs/08 §5.2). Injected by the server; nil =
	// usage is not recorded (telemetry loss never blocks control flow
	// — docs/08 §8 invariant #5).
	usageRecorder UsageRecorderFunc
}

// UsageRecord is the usage sample the adapter emits on step_finish
// (docs/04 §6.1 step_finish carries tokens + cost).
type UsageRecord struct {
	TenantID         string
	ProjectID        string
	TaskID           string
	ExecutionID      string
	WorkerID         string
	Provider         string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	CostUSD          float64
	CorrelationID    string
	TraceID          string
}

// UsageRecorderFunc records a usage sample. Decoupled from the
// aigateway package via a function type so the adapter has no import
// dependency on the gateway (docs/04 §6.0: adapter is a thin bridge).
type UsageRecorderFunc func(ctx context.Context, in UsageRecord) error

// SetUsageRecorder injects the usage recording callback. The server
// constructs it from the aigateway.UsageRecorder.
func (a *Adapter) SetUsageRecorder(fn UsageRecorderFunc) { a.usageRecorder = fn }

// New creates an OpenCode adapter bridge.
func New(log *slog.Logger) *Adapter {
	return &Adapter{
		log:    log,
		active: make(map[string]*exec.Cmd),
	}
}

// Start spawns an OpenCode subprocess for the given execution and
// streams telemetry back via the callbacks (docs/03 §4, docs/04 §6).
// The subprocess runs until completion or context cancellation.
//
// Per AGENTS.md verification standards: this adapter calls the REAL
// `opencode` runtime. Simulation mode is an explicit opt-in via the
// ORCHICON_SIMULATE_ADAPTER=1 env var (offline dev only) — it is NOT
// a silent fallback. If `opencode` is absent from PATH and simulation
// is not explicitly enabled, Start returns an error so the failure is
// loud and visible (do not fall back to simulation and claim dispatch
// works). Verification workers/executions must pin a free model in
// model_ref (e.g. opencode/deepseek-v4-flash-free).
//
// Two recovery-relevant guardrails (docs/06 §2 triggers):
//   - Stall detection: a progress monitor detects stuck-looping (no
//     progress, no file changes, repeated tool calls) and raises
//     OnStall → triggers recovery. Catches the loop a hard timeout
//     can't (a worker making "progress" but spinning).
//   - Wall-clock timeout: the worker's budget_overrides.wall_clock_seconds
//     (default 3600) is enforced as a per-execution context deadline.
//     When it hits, the subprocess is killed (exec.CommandContext) →
//     OnResult(false) → recovery triggered with reason
//     "wall_clock_timeout". This is the runaway-spend backstop.
func (a *Adapter) Start(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	// Simulation mode is opt-in ONLY (offline dev). Never a silent
	// fallback (AGENTS.md verification standards).
	if os.Getenv("ORCHICON_SIMULATE_ADAPTER") == "1" {
		a.log.Warn("opencode simulation mode ENABLED via ORCHICON_SIMULATE_ADAPTER=1 (offline dev only — not for verification)", "execution", execRow.ID)
		return a.runSimulation(ctx, execRow, manifest, callbacks)
	}

	binary, err := exec.LookPath("opencode")
	if err != nil {
		// Loud failure: do not silently fall back to simulation. The
		// caller (TaskReconciler) marks the execution failed_to_start
		// and the operator sees the error (AGENTS.md).
		return fmt.Errorf("opencode binary not found on PATH (set ORCHICON_SIMULATE_ADAPTER=1 for offline dev only): %w", err)
	}

	// Wall-clock timeout backstop (docs/06 §2 budget overrun trigger).
	// The worker's budget_overrides.wall_clock_seconds bounds the
	// subprocess; when the deadline hits the process is killed →
	// OnResult(false) → recovery with reason "wall_clock_timeout".
	if deadline, ok := wallClockDeadline(ctx, manifest.Budgets); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	// Build the command. opencode v1.x uses `opencode run [message]`
	// with --format json for machine-readable stdout events
	// (docs/04 §6.0: CLI subprocess is the v0.1 transport). The goal
	// (task title) is the positional message; the model ref maps to
	// --model. System prompts are configured via opencode's agent
	// config, not a CLI flag, so manifest.SystemPrompt is passed via
	// env for the agent config to pick up if needed.
	args := []string{
		"run",
		"--format", "json",
	}
	if manifest.ModelRef != "" {
		args = append(args, "--model", manifest.ModelRef)
	}
	// The goal (task title) is the positional message. Auto-approve
	// permissions so the non-interactive run doesn't block on prompts
	// (docs/04 §6.1: non-interactive mode).
	args = append(args, "--auto", manifest.Goal)

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(),
		"OPENCODE_EXECUTION_ID="+execRow.ID,
		"OPENCODE_TASK_ID="+manifest.TaskID,
		"OPENCODE_PROJECT_ID="+manifest.ProjectID,
	)
	if manifest.SystemPrompt != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_SYSTEM_PROMPT="+manifest.SystemPrompt)
	}

	// Capture stdout + stderr.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencode: start: %w", err)
	}

	a.mu.Lock()
	a.active[execRow.ID] = cmd
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.active, execRow.ID)
		a.mu.Unlock()
	}()

	// Signal execution started (docs/03 §6: assigned → running).
	callbacks.OnStarted(ctx, execRow.ID)

	// Progress monitor: detects stuck-looping (no progress, no file
	// changes, repeated tool calls) and raises OnStall → triggers
	// recovery (docs/06 §2 stalled trigger; docs/03 §5). One monitor
	// per execution; closed when the subprocess exits.
	monitor := newProgressMonitor(execRow.ID, defaultStallWindows())
	go monitor.run(ctx, func(execID, reason string) {
		callbacks.OnStall(ctx, execID, reason)
	})
	defer monitor.close()

	// PR B (context propagation): accumulate the worker's text output
	// across `text` events. The accumulator is closed over by
	// parseStdoutLine; the value is passed to OnResult so the
	// TaskReconciler can extract the ORCHICON WORKER SUMMARY block
	// and propagate it as upstream context for the next stage.
	var output strings.Builder

	// Parse stdout JSON lines into telemetry events
	// (docs/04 §6.1: line-buffered stdout parsing). Each event is also
	// fed to the progress monitor for stall detection.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		a.parseStdoutLine(ctx, execRow, manifest, line, callbacks, monitor, &output)
	}

	// Wait for the process to exit.
	err = cmd.Wait()
	succeeded := err == nil
	callbacks.OnResult(ctx, execRow.ID, succeeded, output.String())
	if err != nil {
		a.log.Warn("opencode subprocess exited with error", "execution", execRow.ID, "error", err)
	}
	return nil
}

// parseStdoutLine decodes a JSON line from OpenCode's stdout into a
// telemetry event and routes it to the callbacks. The JSON shape follows
// opencode v1.x's `--format json` event stream (docs/04 §6.1):
// each line has `type`, `timestamp`, `sessionID`, and a `part` object.
// Each event is also fed to the progress monitor (may be nil in tests)
// for stall detection (docs/06 §2 stalled trigger).
//
// `output` is the per-execution text accumulator (PR B — context
// propagation). For "text" events the part text is appended so the
// full worker output is available to OnResult.
func (a *Adapter) parseStdoutLine(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, line string, callbacks scheduler.ExecutionCallbacks, monitor *progressMonitor, output *strings.Builder) {
	var evt map[string]any
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		// Non-JSON line: treat as a log/progress marker.
		a.log.Debug("opencode stdout (non-JSON)", "execution", execRow.ID, "line", line)
		return
	}
	eventType, _ := evt["type"].(string)
	part, _ := evt["part"].(map[string]any)
	if monitor != nil {
		monitor.observe(eventType, part)
	}
	execID := execRow.ID
	switch eventType {
	case "step_start":
		a.log.Info("opencode step started", "execution", execID)
	case "text":
		// Text part: the model's response text. PR B: append to the
		// accumulator so the TaskReconciler can extract the
		// ORCHICON WORKER SUMMARY block on completion.
		text, _ := part["text"].(string)
		if output != nil && text != "" {
			output.WriteString(text)
		}
		a.log.Info("opencode prompt response", "execution", execID, "text_len", len(text))
	case "tool_call":
		toolName, _ := part["tool"].(string)
		a.log.Info("opencode tool call", "execution", execID, "tool", toolName)
		inp, _ := json.Marshal(part)
		callbacks.OnToolCall(ctx, execID, toolName, inp, nil)
	case "tool_result":
		toolName, _ := part["tool"].(string)
		a.log.Info("opencode tool result", "execution", execID, "tool", toolName)
		out, _ := json.Marshal(part)
		callbacks.OnToolCall(ctx, execID, toolName, nil, out)
	case "file_diff":
		path, _ := part["path"].(string)
		a.log.Info("opencode file diff", "execution", execID, "path", path)
	case "step_finish":
		// Step completion carries token usage + cost (docs/04 §6.1).
		// Record it via the AI Gateway dual-write: Postgres source of
		// truth + OTel metrics → ClickHouse (docs/08 §5.2). Best-effort
		// — telemetry loss never blocks control flow (docs/08 §8).
		tokens, _ := part["tokens"].(map[string]any)
		cost, _ := part["cost"].(float64)
		a.log.Info("opencode step finished", "execution", execID, "cost", cost, "tokens", tokens)
		a.recordUsage(ctx, execRow, manifest, tokens, cost)
		// Publish the accumulated output text for the runtime session pane.
		if output != nil && output.Len() > 0 {
			callbacks.OnText(ctx, execID, output.String())
		}
	case "health":
		if state, ok := evt["state"].(string); ok {
			callbacks.OnHealth(ctx, execID, state)
		}
	case "error":
		msg, _ := evt["message"].(string)
		a.log.Warn("opencode error", "execution", execID, "message", msg)
		callbacks.OnHealth(ctx, execID, domain.HealthUnhealthy)
	default:
		a.log.Debug("opencode event", "execution", execID, "type", eventType)
	}
}

// recordUsage records a usage sample from a step_finish event via the
// AI Gateway dual-write (docs/08 §5.2). It extracts token counts from
// the opencode JSON shape and derives provider/model from the manifest's
// ModelRef (which the human defined — docs/05 §11). Best-effort: a nil
// recorder means usage is not recorded (docs/08 §8).
func (a *Adapter) recordUsage(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, tokens map[string]any, cost float64) {
	if a.usageRecorder == nil {
		return
	}
	promptTokens := toInt64(tokens["prompt_tokens"])
	completionTokens := toInt64(tokens["completion_tokens"])
	if promptTokens == 0 && completionTokens == 0 && cost == 0 {
		return
	}
	provider, model := parseModelRef(manifest.ModelRef)
	in := UsageRecord{
		TenantID:         execRow.TenantID,
		ProjectID:        execRow.ProjectID,
		TaskID:           execRow.TaskID,
		ExecutionID:      execRow.ID,
		WorkerID:         manifest.WorkerID,
		Provider:         provider,
		Model:            model,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          cost,
	}
	if err := a.usageRecorder(ctx, in); err != nil {
		a.log.Warn("usage record failed", "execution", execRow.ID, "error", err)
	}
}

// parseModelRef splits a model ref like "anthropic/claude-sonnet-4" or
// "opencode/deepseek-v4-flash-free" into (provider, model). If there is
// no "/", provider is "unknown" and model is the whole ref.
func parseModelRef(ref string) (provider, model string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "unknown", "unknown"
	}
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return "unknown", ref
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// runSimulation emits synthetic telemetry events so the dispatch flow
// can be verified end-to-end without the `opencode` binary
// (docs/04 §6.3: in-process adapter for tests/dev only).
func (a *Adapter) runSimulation(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	callbacks.OnStarted(ctx, execRow.ID)

	// Simulate a short execution with progress events.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	steps := 0
	maxSteps := 3
	for {
		select {
		case <-ctx.Done():
			callbacks.OnResult(ctx, execRow.ID, false, "")
			return ctx.Err()
		case <-ticker.C:
			steps++
			a.log.Info("opencode simulation: progress",
				"execution", execRow.ID, "step", steps, "max", maxSteps,
				"goal", manifest.Goal)
			if steps >= maxSteps {
				// Simulation emits no real worker output, so the
				// summary is empty; the workflow run sees an empty
				// _summary for this stage.
				callbacks.OnResult(ctx, execRow.ID, true, "")
				return nil
			}
		}
	}
}

// Compile-time assertion that Adapter satisfies the AdapterBridge
// interface.
var _ scheduler.AdapterBridge = (*Adapter)(nil)
