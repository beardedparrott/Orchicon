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

// probeWorkingToken is the literal single-word answer the completion probe
// offers a still-working model: reply WORKING and you will be left alone
// to continue. Checked as a token (word-bounded, case-insensitive) so a
// word like "networking" or a phrase containing "working on it" does NOT
// match — only the standalone token does.
const probeWorkingToken = "working"

// completionProbeReply classifies whether the session output ANSWERS a
// completion probe. Returns (probeReplySummary, idx>=0) when a real
// ORCHICON WORKER SUMMARY marker is present, (probeReplyWorking, idx>=0)
// when the probe reply is the WORKING token, and (probeReplyNone, -1)
// otherwise (including bare status lines — those are NOT answers; see the
// 2026-09-09 probe-loop incident). The WORKING token is only credited when
// it appears AFTER the probe was sent — implemented by the caller passing
// the probe-time output; here it is checked anywhere in the tail, because
// the probe reply is by construction the LAST turn's output.
func completionProbeReply(output string) (int, int) {
	if idx := realDecisionMarkerIn(output); idx >= 0 {
		return probeReplySummary, idx
	}
	// WORKING token: word-bounded scan of the output tail (the probe
	// reply is the latest turn; earlier turns may legitimately contain the
	// word, so only the last ~200 chars are considered).
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

// completionProbeText mirrors the opencode completion probe (opencode
// session_run.go completionProbeText). 2026-09-09 probe-loop incident: the
// old "report your current status and then continue" phrasing made
// mid-work models reply with a bare status line (no marker), which re-armed
// the probe instantly — an infinite probe loop that never let the worker
// start (transcripts: 15 "I need to re-sync" replies, zero tool calls). The
// probe now DEMANDS the marker in the next response: the only acceptable
// answers are the summary itself or the literal WORKING token (a real
// continue turns into tool work, which re-enters the normal loop and
// delivers the marker when it finishes).
const completionProbeText = "Your response appears to have been cut off before your final ORCHICON WORKER SUMMARY was captured. " +
	"Please do not restart your work and do NOT reply with a status update. " +
	"Your NEXT reply must be one of exactly two things: " +
	"(1) your final summary, in this form: ORCHICON WORKER SUMMARY: success — <summary>  (or  failure — <reason>), or " +
	"(2) the single word WORKING (nothing else) if you still have work to do — you will then be left alone to continue working."

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
	// Budget gate (2026-09-09 liveness-kill fix): a probe only counts
	// against the budget once its PREVIOUS probe was actually answered
	// (completionProbeAwaiting false). A probe that is still awaiting a
	// reply must not spend a slot — the model is mid-reply to it right now,
	// and the very next StopStop (the probe reply landing) would otherwise
	// burn budget #2 or fail outright for a response that had not yet
	// arrived. Parity with the opencode cooldown-wait fix
	// (completionProbeDecision).
	//
	// 2026-09-09 probe-loop fix: a NON-EMPTY turn that streamed since the
	// probe but did not answer it (a bare status line — transcript
	// 01M23C2MTF1ZYYHE8ACK17PMKV) is NOT "still waiting". Falling into the
	// defer branch there looped the probe (re-queue → new status line →
	// defer → …) without ever spending budget. When SawReply is set the
	// gate FALLS THROUGH to the budget check: the non-answer spends the
	// slot, so after completionProbeMaxTurns probes the session fails
	// honestly instead of probing forever.
	if s.completionProbeAwaiting && !s.completionProbeSawReply {
		s.log.Info("native completion probe still awaiting a reply — deferring, not spending budget",
			"execution", s.id, "probes", s.completionProbesSent, "max", completionProbeMaxTurns)
		// Bound the deferral: the probe reply usually lands as deltas
		// (nudgeObserved clears the flag), so reaching here twice in a row
		// means the provider returned EMPTY turns for the probe — fail
		// honestly after one retry instead of looping forever.
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
		// Re-queue the SAME probe turn so the model gets its full reply
		// window: the settle re-enters with the reply's output. (The probe
		// text is already in history; loop continues to let it stream.)
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
