package orchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Loop tuning constants (env-tunable, matching the opencode parity
// surface; defaults below).
const (
	// maxStepsDefault bounds the turn loop per execution when neither the
	// work item's budgets.max_steps nor ORCHICON_SESSION_MAX_STEPS sets
	// one. (The opencode simulation path uses a hardcoded 3.)
	//
	// 100, not 25: one loop iteration is one MODEL turn, and a tool-using
	// engineer spends a turn per tool round-trip — 25 kills honest
	// tool-heavy work (observed: two SSE runs died at 25 doing nothing
	// but singular reads). Genuine loops are caught far earlier by the
	// progress monitor (repetition N-in-window, no-progress window), and
	// wall-clock bounds absolute cost, so this cap is only the
	// catastrophic backstop against pathological-but-progressing sessions.
	// Per-work-item budgets.max_steps still overrides it.
	maxStepsDefault = 100
	// toolParallelism bounds the tool execution worker pool.
	toolParallelismDefault = 4
	// textStreamingChunkSize / Delay match opencode's emitTextChunked
	// pacing so the runtime session pane renders identically.
	textStreamingChunkSize  = 40
	textStreamingChunkDelay = 60 * time.Millisecond
	// maxToolOutputBytes caps one tool result before it re-enters history
	// (parity with the opencode adapter's context-amplifier guard).
	maxToolOutputBytes = 128 * 1024 // 128 KiB ≈ ~30k tokens
	// toolOutputTruncatedMarker marks a capped tool result.
	toolOutputTruncatedMarker = "\n…[output truncated by Orchicon — use a targeted read/grep on the host or project disk for the full tail]\n"
	// toolResultGrace is the bounded grace for finishing an in-flight tool
	// call on cancellation (no new provider call after it).
	toolResultGrace = 5 * time.Second
)

// lengthContinuationMaxTurns bounds the StopLength continuation budget:
// two continuation turns (the same budget shape as the completion probe
// in completion.go). A session that hits the output cap three times in a
// row is pathological — it fails honestly instead of looping.
const lengthContinuationMaxTurns = 2

// loopEnv reads an env-tunable integer with a default.
func loopEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func maxSteps() int { return loopEnvInt("ORCHICON_SESSION_MAX_STEPS", maxStepsDefault) }

// maxStepsFromBudgets resolves the per-execution turn budget. Precedence:
// an explicit budgets.max_steps on the work item/worker (layered over
// tenant defaults by the scheduler's mergeBudgets, so a worker value wins
// per key) beats the server env ORCHICON_SESSION_MAX_STEPS, which beats
// the 25-turn default. Non-positive or non-numeric values fall through —
// the guard is never silently disabled by a bad key.
//
// Parity note: the REAL opencode path has no step cap at all (the CLI
// runs its own loop; only the in-process simulation hardcodes 3), so this
// guard is native-only and intentionally stricter. The budgets key makes
// it per-execution configurable; a tenant-level max_steps would need a
// typed budget-ladder column (ApplyBudgetJSON drops unknown keys), so
// tenant defaults cannot set it today.
func maxStepsFromBudgets(budgets []byte) int {
	if len(budgets) > 0 {
		var m map[string]any
		if json.Unmarshal(budgets, &m) == nil {
			if v, ok := m["max_steps"]; ok {
				if f, ok := jsonNumber(v); ok && f > 0 {
					return int(f)
				}
			}
		}
	}
	return maxSteps()
}

// turnBudget returns the session's resolved turn budget. A zero
// maxStepsVal (a Session built outside NewSession, e.g. older call sites)
// falls back to env/default rather than failing on step one.
func (s *Session) turnBudget() int {
	if s.maxStepsVal > 0 {
		return s.maxStepsVal
	}
	return maxSteps()
}

// jsonNumber coerces a decoded JSON number (float64 from encoding/json,
// json.Number, or a numeric string) to float64.
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}
func toolParallelism() int {
	return loopEnvInt("ORCHICON_SESSION_TOOL_PARALLELISM", toolParallelismDefault)
}

// maxOutputTokensEnv / defaultMaxOutputTokens tune the per-turn output cap
// (the `length` stop-reason ceiling). The hardcoded 4096 previously cut
// long-form workers mid-generation (the reported stop-reason "length"
// failures). ORCHICON_SESSION_MAX_OUTPUT_TOKENS overrides; 0 → default.
const defaultMaxOutputTokens = 32768

