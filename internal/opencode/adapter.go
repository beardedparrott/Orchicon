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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Adapter is the OpenCode CLI adapter bridge. It implements
// scheduler.AdapterBridge by spawning `opencode` as a subprocess and
// parsing stdout JSON lines into telemetry events.
type Adapter struct {
	log    *slog.Logger
	mu     sync.Mutex
	active map[string]*exec.Cmd // execution_id → running subprocess

	// rt is the workflow runtime daemon client. When non-nil AND an
	// execution carries a RuntimeWorkflowID, the adapter dispatches into
	// that workflow's runtime container instead of spawning a local
	// subprocess. Nil keeps everything in-process (headless serve).
	rt *runtime.Client

	// usageRecorder records LLM usage (Postgres dual-write + OTel
	// metrics) on each step_finish event carrying tokens + cost
	// (docs/04 §6.1, docs/08 §5.2). Injected by the server; nil =
	// usage is not recorded (telemetry loss never blocks control flow
	// — docs/08 §8 invariant #5).
	usageRecorder UsageRecorderFunc
}

// SetRuntimeClient injects the workflow runtime daemon client. When set,
// executions with a RuntimeWorkflowID dispatch into that workflow's
// runtime container; without it the adapter runs in-process.
func (a *Adapter) SetRuntimeClient(rt *runtime.Client) { a.rt = rt }

// IsExecutionActive reports whether an in-process execution subprocess is
// still tracked as running. Used by the execution-liveness reaper to
// detect executions orphaned by a control-plane restart (a fresh boot has
// an empty active map, so every previously-running in-process execution
// is correctly reported dead).
func (a *Adapter) IsExecutionActive(execID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cmd, ok := a.active[execID]
	if !ok || cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
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
	WorkflowRunID    string // immutable link to the workflow run; survives execution deletion
}

// UsageRecorderFunc records a usage sample. Decoupled from the
// aigateway package via a function type so the adapter has no import
// dependency on the gateway (docs/04 §6.0: adapter is a thin bridge).
type UsageRecorderFunc func(ctx context.Context, in UsageRecord) error

// SetUsageRecorder injects the usage recording callback. The server
// constructs it from the aigateway.UsageRecorder.
func (a *Adapter) SetUsageRecorder(fn UsageRecorderFunc) { a.usageRecorder = fn }

// workerAgent is the opencode agent name the adapter injects the worker's
// composed system prompt under (selected with --agent).
const workerAgent = "orchicon-worker"

