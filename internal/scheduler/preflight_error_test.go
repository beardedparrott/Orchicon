package scheduler

// preflight_error_test.go — A PREFLIGHT FAILURE MUST BE READABLE WITHOUT DECODING ANYTHING.
//
// REPORTED, run 01M6FCQX70K2W4YQFE8HZ5J8XF (Windows, git_strategy=local): it failed 3.8s after starting, before
// any step dispatched, because git worktree add could not resolve HEAD. The reason was recorded — inside
// run_context, which the API ships base64-encoded — while all three operator surfaces said nothing:
//
//	work item acceptance review: "Quick Software - Orchicon (failed): no summary recorded"
//	diagnose_failure:            {"failure_analysis": [], "workflow_executions": null}
//
// The platform knew; it filed the reason as an INPUT and rendered nothing.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// THE REPORTED STRING, classified so the UI can group it.
func TestInvalidReferenceIsClassifiedAsNoBaseRef(t *testing.T) {
	const reason = "could not create the run worktree: git strategy local, branch x, base ref HEAD, in " +
		"C:\\projects\\weatherapp — git worktree add ... failed: fatal: invalid reference: HEAD"

	if got := preflightErrorKind(domain.WorktreeFailed, reason); got != "worktree_no_base_ref" {
		t.Fatalf("error kind = %q, want worktree_no_base_ref — this is the reported failure and it must be "+
			"identifiable without parsing prose", got)
	}
}

// ...and turned into something an operator can act on, keeping the raw detail underneath.
func TestInvalidReferenceGetsAOperatorRemedy(t *testing.T) {
	const reason = "could not create the run worktree: base ref HEAD — fatal: invalid reference: HEAD"
	msg := preflightRemedy(domain.WorktreeFailed, reason)
	if msg == "" {
		t.Fatal("no remedy for the reported failure — 'fatal: invalid reference: HEAD' tells an operator " +
			"nothing they can act on")
	}
	// The remedy must say what to DO, not restate git.
	for _, want := range []string{"HEAD", "branch"} {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			t.Errorf("remedy does not mention %q: %s", want, msg)
		}
	}
}

// An empty base is its own kind, because the fix is different (add content, not create a branch).
func TestEmptyBaseIsItsOwnKind(t *testing.T) {
	const reason = `the resolved base "develop" (from configured-default) has an EMPTY tree`
	if got := preflightErrorKind(domain.WorktreeFailed, reason); got != "worktree_empty_base" {
		t.Fatalf("error kind = %q, want worktree_empty_base", got)
	}
	if preflightRemedy(domain.WorktreeFailed, reason) == "" {
		t.Error("an empty base has no remedy")
	}
}

// NOT every worktree mark is a failure: 'ready' and 'skipped' must not be labelled with an error kind.
func TestNonFailureMarksCarryNoErrorKind(t *testing.T) {
	for _, st := range []string{domain.WorktreeReady, domain.WorktreeSkipped, domain.WorktreePending} {
		if got := preflightErrorKind(st, "anything"); got != "" {
			t.Errorf("status %q got error kind %q — only a FAILED mark is an error", st, got)
		}
		if got := preflightRemedy(st, "anything"); got != "" {
			t.Errorf("status %q got a remedy", st)
		}
	}
}

// An UNRECOGNISED failure still classifies (catch-all), so no failure is ever left unlabelled.
func TestUnrecognisedWorktreeFailureStillClassifies(t *testing.T) {
	if got := preflightErrorKind(domain.WorktreeFailed, "something else entirely"); got != "worktree_preflight" {
		t.Fatalf("error kind = %q, want the catch-all worktree_preflight", got)
	}
}
