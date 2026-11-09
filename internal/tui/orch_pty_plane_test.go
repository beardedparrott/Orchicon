package tui

// orch_pty_plane_test.go — the PTY verification gate for the PLANE LIFECYCLE command surface.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// planeGateInstance is an instance name that CANNOT exist: no <home>/.local/share/orchicon-<name>
// is ever created by an operator or by these tests.
const planeGateInstance = "ptygate"

// startOrchPtyPlaneGate launches orch against a disposable fixture plane with the instance pinned.
func startOrchPtyPlaneGate(t *testing.T, bin, planeURL, home string) *ptySession {
	t.Helper()
	env := append([]string{
		"ORCHICON_URL=" + planeURL,
		"ORCHICON_TOKEN=oc_pty_seed",
		"ORCHICON_INSTANCE=" + planeGateInstance,
		"HOME=" + home,
		"TERM=xterm-256color",
	}, os.Environ()...)
	cmd := exec.Command(bin)
	cmd.Env = env
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 240, Rows: 40})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	s := &ptySession{cmd: cmd, tty: tty}
	go s.readLoop()
	return s
}

// TestPTYPlaneStateCommand is the gate: the shell launches, the plane state is on screen with a
// remedy naming the fix, and `/plane` typed into the composer actually runs (the "probing the
// plane".
func TestPTYPlaneStateCommand(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	bin := orchBinPath(t)
	plane := connectPlaneFixture(t)
	home := t.TempDir()
	writeOrchConfig(t, home, plane.URL)
	s := startOrchPtyPlaneGate(t, bin, plane.URL, home)
	defer s.close()

	out := s.readFor(3 * time.Second)
	if !strings.Contains(out, "❯") {
		t.Fatalf("shell never painted the composer (%d bytes):\n%s", len(out), tailOf(out, 3000))
	}

	// PHASE 1 — the state indicator is on screen without any command: the first-layout probe runs once
	// the shell lays out.
	state := s.readFor(2 * time.Second)
	combined := out + state
	if !strings.Contains(combined, "plane") {
		t.Fatalf("the plane state indicator never painted:\n%s", tailOf(combined, 3000))
	}
	if !strings.Contains(combined, "plane-start") {
		t.Errorf("the down state must name /plane-start as the remedy:\n%s", tailOf(combined, 3000))
	}
	// The log path is part of the remedy.
	if !strings.Contains(combined, "serve/logs") {
		t.Errorf("the down state must name the plane log path:\n%s", tailOf(combined, 3000))
	}

	// PHASE 2 — THE COMMAND IS REACHABLE FROM REAL KEYSTROKES.
	_, _ = s.tty.WriteString("/plane\r")
	typed := s.readFor(3 * time.Second)
	if !strings.Contains(typed, "probing the plane") {
		t.Fatalf("/plane typed into the composer did not run (no probe notice):\n%s", tailOf(typed, 4000))
	}
	// The re-probe must repaint the same state + remedy: a command that reports nothing is the silent
	// no-op this surface exists to remove.
	after := s.readFor(2 * time.Second)
	if !strings.Contains(after, "/plane-start") {
		t.Errorf("the post-command frame lost the remedy:\n%s", tailOf(after, 3000))
	}
	// The process is still alive: no lifecycle command knocked the shell over.
	if s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited() {
		t.Fatalf("orch exited during the plane command surface test")
	}
}

// startOrchPtyPlaneGatePath is startOrchPtyPlaneGate with a PATH that puts a FAKE `orchicon` first, so the
// lifecycle verbs can be driven end to end without a real plane binary anywhere in reach.
func startOrchPtyPlaneGatePath(t *testing.T, bin, planeURL, home, shimDir string) *ptySession {
	t.Helper()
	// PATH IS REPLACED, NOT PREPENDED: os/exec keeps the LAST value of a duplicated key, so a prepended
	// copy would be discarded in favour of the inherited PATH and the fake `orchicon` would be invisible.
	pinned := map[string]bool{"ORCHICON_URL": true, "ORCHICON_TOKEN": true, "ORCHICON_INSTANCE": true,
		"HOME": true, "PATH": true, "TERM": true}
	env := []string{
		"ORCHICON_URL=" + planeURL,
		"ORCHICON_TOKEN=oc_pty_seed",
		"ORCHICON_INSTANCE=" + planeGateInstance,
		"HOME=" + home,
		"PATH=" + shimDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TERM=xterm-256color",
	}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && pinned[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.Command(bin)
	cmd.Env = env
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 240, Rows: 40})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	s := &ptySession{cmd: cmd, tty: tty}
	go s.readLoop()
	return s
}

// TestPTYPlaneStopReachesTheMechanism is acceptance criterion 1's SUCCESS half, driven from real
// keystrokes.
func TestPTYPlaneStopReachesTheMechanism(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	bin := orchBinPath(t)
	plane := connectPlaneFixture(t)
	home := t.TempDir()
	writeOrchConfig(t, home, plane.URL)

	shimDir := t.TempDir()
	argvLog := filepath.Join(shimDir, "argv.log")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argvLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(shimDir, "orchicon"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write the fake orchicon shim: %v", err)
	}

	s := startOrchPtyPlaneGatePath(t, bin, plane.URL, home, shimDir)
	defer s.close()
	if out := s.readFor(3 * time.Second); !strings.Contains(out, "❯") {
		t.Fatalf("shell never painted the composer (%d bytes):\n%s", len(out), tailOf(out, 3000))
	}

	_, _ = s.tty.WriteString("/plane-stop\r")
	out := s.readFor(5 * time.Second)
	// THE SUCCESS MARKER, not merely the "plane stop (instance …) …" progress notice planeCommand sets
	// before the verb runs — that one would pass even if the mechanism were never invoked.
	if !strings.Contains(out, "✓ plane stop") {
		t.Fatalf("/plane-stop reported nothing on screen (a silent no-op):\n%s", tailOf(out, 4000))
	}
	body, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("/plane-stop never reached the shared mechanism — the fake orchicon was not run: %v", err)
	}
	if !strings.Contains(string(body), "serve --stop") {
		t.Errorf("the mechanism was invoked with %q, want `serve --stop`", strings.TrimSpace(string(body)))
	}
}