func maxOutputTokens() int64 {
	if v := os.Getenv("ORCHICON_SESSION_MAX_OUTPUT_TOKENS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxOutputTokens
}

// turnMaxTokens resolves the per-turn output cap: the model's KNOWN max
// output (live ModelInfo.MaxOutput) when resolved, bounded above by the
// env-tunable ceiling — never exceeding what the model reports.
func (s *Session) turnMaxTokens(ctx context.Context) int64 {
	cap := maxOutputTokens()
	if m, _ := s.resolveModelInfo(ctx); m != nil && m.MaxOutput > 0 && m.MaxOutput < cap {
		return m.MaxOutput
	}
	return cap
}

// Run executes the agent turn loop for the session and streams lifecycle
// callbacks (OnStarted / OnText / OnToolCall / OnWrittenFiles / OnResult)
// with full parity to the opencode adapter. It is the session boundary:
// a `defer recover()` here contains any panic inside the loop (tool
// execution, provider callback, transcript append) so the control-plane
// process and sibling executions survive — the execution fails with the
// panic captured and the transcript marked failed.
//
// Run is synchronous and blocks until the loop finishes (terminal
// OnResult) or the context is cancelled. It is safe to call Run again on
// the same session (resume): the transcript is reopened in append mode,
// history is rebuilt by replay, and the loop continues.
func (s *Session) Run(ctx context.Context, callbacks scheduler.ExecutionCallbacks) (err error) {
	if callbacks == nil {
		return fmt.Errorf("session: nil callbacks")
	}
	// Per-run lifecycle reset (resume parity): Run is safe to call again
	// on the same session, so each invocation gets a fresh terminal gate,
	// a live done channel, and a clear probe latch — a resumed session
	// must be able to deliver its OWN verdict and nudge watchdog.
	s.terminalMu.Lock()
	s.terminalFired = false
	s.terminalMu.Unlock()
	s.noteMu.Lock()
	s.nudgePending = false
	s.nudgeFinished = false
	s.noteMu.Unlock()
	s.doneCh = make(chan struct{})
	// Open (or reopen) the crash-safe transcript FIRST so the panic
	// boundary below can mark the transcript failed while it is still
	// open. Defer order is LIFO: Close is registered BEFORE the recover
	// defer, so on panic the recover runs first (marks failed + appends
	// the panic error while the file is open), then Close runs.
	if s.transcript == nil {
		t, err := s.OpenTranscript()
		if err != nil {
			return fmt.Errorf("session: open transcript: %w", err)
		}
		s.transcript = t
		defer func() {
			if err != nil && s.transcript != nil {
				_ = s.transcript.Append(TransState, map[string]any{"state": "failed"})
			}
			_ = s.transcript.Close()
		}()
	}
	// Panic containment at the session boundary (AC: panic in the loop
	// fails ONLY this execution — never the plane or siblings). Runs
	// BEFORE the Close defer above (registered last → runs first). Must
	// be registered before ANY transcript work (seeding, replay, header)
	// so a panic there is contained too.
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("session panic recovered: %v\n%s", r, debug.Stack())
			s.log.Error("session panic contained", "execution", s.id, "panic", r)
			_ = s.markState(ctx, "failed")
			if s.transcript != nil {
				_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			}
			s.fireTerminalOnce(callbacks, s.id, false, "panic: "+fmt.Sprint(r))
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	// Sequence continuation (opt-in, default off): seed the prior
	// session's transcript into this one so the new file is
	// self-contained and replay produces the full prior conversation.
	// Identity (same worker) is verified by the bridge before this is
	// set; a seed failure falls back to a fresh session (never leaks
	// another worker's transcript).
	if s.continuationPath != "" && s.transcript.Seq() == 0 {
		if err := s.transcript.Append(TransSession, map[string]any{"identity": s.identity}); err != nil {
			return fmt.Errorf("session: header: %w", err)
		}
		if err := s.transcript.SeedFrom(s.continuationPath); err != nil {
			s.log.Warn("session: continuation seed failed — starting fresh", "execution", s.id, "error", err)
			// The header is already appended; drop the seeded path so the
			// fresh session proceeds normally.
		} else {
			s.continued = true
		}
		s.continuationPath = "" // seed once — a resume must not re-seed
	}

	// Replay the transcript into history on resume (idempotent: replays
	// only the durable lines; the loop appends fresh events after them).
	if len(s.history) == 0 {
		if evs, lerr := Load(s.TranscriptPath()); lerr == nil && len(evs) > 0 {
			s.replay(evs)
		}
	}

	// Header (first run) or resume marker.
	if s.transcript.Seq() == 0 {
		if err := s.transcript.Append(TransSession, map[string]any{"identity": s.identity}); err != nil {
			return fmt.Errorf("session: header: %w", err)
		}
	}
	callbacks.OnStarted(ctx, s.id)

	// First user message = manifest Goal (parity: opencode sends the goal
	// as the first user message; the engine does NOT re-render — the goal
	// text IS the manifest field). Only on a FRESH session: a resumed or
	// sequence-continued session already carries its goal in the replayed
	// transcript — appending again would duplicate the goal. A CONTINUED
	// session gets its own new goal as the FIRST message after the seeded
	// history (the chain's next step).
	if s.transcript.Seq() == 1 || s.continued {
		s.appendUser(TransUserMessage, s.identity.Goal, "goal")
		if err := s.transcript.Append(TransUserMessage, map[string]any{"text": s.identity.Goal, "source": "goal"}); err != nil {
			return err
		}
	}

	// Progress monitor (opencode parity): started for the session's
	// lifetime; the monitor's stall/recovered callbacks route advisory
	// signals into nudge interjections and fatal signals into the
	// terminal failure path. Run in a goroutine — run() blocks on its
	// ticker loop (a synchronous call would deadlock the turn loop
	// before the first turn ever streamed).
	go s.pm.run(
		func(execID, reason string) {
			s.handleStallSignal(callbacks, execID, reason)
		},
		func(execID, recovered string) {
			callbacks.OnRecovered(ctx, execID, recovered)
		},
	)
	defer s.pm.close()
	// Session-end latches for the nudge reply watchdog: registered here so
	// EVERY exit path (terminal verdict, transcript-error return, panic,
	// cancellation) stops the watchdog — not just the guarded terminals.
	defer s.markNudgeFinished()
	defer s.closeDoneCh()

	// Turn loop bounded by maxSteps.
	steps := 0
	var lastUsage Usage
	_ = lastUsage // token-growth telemetry (monitor feeds via observeStepFinish)
	for {
		select {
		case <-ctx.Done():
			// Cancellation (or the wall-clock deadline): mark cancelled,
			// leave resumable, no new provider call. The terminal verdict
			// fires here — fireTerminalOnce dedupes against the monitor's
			// terminal paths (opencode finish() first-arrival parity).
			_ = s.markState(ctx, "cancelled")
			s.markNudgeFinished()
			s.fireTerminalOnce(callbacks, s.id, false, "cancelled")
			s.closeDoneCh()
			return nil
		case <-s.stallCh:
			// Fatal monitor stall (no_progress / liveness timeout): the
			// monitor handler already fired the terminal OnResult and the
			// reconciler's OnStall marked the execution unhealthy — the
			// loop unwinds WITHOUT a second verdict (opencode parity).
			_ = s.markState(ctx, "failed")
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil
		default:
		}

		steps++
		if steps > s.turnBudget() {
			// Turn-budget exhaustion is its OWN failure shape — the time-
			// based no_progress monitor owns "no progress within the
			// window". Mislabeled reasons put a step cap in the fatal
			// prefix class and mislead the recovery evidence.
			_ = s.markState(ctx, "failed")
			reason := fmt.Sprintf("stalled:max_steps:turn budget of %d exhausted", s.turnBudget())
			callbacks.OnStall(ctx, s.id, reason, true)
			if s.fireTerminalOnce(callbacks, s.id, false, "max_steps_exceeded") {
				s.markNudgeFinished()
				s.closeDoneCh()
				return nil
			}
			// A concurrent monitor escalation already delivered the
			// verdict — unwind through the stall channel if it is pending.
			select {
			case r := <-s.stallCh:
				_ = r
			default:
			}
			return nil
		}

		// Build the turn request. Two-zone system layout (ADR-0009 D2):
		// the cached static prefix (composite + thin native layer + env
		// facts) carries the cache breakpoint; the mutable zone (memory
		// notes + todo digest) follows AFTER it and is never flagged.
		req := TurnRequest{
			Model:     s.identity.Model,
			System:    s.AssembleSystem(),
			Messages:  s.history,
			MaxTokens: s.turnMaxTokens(ctx),
		}
		// Context-window realization (D5, ollama parity): when the live
		// hint resolved (or the work item declared a window), ride it as
		// options.num_ctx on the native /api/chat transport so the server
		// serves the full window instead of silently truncating to ~4096.
		// OpenAI-compat transports ignore OllamaNumCtx (the header is
		// Ollama-only).
		if hint := s.resolveContextWindow(ctx); hint.Ok && hint.Tokens > 0 {
			req.OllamaNumCtx = hint.Tokens
		}
		if s.tools != nil {
			req.Tools = s.tools.Defs()
		}
		// The native memory-note tool is registered by the loop itself
		// (session-scoped, never the MCP registry) — deduped in case a
		// registry already surfaces the same name. The four durable
		// memory tools (D2) are registered when a store is configured.
		if !hasToolNamed(req.Tools, memoryNoteToolDef().Name) {
			req.Tools = append(req.Tools, memoryNoteToolDef())
		}
		if s.memStore != nil && s.mp.Enabled {
			for _, d := range memoryToolDefs() {
				if !hasToolNamed(req.Tools, d.Name) {
					req.Tools = append(req.Tools, d)
				}
			}
		}

		// Defense-in-depth (no-tools wire): a provider that reports no
		// tool capability can never drive a session — the loop IS tool
		// calls. Silently omitting the tools array makes a tool-trained
		// model improvise its native token-format tool calls as plain
		// text ("<｜DSML｜tool_calls>…"), which the loop reads as a
		// text-only final answer: executions "succeed" in seconds with
		// markup garbage. Fail fast with an actionable message instead.
		if len(req.Tools) > 0 && !s.provider.Capabilities().Tools {
			msg := fmt.Sprintf(
				"provider %q (model %q) reports no tool-call capability, but Orchicon sessions are tool-driven — refusing to send a tool-less request (the model would improvise tool calls as plain text). Enable tool support for this provider in Settings → Adapters → Providers, or pick a tool-capable model.",
				s.identity.ProviderID, s.identity.Model)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			s.fireTerminalOnce(callbacks, s.id, false, msg)
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil
		}

		stream, err := s.provider.StreamTurn(ctx, req)
		if err != nil {
			// Pre-stream failure → execution fails (error surfaced).
			msg := fmt.Sprintf("provider stream failed: %v", err)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			s.fireTerminalOnce(callbacks, s.id, false, msg)
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil
		}

		text, finish, toolCalls, usage, streamErr := s.drain(ctx, callbacks, stream)
		_ = stream.Close()
		s.pm.observeStepFinish(usage)
		s.nudgeObserved() // a completed turn is reply evidence (parity: resolveProbe)
		if streamErr != nil {
			msg := fmt.Sprintf("stream error: %v", streamErr)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			s.fireTerminalOnce(callbacks, s.id, false, msg)
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil
		}
		// Per-turn cache metrics (ADR-0009 D6): classify the turn (hit /
		// miss-write / none) and accumulate cached tokens.
		s.recordTurnUsage(usage)
		// Price this turn's LIVE usage for the budget cost gate through the
		// session model's resolved catalog/probe pricing (shared pipeline —
		// ModelInfo.CostFor is the same pricing the gateway's usage recorder
		// applies). 0 when the model has no pricing — the cost dimension
		// then never fires; never a synthesized estimate.
		usage.CostUSD = s.priceUsage(ctx, usage)
		// Per-turn usage emission (D2, opencode step_finish parity): drain
		// this turn's LIVE provider-reported usage to the per-record sink
		// (the bridge wires it only when a usage recorder is configured).
		// Independent of recordTurnUsage — emitting a record never feeds the
		// per-session CacheStats rollup.
		if s.usageSink != nil {
			s.usageSink(ctx, usage)
		}

		// Guarded compaction at the quiet turn boundary (D1): fires only on
		// true context-window pressure (live hint) or the budget gate, from
		// LIVE provider-reported usage. Never fires on token count alone
		// when no window hint exists. A budget_abort result is TERMINAL
		// (opencode parity): the spend crossed the abort tier — the
		// session fails with the budget_abort reason (recovery owns the
		// re-dispatch decision).
		if res := s.maybeCompact(ctx, steps, usage); strings.HasPrefix(res, "budget_abort:") {
			_ = s.transcript.Append(TransError, map[string]any{"error": res})
			_ = s.markState(ctx, "failed")
			s.fireTerminalOnce(callbacks, s.id, false, res)
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil
		}

		// No-progress guard: the time-based progressMonitor (progress.go)
		// owns repetition/no-progress detection with opencode parity —
		// windowed history, reset-on-progress, nudge-first escalation.
		// There is NO same-turn instant-kill in the opencode adapter, so
		// the native engine has none either (the old ≥2-repeat kill was
		// the reported false-positive killer of healthy local-model
		// sessions).
		lastUsage = usage

		switch finish {
		case StopToolUse:
			// History parity (BUG-1): the assistant's tool_use message must
			// PRECEDE the tool results in history. Record the turn's
			// assistant message (accumulated text + tool_use blocks) into
			// the transcript BEFORE executing tools (so a crash mid-tool
			// still replays the assistant turn), then append the same
			// message to history so the provider sees the full turn.
			if len(toolCalls) > 0 {
				if err := s.transcript.Append(TransToolCall, map[string]any{
					"text": text, "tool_calls": toolCalls,
				}); err != nil {
					return err
				}
				s.appendAssistantToolUse(text, toolCalls)
				// Execute pending tool calls (parallel where independent),
				// append results to history, drain injection queue, loop.
				for _, tc := range toolCalls {
					s.pm.observeToolStart(tc.Name)
				}
				results := s.executeTools(ctx, callbacks, toolCalls)
				// Monitor feed (opencode parity): every executed call is
				// observed with its result status; a file-writing call is
				// file progress (resets the advisory windows + repetition
				// history).
				for _, r := range results {
					s.pm.observeToolCall(r.ToolCall.Name, r.ToolCall.ArgsJSON, r.Err != "")
					if r.Err == "" && isFileWritingTool(r.ToolCall.Name) {
						s.pm.observeFileDiff()
					}
				}
				for _, r := range results {
					if err := s.transcript.Append(TransToolResult, r); err != nil {
						return err
					}
				}
				s.appendToolResults(results)
			}
			// Injection drain between tool rounds: a queued user turn
			// becomes the next user message; the reply streams back into
			// the same session.
			if msg := s.drainInjected(ctx); msg != "" {
				s.appendUser(TransUserMessage, msg, "human")
				if err := s.transcript.Append(TransUserMessage, map[string]any{"text": msg, "source": "human"}); err != nil {
					return err
				}
			}
			continue

		case StopStop:
			// Success gate (opencode parity — decision-signal guard): a
			// session that ends WITHOUT a real ORCHICON WORKER SUMMARY is
			// not a completed worker. The marker is the worker's contract
			// sign-off; its absence means the final response was truncated
			// (StopLength — the output cap mid-monologue), the model
			// went idle early, or it echoed the marker as a plan
			// placeholder. First run the completion probe: a fresh turn
			// asking for the sign-off. The probe turn either delivers the
			// marker (loop continues; the NEXT StopStop turn settles with
			// the marker present) or fails the execution honestly when the
			// probe budget is spent. A genuinely finished session settles.
			if !s.decisionMarkerPresent() {
				if !s.runCompletionProbe(ctx, callbacks) {
					return nil // probe failed the execution — OnResult already fired
				}
				continue
			}
			_ = s.markState(ctx, "done")
			_ = s.transcript.Append(TransFinish, map[string]any{"stop_reason": string(finish)})
			s.fireTerminalOnce(callbacks, s.id, true, "")
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil

		case StopLength:
			// Output-cap continuation (opencode parity — a truncated turn
			// is a RECOVERABLE condition, not a terminal failure): the
			// model hit the per-turn output cap mid-generation. Interject
			// a continuation turn (bounded, like the completion probe) so
			// a long-form worker keeps its accumulated context instead of
			// dying and forcing a cold recovery re-dispatch. After the
			// continuation budget is spent the execution fails honestly.
			if !s.runLengthContinuation(ctx, callbacks) {
				return nil // continuation budget spent — OnResult already fired
			}
			continue

		case StopOther, StopError, StopContentFilter:
			fallthrough
		default:
			// A turn that ended WITHOUT the provider's end-of-response
			// signal is a truncated/aborted response, not a completed one.
			// StopOther arrives when a provider stream never delivered a
			// stop reason at all. Neither may be recorded as success.
			msg := fmt.Sprintf("model terminated with stop reason %q", finish)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			s.fireTerminalOnce(callbacks, s.id, false, msg)
			s.markNudgeFinished()
			s.closeDoneCh()
			return nil
		}
	}
}

// checkNoProgress was REMOVED (opencode parity): the time-based
// progressMonitor (progress.go) owns repetition/no-progress detection with
// windowed history, reset-on-progress, and nudge-first escalation. The
// native engine has no same-turn instant-kill — the old ≥2-repeat guard
// was the reported false-positive killer of healthy local-model sessions
// (0-usage telemetry made every identical read a fatal stall).

// handleStallSignal is the monitor's onStall callback (opencode parity,
// nudge-first routing): a FATAL stall (no_progress) surfaces the stall,
// fires the terminal OnResult(false, reason) (the reconciler's OnStall
// has already marked the execution unhealthy → recovery), and signals the
// turn loop through stallCh so it unwinds without a second verdict —
// the exact onStall shape of the opencode adapter (OnStall(fatal) then
// finish(false, reason)). An ADVISORY stall injects an escalating nudge
// into the live session — the worker is responsive and holds full
// context, so killing it destroys that context for no reason. When the
// nudge budget is spent the session escalates: the execution is failed
// with the stall reason (recovery takes over with the evidence).
//
// The monitor goroutine owns this handler; the nudge-reply watchdog below
// is the only other writer. Both route through the same nudgesSent /
// lastNudgeAt / stallCh state under noteMu.
func (s *Session) handleStallSignal(callbacks scheduler.ExecutionCallbacks, execID, reason string) {
	if isFatalStall(reason) {
		callbacks.OnStall(context.Background(), execID, reason, true)
		// Terminal verdict (opencode finish(false, reason) parity): the
		// reconciler's OnStall already flipped the execution unhealthy;
		// the terminal OnResult carries the reason so recovery gets the
		// evidence. fireTerminalOnce guards against a double verdict
		// (e.g. the loop's select raced a concurrent escalation).
		if s.fireTerminalOnce(callbacks, execID, false, reason) {
			select {
			case s.stallCh <- reason:
			default:
			}
		}
		return
	}
	// Advisory: surface the notice, then nudge-first.
	callbacks.OnStall(context.Background(), execID, reason, false)
	now := time.Now()
	s.noteMu.Lock()
	budgetSpent := s.nudgesSent >= s.nudgeMaxVal
	inCooldown := now.Sub(s.lastNudgeAt) < s.nudgeCooldownVal
	s.noteMu.Unlock()
	if budgetSpent || inCooldown {
		if budgetSpent {
			// Nudge budget spent and the pattern persists — escalate to a
			// fatal stall (opencode parity: "the worker has had its
			// nudges and has not broken the pattern").
			esc := reason + ":nudge_budget_spent"
			s.log.Warn("native session: advisory stall escalated after nudge budget spent",
				"execution", execID, "reason", reason, "nudges", s.nudgesSent, "max", s.nudgeMaxVal)
			callbacks.OnStall(context.Background(), execID, esc, true)
			if s.fireTerminalOnce(callbacks, execID, false, esc) {
				select {
				case s.stallCh <- esc:
				default:
				}
			}
		}
		return
	}
	s.noteMu.Lock()
	s.nudgesSent++
	s.lastNudgeAt = now
	idx := s.nudgesSent - 1
	s.noteMu.Unlock()
	if idx >= len(stallNudgeMessages) {
		idx = len(stallNudgeMessages) - 1
	}
	msg := stallNudgeMessages[idx]
	s.log.Info("native session: advisory stall — nudging live session",
		"execution", execID, "reason", reason, "nudge", s.nudgesSent, "max", s.nudgeMaxVal)
	s.queueInjected(msg)
	s.noteMu.Lock()
	s.nudgePending = true
	s.noteMu.Unlock()
	_ = s.transcript.Append(TransUserMessage, map[string]any{"text": msg, "source": "nudge"})
	// Nudge reply-window enforcement (opencode parity — the probe
	// deadline): a nudged session must ANSWER within the window. The
	// loop drains queued injections between tool rounds, so the reply
	// lands as continued turn activity; observeText/observeStepFinish
	// clear pending. No reply within the window → the worker is not
	// responding to its nudges → fatal stall (the true-hang case).
	if s.nudgeReplyWindowVal <= 0 {
		return
	}
	window := s.nudgeReplyWindowVal
	go func() {
		timer := time.NewTimer(window)
		defer timer.Stop()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-timer.C:
				s.noteMu.Lock()
				// Session end always wins over the watchdog: a finished
				// session never escalates through the reply window.
				pending := s.nudgePending && !s.nudgeFinished
				s.noteMu.Unlock()
				if pending {
					s.log.Warn("native session: nudge reply window elapsed with no response — escalating fatal",
						"execution", execID, "window", window)
					esc := "stalled:no_file_progress:liveness_probe_no_response"
					callbacks.OnStall(context.Background(), execID, esc, true)
					if s.fireTerminalOnce(callbacks, execID, false, esc) {
						select {
						case s.stallCh <- esc:
						default:
						}
					}
				}
				return
			case <-tick.C:
				s.noteMu.Lock()
				pending := s.nudgePending
				finished := s.nudgeFinished
				s.noteMu.Unlock()
				if finished || !pending {
					return // the nudged turn replied — probe cleared
				}
			case <-s.doneCh:
				return
			}
		}
	}()
}

