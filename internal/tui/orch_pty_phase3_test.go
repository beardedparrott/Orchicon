// Package tui — orch_pty_phase3_test.go — the Phase-3 REAL-PTY acceptance
// gate. Rendered-string tests (phase3_test.go) prove the layout MATH; this
// harness proves what an operator's terminal actually does: it drives the
// REAL bin/orch binary inside a real pty and asserts, from the live byte
// stream, the five behaviors the Phase-3 ACs demand — full coverage with no
// alt-screen teardown, palette typing visible in the bottom bar, the tab
// submenu opening by KEY and by MOUSE, and the /connect overlay acting as a
// real modal whose fields can be focused and typed into (masked echo).
//
// The gate is OPT-IN for humans (skipInteractivePTY): a real pty spawn of
// the live binary hijacks an interactive terminal. CI runs it.
package tui

import (
	"strings"
	"testing"
	"time"
)

// TestPTYPhase3PaletteSubmenuConnectGate drives the live binary through the
// Phase-3 interaction set and asserts each behavior from the painted stream.
func TestPTYPhase3PaletteSubmenuConnectGate(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty phase-3 gate: skipped in -short")
	}
	bin := orchBinPath(t)
	s := startOrchPty(t, bin, 120, 40)
	defer s.close()

	out := s.readFor(3 * time.Second)
	if !strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("orch never entered alt-screen (%d bytes)", len(out))
	}
	if !strings.Contains(out, "❯") {
		t.Fatalf("composer prompt never painted (%d bytes)", len(out))
	}

	// 1. PALETTE INPUT DISCIPLINE: typing "/pro" must show the operator's
	// text in the bottom bar while the palette above filters on it.
	typeHuman(s, "/pro")
	out = s.readFor(2 * time.Second)
	plain := stripANSI(out)
	if !strings.Contains(plain, "/pro") {
		t.Fatalf("palette typing not visible in the composer bar (%d bytes)", len(out))
	}
	if !strings.Contains(plain, "command palette") {
		t.Fatal("slash palette never painted above the composer")
	}

	// esc closes the palette (text preserved), backspace clears the bar.
	_, _ = s.tty.WriteString("\x1b")
	_, _ = s.tty.WriteString("\x7f\x7f\x7f\x7f")
	_ = s.readFor(time.Second)

	// 2. SUBMENU BY KEY: Enter on the active tab with an empty composer opens
	// its dropdown (the panel header carries "▾").
	beforeMenu := len(s.readFor(0))
	_, _ = s.tty.WriteString("\r")
	out = s.readFor(2 * time.Second)
	if !strings.Contains(stripANSI(out[beforeMenu:]), "▾") {
		t.Fatal("submenu did not open on Enter with an empty composer")
	}
	// down + enter selects an entry and closes the dropdown.
	_, _ = s.tty.WriteString("\x1b[B\r")
	_ = s.readFor(time.Second)
	// esc (menu already closed) is harmless.
	_, _ = s.tty.WriteString("\x1b")
	_ = s.readFor(500 * time.Millisecond)

	// 3. CONNECT OVERLAY IS A REAL MODAL: /connect opens it in place (no
	// alt-screen teardown) and its fields can be focused + typed into.
	typeHuman(s, "/connect")
	_, _ = s.tty.WriteString("\r")
	out = s.readFor(3 * time.Second)
	plain = stripANSI(out)
	if !strings.Contains(plain, "Connect to an Orchicon instance") {
		t.Fatalf("in-place /connect overlay never painted (%d bytes)", len(out))
	}
	// Field 1 (URL) is focused at open: typing lands there.
	typeHuman(s, "abc")
	out = s.readFor(1500 * time.Millisecond)
	if !strings.Contains(stripANSI(out), "abc") {
		t.Fatal("typing into the connect overlay's focused field did not echo")
	}
	// Tab moves to the credential field: typing there echoes MASKED (•).
	_, _ = s.tty.WriteString("\t")
	_ = s.readFor(500 * time.Millisecond)
	_, _ = s.tty.WriteString("Z")
	out = s.readFor(1500 * time.Millisecond)
	plain = stripANSI(out)
	if !strings.Contains(plain, "•") {
		t.Fatal("tab did not move focus to the masked credential field (no masked echo)")
	}
	// The masked echo is BOUNDED (explicit textinput width): never a
	// full-width dotted line.
	if run := maxBulletRun(plain); run > 45 {
		t.Fatalf("masked echo painted a %d-cell run of • (want the ~40-cell field width)", run)
	}
	// The overlay never tore down alt-screen (in-place overlay discipline).
	if strings.Contains(out, "\x1b[?1049l") {
		t.Fatal("the connect overlay exited alt-screen (must stay in place)")
	}
	// esc cancels back to the shell without quitting.
	_, _ = s.tty.WriteString("\x1b")
	out = s.readFor(2 * time.Second)
	if strings.Contains(out, "\x1b[?1049l") {
		t.Fatal("esc on the connect overlay exited alt-screen (must cancel in place)")
	}
	if !strings.Contains(stripANSI(out), "❯") {
		t.Fatal("shell composer never repainted after cancelling the connect overlay")
	}
}

// TestPTYPhase3SubmenuMouseGate proves the submenu's MOUSE path against the
// live binary: a real SGR click on the tab bar opens that tab's dropdown
// (the panel header "▾" is painted).
func TestPTYPhase3SubmenuMouseGate(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty phase-3 mouse gate: skipped in -short")
	}
	bin := orchBinPath(t)
	s := startOrchPty(t, bin, 120, 40)
	defer s.close()

	out := s.readFor(3 * time.Second)
	if !strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("orch never entered alt-screen (%d bytes)", len(out))
	}
	// A click inside the centered tab bar (row 0) hits a tab and opens its
	// submenu.
	mark := len(s.readFor(0))
	s.sendMouse(0, 60, 1, false) // SGR rows/cols are 1-based; row 1 = tab bar
	s.sendMouse(0, 60, 1, true)
	out = s.readFor(2 * time.Second)
	if !strings.Contains(stripANSI(out[mark:]), "▾") {
		t.Fatalf("mouse click on the tab bar did not open a submenu (%d bytes)", len(out))
	}
}

// typeHuman writes s one byte at a time with a short pause between keystrokes:
// bubbletea coalesces bytes that arrive in ONE read into a single multi-rune
// KeyMsg (k.String() == "pro"), which never matches the shell's single-key
// routes ("/"). A real operator types one key at a time — the harness must too.
func typeHuman(s *ptySession, text string) {
	for _, r := range text {
		_, _ = s.tty.WriteString(string(r))
		time.Sleep(40 * time.Millisecond)
	}
}

// maxBulletRun returns the longest run of masked-echo characters in s.
func maxBulletRun(s string) int {
	best, cur := 0, 0
	for _, r := range s {
		if r == '•' {
			cur++
			if cur > best {
				best = cur
			}
			continue
		}
		cur = 0
	}
	return best
}