// Default reconnect tuning (overridable via tenant settings). Reconnect
// attempts bounds how many times a broken exec stream is retried; the
// reconnect grace is how long the runtime supervisor keeps an orphaned
// child alive waiting for a re-attach.
const (
	defaultReconnectAttempts     = 3
	defaultReconnectGraceSeconds = 60
)

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

	// Resolve the adapter CLI. Orchicon never ships opencode — in
	// containerized deployments the operator's host install is mounted at
	// $HOME/.opencode (see scripts/container.sh and the runtime daemon's
	// standard mounts), so besides PATH we also probe that location
	// directly. The error is loud either way: the caller (TaskReconciler)
	// marks the execution failed_to_start and the operator sees it
	// (AGENTS.md).
	binary, err := exec.LookPath("opencode")
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			cand := filepath.Join(home, ".opencode", "bin", "opencode")
			if st, serr := os.Stat(cand); serr == nil && !st.IsDir() {
				binary = cand
			}
		}
	}
	if binary == "" {
		return fmt.Errorf("opencode binary not found on PATH or ~/.opencode/bin (install it on the host: curl -fsSL https://opencode.ai/install | bash; set ORCHICON_SIMULATE_ADAPTER=1 only for offline dev): %w", err)
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
	// --model.
	//
	// System prompts are configured via opencode's agent config
	// (the `prompt` field on an agent), not a CLI flag. We pass the
	// composed worker system prompt through OPENCODE_CONFIG_CONTENT
	// (a JSON document consumed by opencode v1.x at startup) and
	// select it with --agent. This ensures the worker's Role, Skills,
	// Behavior, and AGENTS.md context are actually delivered to the
	// model on every interaction — see worker prompt fields refactor
	// (v0.1.139). Earlier code set OPENCODE_SYSTEM_PROMPT as an env
	// var, but opencode does not read that var, so the prompt was
	// silently dropped.
	args := []string{
		"run",
		"--format", "json",
	}
	modelRef := manifest.ModelRef
	if modelRef == "" {
		modelRef = manifest.DefaultModelRef
		if modelRef == "" {
			return fmt.Errorf("no model_ref specified on worker and no default model set in tenant settings — dispatch rejected")
		}
		a.log.Info("no model_ref on worker, using tenant default", "model", modelRef, "execution", execRow.ID)
	}
	args = append(args, "--model", modelRef)
	// Inject the worker's composed system prompt via a custom agent
	// (orchicon-worker) so the prompt reaches the model on every
	// turn. Auto-approve permissions so the non-interactive run
	// doesn't block on prompts (docs/04 §6.1: non-interactive mode).
	// --dir alone does NOT prevent out-of-project access (agents can
	// still run commands against paths outside the project root); the
	// actual sandbox is enforced by the permission rules injected into
	// OPENCODE_CONFIG_CONTENT in BuildConfigContent — external_directory
	// is denied and destructive bash commands are hard-denied, both of
	// which hold even under --auto (docs/05 §10: workers must operate
	// within their assigned project directory).
	if manifest.SystemPrompt != "" {
		args = append(args, "--agent", workerAgent)
	}
	args = append(args, "--auto", manifest.Goal)

	// Resolve the working directory FIRST, before creating the
	// command, so --dir is baked into the arg slice. Go's
	// exec.CommandContext captures args at creation time, so any
	// append after that line is silently ignored — which is why
	// earlier code that appended --dir after CommandContext failed
	// to scope opencode to the project directory, causing workers
	// to operate on Orchicon's own repo instead.
	var tmpDir string
	runDir := manifest.ProjectDir
	if runDir == "" {
		tmpDir, _ = os.MkdirTemp("", "orchicon-exec-*")
		if tmpDir != "" {
			runDir = tmpDir
		}
	}
	if runDir != "" {
		args = append(args, "--dir", runDir)
	}

	// Runtime-container dispatch (pure per-workflow execution): when the
	// execution belongs to a workflow run AND a runtime daemon is
	// configured, run opencode inside that workflow's runtime container
	// (created + mounted by the daemon at run start). The in-container
	// supervisor builds the same safety guard, applies the same
	// OPENCODE_CONFIG_CONTENT env, and streams stdout/stderr back; the
	// guard dir lives in the container's /tmp, not the plane's.
	if a.rt != nil && manifest.RuntimeWorkflowID != "" {
		env := a.opencodeEnv(execRow, manifest, modelRef)
		return a.startInRuntime(ctx, execRow, manifest, callbacks, args, env, runDir)
	}

	// Safety guard: shim dangerous binaries ahead of the worker's PATH so
	// destructive commands (rm -rf /, sudo, dd of=/dev/*, mkfs, ...) are
	// refused at the OS level — even when they are issued from inside a
	// subprocess (a python TUI, os.system, subprocess.run) where opencode's
	// permission rules never see the real command. Applied to EVERY worker
	// execution; file operations are scoped to the project directory.
	// See internal/opencode/guard.go.
	g, gErr := newExecutionGuard(runDir)
	if gErr != nil {
		a.log.Warn("opencode: safety guard NOT applied", "execution", execRow.ID, "error", gErr)
	}
	if g != nil {
		defer g.Close()
	}

	// Best-effort: drop the safety lint script into .orchicon/ so review
	// and QA workers can run it (their bash tool is scoped to the project
	// directory). See internal/opencode/lint.go.
	writeSafetyLint(runDir)

	cmd := exec.CommandContext(ctx, binary, args...)
	if runDir != "" {
		cmd.Dir = runDir
	}
	// Build the OPENCODE_CONFIG_CONTENT with the agent config, the user's
	// MCP servers, AND the built-in Orchicon MCP (tenant-scoped) so every
	// worker execution gets Orchicon's tools natively via MCP. The MCP
	// sidecar runs in the same namespace as the plane and reaches the
	// plane's Postgres through the inherited DSN.
	cfgJSON := BuildConfigContent(ConfigOptions{
		AgentName:   workerAgent,
		AgentPrompt: manifest.SystemPrompt,
		ModelRef:    modelRef,
		TenantID:    execRow.TenantID,
		OrchiconMCP: true,
	})
	env := append(os.Environ(),
		"OPENCODE_EXECUTION_ID="+execRow.ID,
		"OPENCODE_TASK_ID="+manifest.TaskID,
		"OPENCODE_PROJECT_ID="+manifest.ProjectID,
		"OPENCODE_CONFIG_CONTENT="+cfgJSON,
	)
	if g != nil {
		env = g.Apply(env)
	}
	cmd.Env = env

	// Capture stdout + stderr. Stderr is logged to the control plane's
	// stderr, captured into a buffer for error reporting, AND emitted
	// as OTel log records into Loki so execution stderr appears
	// in the telemetry logs tab (docs/08 §5.3).
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	stderrReader, stderrWriter := io.Pipe()
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf, stderrWriter)

	// Goroutine: read stderr lines and emit as OTel log records.
	// Each line carries the execution context (execution_id, project_id,
	// task_id, worker_id, trace_id) so it correlates with the execution
	// span in the Grafana UI. The severity is smart-parsed from the line:
	// a leading "[ERROR]" / "[WARN]" / "[INFO]" / "[DEBUG]" tag is
	// honoured; anything else defaults to INFO so the telemetry logs
	// tab reflects the worker's actual progress (tool calls, step
	// boundaries, model responses) rather than only hard errors.
	go func() {
		defer stderrReader.Close()
		sc := bufio.NewScanner(stderrReader)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		baseAttrs := []attribute.KeyValue{
			attribute.String("execution_id", execRow.ID),
			attribute.String("project_id", manifest.ProjectID),
			attribute.String("task_id", manifest.TaskID),
		}
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\n\r")
			if line == "" {
				continue
			}
			severity := "INFO"
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "[ERROR]"), strings.HasPrefix(upper, "ERROR:"), strings.HasPrefix(upper, "ERROR "):
				severity = "ERROR"
			case strings.HasPrefix(upper, "[WARN]"), strings.HasPrefix(upper, "WARN:"), strings.HasPrefix(upper, "WARN "):
				severity = "WARN"
			case strings.HasPrefix(upper, "[DEBUG]"), strings.HasPrefix(upper, "DEBUG:"), strings.HasPrefix(upper, "DEBUG "):
				severity = "DEBUG"
			}
			telemetry.EmitLog(ctx, severity, line, baseAttrs...)
		}
	}()

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
		if tmpDir != "" {
			os.RemoveAll(tmpDir)
		}
	}()

	// Signal execution started (docs/03 §6: assigned → running).
	callbacks.OnStarted(ctx, execRow.ID)

	// Per-execution streaming state. textSeq is a monotonically
	// increasing per-execution counter so the frontend can order chunks
	// even if NATS delivers them out of order. The text accumulator
	// (`output`) is closed-over by parseStdoutLine so the ORCHICON
	// WORKER SUMMARY block can still be extracted at OnResult time
	// (PR B — context propagation), independent of how the chunks were
	// fanned out for the live UI.
	textSeq := 0

	// Accumulated JSON error message. opencode's `--format json`
	// stream emits {"type":"error","error":{"data":{"message":"..."}}}
	// when the model/API reports a failure. PR #64 wired error_message
	// through every failure path except this one — without it, a failed
	// stream shows up as just "exit status 1" (cmd.Wait()'s generic
	// error) and the operator can't tell *why* the run failed. We
	// stash the most recent JSON error message and fold it into the
	// OnResult error so the execution detail page shows the real reason.
	var lastStreamErr string

	// Stream-structure counters (step_start vs step_finish) so a clean
	// exit that never delivered the final model step can be flagged as
	// failed instead of reported as a successful empty run.
	stats := &execStreamState{}

	// PR B (context propagation): accumulate the worker's text output
	// across `text` events. The accumulator is closed over by
	// parseStdoutLine; the value is passed to OnResult so the
	// TaskReconciler can extract the ORCHICON WORKER SUMMARY block
	// and propagate it as upstream context for the next stage.
	var output strings.Builder

	// Progress monitor: detects stuck-looping (no progress, no file
	// changes, repeated tool calls) and raises OnStall → triggers
	// recovery (docs/06 §2 stalled trigger; docs/03 §5). One monitor
	// per execution; closed when the subprocess exits.
	// Stall thresholds come from tenant settings (ExecutionManifest)
	// with env-var fallback for dev debugging overrides.
	monitor := newProgressMonitor(execRow.ID, stallWindowsFromManifest(manifest))
	go monitor.run(ctx, func(execID, reason string) {
		callbacks.OnStall(ctx, execID, reason)
	})
	defer monitor.close()

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
		a.parseStdoutLine(ctx, execRow, manifest, line, callbacks, monitor, &output, &lastStreamErr, &textSeq, stats)
	}

	// Check for scanner error (e.g. truncated output).
	scanErr := scanner.Err()
	if scanErr != nil {
		a.log.Warn("opencode stdout scan error", "execution", execRow.ID, "error", scanErr)
	}

	// Wait for the process to exit.
	err = cmd.Wait()
	succeeded := err == nil

	// A clean exit with a step still in flight means the final model
	// response never reached us (line-cap drop, stream truncation,
	// runtime disconnect). That is NOT a success — the worker's output
	// and ORCHICON WORKER SUMMARY / decision signal are missing. Downgrade
	// the result to a failure so the run surfaces the real problem
	// instead of routing an empty execution downstream as if it worked.
	unfinishedStep := stats.stepStarts > stats.stepFinishes
	if succeeded && unfinishedStep {
		succeeded = false
	}

	// Build the error message from most specific to least. The
	// JSON-stream error (extracted from opencode's structured error
	// event) is the real cause — e.g. a provider 401 or rate-limit.
	// Stderr has surrounding context. Exit status is the fallback.
	var parts []string
	if unfinishedStep {
		parts = append(parts, "execution ended before the final model step completed (model response stream truncated or event dropped)")
	}
	if lastStreamErr != "" {
		parts = append(parts, lastStreamErr)
	}
	if stderrBuf.Len() > 0 {
		parts = append(parts, strings.TrimSpace(stderrBuf.String()))
	}
	if err != nil {
		parts = append(parts, err.Error())
	}
	if scanErr != nil {
		parts = append(parts, "stdout scan: "+scanErr.Error())
	}
	errorMsg := strings.Join(parts, "; ")
	callbacks.OnResult(ctx, execRow.ID, succeeded, output.String(), errorMsg)
	return nil
}

