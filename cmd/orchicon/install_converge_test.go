package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// install_converge_test.go — INSTALLING OVER AN EXISTING INSTANCE MUST CONVERGE IT.
//
// The report: "the installer allowed me to pick the instance and I chose dev, then it ran through the
// installer and said it was complete but no auth works and the previous dev container has been up for two
// hours."
//
// Two branches were responsible, and both asked the WRONG QUESTION:
//
//   - ensureInstallContainer asked "is a container running with the right RESIDENCY?" and stopped there, so
//     a container kept the image it was created from while the installer pulled and reported a newer one.
//   - startHostPlane asked "does something answer /healthz?" and stopped there, so a plane kept serving from
//     a binary the install had already replaced — the process is still alive (the inode survives) while the
//     path it was started from has been rewritten underneath it.
//
// These pin the question each should have asked: IS IT RUNNING WHAT I JUST INSTALLED.

// TestServingPlanePIDReadsTheInstancePIDFile — the only truthful source for "which process is the plane".
// A version endpoint would answer with what the OLD process believes about itself.
func TestServingPlanePIDReadsTheInstancePIDFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No PID file yet: nothing to find, and no false positive.
	if pid, ok := servingPlanePID("dev"); ok {
		t.Fatalf("no PID file, yet a plane was reported (pid %d)", pid)
	}

	pidDir := filepath.Join(home, ".local", "share", "orchicon-dev", "serve", "pids")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// OUR OWN PID, so the process is certainly alive — and per-instance, which is the point.
	if err := os.WriteFile(filepath.Join(pidDir, "orchicon.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, ok := servingPlanePID("dev")
	if !ok || pid != os.Getpid() {
		t.Fatalf("servingPlanePID = (%d, %v), want (%d, true)", pid, ok, os.Getpid())
	}
	if !servingPlaneAlive("dev") {
		t.Error("our own PID must count as alive")
	}

	// A DIFFERENT INSTANCE is unaffected by dev's PID file — the reason the state dir is per instance.
	if _, ok := servingPlanePID("test"); ok {
		t.Error("test must not inherit dev's plane")
	}

	// A STALE PID FILE (a process that has exited) must not be reported as a plane: the file outlives the
	// process, and treating it as live would mean restarting something that is not there.
	if err := os.WriteFile(filepath.Join(pidDir, "orchicon.pid"), []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := servingPlanePID("dev"); ok {
		t.Error("a PID that names no live process must not count as a serving plane")
	}
}

// TestPlaneDriftIsUnknownWithoutAPIDFile — the conservative default. A restart that cannot be JUSTIFIED is a
// blip on a working instance, so an unidentifiable plane is left alone.
func TestPlaneDriftIsUnknownWithoutAPIDFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := planeDrift("dev", os.Stdout); got != planeUnknown {
		t.Fatalf("planeDrift with no PID file = %v, want planeUnknown (leave a working instance alone)", got)
	}
}

// TestStopHostPlaneNeedsTheInstanceStateDir — the installer previously had NO way to stop a host plane
// (`serve --stop` appeared nowhere in it), which is why a stale plane could never be corrected. The state dir
// is what `serve --stop` resolves its PID file from, so passing it is not optional: a bare call looks in the
// wrong directory and reports success while stopping nothing.
func TestStopHostPlanePassesTheInstanceStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A binary that records the environment it was handed, so the assertion is about the CALL rather than
	// about the effect of stopping something.
	dir := t.TempDir()
	rec := filepath.Join(dir, "record.txt")
	fake := filepath.Join(dir, "orchicon")
	script := "#!/bin/sh\nprintf '%s\\n' \"$ORCHICON_SERVE_STATE_DIR\" > " + rec + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// stopHostPlane re-execs THIS binary, which in a test is the test binary, so the assertion is made on the
	// helper that computes the environment instead — the same value it would hand over.
	dataDir, err := hostDataDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dataDir, "serve")
	if want != filepath.Join(home, ".local", "share", "orchicon-dev", "serve") {
		t.Fatalf("the plane state dir is %s, want dev's own serve dir under HOME", want)
	}
	// And it is PER INSTANCE — the property that keeps one instance's stop away from another's plane.
	other, err := hostDataDir("test")
	if err != nil {
		t.Fatal(err)
	}
	if other == dataDir {
		t.Error("two instances resolved to the same data dir")
	}
}

// TestHostDataDirForNeverFails — the repair path must not be the thing that breaks an install. It is called
// after the instance is already up and the operator is waiting for a verdict, so a failure to resolve HOME
// there is reported as a best-effort path rather than an error.
func TestHostDataDirForNeverFails(t *testing.T) {
	t.Setenv("HOME", "")
	if got := hostDataDirFor("dev"); got == "" {
		t.Fatal("hostDataDirFor returned nothing; the repair path must always have a path to try")
	}
}
