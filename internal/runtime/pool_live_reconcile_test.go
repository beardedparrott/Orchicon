package runtime

// pool_live_reconcile_test.go — LIVE container reconciliation: the untracked container that used to survive
// until the next daemon restart.
//
// REPORTED: an orphan `orchicon-runtime-…` was reclaimed only when the runtime daemon restarted, and in the
// meantime it held its published ports and its memory. The only reclamation was sweepPreRestartOrphans,
// which runs once per instance per daemon lifetime — it covers what a PREVIOUS daemon left behind and
// nothing that stops being tracked while this one is up.

import (
	"strings"
	"testing"
	"time"
)

// A container that is present but untracked is reclaimed once the grace window has passed.
func TestLiveSweepReclaimsAnUntrackedContainerAfterTheGraceWindow(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-orphan-1")
	p.served["dev"] = true

	// Pass 1: FIRST sighting. It must NOT be removed — this is also the state of a container mid-create.
	p.reconcileOrphans()
	if r := h.removedNames(); len(r) != 0 {
		t.Fatalf("the first sighting removed %v — a container being created looks exactly like this, so a "+
			"single-pass sweep deletes the container it is racing", r)
	}

	// Age the candidate past the grace window, as if two minutes had passed.
	p.mu.Lock()
	for n, c := range p.untracked {
		c.since = time.Now().Add(-p.orphanGrace() - time.Second)
		p.untracked[n] = c
	}
	p.mu.Unlock()

	// Pass 2: still untracked and past the window → reclaimed, WITHOUT a daemon restart.
	p.reconcileOrphans()
	if r := h.removedNames(); len(r) != 1 || r[0] != "orchicon-runtime-orphan-1" {
		t.Fatalf("reaped %v, want exactly [orchicon-runtime-orphan-1] — an untracked container must not "+
			"survive until the next daemon restart", r)
	}
}

// THE SAFETY HALF: a TRACKED container is never touched, however long it sits in the engine's listing.
func TestLiveSweepNeverRemovesATrackedContainer(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-live-1")
	p.served["dev"] = true
	// Record it the way a create would.
	p.mu.Lock()
	p.entries["orchicon-runtime-live-1"] = &poolEntry{name: "orchicon-runtime-live-1", envKey: "k"}
	p.mu.Unlock()

	// Even with a candidate aged past the window (the state a race would leave behind), a container that
	// IS tracked must survive: the tracking check comes first, on every pass.
	p.mu.Lock()
	p.untracked["orchicon-runtime-live-1"] = orphanCandidate{instance: "dev", since: time.Now().Add(-time.Hour)}
	p.mu.Unlock()

	p.reconcileOrphans()

	if r := h.removedNames(); len(r) != 0 {
		t.Fatalf("the live sweep REMOVED a tracked container %v — that is a live run losing its container", r)
	}
	if _, still := p.untracked["orchicon-runtime-live-1"]; still {
		t.Error("a tracked container stayed in the untracked candidate map — it will be re-examined forever")
	}
}

// A container that becomes tracked between passes is forgotten rather than reclaimed: the create won.
func TestLiveSweepForgetsACandidateThatBecomesTracked(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-racing-1")
	p.served["dev"] = true

	p.reconcileOrphans() // first sighting
	p.mu.Lock()
	for n, c := range p.untracked {
		c.since = time.Now().Add(-time.Hour)
		p.untracked[n] = c
	}
	// The create completed in the meantime.
	p.entries["orchicon-runtime-racing-1"] = &poolEntry{name: "orchicon-runtime-racing-1", envKey: "k"}
	p.mu.Unlock()

	p.reconcileOrphans()

	if r := h.removedNames(); len(r) != 0 {
		t.Fatalf("the sweep removed %v even though the container became tracked", r)
	}
	if _, still := p.untracked["orchicon-runtime-racing-1"]; still {
		t.Error("the candidate was not forgotten after the container became tracked")
	}
}

// An instance this daemon does not serve is never listed: a daemon must not reach into a sibling instance.
func TestLiveSweepOnlyTouchesInstancesItServes(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-other-1")
	// NO entry in served.

	p.reconcileOrphans()

	if n := h.psCount(); n != 0 {
		t.Fatalf("the live sweep listed containers for an instance it has never leased for (%d ps calls) — "+
			"that is the cross-instance hazard resetPool documents", n)
	}
	if r := h.removedNames(); len(r) != 0 {
		t.Fatalf("the live sweep removed %v on an unserved instance", r)
	}
}

// The listing is instance-scoped and non-empty, exactly like the restart sweep.
func TestLiveSweepFiltersByInstance(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-x-1")
	p.served["dev"] = true

	p.reconcileOrphans()

	args := h.psArgs()
	if args == nil {
		t.Fatal("the live sweep must list the instance's containers")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "label=orchicon.instance=dev") {
		t.Errorf("the live sweep must filter by instance; got: %s", joined)
	}
	for i, a := range args {
		if a == "label=orchicon.instance=" || (a == "--filter" && i+1 < len(args) && args[i+1] == "label=orchicon.instance=") {
			t.Errorf("the live sweep used an EMPTY instance filter — that is the host-wide sweep; got: %s", joined)
		}
	}
}

// A container that disappears between passes is dropped from the candidate map, so the map cannot grow.
func TestLiveSweepForgetsACandidateThatDisappears(t *testing.T) {
	h := &sweepHarness{listed: []string{"orchicon-runtime-gone-1"}}
	d := &Daemon{Log: discardLogger(), engineFn: h.docker}
	p := newDaemonPool(d)
	p.served["dev"] = true

	p.reconcileOrphans()
	if len(p.untracked) != 1 {
		t.Fatalf("want 1 candidate after the first sighting, got %d", len(p.untracked))
	}

	// It is removed by someone else (a hand cleanup, or the idle pass).
	h.mu.Lock()
	h.listed = nil
	h.mu.Unlock()

	p.reconcileOrphans()
	if len(p.untracked) != 0 {
		t.Fatalf("a container that no longer exists stayed in the candidate map (%d entries)", len(p.untracked))
	}
}

// The grace window is overridable and defaults well above a create's duration.
func TestOrphanGraceDefaultIsAboveACreateDuration(t *testing.T) {
	p, _ := newSweepPool(t)
	if got := p.orphanGrace(); got < time.Minute {
		t.Fatalf("orphan grace = %s, want at least a minute — a create is one engine subprocess, and being "+
			"too eager here deletes a live run's container", got)
	}
}