// fireTerminalOnce delivers the terminal OnResult exactly once per
// session (the monitor goroutine and the turn loop both own terminal
// paths — opencode parity: finish() is first-arrival-wins). succeeded
// carries the verdict (true only on the success path); reason is the
// error message ("" on success). Returns true when THIS call delivered
// the verdict (the caller may then signal stallCh); false when a verdict
// already fired.
func (s *Session) fireTerminalOnce(callbacks scheduler.ExecutionCallbacks, execID string, succeeded bool, reason string) bool {
	s.terminalMu.Lock()
	if s.terminalFired {
		s.terminalMu.Unlock()
		return false
	}
	s.terminalFired = true
	s.terminalMu.Unlock()
	callbacks.OnResult(context.Background(), execID, succeeded, s.output.String(), reason)
	return true
}

// nudgeObserved marks nudge-reply progress: any text/step activity after
// a nudge clears the pending probe (the reply IS the liveness evidence).
func (s *Session) nudgeObserved() {
	s.noteMu.Lock()
	s.nudgePending = false
	s.noteMu.Unlock()
}

// markNudgeFinished latches session end so the reply watchdog stops.
func (s *Session) markNudgeFinished() {
	s.noteMu.Lock()
	s.nudgeFinished = true
	s.noteMu.Unlock()
}

