package opencode

// servehost_supervision_test.go — the four supervision defects, each pinned by the symptom it caused.
//
// The host opencode serve is the process every opencode turn runs through, so a defect in its supervision
// is not a cosmetic one: it shows up as turns that fail with "connection refused", a serve that is dead
// while the plane believes it is up, and defunct processes in `ps`.

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func supervisionLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls cond for up to d, so a test never depends on a sleep being long enough.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// DEFECT 2: a nil client used to read as a HEALTHY serve.
//
// The condition was `client == nil || client.Healthy(ctx)`, so a watchdog that outlived a Stop() — which
// nils the client — continued forever, reporting nothing, while no serve existed at all. Nil means there
// is nothing to answer with, which is exactly when a restart is due.
func TestNilClientIsNotAHealthyServe(t *testing.T) {
	if serveAnswering(context.Background(), nil) {
		t.Fatal("a nil client reported as answering — the watchdog would spin forever over a dead serve " +
			"instead of restarting it")
	}
}

// DEFECTS 1 + 4: the exit is recorded, and the process is reaped.
//
// A serve that exits on its own used to leave `started`/`client` set, so ready() kept answering true and
// EnsureStarted short-circuited ("already up") — the dispatch then failed with "connection refused"
// rather than restarting. And nothing Wait()ed it, so it lingered as a defunct process until the
// watchdog's next 15s poll happened to notice.
//
// This drives the REAL path (reapAsync + markExited) against a real child process.
func TestASelfExitedServeIsReapedAndReadsAsDown(t *testing.T) {
	h := NewHostServe(supervisionLogger(), t.TempDir(), t.TempDir())

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process here: %v", err)
	}

	h.mu.Lock()
	h.cmd = cmd
	h.client = NewSessionClient("http://127.0.0.1:1", "pw", "")
	h.started = true
	h.port = 4242
	h.mu.Unlock()

	if !h.ready() {
		t.Fatal("fixture: the serve must read as ready BEFORE it exits, or this measures nothing")
	}

	h.reapAsync(cmd)

	waitFor(t, 5*time.Second, "the exited serve to read as DOWN", func() bool { return !h.ready() })

	h.mu.Lock()
	gotCmd, gotClient, gotStarted, gotPort := h.cmd, h.client, h.started, h.port
	h.mu.Unlock()
	if gotCmd != nil || gotClient != nil || gotStarted || gotPort != 0 {
		t.Fatalf("exit did not clear the state ready() reads: cmd=%v client=%v started=%v port=%d",
			gotCmd, gotClient, gotStarted, gotPort)
	}
	// REAPED, not merely observed: Wait() ran, so the process is not a zombie.
	if cmd.ProcessState == nil {
		t.Error("the child was never waited for — it would show as <defunct> in ps")
	}
}

// markExited must not clear the state of a serve that REPLACED the one that exited.
//
// A restart kills the old process and starts a new one; the old process's reaper then fires with a stale
// cmd. Clearing unconditionally would null out the live serve's fields — after which ready() would report
// a running serve as absent and EnsureStarted would stack a second process on the same port.
func TestASupersededProcessCannotClearItsReplacement(t *testing.T) {
	h := NewHostServe(supervisionLogger(), t.TempDir(), t.TempDir())

	stale := exec.Command("/bin/sh", "-c", "exit 0")
	current := exec.Command("/bin/sh", "-c", "exit 0")

	h.mu.Lock()
	h.cmd = current // the serve that is running NOW
	h.client = NewSessionClient("http://127.0.0.1:1", "pw", "")
	h.started = true
	h.port = 4243
	h.mu.Unlock()

	h.markExited(stale, nil) // the REPLACED process's reaper firing late

	if !h.ready() {
		t.Fatal("a superseded process's exit cleared the LIVE serve's state — EnsureStarted would then " +
			"start a second serve on the same port")
	}
	h.mu.Lock()
	port := h.port
	h.mu.Unlock()
	if port != 4243 {
		t.Errorf("port = %d, want the live serve's 4243", port)
	}
}

// DEFECT 3: supervision was armed by exactly ONE caller (EnsureStarted), and arming twice started a second
// watchdog whose handle overwrote the first — leaking it, with both then racing to restart one serve.
//
// The invariant is "exactly one watchdog", and it must survive repeated arming and survive Stop() so a later
// Start can re-arm.
func TestArmSupervisionStartsExactlyOneWatchdog(t *testing.T) {
	var live int32
	watchLiveHook = func(delta int) { atomic.AddInt32(&live, int32(delta)) }
	t.Cleanup(func() { watchLiveHook = nil })

	h := NewHostServe(supervisionLogger(), t.TempDir(), t.TempDir())

	h.armSupervision()
	waitFor(t, 2*time.Second, "the first watchdog", func() bool { return atomic.LoadInt32(&live) == 1 })

	// Repeated arming must be REFUSED, not replace the running watchdog: replacing it would let the
	// superseded one call Stop() on its way out and tear down the live serve.
	h.armSupervision()
	h.armSupervision()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&live); got != 1 {
		t.Fatalf("live watchdogs = %d after repeated arming, want exactly 1", got)
	}

	// Stop releases it, so a later Start can arm a replacement.
	h.Stop()
	waitFor(t, 2*time.Second, "the watchdog to exit after Stop", func() bool { return atomic.LoadInt32(&live) == 0 })

	h.armSupervision()
	waitFor(t, 2*time.Second, "supervision to re-arm after Stop", func() bool { return atomic.LoadInt32(&live) == 1 })
	h.Stop()
	waitFor(t, 2*time.Second, "the second watchdog to exit", func() bool { return atomic.LoadInt32(&live) == 0 })
}
