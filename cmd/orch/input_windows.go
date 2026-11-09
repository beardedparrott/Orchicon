//go:build windows

package main

// input_windows.go — the program input on Windows.

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
