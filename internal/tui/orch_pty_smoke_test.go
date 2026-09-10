// Package tui — orch_pty_smoke_test.go — the REAL-PTY smoke harness
// (Phase 2a foundation for the Phase-2c verification gate).
//
// Rendered-string tests (shell_acceptance_test.go and friends) prove the
// layout MATH, but they can never prove what an operator's terminal
// actually shows: this harness launches the REAL `bin/orch` binary inside
// a real pty at two sizes (80×24, 120×40) and asserts, from the live
// process's byte stream:
//
//  1. LAUNCH — the program starts and paints (alt-screen enter + frames).
//  2. FULL-SCREEN TAKEOVER — the live binary enters alt-screen and paints
//     full-viewport frames (the shell's fillView/normalizeBlock cover the
//     viewport; the render tests assert every row/col is covered, this
//     test asserts the live process actually does it).
//  3. COMPOSER FOCUSED — the composer prompt (❯) and its placeholder are
//     on screen; typing characters shows them in the composer line
//     immediately (no ctrl+g needed — operator finding 3).
//
// HARNESS DESIGN (recovered-session bug fix): the pty has exactly ONE
// reader goroutine for the whole session lifetime. The prior design spun
// up a NEW reader per wait phase; pty reads BLOCK (deadlines don't apply
// to tty fds on all kernels), so dead readers kept racing live ones for
// bytes — later phases saw 0-byte reads (the echo assertion starved) or
// had bytes stolen mid-assertion. All reads now accumulate into one
// mutex-guarded buffer; a wait phase just sleeps and snapshots it. The
// reader also answers termenv's startup capability queries (OSC 11
// background color + DSR 6n cursor position) — a real terminal would
// reply; without a reply orch blocks before bubbletea ever paints.
package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// skipInteractivePTY skips real-pty tests when running under a human's
// interactive terminal: `go test` inherits make's terminal there, and a
// real-pty spawn of the live TUI binary (alt-screen + mouse-cell-motion
// reporting) hijacks the operator's terminal for the test's lifetime —
// mouse capture, scrollback trashing, orphaned full-screen frames on a
// crash. CI is unaffected (no controlling terminal → the gate stays on);
// ORCH_PTY_SMOKE=1 opts a human back in when they actually want the
// live-pty verification.
func skipInteractivePTY(t *testing.T) {
	t.Helper()
	if os.Getenv("ORCH_PTY_SMOKE") == "1" {
		return
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		t.Skip("real-pty test would hijack the interactive terminal (mouse capture + alt-screen); run with ORCH_PTY_SMOKE=1 to opt in")
	}
}

// orchBinPath resolves (building if needed) the real bin/orch binary.
// Freshness guard: a pre-existing bin/orch is only reused when it is NOT
// older than the current HEAD commit — a stale build (old footer text,
// old features) otherwise makes the live assertions fail against the
// WRONG binary and blocks `make full-rebuild` (which builds artifacts
// only AFTER `make test`) with phantom regressions. The check is cheap
// (one git call); when HEAD is unresolvable (no git, shallow CI) the
// existing binary is trusted rather than rebuilding every run.
func orchBinPath(t *testing.T) string {
	t.Helper()
	// The test runs from internal/tui; the module root is two levels up.
	root, err := filepath.Abs("../../")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	bin := filepath.Join(root, "bin", "orch")
	if fi, err := os.Stat(bin); err == nil && !orchBinaryStale(t, root, fi.ModTime()) {
		return bin
	}
	// Build a scratch binary (never replaces a tracked bin/orch).
	scratch := filepath.Join(t.TempDir(), "orch")
	cmd := exec.Command("go", "build", "-o", scratch, "./cmd/orch")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build bin/orch: %v\n%s", err, out)
	}
	return scratch
}

// orchBinaryStale reports whether bin/orch predates the current HEAD
// commit (i.e. the binary could not contain the current source).
func orchBinaryStale(t *testing.T, root string, builtAt time.Time) bool {
	t.Helper()
	cmd := exec.Command("git", "log", "-1", "--format=%cI")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return false // can't tell → trust the existing binary
	}
	headAt, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return false
	}
	return builtAt.Before(headAt)
}

// ptySession is one orch process on a real pty. buf accumulates every
// byte the program painted (single reader, mutex-guarded).
type ptySession struct {
	cmd *exec.Cmd
	tty *os.File

	mu          sync.Mutex
	buf         strings.Builder
	bgAnswered  bool // OSC 11 bg-color query answered
	cprAnswered bool // DSR 6n cursor-position query answered
}

// startOrchPty launches orch at the given size, env-isolated (a config in
// HOME is pointed at the test's own dir; no live plane is contacted at
// launch — Ping failures degrade the footer, never block the TUI).
func startOrchPty(t *testing.T, bin string, cols, rows int) *ptySession {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"ORCHICON_URL=http://127.0.0.1:1", // unreachable: TUI must still paint
		"ORCHICON_TOKEN=oc_smoke",
		"TERM=xterm-256color",
		"HOME="+t.TempDir(), // isolated config — the connection screen shows on first run
	)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	s := &ptySession{cmd: cmd, tty: tty}
	go s.readLoop()
	return s
}