// execStreamState tracks per-execution opencode stream structure so a
// run that ends before its final model step completes can be flagged as
// failed instead of reported as a clean, empty success. opencode emits a
// step_start before every model turn and a step_finish when the turn
// ends; a clean exit (exit 0) with an unpaired step_start means the
// final response was lost in transit (line-cap drop, stream truncation,
// runtime disconnect) — the worker's output and decision signal are
// missing, so the run must NOT count as succeeded.
type execStreamState struct {
	stepStarts   int
	stepFinishes int
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
//
// `textSeq` is a per-execution monotonic counter for streamed text
// chunks. The frontend uses it to order chunks if NATS delivers them
// out of order; it is incremented once per emitTextChunk call.
//
// `stats` (may be nil in tests) counts step_start/step_finish events so
// the caller can detect an execution that ended mid-step.
func (a *Adapter) parseStdoutLine(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, line string, callbacks scheduler.ExecutionCallbacks, monitor *progressMonitor, output *strings.Builder, lastStreamErr *string, textSeq *int, stats *execStreamState) {
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
	case evtStepStart:
		if stats != nil {
			stats.stepStarts++
		}
		a.log.Info("opencode step started", "execution", execID)
	case evtText:
		// Text part: the model's response text. PR B: append to the
		// accumulator so the TaskReconciler can extract the
		// ORCHICON WORKER SUMMARY block on completion. Also fan the
		// text out as incremental chunks (textStreamingChunkSize +
		// textStreamingChunkDelay) so the runtime session pane shows
		// a typing-style live stream — opencode's --format json
		// delivers the full text in one event at step_finish, so
		// without chunking the user would see no streaming at all.
		text, _ := part["text"].(string)
		if output != nil && text != "" {
			output.WriteString(text)
		}
		if text != "" {
			a.emitTextChunked(ctx, callbacks, execID, text, textSeq)
		}
		a.log.Debug("opencode text", "execution", execID, "text_len", len(text))
	case evtToolUse:
		// opencode v1.x: input + output both arrive in a single
		// `tool_use` event. The previous dispatch only matched the
		// legacy `tool_call` / `tool_result` pair — which v1.x never
		// emits — so tool calls were silently dropped and the
		// runtime session pane never saw any tool cards. The model's
		// actual work (file writes, bash commands, web fetches) was
		// invisible to the operator. Handle the v1.x shape:
		//   {
		//     "type": "tool_use",
		//     "part": {
		//       "tool": "bash",
		//       "callID": "...",
		//       "state": {
		//         "status": "completed",
		//         "input":  {...},
		//         "output": "...",
		//         "title":  "...",
		//         "time":   {...}
		//       }
		//     }
		//   }
		toolName, _ := part["tool"].(string)
		state, _ := part["state"].(map[string]any)
		inRaw, _ := state["input"]
		outStr, _ := state["output"].(string)

		// Detect `write` tool calls (opencode built-in file writer)
		// and route them as artifacts instead of raw tool calls. The
		// model uses `write` to save output files (essays, configs,
		// code); capturing the content as an artifact event lets the
		// frontend render it inline as a rich document card instead
		// of a truncated tool input (docs/10 §11).
		if toolName == "write" {
			if inputMap, ok := inRaw.(map[string]any); ok {
				if content, ok := inputMap["content"].(string); ok && content != "" {
					path, _ := inputMap["path"].(string)
					a.log.Info("opencode write artifact",
						"execution", execID, "path", path, "content_len", len(content))
					// Stream the content as text FIRST so the user sees
					// the story appear incrementally in the runtime
					// session pane (chunked into 40-char pieces with
					// 60ms delay for a typing-style effect). Then emit
					// the artifact card with the full content so the
					// operator can inspect/download/copy the file.
					// Without this, the entire content arrives as one
					// artifact event at the END of the model's
					// processing, and the user sees "Waiting for model
					// output…" → nothing for 30s → artifact burst.
					if output != nil {
						output.WriteString(content)
					}
					a.emitTextChunked(ctx, callbacks, execID, content, textSeq)
					callbacks.OnArtifact(ctx, execID, path, artifactTypeFromPath(path), content)
					break // skip normal tool call — artifact event is sufficient
				}
			}
		}
		if toolName == "write_artifact" {
			if inputMap, ok := inRaw.(map[string]any); ok {
				if content, ok := inputMap["content"].(string); ok && content != "" {
					name, _ := inputMap["name"].(string)
					typ, _ := inputMap["type"].(string)
					if typ == "" {
						typ = "text"
					}
					if name == "" {
						name, _ = inputMap["path"].(string)
					}
					a.log.Info("opencode write_artifact",
						"execution", execID, "name", name, "type", typ, "content_len", len(content))
					if output != nil {
						output.WriteString(content)
					}
					a.emitTextChunked(ctx, callbacks, execID, content, textSeq)
					callbacks.OnArtifact(ctx, execID, name, typ, content)
					break
				}
			}
		}

		// `inp` is the JSON-marshalled input object so the frontend
		// can render it as a structured "Input:" block. If
		// marshalling fails (rare — input is always a JSON object)
		// fall back to a string form so the operator still sees what
		// was attempted.
		inp, err := json.Marshal(inRaw)
		if err != nil {
			inp = []byte(fmt.Sprintf("%v", inRaw))
		}
		a.log.Info("opencode tool use",
			"execution", execID, "tool", toolName,
			"status", state["status"], "output_len", len(outStr))
		callbacks.OnToolCall(ctx, execID, toolName, inp, []byte(outStr))
	case evtReasoning:
		// v1.x reasoning content (only when --thinking is enabled
		// on the opencode CLI). Show as a separate reasoning block
		// so the operator can see what the model was "thinking"
		// before each assistant turn. Without this the live pane
		// just shows the final answer.
		reasonText, _ := part["text"].(string)
		if reasonText == "" {
			return
		}
		a.log.Debug("opencode reasoning", "execution", execID, "len", len(reasonText))
		// Reasoning is also streamed chunk-by-chunk so it appears to
		// unfold live. We tag it with a `kind: reasoning` prefix in
		// the JSON payload so the frontend can render it in a
		// distinct style without a new event-type enum.
		a.emitReasoningChunked(ctx, callbacks, execID, reasonText, textSeq)
	case evtStepFinish:
		// Step completion carries token usage + cost (docs/04 §6.1).
		// Record it via the AI Gateway dual-write: Postgres source of
		// truth + OTel metrics → VictoriaMetrics (docs/08 §5.2). Best-effort
		// — telemetry loss never blocks control flow (docs/08 §8).
		tokens, _ := part["tokens"].(map[string]any)
		cost, _ := part["cost"].(float64)
		if stats != nil {
			stats.stepFinishes++
		}
		a.log.Info("opencode step finished", "execution", execID, "cost", cost, "tokens", tokens)
		a.recordUsage(ctx, execRow, manifest, tokens, cost)
	case evtHealth:
		if state, ok := evt["state"].(string); ok {
			callbacks.OnHealth(ctx, execID, state)
		}
	case evtError:
		// opencode's --format json emits an error event shaped like
		// {"type":"error","error":{"name":"...","data":{"message":"..."}}}
		// — the human-readable message lives at error.data.message, NOT
		// at the top level (which has no `message` field). The previous
		// implementation read evt["message"] and got "" every time,
		// silently dropping the actual reason. Read it correctly here
		// AND stash the message in lastStreamErr so the OnResult call
		// below can include it in error_message (docs/04 §6.1: errors
		// surfaced via telemetry must also be surfaced in the failure
		// reason).
		msg := extractErrorMessage(evt)
		a.log.Warn("opencode error", "execution", execID, "message", msg)
		if msg != "" {
			*lastStreamErr = msg
		}
		callbacks.OnHealth(ctx, execID, domain.HealthUnhealthy)
	case evtToolCall, evtToolResult, evtFileDiff:
		// Legacy event names. opencode v1.x does NOT emit these —
		// it uses `tool_use` (above) with embedded state for both
		// input and output. Keep these branches as no-ops so a
		// future rename is a one-line change.
		a.log.Debug("opencode legacy event ignored", "execution", execID, "type", eventType)
	default:
		a.log.Debug("opencode event", "execution", execID, "type", eventType)
	}
}

