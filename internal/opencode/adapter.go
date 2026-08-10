// Package opencode implements the OpenCode adapter bridge — the single
// session-transport bridge that drives worker executions as persistent
// opencode sessions on a serve instance (docs/04_Runtime_Adapter_SDK.md
// §6).
//
// Transport strategy (docs/04 §6.0): every execution runs as a persistent
// opencode session created + driven over the serve HTTP+SSE API
// (session_run.go). The legacy one-shot `opencode run` subprocess path
// was REMOVED: a run that cannot get a session fails loudly
// (failed_to_start → workflow recovery) instead of silently degrading to
// a second, inferior transport. When OpenCode ships a stable IPC API, the
// adapter swaps its internals to an IPC client; the gRPC contract and
// control plane are unaffected.
//
// The adapter MUST NOT advertise capabilities the CLI surface cannot
// honestly deliver (docs/04 §6.2). v0.1 advertises a reduced
// capability set.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Adapter is the OpenCode adapter bridge. It implements
// scheduler.AdapterBridge. Every execution runs as a persistent opencode
// session driven through a serve's HTTP+SSE API (session_run.go); the
// legacy one-shot `opencode run` subprocess transport was removed.
type Adapter struct {
	log *slog.Logger
	mu  sync.Mutex

	// sessions tracks live session-transport executions (execution_id →
	// runner). It is the routing table for SendExecutionMessage and makes
	// IsExecutionActive report session executions as alive until the plane
	// restarts (an empty map after boot = every session execution orphaned
	// and correctly reported dead).
	sessions map[string]*sessionRun

	// host is the always-on host opencode serve for the in-process
	// (local) execution population. Session-transport only.
	host *HostServe

	// rt is the workflow runtime daemon client. When non-nil AND an
	// execution carries a RuntimeWorkflowID, the adapter reaches that
	// workflow's runtime container serve for the session. Nil keeps
	// everything in-process (headless serve).
	rt *runtime.Client

	// usageRecorder records LLM usage (Postgres dual-write + OTel
	// metrics) on each step_finish event carrying tokens + cost
	// (docs/04 §6.1, docs/08 §5.2). Injected by the server; nil =
	// usage is not recorded (telemetry loss never blocks control flow
	// — docs/08 §8 invariant #5).
	usageRecorder UsageRecorderFunc

	// sessionStore persists the durable per-execution session transcript
	// (execution_session_parts). Injected by the server; nil = the
	// transcript is not recorded (e.g. tests).
	sessionStore SessionStoreFunc
}

// SessionStoreFunc persists transcript entries for one execution. The
// implementation owns the tenant transaction.
type SessionStoreFunc func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error

// SetRuntimeClient injects the workflow runtime daemon client. When set,
// executions with a RuntimeWorkflowID dispatch into that workflow's
// runtime container; without it the adapter runs in-process.
func (a *Adapter) SetRuntimeClient(rt *runtime.Client) { a.rt = rt }

// SetHostServe injects the always-on host opencode serve manager. When
// set AND sessions are enabled, local (in-process) executions run as
// persistent sessions on it. Nil means no host serve is available — such
// executions fail fast (the one-shot subprocess path was removed).
func (a *Adapter) SetHostServe(hs *HostServe) { a.host = hs }

// SendExecutionMessage routes a mid-run human message into a live session
// execution. It does NOT create a new execution, work item, or workflow
// state — the message joins the session's existing turn queue and the
// reply streams back through the normal execution event stream. Returns
// an error when the execution has no live session (already finished or
// unknown execution).
func (a *Adapter) SendExecutionMessage(ctx context.Context, execID, message string) error {
	a.mu.Lock()
	r := a.sessions[execID]
	a.mu.Unlock()
	if r == nil {
		return fmt.Errorf("execution %s has no live session (not running on the session transport)", execID)
	}
	if err := r.client.SendMessage(ctx, r.sessionID, r.system, r.modelRef, message); err != nil {
		return fmt.Errorf("send execution message: %w", err)
	}
	r.bumpPending()
	r.recordHumanMessage(message)
	a.log.Info("mid-run execution message sent", "execution", execID, "session", r.sessionID)
	return nil
}

