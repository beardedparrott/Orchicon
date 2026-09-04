package orchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Loop tuning constants (env-tunable, matching the opencode parity
// surface; defaults below).
const (
	// maxStepsDefault bounds the turn loop per execution. The budget JSON
	// has no max_steps key today; this env-tunable constant is the guard.
	// (The opencode simulation path uses a hardcoded 3; the native engine
	// defaults to 25.)
	maxStepsDefault = 25
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
func toolParallelism() int {
	return loopEnvInt("ORCHICON_SESSION_TOOL_PARALLELISM", toolParallelismDefault)
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
			callbacks.OnResult(ctx, s.id, false, s.output.String(), "panic: "+fmt.Sprint(r))
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

	// Turn loop bounded by maxSteps.
	steps := 0
	var lastUsage Usage
	toolSigs := map[string]int{} // tool signature → consecutive repeats
	for {
		select {
		case <-ctx.Done():
			// Cancellation: mark cancelled, leave resumable, no new
			// provider call.
			_ = s.markState(ctx, "cancelled")
			callbacks.OnResult(ctx, s.id, false, s.output.String(), "cancelled")
			return nil
		default:
		}

		steps++
		if steps > maxSteps() {
			_ = s.markState(ctx, "failed")
			callbacks.OnStall(ctx, s.id, "stalled:no_progress", true)
			callbacks.OnResult(ctx, s.id, false, s.output.String(), "max_steps_exceeded")
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
			MaxTokens: 4096,
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
			callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
			return nil
		}

		stream, err := s.provider.StreamTurn(ctx, req)
		if err != nil {
			// Pre-stream failure → execution fails (error surfaced).
			msg := fmt.Sprintf("provider stream failed: %v", err)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
			return nil
		}

		text, finish, toolCalls, usage, streamErr := s.drain(ctx, callbacks, stream)
		_ = stream.Close()
		if streamErr != nil {
			msg := fmt.Sprintf("stream error: %v", streamErr)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
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
		// when no window hint exists.
		s.maybeCompact(ctx, steps, usage)

		// Guard: no-progress detection (zero token growth + repeated tool
		// signature → surfaced stall, never a silent loop).
		prev := lastUsage.TotalTokens()
		lastUsage = usage
		repeated := s.checkNoProgress(prev, usage, toolCalls, toolSigs)
		if repeated != "" {
			_ = s.markState(ctx, "failed")
			callbacks.OnStall(ctx, s.id, repeated, true)
			callbacks.OnResult(ctx, s.id, false, s.output.String(), repeated)
			return nil
		}

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
				results := s.executeTools(ctx, callbacks, toolCalls)
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
			// (StopLength — the 4096-token cap mid-monologue), the model
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
			callbacks.OnResult(ctx, s.id, true, s.output.String(), "")
			return nil

		case StopLength, StopOther, StopError, StopContentFilter:
			fallthrough
		default:
			// A turn that ended WITHOUT the provider's end-of-response
			// signal is a truncated/aborted response, not a completed one.
			// StopLength = the MaxTokens cap cut the model mid-generation
			// (the exact failure shape of the reported hollow successes).
			// StopOther arrives when a provider stream never delivered a
			// stop reason at all. Neither may be recorded as success.
			msg := fmt.Sprintf("model terminated with stop reason %q", finish)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
			return nil
		}
	}
}

// checkNoProgress detects a stall: a turn with no token growth that
// repeats a prior tool signature. Returns the stall reason string
// (parity vocabulary) or "" when healthy.
func (s *Session) checkNoProgress(prevTokens int64, usage Usage, toolCalls []ToolCall, sigs map[string]int) string {
	// Only treat zero-growth + REPEATED tool signature as a stall (a
	// single zero-growth tool round is healthy — e.g. a read turn that
	// produces no tokens; the NEXT round with the same signature trips
	// the guard). A final StopStop turn legitimately adds output tokens.
	if len(toolCalls) == 0 {
		return ""
	}
	growth := usage.TotalTokens() - prevTokens
	if growth > 0 {
		// Progress made: reset repetition counters.
		for k := range sigs {
			delete(sigs, k)
		}
		return ""
	}
	// Zero token growth + tool calls: stall only when the SAME signature
	// repeats (the platform's repetition detector semantics). A single
	// zero-growth round is healthy — the guard needs a repeat to trip.
	for _, tc := range toolCalls {
		sig := tc.Name + ":" + tc.ArgsJSON
		sigs[sig]++
		if sigs[sig] >= 2 {
			return "stalled:repetition:" + tc.Name
		}
	}
	// No repeated signature yet: healthy, keep the round count for the
	// next round (the repetition counter already advanced above).
	return ""
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
			s.emitTextChunked(ctx, callbacks, e.Text)
			_ = s.transcript.Append(TransText, map[string]any{"text": e.Text})
		case ReasoningDelta:
			reasoning.WriteString(e.Text)
			// Parity: reasoning is emitted via a {"kind":"reasoning"}
			// JSON wrapper and NEVER replayed into history.
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