// emitTextChunked fans a single assistant-text payload out as a stream
// of smaller chunks, each separated by a short delay, so the frontend
// RuntimeSessionPane can render a typing-style live view. Without this,
// opencode's --format json mode delivers the entire response in one
// `text` event at step_finish and the user would see nothing until
// completion.
//
// Each chunk is delivered via callbacks.OnText with a per-execution
// sequence number so the frontend can order chunks even if NATS
// delivers them out of order. The accumulator (`output`) is NOT
// touched here — it is updated separately in parseStdoutLine so the
// ORCHICON WORKER SUMMARY block is still extractable at OnResult time.
func (a *Adapter) emitTextChunked(ctx context.Context, callbacks scheduler.ExecutionCallbacks, execID, text string, seq *int) {
	if text == "" {
		return
	}
	// Honor cancellation: if the context is done (execution
	// cancelled/terminated), drop remaining chunks. The final
	// accumulated output is still in `output` and will be folded
	// into OnResult.
	for i := 0; i < len(text); i += textStreamingChunkSize {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := i + textStreamingChunkSize
		if end > len(text) {
			end = len(text)
		}
		chunk := text[i:end]
		*seq++
		callbacks.OnText(ctx, execID, chunk)
		// Pace the chunks so the frontend actually has time to
		// render them. We skip the delay after the very last
		// chunk — there's nothing to wait for.
		if end < len(text) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(textStreamingChunkDelay):
			}
		}
	}
}