// sessionsEnabled reports whether the session transport is enabled for an
// execution. The global kill-switch ORCHICON_OPCODE_SESSION_TRANSPORT=0
// disables it everywhere — with the one-shot path removed, a disabled
// transport means executions FAIL (fail-fast) rather than degrading.
func (a *Adapter) sessionsEnabled(manifest scheduler.ExecutionManifest) bool {
	return os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT") != "0"
}

// sessionClientFor resolves the SessionClient for an execution: the
// per-container serve for workflow-run executions (ensuring the container
// exists + is serving), or the host serve for the in-process population.
// Returns nil when no serve is available — the caller fails the execution
// (the legacy one-shot fallback was removed).
func (a *Adapter) sessionClientFor(ctx context.Context, manifest scheduler.ExecutionManifest) *SessionClient {
	if a.rt != nil && manifest.RuntimeWorkflowID != "" {
		resp, err := a.rt.Create(ctx, runtime.CreateRequest{
			WorkflowID:  manifest.RuntimeWorkflowID,
			Image:       manifest.RuntimeImage,
			Mounts:      projectMount(manifest.ProjectDir),
			ServeConfig: a.runtimeServeConfig(manifest),
			ProjectDir:  manifest.ProjectDir,
		})
		if err != nil {
			a.log.Warn("session transport: ensure runtime container failed",
				"run", manifest.RuntimeWorkflowID, "execution", manifest.ExecutionID, "error", err)
			return nil
		}
		if resp.ServePort == 0 || resp.ServePassword == "" {
			return nil
		}
		// The daemon returns the plane-reachable base URL (the docker
		// bridge gateway, reachable from a containerized plane). Fall back
		// to loopback for host-plane deployments.
		baseURL := resp.ServeURL
		if baseURL == "" {
			baseURL = fmt.Sprintf("http://127.0.0.1:%d", resp.ServePort)
		}
		return NewSessionClient(baseURL, resp.ServePassword, manifest.ProjectDir)
	}
	if a.host != nil {
		return a.host.Client()
	}
	return nil
}

// runtimeServeConfig builds the OPENCODE_CONFIG_CONTENT for a runtime
// container's serve: the permission rules only, with the operator's MCP
// servers omitted — a SERVE eagerly connects to every configured MCP
// server at startup, and the operator's entries (an `orchicon` MCP that
// docker execs, a local Playwright MCP) cannot run inside the sandbox,
// which would hang the serve (the one-shot run path tolerates MCP
// failures and keeps them). Worker system prompts ride the per-message
// `system` field instead.
func (a *Adapter) runtimeServeConfig(manifest scheduler.ExecutionManifest) string {
	return BuildConfigContent(ConfigOptions{
		AgentName:   workerAgent,
		AgentPrompt: "",
		ModelRef:    "",
		OrchiconMCP: false,
		SkipUserMCP: true,
	})
}

// startViaSession runs an execution through a persistent opencode session
// on the given serve. Returns nil once the execution completes (OnResult
// fired); a non-nil error means the session transport could not be set up
// — the caller surfaces it as a failed execution (no one-shot fallback).
func (a *Adapter) startViaSession(ctx context.Context, procCtx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks, client *SessionClient, modelRef string) error {
	if client == nil {
		return fmt.Errorf("no opencode serve available")
	}
	runner := &sessionRun{
		a:         a,
		parentCtx: ctx,
		procCtx:   procCtx,
		execRow:   execRow,
		manifest:  manifest,
		callbacks: callbacks,
		client:    client,
		modelRef:  modelRef,
		system:    manifest.SystemPrompt,
		done:      make(chan struct{}),
		stats:     &execStreamState{},
	}
	return runner.run()
}

