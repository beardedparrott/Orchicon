package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// The safety rule of the operator-invoked release.
//
// ReleaseStuckExecutions exists so a project pinned at its dispatch cap can be
// freed without waiting for the reaper's next tick. That is only safe if it
// cannot release an execution that is ALIVE — otherwise the button becomes a
// way to kill healthy work with one click, which is strictly worse than the
// wedge it fixes.
//
// These cases pin that directly, using the reaper's own rule with the pool
// absent: reapOne is the single decision point both the tick and the release
// call, so pinning it pins both.
func TestReapOneOnlyReleasesExecutionsTheProbeSaysAreDead(t *testing.T) {
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	started := time.Now().Add(-3 * time.Hour) // well past any grace window
	fresh := time.Now()                       // inside the grace window

	cases := []struct {
		name             string
		exec             db.ExecutionRow
		alive            bool
		allowRecorded    bool
		wantReleased     bool
		whyItMustNotKill string
	}{
		{
			name:  "LIVE execution with a not-well status is NOT released",
			exec:  db.ExecutionRow{ID: "e1", Status: domain.ExecutionUnhealthy, StartedAt: &started},
			alive: true, allowRecorded: true, wantReleased: false,
			whyItMustNotKill: "a row can be marked unhealthy and still be tracked by the adapter; " +
				"the PROBE is authoritative, and it says alive",
		},
		{
			name:  "LIVE running execution is NOT released",
			exec:  db.ExecutionRow{ID: "e2", Status: domain.ExecutionRunning, StartedAt: &started},
			alive: true, allowRecorded: true, wantReleased: false,
			whyItMustNotKill: "one click must never kill healthy work",
		},
		{
			name:  "DEAD execution carrying a not-well verdict IS released (the field case)",
			exec:  db.ExecutionRow{ID: "e3", Status: domain.ExecutionUnhealthy, StartedAt: &started},
			alive: false, allowRecorded: true, wantReleased: true,
			whyItMustNotKill: "this is the ten-row incident: dead, marked unhealthy, holding every slot",
		},
		{
			name:  "DEAD execution inside the grace window is NOT released",
			exec:  db.ExecutionRow{ID: "e4", Status: domain.ExecutionUnhealthy, StartedAt: &fresh},
			alive: false, allowRecorded: true, wantReleased: false,
			whyItMustNotKill: "too fresh for the probe to be authoritative — it may not be registered yet; " +
				"the grace window is the protection against a dispatch race",
		},
		{
			name:  "DEAD without a recorded verdict needs the streak on the release path too",
			exec:  db.ExecutionRow{ID: "e5", Status: domain.ExecutionRunning, StartedAt: &started},
			alive: false, allowRecorded: true, wantReleased: false,
			whyItMustNotKill: "the waiver is only for rows whose verdict is ALREADY recorded; a plain " +
				"running row still needs consecutive not-alive probes, so a stray click cannot kill it",
		},
		{
			name:  "no start time at all is NOT released",
			exec:  db.ExecutionRow{ID: "e6", Status: domain.ExecutionUnhealthy},
			alive: false, allowRecorded: true, wantReleased: false,
			whyItMustNotKill: "an execution that never recorded a start cannot be aged, and age is half the rule",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var failed []string
			r := NewExecutionReaper(nil,
				func(db.ExecutionRow) bool { return tc.alive },
				func(_ context.Context, execID, _ string) { failed = append(failed, execID) },
				nopLog)

			released := r.reapOne(context.Background(), tc.exec, time.Minute, 3, tc.allowRecorded)

			if released != tc.wantReleased {
				t.Errorf("released = %v, want %v — %s", released, tc.wantReleased, tc.whyItMustNotKill)
			}
			if released && len(failed) != 1 {
				t.Errorf("released but the failure action ran %d time(s); the release must go through the "+
					"same action as the tick so recovery and the work item behave identically", len(failed))
			}
			if !released && len(failed) != 0 {
				t.Errorf("an execution was failed despite released=false")
			}
		})
	}
}

// The unattended tick must NEVER take the recorded-verdict waiver: it acts with
// nobody watching, so it must always see a sustained not-alive streak. This is
// what keeps the waiver a property of the attended path alone.
func TestPeriodicTickNeverTakesTheRecordedVerdictWaiver(t *testing.T) {
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	started := time.Now().Add(-3 * time.Hour)
	var failed []string
	r := NewExecutionReaper(nil,
		func(db.ExecutionRow) bool { return false }, // always not-alive
		func(_ context.Context, execID, _ string) { failed = append(failed, execID) },
		nopLog)

	exec := db.ExecutionRow{ID: "e1", Status: domain.ExecutionUnhealthy, StartedAt: &started}

	// Tick semantics: threshold 3, no waiver. Two probes must NOT reap.
	if r.reapOne(context.Background(), exec, time.Minute, 3, false) {
		t.Fatal("reaped on the unattended path before the consecutive threshold — the tick must always " +
			"see a sustained streak, because nobody is watching it")
	}
	if r.reapOne(context.Background(), exec, time.Minute, 3, false) {
		t.Fatal("reaped on the second probe; threshold is 3")
	}
	if !r.reapOne(context.Background(), exec, time.Minute, 3, false) {
		t.Fatal("did not reap on the third consecutive not-alive probe")
	}
	if len(failed) != 1 {
		t.Fatalf("failure action ran %d time(s), want 1", len(failed))
	}
}