// emitReasoningChunked is the reasoning-content counterpart to
// emitTextChunked. Reasoning text arrives on a `reasoning` event
// (opencode --thinking mode) and is also delivered in one big chunk,
// so we fan it out the same way. Each chunk is tagged via a JSON
// wrapper in the payload so the frontend can distinguish reasoning
// from assistant text without needing a new event-type enum value.
func (a *Adapter) emitReasoningChunked(ctx context.Context, callbacks scheduler.ExecutionCallbacks, execID, text string, seq *int) {
	if text == "" {
		return
	}
	for i := 0; i < len(text); i += textStreamingChunkSize {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := i + textStreamingChunkSize
		if end > len(text) {
			end = len(text)
		}
		chunk := text[i:end]
		*seq++
		wrapped := map[string]any{
			"kind": "reasoning",
			"text": chunk,
			"seq":  *seq,
		}
		payload, _ := json.Marshal(wrapped)
		// Use the existing OnText callback but encode the reasoning
		// marker in the payload so the frontend can render it
		// differently. OnText writes the payload verbatim into the
		// outbox row.
		callbacks.OnText(ctx, execID, string(payload))
		if end < len(text) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(textStreamingChunkDelay):
			}
		}
	}
}

// recordUsage records a usage sample from a step_finish event via the
// AI Gateway dual-write (docs/08 §5.2). It extracts token counts from
// the opencode JSON shape and derives provider/model from the manifest's
// ModelRef (which the human defined — docs/05 §11). Best-effort: a nil
// recorder means usage is not recorded (docs/08 §8).
//
// Token field naming: opencode emits `tokens.input` / `tokens.output`
// (plus reasoning and a cache sub-object) at the top level of the
// tokens object. The previous version read `prompt_tokens` /
// `completion_tokens` — names that don't appear on the wire — so
// `recordUsage` always saw zeros and the early-return dropped every
// sample. Result: usage_records stayed empty even when the model was
// clearly running, and the AI Gateway's Postgres source-of-truth had
// no data to surface. Now reads the actual opencode fields.
func (a *Adapter) recordUsage(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, tokens map[string]any, cost float64) {
	if a.usageRecorder == nil {
		return
	}
	promptTokens := toInt64(tokens["input"])
	completionTokens := toInt64(tokens["output"])
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
		WorkflowRunID:    execRow.WorkflowRunID,
	}
	if err := a.usageRecorder(ctx, in); err != nil {
		a.log.Warn("usage record failed", "execution", execRow.ID, "error", err)
	}
}