// closeDoneCh closes the done channel (idempotent) so the nudge reply
// watchdog exits instead of leaking past session end.
func (s *Session) closeDoneCh() {
	if s.doneCh == nil {
		return
	}
	select {
	case <-s.doneCh:
	default:
		close(s.doneCh)
	}
}

// runLengthContinuation interjects ONE continuation turn when the model
// hit the output cap mid-generation (StopLength), up to a bounded budget.
// Returns true when the loop should continue (the probe was delivered);
// false when the budget is spent — the execution has been failed and
// OnResult fired.
func (s *Session) runLengthContinuation(ctx context.Context, callbacks scheduler.ExecutionCallbacks) bool {
	if s.lengthContinuationsSent >= lengthContinuationMaxTurns {
		msg := fmt.Sprintf("model terminated with stop reason \"length\" — output-cap continuation budget of %d spent", lengthContinuationMaxTurns)
		_ = s.transcript.Append(TransError, map[string]any{"error": msg})
		_ = s.markState(ctx, "failed")
		s.fireTerminalOnce(callbacks, s.id, false, msg)
		s.markNudgeFinished()
		s.closeDoneCh()
		return false
	}
	s.lengthContinuationsSent++
	msg := "Your previous response was cut off by the output limit. Continue EXACTLY where you stopped — do not restart, do not repeat what you already wrote. If you were mid-thought, continue it; if the turn is effectively done, deliver the final ORCHICON WORKER SUMMARY now."
	s.appendUser(TransUserMessage, msg, "length_continuation")
	if err := s.transcript.Append(TransUserMessage, map[string]any{"text": msg, "source": "length_continuation"}); err != nil {
		msg := fmt.Sprintf("length continuation transcript append failed: %v", err)
		_ = s.markState(ctx, "failed")
		s.fireTerminalOnce(callbacks, s.id, false, msg)
		s.markNudgeFinished()
		s.closeDoneCh()
		return false
	}
	s.log.Info("native session: StopLength — sending continuation turn",
		"execution", s.id, "continuation", s.lengthContinuationsSent, "max", lengthContinuationMaxTurns)
	return true
}