// IsExecutionActive reports whether an in-process execution subprocess is
// still tracked as running. Used by the execution-liveness reaper to
// detect executions orphaned by a control-plane restart (a fresh boot has
// an empty sessions registry, so every previously-running execution is
// correctly reported dead). Session-transport executions are tracked in
// the sessions registry and report active while their runner lives.
func (a *Adapter) IsExecutionActive(execID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[execID]
	return ok
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

// SetSessionStore injects the durable transcript writer. The server wraps
// db.AppendExecutionSessionParts in a tenant transaction. Nil = the
// session transcript is not persisted.
func (a *Adapter) SetSessionStore(fn SessionStoreFunc) { a.sessionStore = fn }

// workerAgent is the opencode agent name the adapter injects the worker's
// composed system prompt under (selected with --agent).
const workerAgent = "orchicon-worker"

// New creates an OpenCode adapter bridge.
func New(log *slog.Logger) *Adapter {
	return &Adapter{
		log:      log,
		sessions: make(map[string]*sessionRun),
	}
}

// Start runs the given execution as a persistent opencode session on a
// serve instance (the host serve for the in-process population, the
// workflow's runtime container serve for workflow runs) and streams
// telemetry back via the callbacks (docs/03 §4, docs/04 §6). The session
// runs until completion or context cancellation.
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
// The legacy one-shot `opencode run` subprocess path was REMOVED: when
// no serve is available (or the session transport is disabled via
// ORCHICON_OPCODE_SESSION_TRANSPORT=0), Start returns an error and the
// execution fails (failed_to_start → workflow recovery) instead of
// degrading to a second, inferior transport.
//
// Two recovery-relevant guardrails (docs/06 §2 triggers):
//   - Stall detection: a progress monitor detects stuck-looping (no
//     progress, no file changes, repeated tool calls) and raises
//     OnStall → triggers recovery. Catches the loop a hard timeout
//     can't (a worker making "progress" but spinning).
//   - Wall-clock timeout: the worker's budget_overrides.wall_clock_seconds
//     (default 3600) is enforced as a per-execution context deadline.
//     When it hits, the session turn is aborted →
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
	//
	// The deadline is applied ONLY to the subprocess context (procCtx),
	// never to the ctx handed to callbacks: when the deadline fires the
	// process is killed, but the terminal OnResult/OnStall writebacks must
	// still complete against the DB. Passing the exhausted ctx into
	// BeginTenantTx fails with "context deadline exceeded" and the
	// execution stays running forever (observed live on a wall-clock E2E).
	procCtx := ctx
	if deadline, ok := wallClockDeadline(ctx, manifest.Budgets); ok {
		var cancel context.CancelFunc
		procCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	// Resolve the model reference. This is the ONLY dispatch gate here —
	// the session transport sends it per message (docs/04 §6.0).
	modelRef := manifest.ModelRef
	if modelRef == "" {
		modelRef = manifest.DefaultModelRef
		if modelRef == "" {
			return fmt.Errorf("no model_ref specified on worker and no default model set in tenant settings — dispatch rejected")
		}
		a.log.Info("no model_ref on worker, using tenant default", "model", modelRef, "execution", execRow.ID)
	}

	// Best-effort: drop the safety lint script into .orchicon/ so review
	// and QA workers can run it (their bash tool is scoped to the project
	// directory). See internal/opencode/lint.go.
	writeSafetyLint(manifest.ProjectDir)

	// Session transport is the ONLY execution transport. Drive the
	// execution through a persistent opencode session on a serve instance
	// (the always-on host serve for the in-process population, or the
	// workflow's runtime container serve). It enables the liveness nudge,
	// mid-run human messages, and SSE-streamed progress.
	//
	// The legacy one-shot `opencode run` subprocess path is REMOVED: when
	// no serve is available the execution FAILS (failed_to_start →
	// workflow recovery) instead of silently degrading to a second,
	// inferior transport that Orchicon deliberately moved away from.
	if !a.sessionsEnabled(manifest) {
		return fmt.Errorf("opencode session transport disabled (ORCHICON_OPCODE_SESSION_TRANSPORT=0) — no execution transport available for execution %s", execRow.ID)
	}
	client := a.sessionClientFor(ctx, manifest)
	if client == nil {
		return fmt.Errorf("no opencode serve available for execution %s (host serve down or runtime container serve unavailable) — execution failed to start", execRow.ID)
	}

	// The serve converges within a minute of its container starting (cold
	// start: providers/MCP + the docker-proxy settling). Retry the session
	// setup with backoff before failing the execution — but never fall
	// back to a one-shot subprocess.
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		if err := a.startViaSession(ctx, procCtx, execRow, manifest, callbacks, client, modelRef); err == nil {
			return nil
		} else {
			lastErr = err
			a.log.Info("session transport setup attempt failed — retrying",
				"execution", execRow.ID, "attempt", attempt+1, "max", 4, "error", err)
		}
	}
	return fmt.Errorf("session transport setup failed after 4 attempts: %w", lastErr)
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

	// truncatedFinish marks a FINAL step_finish that indicates the model
	// turn was interrupted rather than completed: reason "unknown" with
	// zero tokens (the signature of a truncated/aborted response — the
	// failing run's last part was exactly `step_finish {reason:"unknown",
	// tokens all 0}`). A run ending on such a step is not a clean success:
	// the worker's final text / ORCHICON WORKER SUMMARY is missing even
	// though the step counters are balanced (38/38 in the original bug).
	truncatedFinish bool
}

