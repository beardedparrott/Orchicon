package main

// install_setup_record_test.go — the installer must WRITE the setup record a GUI polls (design of record,
// D4).
//
// The record is only worth having if the thing doing the work fills it in. A reader with no writer is a
// progress bar that never moves — the same "seam nothing calls" failure as a wizard page with no route to
// it.

import (
	"strings"
	"testing"
)

// THE INSTALL FLOW FEEDS THE RECORD, on both exit paths.
func TestInstallFeedsTheSetupRecord(t *testing.T) {
	body := readGoFile(t, "install.go")

	// The PROPERTY, not the exact statement: a record must be begun for the parsed instance and installed as
	// the active tracker. Naming the assignment literally would fail on a refactor that captured the tracker
	// in a local first — which is a change this file's own comment recommends.
	if !strings.Contains(body, "beginSetup(instance)") {
		t.Error("runInstall never starts a setup record, so a GUI polling setup-progress.json sees " +
			"nothing for the whole install")
	}
	if !strings.Contains(body, "installProgress = tracker") && !strings.Contains(body, "installProgress = beginSetup(instance)") {
		t.Error("the begun tracker is never installed as the active one, so installPhase cannot feed it")
	}
	// BOTH terminal states. Without the failure arm a failed install leaves a record claiming to be
	// running for ever — the eternal spinner the staleness rule exists to CATCH, which should never be the
	// normal way a failure is reported.
	if !strings.Contains(body, ".fail(err)") {
		t.Error("the install does not record its failure, so a GUI cannot say WHY setup stopped")
	}
	if !strings.Contains(body, ".done()") {
		t.Error("the install does not record a clean finish")
	}
	// The phase banner must feed it, or the label never moves off "starting".
	if !strings.Contains(body, "installProgress.phase(msg)") {
		t.Error("installPhase does not update the record, so a GUI shows one frozen label for the whole " +
			"install instead of following the steps")
	}
}

// THE RECORD IS CLEARED BEFORE A FRESH RUN, so an older finished record cannot be read as this run's
// progress and report "done" for an install that has only just started.
func TestBeginSetupClearsTheOldRecord(t *testing.T) {
	if !strings.Contains(readGoFile(t, "install.go"), "instancectl.ClearSetupProgress(instance)") {
		t.Error("beginSetup does not clear the previous record, so a stale 'done' from an earlier install " +
			"renders as this run having already finished")
	}
}

// THE WRITER MUST NOT COMPUTE ITS OWN PATH. A writer and a reader that disagree about the location is the
// same bug with an extra step.
func TestWriterUsesTheContractsPath(t *testing.T) {
	body := readGoFile(t, "install.go")
	if strings.Contains(body, `"setup-progress.json"`) {
		t.Error("cmd/orchicon hardcodes the setup record's filename instead of going through " +
			"instancectl, so changing the reader's path would silently orphan the writer")
	}
	if !strings.Contains(body, "instancectl.WriteSetupProgress") {
		t.Error("the install never calls the contract's writer")
	}
}

// THE SLUG IS A STABLE KEY, because the GUI switches on it. It must not leak punctuation or case, and it
// must not produce an empty key for a plain message.
func TestInstallPhaseSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"starting the host services", "starting-the-host-services"},
		{"Starting the Host Services", "starting-the-host-services"},
		{"resolving the infrastructure binaries (postgres, nats)", "resolving-the-infrastructure-binaries-postgres-nats"},
		{"waiting for http://localhost:8080/healthz", "waiting-for-http-localhost-8080-healthz"},
		{"a  b", "a-b"},
		{"", ""},
	} {
		if got := slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// EVERY WRITE CARRIES THE FIELDS THAT MUST PERSIST — found by a real run, not by reading.
//
// The first version of this tracker wrote each state as a FRESH struct, so the terminal records dropped two
// fields the intermediate ones had set:
//
//	started_at   lost, so WriteSetupProgress stamped "now" on the finished record and the install appeared
//	             to have STARTED at the moment it FINISHED ("elapsed: 0s" for a minutes-long download)
//	pid          lost (always 0), so staleness fell back to the timestamp alone — the weaker of the two
//	             signals the design specifies, when the process table was available all along
//
// The fix is one write path. This asserts it stays one: every record this file writes must go through
// setupTracker.write, so a future "just add a state" cannot silently shed a field again.
func TestSetupTrackerHasOneWritePath(t *testing.T) {
	body := readGoFile(t, "install.go")
	// Count the calls to the contract's writer: exactly one, inside write().
	if n := strings.Count(body, "instancectl.WriteSetupProgress("); n != 1 {
		t.Errorf("found %d calls to instancectl.WriteSetupProgress; there must be exactly ONE (inside "+
			"setupTracker.write). Three writers is how started_at and pid came to be set on some records "+
			"and dropped on others.", n)
	}
	for _, want := range []string{"StartedAt: t.startedAt", "PID: t.pid"} {
		if !strings.Contains(body, want) {
			t.Errorf("the single write path does not carry %q, so a record loses a field the design "+
				"requires", want)
		}
	}
	// And the tracker must actually capture them.
	if !strings.Contains(body, "pid: os.Getpid()") {
		t.Error("beginSetup does not record this install's pid, so staleness can only use the timestamp")
	}
	if !strings.Contains(body, "nowRFC3339()") {
		t.Error("the tracker does not capture a start time, so the finished record cannot say when the " +
			"install began")
	}
}
