package opencode

import "testing"

// TestAuthRejectionsAreNeverInfrastructureFailures is the guard for a MISCLASSIFICATION
// that the v2 terminal-event fix made reachable.
//
// isInfraModelTurnError decides whether a model-turn failure recycles the run's runtime
// container (an infrastructure problem) or stays on the failure path (a provider
// decision). Its own doc says an "auth ... rejection must never be reclassified as
// infra", but the guard list held only status CODES and the word "unauthorized" — so an
// auth failure phrased any other way fell through to the socket check.
//
// The case that matters: opencode refuses to serve a free-tier model unattended with
// `AI.Error.Authentication: OpenCode's free tier can only be used from within OpenCode`
// — no 401, no "unauthorized". Reclassifying that as infrastructure would churn the
// container instead of telling the operator the MODEL is the problem.
func TestAuthRejectionsAreNeverInfrastructureFailures(t *testing.T) {
	for _, msg := range []string{
		"AI.Error.Authentication: OpenCode's free tier can only be used from within OpenCode",
		"Authentication failed",
		"unauthenticated",
		"Access denied for this model",
		"HTTP 401 Unauthorized",
		"You have insufficient quota",
		"rate limit exceeded (429)",
	} {
		if isInfraModelTurnError(msg) {
			t.Errorf("isInfraModelTurnError(%q) = true — a provider DECISION must not recycle the container; it must fail and say why", msg)
		}
	}
}

// TestGenuineTransportFailuresStayInfrastructure pins the class the function exists FOR,
// so the guard terms do not swallow it: a model call that cannot reach the API at the
// socket layer must still recycle immediately rather than burn three dead turns.
func TestGenuineTransportFailuresStayInfrastructure(t *testing.T) {
	for _, msg := range []string{
		"Cannot connect to API: Unable to connect. Is the computer able to access the url?",
		"dial tcp 1.2.3.4:443: connect: connection refused",
		"no such host",
	} {
		if !isInfraModelTurnError(msg) {
			t.Errorf("isInfraModelTurnError(%q) = false — a socket-layer failure is the case this exists for", msg)
		}
	}
	// An empty message is not an infra failure: there is nothing to classify.
	if isInfraModelTurnError("") {
		t.Error("an empty message must not classify as infrastructure")
	}
}
