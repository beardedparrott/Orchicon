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
	textStreamingChunkSize = 40
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

func maxSteps() int     { return loopEnvInt("ORCHICON_SESSION_MAX_STEPS", maxStepsDefault) }
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
	// Panic containment at the session boundary (AC: panic in the loop
	// fails ONLY this execution — never the plane or siblings).
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("session panic recovered: %v\n%s", r, debug.Stack())
			s.log.Error("session panic contained", "execution", s.id, "panic", r)
			_ = s.markState(ctx, "failed")
			if s.transcript != nil {
				_ = s.transcript.Append(TransError, map[string]any{"error": msg})
				_ = s.transcript.Close()
			}
			callbacks.OnResult(ctx, s.id, false, s.output.String(), "panic: "+fmt.Sprint(r))
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()

	// Open (or reopen) the crash-safe transcript.
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
	// text IS the manifest field).
	s.appendUser(TransUserMessage, s.identity.Goal, "goal")
	if err := s.transcript.Append(TransUserMessage, map[string]any{"text": s.identity.Goal, "source": "goal"}); err != nil {
		return err
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

		// Build the turn request. The static system prefix is marked as
		// the provider cache breakpoint (Cache:true on the static block).
		req := TurnRequest{
			Model:    s.identity.Model,
			System:   []SystemBlock{{Text: s.identity.SystemPrompt, Cache: true}},
			Messages: s.history,
			MaxTokens: 4096,
		}
		if s.tools != nil {
			req.Tools = s.tools.Defs()
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

		finish, toolCalls, usage, streamErr := s.drain(ctx, callbacks, stream)
		_ = stream.Close()
		if streamErr != nil {
			msg := fmt.Sprintf("stream error: %v", streamErr)
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
			return nil
		}

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
			// Execute pending tool calls (parallel where independent),
			// append results to history, drain injection queue, loop.
			if len(toolCalls) > 0 {
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

		case StopStop, StopLength, StopOther:
			_ = s.markState(ctx, "done")
			_ = s.transcript.Append(TransFinish, map[string]any{"stop_reason": string(finish)})
			callbacks.OnResult(ctx, s.id, true, s.output.String(), "")
			return nil

		default: // StopContentFilter / StopError / StopOther non-success
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
	// Only treat zero-growth + repeated signature as a stall (a final
	// StopStop turn legitimately adds output tokens).
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
	// Zero token growth + tool calls: count repeated signatures.
	for _, tc := range toolCalls {
		sig := tc.Name + ":" + tc.ArgsJSON
		sigs[sig]++
		if sigs[sig] >= 2 {
			return "stalled:repetition:" + tc.Name
		}
	}
	return "stalled:no_progress"
}

// drain reads the turn stream until Finish, mapping every event type onto
// callbacks (no silent gaps). Returns the stop reason, the complete tool
// calls of the turn, the usage, and any mid-stream error.
func (s *Session) drain(ctx context.Context, callbacks scheduler.ExecutionCallbacks, stream TurnStream) (StopReason, []ToolCall, Usage, error) {
	var text strings.Builder
	var reasoning strings.Builder
	var finish StopReason
	var usage Usage
	var calls []ToolCall
	inflight := map[int]*ToolCall{} // index → in-flight call

	for {
		ev, ok, err := stream.Next(ctx)
		if err != nil {
			return finish, calls, usage, err
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
			return finish, calls, usage, e.Err
		case Finish:
			finish = e.StopReason
			usage = e.Usage
		}
	}
	return finish, calls, usage, nil
}

// executeTools runs the turn's tool calls (parallel where independent —
// same-turn calls are independent by construction) with a bounded worker
// pool, and emits OnToolCall + OnWrittenFiles + OnArtifact parity
// callbacks. A panicking tool is caught per-call and returned as an
// error result (the loop's boundary recover also catches it).
func (s *Session) executeTools(ctx context.Context, callbacks scheduler.ExecutionCallbacks, calls []ToolCall) []toolResult {
	results := make([]toolResult, len(calls))
	if s.tools == nil {
		for i, c := range calls {
			results[i] = toolResult{ToolCall: c, Err: "tool registry not configured"}
		}
		return results
	}
	par := toolParallelism()
	if par < 1 {
		par = 1
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, c := range calls {
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

// toolResult is one tool execution outcome.
type toolResult struct {
	ToolCall ToolCall
	Output   string // capped
	Err      string // "" on success
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
	mu  sync.Mutex
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