// extractErrorMessage pulls the human-readable message out of an opencode
// error event. The shape is
//   {"type":"error","error":{"name":"...","data":{"message":"..."}}}
// The `data.message` field can be a JSON-stringified payload (e.g. when
// the upstream provider returned a structured error), so we try to
// unquote it for readability. Falls back to whatever it can find.
func extractErrorMessage(evt map[string]any) string {
	errObj, ok := evt["error"].(map[string]any)
	if !ok {
		// Older shapes may still surface a top-level message field.
		if m, ok := evt["message"].(string); ok {
			return m
		}
		return ""
	}
	if data, ok := errObj["data"].(map[string]any); ok {
		if m, ok := data["message"].(string); ok && m != "" {
			// If the message looks like a JSON object (common with
			// provider errors like OpenRouter 500s), unquote it so the
			// error_message column carries the readable form.
			if len(m) > 0 && m[0] == '{' {
				if unq, err := strconv.Unquote(m); err == nil {
					return unq
				}
			}
			return m
		}
	}
	if n, ok := errObj["name"].(string); ok {
		return n
	}
	return ""
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
			callbacks.OnResult(ctx, execRow.ID, false, "", ctx.Err().Error())
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
				callbacks.OnResult(ctx, execRow.ID, true, "", "")
				return nil
			}
		}
	}
}

// Compile-time assertion that Adapter satisfies the AdapterBridge
// interface.
var _ scheduler.AdapterBridge = (*Adapter)(nil)

// artifactTypeFromPath returns a type label for an artifact based on its
// file extension. Used by the `write` tool handler to tag artifact events
// so the frontend can display them appropriately (docs/10 §11).
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

