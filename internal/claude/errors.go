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
