package claude

import "fmt"

// errNoExecutionID is returned when Start is called with no execution id.
var errNoExecutionID = fmt.Errorf("claude: execution id is required")

// errAlreadyLive is returned when Start is called for an execution that
// already has a live session (a second writer is never allowed).
func errAlreadyLive(execID string) error {
	return fmt.Errorf("claude: execution %s already has a live session", execID)
}

// errSessionLive is the single-writer refusal: a claude session id may have
// EXACTLY ONE active subprocess (never two writers to the same JSONL).
func errSessionLive(sid, owner string) error {
	return fmt.Errorf("claude: session %s already has a live subprocess (execution %s) — refusing a second writer to the same transcript", sid, owner)
}

// errNoLiveSession is the actionable error SendExecutionMessage returns when
// the execution has no live claude session.
func errNoLiveSession(execID string) error {
	return fmt.Errorf("claude execution %s has no live session (not running on the streaming transport)", execID)
}

// errNoLiveSessionForCompact is the actionable error CompactExecution returns
// when the execution has no live claude session to compact. It names the
// execution and the capability instead of nil-panicking, per the bridge
// contract rule (scheduler/bridge.go: a missing capability surfaces an
// actionable error on its path, never a panic).
func errNoLiveSessionForCompact(execID string) error {
	return fmt.Errorf("claude execution %s has no live session to compact (context compaction needs the streaming session)", execID)
}

// errCompactUnsupported is the actionable error returned when compaction
// cannot be performed at all (no budget ladder resolved for the execution).
func errCompactUnsupported(execID, why string) error {
	return fmt.Errorf("claude execution %s does not support context compaction: %s", execID, why)
}

// errCompactNotReady is the actionable error returned when a compaction
// request is refused by a shared guard (at most once per step, never at
// start, min-turn floor, per-execution cap).
func errCompactNotReady(execID, why string) error {
	return fmt.Errorf("claude execution %s cannot compact now: %s", execID, why)
}

// errCompactWriteFailed is the actionable error returned when the LIVE
// session refused the compact directive write (a broken stdin / a dead
// transport). The session IS live, so the underlying cause rides along
// instead of being flattened into a misleading "no live session" diagnosis.
func errCompactWriteFailed(execID string, cause error) error {
	return fmt.Errorf("claude execution %s: context compaction could not be written to the live session: %v", execID, cause)
}
