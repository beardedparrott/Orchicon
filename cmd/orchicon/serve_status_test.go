package main

// serve_status_test.go — `serve --status` is the SAME surface as `orchicon plane status`.
//
// WHY THIS EXISTS: there used to be two status answers. `serve --status` read the PID file itself and
// printed "serve is running (PID n)", while `plane status` printed the instance, the residency, the
// serving binary, drift and health. Two surfaces that can disagree is the defect class this epic exists
// to close, and the fix is delegation — so what must never come back is a SECOND implementation inside
// serveStatus. The source pin below is what stops that, and the output pin is what proves the two
// spellings agree on every field they share.

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// captureStderr collects everything fn prints to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

// TestServeStatusDelegatesToTheSharedStatusSurface is a SOURCE-level pin, because the defect it guards
// is invisible in behaviour: a re-implemented status body can agree with `plane status` today and drift
// tomorrow. The body must call the shared surface and must read neither the PID file nor `procRunning`
// itself — those are the two ingredients of the second answer this task removed.
func TestServeStatusDelegatesToTheSharedStatusSurface(t *testing.T) {
	body := functionBody(t, "serve.go", "func serveStatus() int {")

	if !strings.Contains(body, "planecontrol.StatusOf(") {
		t.Fatalf("serveStatus no longer calls planecontrol.StatusOf — `serve --status` must report the same\n"+
			"surface as `orchicon plane status`, or the two can disagree:\n%s", body)
	}
	for _, reimplementation := range []string{"procRunning(", "servePIDFile("} {
		if strings.Contains(body, reimplementation) {
			t.Errorf("serveStatus calls %s again — that is the SECOND status implementation this change\n"+
				"removed (it printed a PID-only one-liner that could contradict `plane status`)", reimplementation)
		}
	}
}

// hermeticServeHome gives the test an instance of its own: both status surfaces derive every path from
// <home>/.local/share/orchicon-<instance>, so a temporary HOME is what keeps the developer's own planes
// out of the answer.
func hermeticServeHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCHICON_INSTANCE", "dev")
	if err := os.Unsetenv(serveStateDirEnv); err != nil {
		t.Fatal(err)
	}
	return "dev"
}