// textStreamingChunkSize is the number of bytes per chunk when the
// adapter fans out a single opencode `text` event into the streaming
// pipeline (docs/04 §6.1). opencode's `--format json` mode delivers
// the full assistant text as one event at step_finish — there is no
// per-token streaming on the wire — so we chunk it on the adapter
// side to give the frontend a typing-style experience. ~40 chars ≈
// one short clause; small enough to feel incremental, large enough to
// keep outbox writes under ~150/sec for the typical 1000-word
// response.
const textStreamingChunkSize = 40

// textStreamingChunkDelay is the gap between emitted text chunks.
// Tuned so a 1000-word response (~6000 chars) takes ~10s to "type
// out" — fast enough to feel live, slow enough that each chunk is a
// visible update rather than a flash.
const textStreamingChunkDelay = 60 * time.Millisecond

// opencode v1.x's --format json stream emits these event types.
// Keep them as named constants so the dispatch table in
// parseStdoutLine reads like the schema.
const (
	evtStepStart    = "step_start"
	evtStepFinish   = "step_finish"
	evtText         = "text"
	evtToolUse      = "tool_use"  // v1.x: input + output in one event
	evtReasoning    = "reasoning" // v1.x: only when --thinking is enabled
	evtError        = "error"
	evtHealth       = "health"
	// Legacy names kept for backwards compatibility with older
	// opencode builds / fork compat — we ignore these now but the
	// case branches remain as no-ops so a future event-type rename
	// is a one-liner.
	evtToolCall     = "tool_call"
	evtToolResult   = "tool_result"
	evtFileDiff     = "file_diff"
)

// opencodeEnv builds the execution environment shared by the in-process
// and runtime dispatch paths: the plane's environment plus the opencode
// overrides (execution/task/project ids + the merged agent/MCP config).
func (a *Adapter) opencodeEnv(execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, modelRef string) []string {
	// Runtime-container executions deliberately do NOT register the
	// built-in Orchicon MCP: the runtime container is an isolated, root-free
	// execution sandbox with no network route to the plane's Postgres, and
	// handing it the plane's DB DSN would break the security model. The
	// user's own opencode-config MCP servers (which need no Orchicon DB)
	// are still merged in.
	cfgJSON := BuildConfigContent(ConfigOptions{
		AgentName:   workerAgent,
		AgentPrompt: manifest.SystemPrompt,
		ModelRef:    modelRef,
		OrchiconMCP: false,
	})
	return append(os.Environ(),
		"OPENCODE_EXECUTION_ID="+execRow.ID,
		"OPENCODE_TASK_ID="+manifest.TaskID,
		"OPENCODE_PROJECT_ID="+manifest.ProjectID,
		"OPENCODE_CONFIG_CONTENT="+cfgJSON,
	)
}

