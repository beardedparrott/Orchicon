package orchicon

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Decision-signal guard + completion probe for native sessions (opencode parity — the opencode
// session engine's realDecisionMarkerIn / completionProbe semantics, ADR parity contract).

// decisionMarker is the single marker signal every worker execution ends with — identical to the
// opencode adapter's decisionMarker (internal/opencode/session_run.go) and the scheduler's.
const decisionMarker = "ORCHICON WORKER SUMMARY:"

// completionProbeMaxTurns bounds the completion-probe budget.
const completionProbeMaxTurns = 2

// probeReplyKind classifies the model's reply to a completion probe.
const (
	// probeReplyNone — no answer to the probe in the output.
	probeReplyNone = iota
	// probeReplySummary — the output carries a REAL decision marker.
	probeReplySummary
	// probeReplyWorking — the reply is (contains) the literal WORKING
	// token: the model's explicit "still working, leave me alone" answer.
	probeReplyWorking
)

// probeWorkingToken is the literal single-word answer the completion probe offers a still-working
// model: reply WORKING and you will be left alone to continue.
const probeWorkingToken = "working"

// completionProbeReply classifies whether the session output ANSWERS a completion probe.
func completionProbeReply(output string) (int, int) {
	if idx := realDecisionMarkerIn(output); idx >= 0 {
		return probeReplySummary, idx
	}
	// WORKING token: word-bounded scan of the output tail (the probe reply is the latest turn; earlier
	// turns may legitimately contain the word, so only the last ~200 chars are considered).
	tail := output
	if len(tail) > 200 {
		tail = tail[len(tail)-200:]
	}
	lower := strings.ToLower(tail)
	search := 0
	for search < len(lower) {
		at := strings.Index(lower[search:], probeWorkingToken)
		if at < 0 {
			break
		}
		at += search
		before := byte(' ')
		if at > 0 {
			before = lower[at-1]
		}
		after := byte(' ')
		if at+len(probeWorkingToken) < len(lower) {
			after = lower[at+len(probeWorkingToken)]
		}
		isBoundary := func(b byte) bool {
			return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '.' || b == ',' || b == '!' || b == '?' || b == '"' || b == '\'' || b == '*' || b == '`' || b == '_' || b == '-' || b == ':' || b == ';'
		}
		if isBoundary(before) && isBoundary(after) {
			return probeReplyWorking, at
		}
		search = at + len(probeWorkingToken)
	}
	return probeReplyNone, -1
}

// completionProbeText mirrors the opencode completion probe (opencode session_run.go
// completionProbeText).
const completionProbeText = "Your response appears to have been cut off before your final ORCHICON WORKER SUMMARY was captured. " +
	"Please do not restart your work and do NOT reply with a status update. " +
	"Your NEXT reply must be one of exactly two things: " +
	"(1) your final summary, in this form: ORCHICON WORKER SUMMARY: success — <summary>  (or  failure — <reason>), or " +
	"(2) the single word WORKING (nothing else) if you still have work to do — you will then be left alone to continue working."

// placeholderMarkerBody reports whether the text following an ORCHICON WORKER SUMMARY marker is a
// doc/plan placeholder ("success — <summary>", "<reason>".
func placeholderMarkerBody(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	// Inline code (backtick-quoted) markers are seed/instruction echo, never a real sign-off — strip a
	// leading backtick from the first word; a bare success/failure in backticks is a placeholder.
	words := strings.Fields(rest)
	if len(words) > 0 {
		raw := words[0]
		before, _ := strings.CutPrefix(raw, "`")
		after, afterBacktick := strings.CutSuffix(before, "`")
		if afterBacktick {
			lower := strings.ToLower(after)
			if lower == "success" || lower == "failure" {
				return true
			}
		}
	}
	if strings.Contains(rest, "<summary>") || strings.Contains(rest, "<reason>") ||
		strings.Contains(rest, "<your summary>") || strings.Contains(rest, "<your-summary>") {
		return true
	}
	// "success — <summary>", "success", "—", "failure" with nothing real.
	lower := strings.ToLower(rest)
	switch lower {
	case "", "success", "failure", "success —", "failure —", "success — <summary>", "failure — <reason>":
		return true
	}
	return false
}

// decisionMarkerPresent reports whether the session output carries a REAL ORCHICON WORKER SUMMARY
// sign-off (last real occurrence wins.
func (s *Session) decisionMarkerPresent() bool {
	return realDecisionMarkerIn(s.output.String()) >= 0
}

// realDecisionMarkerIn reports the index of the LAST real ORCHICON WORKER SUMMARY marker in output
// — one whose body is actual content, not a placeholder/template echo.
func realDecisionMarkerIn(output string) int {
	idx := strings.LastIndex(output, decisionMarker)
	for idx >= 0 {
		if !placeholderMarkerBody(output[idx+len(decisionMarker):]) {
			return idx
		}
		// The last occurrence was a placeholder echo — look for an earlier
		// genuine one.
		idx = strings.LastIndex(output[:idx], decisionMarker)
	}
	return -1
}

// runCompletionProbe interjects the completion-probe user turn when a session reached the
// settle-point without the decision marker.
func (s *Session) runCompletionProbe(ctx context.Context, callbacks scheduler.ExecutionCallbacks) bool {
	// Budget gate (2026-09-09 liveness-kill fix): a probe only counts against the budget once its
	// PREVIOUS probe was actually answered (completionProbeAwaiting false).
	if s.completionProbeAwaiting && !s.completionProbeSawReply {
		s.log.Info("native completion probe still awaiting a reply — deferring, not spending budget",
			"execution", s.id, "probes", s.completionProbesSent, "max", completionProbeMaxTurns)
		// Bound the deferral: the probe reply usually lands as deltas (nudgeObserved clears the flag), so
		// reaching here twice in a row means the provider returned EMPTY turns for the probe.
		s.completionProbeDeferrals++
		if s.completionProbeDeferrals > 1 {
			msg := "stalled:missing_decision_signal:completion_probe_no_response"
			_ = s.transcript.Append(TransError, map[string]any{"error": msg})
			_ = s.markState(ctx, "failed")
			s.log.Warn("native completion probe never produced a reply — failing",
				"execution", s.id, "probes", s.completionProbesSent, "deferrals", s.completionProbeDeferrals)
			callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
			s.markNudgeFinished()
			s.closeDoneCh()
			return false
		}
		// Re-queue the SAME probe turn so the model gets its full reply window: the settle re-enters with
		// the reply's output.
		return true
	}
	if s.completionProbesSent >= completionProbeMaxTurns {
		msg := "stalled:missing_decision_signal:completion_probe_no_response"
		_ = s.transcript.Append(TransError, map[string]any{"error": msg})
		_ = s.markState(ctx, "failed")
		s.log.Warn("native session idle without decision marker after probes — failing",
			"execution", s.id, "probes", s.completionProbesSent)
		callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
		s.markNudgeFinished()
		s.closeDoneCh()
		return false
	}
	s.completionProbesSent++
	s.completionProbeAwaiting = true
	s.completionProbeSawReply = false
	s.completionProbeDeferrals = 0
	s.appendUser(TransUserMessage, completionProbeText, "completion_probe")
	if err := s.transcript.Append(TransUserMessage, map[string]any{"text": completionProbeText, "source": "nudge"}); err != nil {
		// Transcript failure: fail the execution with the underlying error —
		// never a silent success.
		msg := fmt.Sprintf("completion probe transcript append failed: %v", err)
		_ = s.markState(ctx, "failed")
		s.fireTerminalOnce(callbacks, s.id, false, msg)
		s.markNudgeFinished()
		s.closeDoneCh()
		return false
	}
	s.log.Info("native session settled without decision marker — sending completion probe",
		"execution", s.id, "probe", s.completionProbesSent, "max", completionProbeMaxTurns)
	return true
}