// TestServeStatusOutputMatchesPlaneStatus is AC5's pin: for ONE instance the two spellings cannot
// disagree — same instance, residency, PID, serving binary, drift and health — and the exit code is the
// machine-read contract `scripts/orchicon.sh` depends on (0 iff the PID file names a live process).
func TestServeStatusOutputMatchesPlaneStatus(t *testing.T) {
	instance := hermeticServeHome(t)

	// A LIVE PID FILE: this process. /proc/<pid> is what says a PID-file process is alive, so pointing it
	// at ourselves is the only way to assert the running arm without starting a plane.
	pidFile := planecontrol.ServePIDFile(instance)
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	var rc int
	out := captureStdout(t, func() { rc = serveStatus() })

	// THE OTHER SPELLING, DRIVEN THROUGH ITS REAL COMMAND ARM. Comparing against StatusOf twice would only
	// prove the library agrees with itself; going through runPlane is what pins that the two COMMANDS an
	// operator types cannot disagree.
	assertPlaneStatusAgrees(t, instance, out)

	// THE EXPECTED STRING IS COMPUTED WITH THE SAME WRITER THE COMMANDS USE, and getting that wrong is
	// what made this test fail on a developer's machine while passing in CI.
	//
	// Drift writes an EXPLANATION to the writer it is given when the live process is not the binary this
	// build would install ("plane runs from X, not Y — leaving it"). serveStatus passes os.Stdout, so that
	// line lands in the captured output. This test used to compute `want` with io.Discard, which SUPPRESSES
	// the line — so the two strings differed by exactly that sentence and the test failed.
	//
	// It only bit a host where the drift probe reaches the "different binary" branch, which needs a plane
	// binary at ~/.local/bin/orchicon (resolvePlaneBinary's fallback). CI has none, so the branch was never
	// taken there and the test passed; a developer who had installed the plane hit it every time. A test
	// that fails on the developer's machine and passes in CI is the least actionable kind, because the code
	// is fine and the environment is the difference.
	//
	// Same writer, same content: the comparison now asks the question the test is FOR (do the two commands
	// agree?) rather than accidentally asking whether the writer happened to be silent.
	var wantBuf strings.Builder
	want, err := planecontrol.StatusOf(planecontrol.Defaults(instance, &wantBuf))
	if err != nil {
		t.Fatalf("planecontrol.StatusOf: %v", err)
	}
	if want.PID != os.Getpid() {
		t.Fatalf("the PID file did not resolve to this process (got PID %d), so this test would prove "+
			"nothing about agreeing surfaces: %+v", want.PID, want)
	}
	// The drift explanation is part of what the command PRINTS, so it is part of what must agree.
	wantOut := wantBuf.String() + want.String() + "\n"
	if got := strings.TrimSpace(out); got != strings.TrimSpace(wantOut) {
		t.Errorf("serve --status and plane status disagree for instance %q.\n  serve --status: %s\n  plane status:   %s", instance, got, strings.TrimSpace(wantOut))
	}
	if rc != 0 {
		t.Errorf("a live PID file must exit 0 — `scripts/orchicon.sh` machine-reads it as \"the plane is "+
			"running\" — got %d", rc)
	}

	// THE OTHER ARM: nothing running is a STATE, not an error, and it must still exit non-zero so a
	// post-stop verification in the script can tell it from a live plane.
	if err := os.Remove(pidFile); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { rc = serveStatus() })

	stopped, err := planecontrol.StatusOf(planecontrol.Defaults(instance, io.Discard))
	if err != nil {
		t.Fatalf("planecontrol.StatusOf: %v", err)
	}
	if stopped.PID != 0 {
		t.Fatalf("expected no live plane after removing the PID file, got PID %d", stopped.PID)
	}
	if got := strings.TrimSpace(out); got != stopped.String() {
		t.Errorf("with nothing running the two surfaces still have to agree.\n  serve --status: %s\n  plane status:   %s", got, stopped.String())
	}
	// …and the same equality through `orchicon plane status` itself, in the not-running arm too.
	assertPlaneStatusAgrees(t, instance, out)
	if rc != 1 {
		t.Errorf("no live PID file must exit 1, got %d", rc)
	}
}

// assertPlaneStatusAgrees drives `orchicon plane status <instance>` and requires it to print exactly what
// `serve --status` printed for the same instance: one surface, two spellings, no room to disagree.
func assertPlaneStatusAgrees(t *testing.T, instance, serveOut string) {
	t.Helper()
	planeOut := captureStdout(t, func() {
		if err := runPlane([]string{"status", instance}); err != nil {
			t.Fatalf("plane status %s: %v", instance, err)
		}
	})
	if got, want := strings.TrimSpace(planeOut), strings.TrimSpace(serveOut); got != want {
		t.Errorf("`serve --status` and `plane status %s` print different things — they are supposed to be ONE\n"+
			"surface: two status answers that can disagree is the defect this delegation removed.\n"+
			"  serve --status:        %s\n  plane status %s: %s", instance, want, instance, got)
	}
}

// TestServeStartIsRefusedAndNamesTheReplacement pins AC1 at the surface a reader would actually try. An
// unrecognised `serve` flag falls through to serveForeground() — a BLOCKING foreground plane — so silence
// here is not neutral: it starts the wrong thing and looks like a hang.
func TestServeStartIsRefusedAndNamesTheReplacement(t *testing.T) {
	var rc int
	errOut := captureStderr(t, func() { rc = runServe([]string{"--start"}) })

	if rc != 2 {
		t.Fatalf("`serve --start` must be refused with exit 2 (a usage error, distinct from 1 = \"not "+
			"running\"), got %d", rc)
	}
	if !strings.Contains(errOut, "does not exist") {
		t.Errorf("the refusal must say the flag does not exist, so a reader stops looking for it:\n%s", errOut)
	}
	// The verb that replaced it, by name — a refusal that does not say where to go is only half an answer.
	for _, want := range []string{" start [instance]", "plane start [instance]"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal must point at %q:\n%s", want, errOut)
		}
	}
}