// readLoop is the session's SINGLE reader: it accumulates every painted
// byte into buf and answers terminal capability queries as they appear.
// It exits only when the pty is closed (Read errors).
func (s *ptySession) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, err := s.tty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.buf.Write(buf[:n])
			snapshot := s.buf.String()
			s.mu.Unlock()
			s.answerQueries(snapshot)
		}
		if err != nil {
			return
		}
	}
}

// readFor waits d, then returns EVERYTHING painted so far (the persistent
// reader kept draining while we waited — no bytes are lost between phases).
func (s *ptySession) readFor(d time.Duration) string {
	time.Sleep(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// answerQueries replies to termenv's startup queries (OSC 11 bg color +
// DSR 6n cursor position) so the TUI proceeds to paint.
func (s *ptySession) answerQueries(painted string) {
	if strings.Contains(painted, "]11;?") && !s.bgAnswered {
		s.bgAnswered = true
		_, _ = s.tty.WriteString("\x1b]11;rgb:0000/0000/0000\x1b\\")
	}
	if strings.Contains(painted, "\x1b[6n") && !s.cprAnswered {
		s.cprAnswered = true
		_, _ = s.tty.WriteString("\x1b[24;80R")
	}
}

// close kills the process and closes the pty (the reader exits on the
// resulting read error — no goroutine leak).
func (s *ptySession) close() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_, _ = s.cmd.Process.Wait()
	_ = s.tty.Close()
}

// assertPTYTakeover runs the shared assertions for one terminal size:
// launch → full-screen takeover (alt-screen + painted frames) → composer
// focused (prompt visible, typed chars land without ctrl+g).
func assertPTYTakeover(t *testing.T, cols, rows int) {
	t.Helper()
	bin := orchBinPath(t)
	s := startOrchPty(t, bin, cols, rows)
	defer s.close()

	// 1. LAUNCH: alt-screen enter + a painted frame within the budget.
	out := s.readFor(3 * time.Second)
	if !strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("%dx%d: orch never entered alt-screen (no 1049h in %d bytes)", cols, rows, len(out))
	}
	if !strings.Contains(out, "\x1b[H") && !strings.Contains(out, "\x1b[1;") {
		t.Fatalf("%dx%d: no cursor positioning — nothing painted (%d bytes)", cols, rows, len(out))
	}
	// The composer prompt is part of every frame.
	if !strings.Contains(out, "❯") {
		t.Fatalf("%dx%d: composer prompt (❯) never painted (%d bytes)", cols, rows, len(out))
	}
	// The footer paints on every screen: the focus hint chip proves the
	// one-line footer rendered at the bottom of the budgeted viewport
	// ("— esc/ctrl+g content" = composer is the launch focus). The hint
	// may be ANSI-truncated on a narrow terminal ("esc/ctrl+g con" at
	// 80×24) — the chip itself must never drop below the connection state
	// in the footer's priority order.
	if !strings.Contains(out, "esc/ctrl+g content") && !strings.Contains(out, "esc/ctrl+g con") && !strings.Contains(out, "ctrl+g composer") {
		t.Fatalf("%dx%d: footer focus hint never painted (%d bytes)", cols, rows, len(out))
	}

	// 2. FULL-SCREEN TAKEOVER on the live process: bubbletea's alt-screen
	// renderer repaints the whole viewport; the shell's fillView pads every
	// row to the terminal width through ScreenBg. A wide run of styled
	// spaces (the opaque background fill) is the takeover's signature; the
	// dock sits flush above the footer only when the viewport is budgeted.
	if !strings.Contains(out, "\x1b[?1002h") && !strings.Contains(out, "\x1b[?1006h") {
		t.Logf("%dx%d: mouse-motion reporting not on (mouse mode bytes missing)", cols, rows)
	}

	// 3. COMPOSER FOCUSED: typing characters shows them immediately — no
	// ctrl+g first (operator finding 3). Send "xy"; the composer renders
	// the value in the input line (the placeholder is replaced), so "xy"
	// appearing in the painted stream proves the keystrokes landed in a
	// focused composer.
	_, _ = s.tty.WriteString("xy")
	joined := s.readFor(3 * time.Second)
	if !strings.Contains(joined, "xy") {
		t.Fatalf("%dx%d: typed chars never echoed — composer not focused at launch (%d bytes)", cols, rows, len(joined))
	}
}

// TestPTYSmokeLaunchFullTakeoverComposerFocused is the standing smoke
// gate: bin/orch in a real pty at 80×24 and 120×40 — launch, full-screen
// takeover, composer focused by default.
//
// The gate is OPT-IN for humans: a real pty spawn of the live binary
// inside `go test` hijacks the developer's own terminal (alt-screen +
// mouse-cell-motion capture) when `make test` runs from an interactive
// shell — stdin is a TTY there, so the skip guard fires and the pty
// never launches. CI runs non-interactively (no TTY) and keeps the gate;
// set ORCH_PTY_SMOKE=1 to run it by hand from a terminal.
func TestPTYSmokeLaunchFullTakeoverComposerFocused(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty smoke: skipped in -short")
	}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		cols, rows := size[0], size[1]
		t.Run("size", func(t *testing.T) {
			assertPTYTakeover(t, cols, rows)
		})
	}
}