// drain reads the turn stream until Finish, mapping every event type onto
// callbacks (no silent gaps). Returns the turn's accumulated assistant
// text, the stop reason, the complete tool calls of the turn, the usage,
// and any mid-stream error.
func (s *Session) drain(ctx context.Context, callbacks scheduler.ExecutionCallbacks, stream TurnStream) (string, StopReason, []ToolCall, Usage, error) {
	var text strings.Builder
	var reasoning strings.Builder
	var finish StopReason
	var usage Usage
	var calls []ToolCall
	inflight := map[int]*ToolCall{} // index → in-flight call

	for {
		ev, ok, err := stream.Next(ctx)
		if err != nil {
			return text.String(), finish, calls, usage, err
		}
		if !ok {
			break
		}
		switch e := ev.(type) {
		case TextDelta:
			text.WriteString(e.Text)
			s.output.WriteString(e.Text)
			s.pm.observeText()
			s.nudgeObserved() // continued output = the nudged turn replied
			s.emitTextChunked(ctx, callbacks, e.Text)
			_ = s.transcript.Append(TransText, map[string]any{"text": e.Text})
		case ReasoningDelta:
			reasoning.WriteString(e.Text)
			// Parity: reasoning is emitted via a {"kind":"reasoning"}
			// JSON wrapper and NEVER replayed into history.
			s.pm.observeText()
			s.nudgeObserved()
			payload := map[string]any{"kind": "reasoning", "text": e.Text}
			callbacks.OnText(ctx, s.id, mustJSON(payload))
			_ = s.transcript.Append(TransReasoning, map[string]any{"text": e.Text})
		case ToolCallStart:
			inflight[e.Index] = &ToolCall{Index: e.Index, ToolCallID: e.ToolCallID, Name: e.Name}
		case ToolCallDelta:
			if tc, ok := inflight[e.Index]; ok {
				tc.ArgsJSON += e.ArgsJSONDelta
			}
		case ToolCallEnd:
			// Complete: promoted to the turn's pending set.
			if tc, ok := inflight[e.Index]; ok {
				calls = append(calls, *tc)
				delete(inflight, e.Index)
			}
		case ToolCall:
			// Already-complete tool call event.
			calls = append(calls, e)
		case StreamError:
			return text.String(), finish, calls, usage, e.Err
		case Finish:
			finish = e.StopReason
			usage = e.Usage
		}
	}
	return text.String(), finish, calls, usage, nil
}

