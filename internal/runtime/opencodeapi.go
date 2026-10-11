package runtime

// opencodeapi.go — the opencode HTTP paths the DAEMON probes, and why they are tried in both forms.
//
// WHY THIS FILE EXISTS AT ALL, since internal/opencode already has a shape table: the dependency runs
// the WRONG WAY for reuse. internal/opencode imports internal/runtime (the adapter drives the daemon),
// so internal/runtime cannot import that table without a cycle. The daemon therefore keeps its own
// list — and a test in internal/opencode asserts the two agree, so the duplication cannot drift
// silently. That test is the thing that makes a second copy acceptable rather than a future bug.
//
// WHY BOTH FORMS ARE TRIED rather than a version being detected here. The daemon probes a serve that
// runs INSIDE a runtime container; asking its version would mean a `docker exec` per readiness check,
// which is the wrong shape for a gate that runs on every run start. The gate's question is not "which
// version is this?" but "is this serve usable?", and a serve answers exactly one of these paths — so
// trying them in order answers the real question directly, with no subprocess and no cached verdict to
// go stale.
//
// THE REGRESSION THIS FIXES, measured against opencode 2.0.26: the gate hardcoded the v1 pair, so
// against a 2.x serve the health path could answer but the SESSION-CREATE path did not — the probe
// require-200 on `POST /session` while the real surface answers that route with 405 — and
// serveUsableAt returned false forever. Its caller reports "serve did not become usable within 30s",
// which is the run-start gate for every workflow execution: on a 2.x install, EXECUTIONS COULD NOT
// START, while Ask Orchicon worked, because Ask uses the session client that had been fixed and the
// daemon does not.

// OpenCodeHealthPaths are the readiness paths a serve may expose, newest surface last. v1 is first so
// that a v1 serve — every install in the field today — is answered by the first request exactly as
// before, with no extra round trip.
func OpenCodeHealthPaths() []string {
	return []string{"/global/health", "/api/info"}
}

// OpenCodeSessionCreatePaths are the session-create paths a serve may expose, same ordering rule.
//
// Session creation is part of the readiness probe because a cold-starting serve answers health before
// its session machinery is up, so health alone is not "usable" for dispatch.
func OpenCodeSessionCreatePaths() []string {
	return []string{"/session", "/api/session"}
}

// opencodeAcceptableCreateStatus reports whether a session-create status means the machinery is up.
//
// 2xx RATHER THAN 200 EXACTLY: a create that succeeds may reasonably answer 200 or 201, and requiring
// the one value is how a working serve gets reported unusable. Anything else — including the 405 a
// wrong-surface route returns — is not usable.
func opencodeAcceptableCreateStatus(code int) bool {
	return code >= 200 && code < 300
}
