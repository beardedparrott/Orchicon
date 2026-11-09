//go:build !windows

package main

// input_other.go — the program input for every platform but Windows.

import (
	"io"
	"os"

	"github.com/beardedparrott/orchicon/internal/tui/input"
)

// programInput is stdin, decorated with the byte rewrites.
func programInput() io.Reader { return input.TranslateTerminalKeys(os.Stdin) }