// hasToolNamed reports whether a tool definition list already carries a
// name (used to dedupe the loop-registered native tools against the MCP
// registry surface).
func hasToolNamed(defs []ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// nativeToolName reports whether a call targets a loop-registered
// session-scoped tool (handled here, never routed to the registry).
func nativeToolName(name string) bool {
	return name == memoryNoteToolDef().Name || isMemoryTool(name)
}

// stashMutableToolCall folds a turn's tool calls into the session's
// mutable zone state (ADR-0009 D2): the latest todowrite payload is
// stashed for the todo digest; the memory-note tool appends a note.
// Name-gated and tolerant — a malformed payload is skipped, never an
// error to the model.
func (s *Session) stashMutableToolCall(calls []ToolCall) {
	for _, c := range calls {
		switch c.Name {
		case "todowrite":
			var probe todoDigestArgs
			if err := json.Unmarshal([]byte(c.ArgsJSON), &probe); err != nil || probe.Todos == nil {
				continue
			}
			s.noteMu.Lock()
			s.latestTodos = append([]byte(nil), c.ArgsJSON...)
			s.noteMu.Unlock()
		case "orchicon_memory_note":
			var a struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(c.ArgsJSON), &a); err == nil {
				s.AddMemoryNote(a.Text)
			}
		}
	}
}

