//go:build windows

package tui

// reader_windows.go — make bubbletea READ THE CONSOLE instead of decoding it.
//
// THE FAULT, and it is one cause for two reported symptoms.
//
// bubbletea v1.3.10 takes two DIFFERENT input paths on Windows, and the choice is not configurable:
//
//	newInputReader (inputreader_windows.go):
//	    if f, ok := r.(term.File); !ok || f.Fd() != os.Stdin.Fd() {
//	        return fallback(r)            // ANSI path — reads bytes
//	    }
//	    ...                               // CONINPUT path — ReadConsoleInput + its own key decoding
//
// Coninput decodes a SCANCODE stream with its own table (key_windows.go). That table has two properties this
// composer cannot live with:
//
//   - VK_RETURN becomes KeyEnter with ONLY the Alt modifier read. Shift is never consulted for Enter, and a
//     Shift keypress is skipped outright (VirtualKeyCode == VK_SHIFT -> continue). So Shift+Enter arrives as
//     a plain Enter.
//   - ENABLE_BRACKETED_PASTE is never set on that path (newInputReader enables WINDOW/EXTENDED_FLAGS/mouse
//     and nothing else), so a paste is NOT a paste: it is a burst of ordinary key events, and every line
//     break in it is another VK_RETURN.
//
// The dock's documented contract is "enter send · alt+enter newline", so each of those Returns SENDS. That
// is the operator's "copy and paste into the composer on Windows seemed to send it as multiple lines on a
// big block of code pasted" — and the same missing Shift is their "shift+enter and alt+enter do not
// newline". ONE mechanism, two symptoms.
//
// IT ALSO MADE internal/tui/input INERT ON WINDOWS. That package rewrites the Shift+Enter byte sequence
// (ESC O M) and the SS3 Home/End spellings as bytes are READ. Because it embeds *os.File it satisfies
// term.File and keeps Stdin's own Fd, so it passes the assertion above and lands on the CONINPUT path —
// where no bytes are ever read, so the rewrites never run. Its own header reasons that "a plain io.Reader
// wrapper fails that assertion", which is true, and is exactly why embedding was the wrong shape on Windows:
// passing the assertion is what BYPASSES the decorator there.
//
// THE FIX: hand bubbletea a console handle that is NOT stdin's, so it takes the ANSI path. Everything the
// ANSI path needs is already done by bubbletea's own initInput (tty_windows.go), which runs FIRST and does
// term.MakeRaw plus ENABLE_VIRTUAL_TERMINAL_INPUT on whatever file it is given. With bytes on the wire:
//
//   - the renderer's ESC[?2004h bracketed-paste enable now has an input side, so a paste arrives as ONE
//     KeyMsg with Paste set — the dock already renders that verbatim (dock.go, "bracketed paste ... never
//     send"), so a pasted block stops sending itself;
//   - the ESC O M rewrite in internal/tui/input finally runs, so Shift+Enter newlines;
//   - the SS3 Home/End rewrite runs too, closing the "OH"/"OF" litter on Windows.
//
// AND THE REWRITES STAY ON, which is why the decorator is applied HERE rather than in cmd/orch. The Windows
// reader is a different file from os.Stdin, so the argument that made the decorator bypassable on Windows
// (its Fd equal to Stdin's, which selected coninput) no longer holds — wrapping it cannot change which
// bubbletea path is taken, because that was decided by the handle before the wrapper existed. So Windows
// gets bracketed paste AND the byte rewrites, and cmd/orch needs no platform branch at all.
//
// WHY A SECOND HANDLE AND NOT A WRAPPER AROUND STDIN. Wrapping stdin itself would also take the ANSI path,
// but it would FAIL the initInput assertion (the wrapper's Fd is still Stdin's, and that is the only thing
// initInput tests for a raw mode it then applies to the WRONG file), so raw mode would be applied to — or
// skipped on — the console in a way this cannot reason about. Opening a REAL console handle (CONIN$, the
// same file bubbletea's own openInputTTY uses) keeps MakeRaw, the mode flags and the eventual restore all
// applied to a genuine handle.
//
// WHAT IT TRADES AWAY: bubbletea's Windows resize detection is a console INPUT event, read by the reader
// this bypasses, and Windows has no SIGWINCH. The initial size still lands (bubbletea's own checkResize
// reads the OUTPUT file), so a section is left for the live case: resize_windows.go re-derives it on the
// shell's existing refresh tick.
//
// VERIFICATION STATUS, stated plainly because it matters. This builds and vets for GOOS=windows, and
// Linux/macOS are untouched (this file does not exist there). Its RUNTIME behaviour is Windows-console
// specific — whether the host's ConPTY delivers bracketed-paste markers on the VT input stream — and cannot
// be exercised from a Linux box. It is therefore its own commit, so it can be reverted on its own.

import (
	"io"
	"os"

	"github.com/beardedparrott/orchicon/internal/tui/input"
)

// NewWindowsInputReader returns the reader bubbletea should use on Windows.
//
// in is the process's stdin. The returned reader reads a SEPARATE console file, decorated with the byte
// rewrites of internal/tui/input. When CONIN$ cannot be opened — a redirected stdin, or a console this
// process cannot reopen — stdin is returned unchanged: a TUI that still works the old way (with the two
// decoding faults) is strictly better than one that cannot start at all.
func NewWindowsInputReader(in *os.File) io.Reader {
	if in == nil {
		return nil
	}
	conin, err := os.OpenFile("CONIN$", os.O_RDWR, 0o644) //nolint:gosec // fixed console name, not operator input
	if err != nil {
		return in
	}
	// A console that hands back the SAME handle as stdin would take the coninput path anyway (the check is
	// on the value), so there is nothing gained and a duplicated handle to leak. Give it back.
	if conin.Fd() == in.Fd() {
		_ = conin.Close()
		return in
	}
	return input.TranslateTerminalKeys(conin)
}
