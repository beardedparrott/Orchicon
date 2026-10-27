//go:build !windows

package main

// input_other.go — the program input for every platform but Windows.
//
// THE TERMINAL'S KEY VOCABULARY IS WIDER THAN THE LIBRARY'S, and the failure is SILENT: a sequence bubbletea
// cannot decode is not rejected, it is read as ordinary characters, so the symptom is litter typed into
// whatever field has focus. internal/tui/input rewrites the two spellings this codebase has met — Shift+Enter
// (which bubbletea splits into alt+O then M, making the composer type "OM") and the SS3 form of Home/End,
// which a laptop's fn+Left/Right emits and which bubbletea splits into alt+O then the letter, making the
// composer type "OH" and "OF".
//
// WINDOWS IS DIFFERENT and has its own file, because the decoration there must be applied to a different
// reader: bubbletea routes a Windows console to its coninput decoder rather than to the byte reader, which
// bypassed this decorator entirely. See cmd/orch/input_windows.go and internal/tui/reader_windows.go.

import (
	"io"
	"os"

	"github.com/beardedparrott/orchicon/internal/tui/input"
)

// programInput is stdin, decorated with the byte rewrites.
func programInput() io.Reader { return input.TranslateTerminalKeys(os.Stdin) }