// executeTools runs the turn's tool calls (parallel where independent —
// same-turn calls are independent by construction) with a bounded worker
// pool, and emits OnToolCall + OnWrittenFiles + OnArtifact parity
// callbacks. A panicking tool is caught per-call and returned as an
// error result (the loop's boundary recover also catches it).
func (s *Session) executeTools(ctx context.Context, callbacks scheduler.ExecutionCallbacks, calls []ToolCall) []toolResult {
	results := make([]toolResult, len(calls))
	// Mutable-zone state capture (ADR-0009 D2): the latest todowrite
	// payload and memory notes are folded into the session BEFORE the
	// calls execute, so the next turn's digest sees this turn's intent.
	// Session-scoped tools (orchicon_memory_note) are answered here and
	// never routed to the registry.
	s.stashMutableToolCall(calls)
	var pending []int // indices routed to the registry
	for i, c := range calls {
		if nativeToolName(c.Name) {
			var out string
			var execErr error
			if isMemoryTool(c.Name) {
				out, execErr = s.execMemoryTool(ctx, c.Name, c.ArgsJSON)
			}
			if execErr != nil {
				results[i] = toolResult{ToolCall: c, Err: execErr.Error()}
			} else {
				if out == "" {
					out = `{"ok":true}`
				}
				results[i] = toolResult{ToolCall: c, Output: out}
			}
			callbacks.OnToolCall(ctx, s.id, c.Name, []byte(c.ArgsJSON), []byte(out))
			s.toolUses++
			continue
		}
		pending = append(pending, i)
	}
	if s.tools == nil {
		for _, i := range pending {
			results[i] = toolResult{ToolCall: calls[i], Err: "tool registry not configured"}
		}
		return results
	}
	par := toolParallelism()
	if par < 1 {
		par = 1
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for _, i := range pending {
		c := calls[i]
		wg.Add(1)
		go func(i int, c ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Per-call panic containment (tool-suite tests inject panics).
			defer func() {
				if r := recover(); r != nil {
					results[i] = toolResult{ToolCall: c, Err: fmt.Sprintf("tool %q panicked: %v", c.Name, r)}
				}
			}()
			out, err := s.tools.Execute(ctx, c.Name, c.ArgsJSON)
			if err != nil {
				results[i] = toolResult{ToolCall: c, Err: err.Error()}
				return
			}
			capped := capToolOutput(out)
			results[i] = toolResult{ToolCall: c, Output: capped}
			// OnToolCall parity (output capped).
			callbacks.OnToolCall(ctx, s.id, c.Name, []byte(c.ArgsJSON), []byte(capped))
			// Artifact parity for write/write_artifact tools.
			if isWriteTool(c.Name) {
				path, content, ok := parseArtifactInput(c.Name, c.ArgsJSON)
				if ok {
					callbacks.OnArtifact(ctx, s.id, path, artifactTypeFromPath(path), content)
				}
			}
			if isFileWritingTool(c.Name) {
				if path, ok := filePathFromArgs(c.ArgsJSON); ok {
					s.markWritten(path)
				}
			}
		}(i, c)
	}
	wg.Wait()
	if len(s.writtenList()) > 0 {
		callbacks.OnWrittenFiles(ctx, s.id, s.writtenList())
	}
	return results
}

// toolResult is one tool execution outcome. The JSON tags are the
// durable transcript shape (TransToolResult): replay unmarshals these
// exact keys, so the transcript round-trips.
type toolResult struct {
	ToolCall ToolCall `json:"tool_call"`
	Output   string   `json:"output"`
	Err      string   `json:"error,omitempty"` // "" on success
}

// appendAssistantToolUse appends the assistant's turn message to history:
// the accumulated text (if any) plus one ContentToolUse block per tool
// call. This is the assistant tool_use message that MUST precede the tool
// results in history (BUG-1 — provider parity).
func (s *Session) appendAssistantToolUse(text string, calls []ToolCall) {
	msg := Message{Role: RoleAssistant}
	if text != "" {
		msg.Content = append(msg.Content, Content{Text: &text})
	}
	for _, c := range calls {
		use := &ContentToolUse{ToolCallID: c.ToolCallID, Name: c.Name, ArgsJSON: c.ArgsJSON}
		msg.Content = append(msg.Content, Content{ToolUse: use})
	}
	s.history = append(s.history, msg)
}

// appendToolResults appends tool results to history as RoleTool messages,
// merging consecutive tool messages (parity with the Anthropic client's
// tool-role merge).
func (s *Session) appendToolResults(results []toolResult) {
	merged := Message{Role: RoleTool}
	for _, r := range results {
		content := r.Output
		isErr := r.Err != ""
		if isErr {
			content = r.Err
		}
		merged.Content = append(merged.Content, Content{
			ToolResult: &ContentToolResult{ToolCallID: r.ToolCall.ToolCallID, Content: content, IsError: isErr},
		})
	}
	s.history = append(s.history, merged)
}

// appendUser appends a user message to history.
func (s *Session) appendUser(kind, text, source string) {
	s.history = append(s.history, Message{Role: RoleUser, Content: []Content{{Text: &text}}})
}

// emitTextChunked streams text to OnText in 40-char/60ms chunks (parity
// with opencode's emitTextChunked), honoring cancellation.
func (s *Session) emitTextChunked(ctx context.Context, callbacks scheduler.ExecutionCallbacks, text string) {
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
		callbacks.OnText(ctx, s.id, text[i:end])
		if end < len(text) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(textStreamingChunkDelay):
			}
		}
	}
}

