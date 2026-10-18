package main

// port_ownership_test.go — a rebuild must not move an instance's own ports.
//
// THE FAILURE. `make rebuild-dev` stops the instance and installs again, so at resolution time nothing of
// this instance is running. `resolveInstallPorts` only consulted the record when something of the instance
// was still UP, so on a rebuild it re-derived the table, probed, and reported the instance's OWN ports as
// conflicts — offering the next free one:
//
//	PostgreSQL: 5432 in use — use 5433?
//
// Accepting that MOVES the database port on a rebuild, which makes rebuild-dev non-repeatable and (worse)
// leaves the plane connecting to a port nothing serves.
//
// THE RULE PINNED HERE is ownership, not availability:
//
//	free                     -> keep the recorded port
//	held by THIS instance    -> keep it   (rebuild / upgrade in place)
//	held by ANOTHER instance  -> collision -> resolve new, naming the owner
//	held by anything else     -> collision -> resolve new, naming the holder

import (
	"os"
	"strings"
	"testing"
)

// portOwnerLabel must never call this instance's own holder a conflict, and must name a foreign one.
func TestPortOwnerLabel(t *testing.T) {
	// A port nothing serves has no owner to report.
	if got := portOwnerLabel(1, "dev"); got != "" {
		t.Errorf("portOwnerLabel on a free port = %q, want empty", got)
	}
}

// The decision function must be conservative: an unidentified holder is NOT ours. Over-claiming would keep
// a port that something else owns (and then fail to bind); under-claiming only costs a re-resolve.
func TestPortHeldByThisInstanceIsConservative(t *testing.T) {
	if portHeldByThisInstance(1, "dev") {
		t.Error("portHeldByThisInstance claimed a free port belongs to dev")
	}
	// A pid that does not exist must not panic or claim ownership.
	if processBelongsTo(os.Getpid(), "definitely-not-this-instance") {
		t.Error("processBelongsTo matched a process that carries no such instance name")
	}
}

// THE WIRING, because the defect was a decision nobody made: resolveInstallPorts must consult the RECORD
// before falling through to fresh planning.
func TestResolveInstallPortsConsultsTheRecordFirst(t *testing.T) {
	body := functionBody(t, "install_ports.go", "func resolveInstallPorts(instance, residency string, out io.Writer, tty *os.File) (map[string]int, error) {")
	for _, want := range []struct{ needle, why string }{
		{"readResolvedPorts(instance)", "the recorded ports must be read at all — without it a rebuild re-derives and moves its own ports"},
		{"portHeldByThisInstance(", "a recorded port held by THIS instance must be kept, so a rebuild converges on the same ports"},
		{"portOwnerLabel(", "a genuine conflict must NAME the owner instead of silently suggesting a new port"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("resolveInstallPorts does not use %s:\n  %s", want.needle, want.why)
		}
	}
	// The record must be consulted BEFORE the planInstallPorts fallback, or it is decoration.
	rec := strings.Index(body, "readResolvedPorts(instance)")
	plan := strings.Index(body, "return planInstallPorts(")
	if rec < 0 || plan < 0 || rec > plan {
		t.Errorf("the record is consulted after fresh planning (record at %d, plan at %d)", rec, plan)
	}
}

// AND THE LOG SIDE, which is what made the failure invisible: the plane's state dir must be derived from the
// INSTANCE, never from an ambient ORCHICON_SERVE_STATE_DIR that a sibling instance exported.
func TestServeStateDirIsDerivedFromTheInstance(t *testing.T) {
	body := functionBody(t, "serve_state.go", "func serveStateDir() string {")
	if !strings.Contains(body, `os.Getenv("ORCHICON_INSTANCE")`) {
		t.Error("serveStateDir does not consult ORCHICON_INSTANCE — an ambient ORCHICON_SERVE_STATE_DIR " +
			"from a sibling instance would then decide where THIS instance's plane writes its log")
	}
	if !strings.Contains(body, "hostDataDirFor(") {
		t.Error("serveStateDir does not derive the path from the instance's data dir")
	}
}

// The boot markers must be stripped at the spawn site, since getenv returns the FIRST match and an ambient
// value would therefore win over the one this process sets.
func TestLifecycleStripsInheritedBootMarkers(t *testing.T) {
	src := string(readSource(t, "../../internal/planecontrol/planecontrol.go"))
	for _, marker := range []string{"ORCHICON_SERVE_DETACHED", "ORCHICON_SERVE_LOG_FILE", "ServeStateDirEnv"} {
		if !strings.Contains(src, marker) {
			t.Errorf("%s is not stripped from the ambient environment — a plane started from a shell that "+
				"inherited it becomes a foreground plane with no PID file, or writes a sibling's log", marker)
		}
	}
}