// unfinished reports whether the stream ended mid-step OR on a truncated
// final step — either way the final model response never completed.
func (s *execStreamState) unfinished() bool {
	if s == nil {
		return false
	}
	return s.stepStarts > s.stepFinishes || s.truncatedFinish
}

// allTokensZero reports whether a step_finish's tokens map is empty or all
// counts are zero. A real completion carries input/output/cache counts; an
// interrupted turn (reason "unknown") is emitted with no usage at all.
func allTokensZero(tokens map[string]any) bool {
	if len(tokens) == 0 {
		return true
	}
	for _, v := range tokens {
		if n, ok := v.(float64); ok && n > 0 {
			return false
		}
	}
	return true
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
	a.parseEvent(ctx, execRow, manifest, evt, callbacks, monitor, output, lastStreamErr, textSeq, stats)
}

// parseEvent dispatches a decoded opencode event into the telemetry
// pipeline. It is the single dispatch used by the session transport
// (legacyEventFromBus builds the {type, part} object from the server SSE
// bus; parseStdoutLine unmarshals a JSON line into the same shape). One
// dispatch guarantees consistent downstream behavior: progress monitor,
// usage recording, artifact capture, summary accumulation, and the
// streaming callbacks.
func (a *Adapter) parseEvent(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, evt map[string]any, callbacks scheduler.ExecutionCallbacks, monitor *progressMonitor, output *strings.Builder, lastStreamErr *string, textSeq *int, stats *execStreamState) {
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
			// A step_finish with reason "unknown" and zero tokens is the
			// signature of a truncated/interrupted model turn (the failing
			// run's last part was exactly that). Flag it so the final
			// result can be downgraded to a failure instead of a clean
			// success with no decision signal.
			reason, _ := part["reason"].(string)
			if (reason == "unknown" || reason == "") && allTokensZero(tokens) {
				stats.truncatedFinish = true
			}
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

// projectMount returns the project-dir mount spec for a runtime container
// (empty when no project dir — the daemon still adds the standard home
// mounts).
func projectMount(projectDir string) []runtime.MountSpec {
	if projectDir == "" {
		return nil
	}
	return []runtime.MountSpec{{Source: projectDir, Dest: projectDir}}
}
