package orchicon

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Decision-signal guard + completion probe for native sessions
// (opencode parity — the opencode session engine's realDecisionMarkerIn /
// completionProbe semantics, ADR parity contract). A native session that
// reaches a settle-point WITHOUT a real ORCHICON WORKER SUMMARY marker must
// never be recorded as a clean success: the marker is the worker's contract
// sign-off, and its absence means the final response was truncated (the
// MaxTokens cap cutting the model mid-monologue — the reported hollow
// successes), the model idled early, or the marker was echoed as a plan
// placeholder.
//
// The completion probe mirrors the opencode completion probe: when the
// session settles without the marker, interject ONE user turn asking for
// the sign-off. The probe turn either delivers the marker (the loop
// continues; the next StopStop turn settles with the marker present) or the
// budget exhausts and the execution fails honestly
// (stalled:missing_decision_signal:completion_probe_no_response).

// decisionMarker is the single marker signal every worker execution ends
// with — identical to the opencode adapter's decisionMarker
// (internal/opencode/session_run.go) and the scheduler's summaryMarker
// (internal/scheduler/reconciler.go). One contract, three consumers.
const decisionMarker = "ORCHICON WORKER SUMMARY:"

// completionProbeMaxTurns bounds the completion-probe budget. Two probes:
// the first asks for the sign-off, the second asks again (a model that
// replies with more work gets exactly one more chance). After that the
// session is failed — the workflow's loop decision / re-ask / fail path is
// the correct owner of a missing signal, never a hollow success.
const completionProbeMaxTurns = 2

// completionProbeText mirrors the opencode completion probe (opencode
// session_run.go completionProbeText): do not restart work; deliver the
// final summary now.
const completionProbeText = "Your response appears to have been cut off before your final ORCHICON WORKER SUMMARY was captured. " +
	"Please do not restart your work. " +
	"If you have finished your task, reply with your final summary exactly in this form: " +
	"ORCHICON WORKER SUMMARY: success — <summary>  (or  failure — <reason>). " +
	"If you are still working, report your current status and then continue, and be sure to end with your ORCHICON WORKER SUMMARY when done."

// placeholderMarkerBody reports whether the text following an
// ORCHICON WORKER SUMMARY marker is a doc/plan placeholder ("success — <summary>",
// "<reason>", an empty body) rather than a real worker-written summary. A
// worker that echoes the marker as an example inside its plan must not be
// treated as having delivered the signal. Keep in sync with
// internal/opencode/session_run.go placeholderMarkerBody and
// internal/scheduler/reconciler.go placeholderSummaryBody.
func placeholderMarkerBody(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	// Inline code (backtick-quoted) markers are seed/instruction echo, never
	// a real sign-off — strip a leading backtick from the first word; a bare
	// success/failure in backticks is a placeholder, not a delivery.
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

// decisionMarkerPresent reports whether the session output carries a REAL
// ORCHICON WORKER SUMMARY sign-off (last real occurrence wins — a worker
// may plan with the marker as an example and then deliver it later).
func (s *Session) decisionMarkerPresent() bool {
	return realDecisionMarkerIn(s.output.String()) >= 0
}

// realDecisionMarkerIn reports the index of the LAST real ORCHICON WORKER
// SUMMARY marker in output — one whose body is actual content, not a
// placeholder/template echo. Returns -1 when no real marker exists. Keep in
// sync with internal/opencode/session_run.go realDecisionMarkerIn.
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

// runCompletionProbe interjects the completion-probe user turn when a
// session reached the settle-point without the decision marker. Returns
// true when the probe was delivered and the loop should continue (the
// probe turn's StopStop re-enters the success gate with fresh output);
// false when the probe budget is exhausted — the execution has been failed
// (OnResult fired) and the caller must return immediately.
func (s *Session) runCompletionProbe(ctx context.Context, callbacks scheduler.ExecutionCallbacks) bool {
	if s.completionProbesSent >= completionProbeMaxTurns {
		msg := "stalled:missing_decision_signal:completion_probe_no_response"
		_ = s.transcript.Append(TransError, map[string]any{"error": msg})
		_ = s.markState(ctx, "failed")
		s.log.Warn("native session idle without decision marker after probes — failing",
			"execution", s.id, "probes", s.completionProbesSent)
		callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
		return false
	}
	s.completionProbesSent++
	s.appendUser(TransUserMessage, completionProbeText, "completion_probe")
	if err := s.transcript.Append(TransUserMessage, map[string]any{"text": completionProbeText, "source": "nudge"}); err != nil {
		// Transcript failure: fail the execution with the underlying error —
		// never a silent success.
		msg := fmt.Sprintf("completion probe transcript append failed: %v", err)
		_ = s.markState(ctx, "failed")
		callbacks.OnResult(ctx, s.id, false, s.output.String(), msg)
		return false
	}
	s.log.Info("native session settled without decision marker — sending completion probe",
		"execution", s.id, "probe", s.completionProbesSent, "max", completionProbeMaxTurns)
	return true
}