// markState writes a lifecycle state marker (fsync'd).
func (s *Session) markState(ctx context.Context, state string) error {
	return s.transcript.Append(TransState, map[string]any{"state": state})
}

// replay rebuilds history from the durable transcript (resume path).
func (s *Session) replay(evs []replayEvent) {
	for _, e := range evs {
		switch e.Type {
		case TransUserMessage:
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if d.Text != "" {
				s.appendUser(TransUserMessage, d.Text, "replay")
			}
		case TransText:
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.Data, &d)
			// Reassembled from chunks; the output buffer holds the full text.
			s.output.WriteString(d.Text)
		case TransToolCall:
			// Rebuild the assistant's tool_use turn (BUG-1 parity): the
			// accumulated text + one ToolUse block per call. This message
			// MUST precede the following TransToolResult lines in history.
			var d struct {
				Text      string     `json:"text"`
				ToolCalls []ToolCall `json:"tool_calls"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if d.Text != "" {
				s.output.WriteString(d.Text)
			}
			s.appendAssistantToolUse(d.Text, d.ToolCalls)
		case TransToolResult:
			var d struct {
				ToolCall ToolCall `json:"tool_call"`
				Output   string   `json:"output"`
				Err      string   `json:"error,omitempty"`
			}
			_ = json.Unmarshal(e.Data, &d)
			merged := Message{Role: RoleTool}
			merged.Content = append(merged.Content, Content{
				ToolResult: &ContentToolResult{ToolCallID: d.ToolCall.ToolCallID, Content: d.Output, IsError: d.Err != ""},
			})
			s.history = append(s.history, merged)
		}
	}
}

// --- injection queue -----------------------------------------------------

// injected holds queued mid-run user turns (SendExecutionMessage).
type injected struct {
	mu   sync.Mutex
	msgs []string
}

func (s *Session) queueInjected(msg string) {
	if s.inj == nil {
		s.inj = &injected{}
	}
	s.inj.mu.Lock()
	s.inj.msgs = append(s.inj.msgs, msg)
	s.inj.mu.Unlock()
}

// drainInjected pops one queued message (between tool rounds). Returns ""
// when the queue is empty.
func (s *Session) drainInjected(ctx context.Context) string {
	if s.inj == nil {
		return ""
	}
	s.inj.mu.Lock()
	defer s.inj.mu.Unlock()
	if len(s.inj.msgs) == 0 {
		return ""
	}
	msg := s.inj.msgs[0]
	s.inj.msgs = s.inj.msgs[1:]
	return msg
}

// --- written-files tracking ----------------------------------------------

// markWritten records a written file path (deduped).
func (s *Session) markWritten(path string) {
	s.writtenMu.Lock()
	defer s.writtenMu.Unlock()
	if s.writtenSet == nil {
		s.writtenSet = map[string]bool{}
	}
	if !s.writtenSet[path] {
		s.writtenSet[path] = true
		s.writtenPaths = append(s.writtenPaths, path)
	}
}

// writtenList returns the deduped written paths (sorted for determinism).
func (s *Session) writtenList() []string {
	s.writtenMu.Lock()
	defer s.writtenMu.Unlock()
	out := append([]string(nil), s.writtenPaths...)
	return out
}

// --- helpers -------------------------------------------------------------

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// capToolOutput truncates a tool result to maxToolOutputBytes keeping the
// head + truncation marker (parity with opencode's capToolOutput).
func capToolOutput(s string) string {
	if maxToolOutputBytes < 1 || len(s) <= maxToolOutputBytes {
		return s
	}
	head := maxToolOutputBytes - len(toolOutputTruncatedMarker)
	if head < 1 {
		head = 1
	}
	return s[:head] + toolOutputTruncatedMarker
}

// isWriteTool reports whether the tool is an artifact-writing tool.
func isWriteTool(name string) bool {
	switch name {
	case "write", "write_artifact":
		return true
	}
	return false
}

// isFileWritingTool reports whether the tool writes files (OnWrittenFiles
// parity).
func isFileWritingTool(name string) bool {
	switch name {
	case "write", "write_artifact", "edit", "batch_write":
		return true
	}
	return false
}

// filePathFromArgs extracts a "path" field from tool args JSON.
func filePathFromArgs(argsJSON string) (string, bool) {
	var d struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &d); err != nil || d.Path == "" {
		return "", false
	}
	return d.Path, true
}

// parseArtifactInput extracts (path, content) from a write tool's args.
func parseArtifactInput(name, argsJSON string) (string, string, bool) {
	var d struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &d); err != nil || d.Path == "" {
		return "", "", false
	}
	return d.Path, d.Content, true
}

// artifactTypeFromPath maps a file path to an artifact type hint (parity).
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

var (
	_ = errors.Is
	_ = slog.LevelInfo
)
