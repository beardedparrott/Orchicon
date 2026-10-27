//go:build windows

package main

// input_windows.go — the program input on Windows.
//
// IT IS NOT stdin, AND THAT IS THE WHOLE POINT. bubbletea v1.3.10 chooses its input path on Windows by
// comparing the reader's file descriptor to stdin's (inputreader_windows.go): equal means the CONINPUT
// scanner (ReadConsoleInput, its own scancode-to-key table), different means the ANSI BYTE reader. Coninput
// is the path that cannot see Shift+Enter (it never reads Shift for Enter) and never enables bracketed paste
// (so a pasted block is a burst of ordinary Enter keys, each of which SENDS) — both reported on Windows from
// the same mechanism.
//
// internal/tui.NewWindowsInputReader opens CONIN$ as a SEPARATE handle, which selects the ANSI path, and
// applies the same byte rewrites the other platforms get — so Windows finally benefits from the Shift+Enter
// and SS3 Home/End translations that internal/tui/input has always performed. It falls back to decorated
// stdin when CONIN$ cannot be opened, so a redirected or unopenable console still starts (with the old
// decoding faults) rather than not starting at all.
//
// See internal/tui/reader_windows.go for the full reasoning and the resize trade-off it makes, and
// internal/tui/resize_probe.go for the chain that replaces the resize events coninput used to deliver.

import (
	"io"
	"os"

	"github.com/beardedparrott/orchicon/internal/tui"
	"github.com/beardedparrott/orchicon/internal/tui/input"
)

// programInput is a separate console handle, decorated with the byte rewrites.
func programInput() io.Reader {
	in := tui.NewWindowsInputReader(os.Stdin)
	if in == nil {
		return nil
	}
	// The reader already carries the rewrites; decorating it a second time would be a nested Read and a
	// second copy of the same translation. Only the FALLBACK (raw stdin) needs decorating here.
	if f, ok := in.(*os.File); ok && f == os.Stdin {
		return input.TranslateTerminalKeys(f)
	}
	return in
}