// startInRuntime dispatches opencode into the workflow's runtime
// container via the daemon. The in-container supervisor applies the
// safety guard, sets the working directory, and streams stdout/stderr
// back as AgentEvents; this method decodes them through the same
// parseStdoutLine/handleStderrLine pipeline as the in-process path.
func (a *Adapter) startInRuntime(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks, args, env []string, runDir string) error {
	callbacks.OnStarted(ctx, execRow.ID)

	var stderrBuf bytes.Buffer
	textSeq := 0
	var lastStreamErr string
	var output strings.Builder
	stats := &execStreamState{}
	monitor := newProgressMonitor(execRow.ID, stallWindowsFromManifest(manifest))
	go monitor.run(ctx, func(execID, reason string) {
		callbacks.OnStall(ctx, execID, reason)
	})
	defer monitor.close()

	// execCtx is the exec session's context. The parent ctx carries the
	// wall-clock deadline; execCtx lets us distinguish "the exec finished
	// normally" from "the parent ctx was cancelled" for the explicit-kill
	// signal below.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()

	// Explicit termination: if the exec context is cancelled while the run
	// is in flight (wall-clock deadline, abort, plane shutdown), tell the
	// supervisor to kill the child PROMPTLY rather than leaving it to the
	// reconnect grace. A pure transport blip does NOT cancel the context,
	// so this does not fire on a broken stream (the retry loop below
	// re-attaches instead).
	go func() {
		<-execCtx.Done()
		if ctx.Err() == nil {
			return // normal completion — execCtx was cancelled by the defer
		}
		_ = a.rt.Signal(ctx, manifest.RuntimeWorkflowID, runtime.SignalRequest{ExecID: execRow.ID, Signal: "SIGKILL"})
	}()

	req := runtime.ExecRequest{
		ExecID:                execRow.ID,
		Argv:                  append([]string{"opencode"}, args...),
		Env:                   env,
		Cwd:                   runDir,
		ProjectDir:            manifest.ProjectDir,
		ReconnectGraceSeconds: reconnectGraceSeconds(manifest.ReconnectGraceSeconds),
	}
	// Self-healing: ensure the runtime container exists before dispatching.
	// A recovery re-dispatch can otherwise race ahead of the adopt sweep
	// after a rebuild/container loss and fail by exec-ing into a missing
	// container. Create is idempotent (returns the running container if
	// present); the daemon appends the standard home mounts and validates
	// the project mount against the allowed roots.
	if _, cerr := a.rt.Create(ctx, runtime.CreateRequest{
		WorkflowID: manifest.RuntimeWorkflowID,
		Mounts:     projectMount(manifest.ProjectDir),
	}); cerr != nil {
		a.log.Warn("ensure runtime container on dispatch failed", "run", manifest.RuntimeWorkflowID, "execution", execRow.ID, "error", cerr)
	}

	handleEvent := func(ev runtime.AgentEvent) error {
		if ev.Stream == "stderr" {
			a.handleStderrLine(ctx, execRow, manifest, ev.Data)
			stderrBuf.WriteString(ev.Data + "\n")
			return nil
		}
		line := strings.TrimSpace(ev.Data)
		if line == "" {
			return nil
		}
		a.parseStdoutLine(ctx, execRow, manifest, line, callbacks, monitor, &output, &lastStreamErr, &textSeq, stats)
		return nil
	}

	// The exec stream can break on a transient transport blip (socket /
	// docker hiccup). That must NOT fail the execution: retry with backoff
	// for the same exec_id — the supervisor keeps the child running during
	// the reconnect grace and re-attaches the stream. Only give up (and let
	// recovery handle it) once the retries are exhausted or the context was
	// cancelled (explicit termination).
	attempts := int(manifest.ReconnectAttempts)
	if attempts <= 0 {
		attempts = defaultReconnectAttempts
	}
	exitCode := 1
	var execErr error
	for attempt := 0; ; attempt++ {
		exitCode, execErr = a.rt.Exec(execCtx, manifest.RuntimeWorkflowID, req, handleEvent)
		if execErr == nil || execCtx.Err() != nil || attempt+1 >= attempts {
			break
		}
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		a.log.Info("runtime exec stream broke — re-attaching",
			"execution", execRow.ID, "attempt", attempt+1, "max", attempts, "error", execErr, "backoff", backoff.String())
		select {
		case <-time.After(backoff):
		case <-execCtx.Done():
		}
	}
	err := execErr

	succeeded := err == nil && exitCode == 0
	unfinishedStep := stats.stepStarts > stats.stepFinishes
	if succeeded && unfinishedStep {
		succeeded = false
	}
	var parts []string
	if unfinishedStep {
		parts = append(parts, "execution ended before the final model step completed (model response stream truncated or event dropped)")
	}
	if lastStreamErr != "" {
		parts = append(parts, lastStreamErr)
	}
	if stderrBuf.Len() > 0 {
		parts = append(parts, strings.TrimSpace(stderrBuf.String()))
	}
	if err != nil {
		parts = append(parts, err.Error())
	}
	if exitCode != 0 && err == nil {
		parts = append(parts, fmt.Sprintf("exit status %d", exitCode))
	}
	callbacks.OnResult(ctx, execRow.ID, succeeded, output.String(), strings.Join(parts, "; "))
	return err
}

// reconnectGraceSeconds resolves the supervisor reconnect-grace duration,
// falling back to the built-in default when the manifest value is unset.
func reconnectGraceSeconds(v int64) int64 {
	if v <= 0 {
		return defaultReconnectGraceSeconds
	}
	return v
}

// projectMount returns the project-dir mount spec for a runtime container
// (empty when no project dir — the daemon still adds the standard home
// mounts).
func projectMount(projectDir string) []runtime.MountSpec {
	if projectDir == "" {
		return nil
	}
	return []runtime.MountSpec{{Source: projectDir, Dest: projectDir}}
}

// handleStderrLine emits one worker stderr line as an OTel log record so
// execution stderr appears in the telemetry logs tab (docs/08 §5.3).
func (a *Adapter) handleStderrLine(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, line string) {
	line = strings.TrimRight(line, "\n\r")
	if line == "" {
		return
	}
	severity := "INFO"
	upper := strings.ToUpper(line)
	switch {
	case strings.HasPrefix(upper, "[ERROR]"), strings.HasPrefix(upper, "ERROR:"), strings.HasPrefix(upper, "ERROR "):
		severity = "ERROR"
	case strings.HasPrefix(upper, "[WARN]"), strings.HasPrefix(upper, "WARN:"), strings.HasPrefix(upper, "WARN "):
		severity = "WARN"
	case strings.HasPrefix(upper, "[DEBUG]"), strings.HasPrefix(upper, "DEBUG:"), strings.HasPrefix(upper, "DEBUG "):
		severity = "DEBUG"
	}
	telemetry.EmitLog(ctx, severity, line,
		attribute.String("execution_id", execRow.ID),
		attribute.String("project_id", manifest.ProjectID),
		attribute.String("task_id", manifest.TaskID),
	)
}
