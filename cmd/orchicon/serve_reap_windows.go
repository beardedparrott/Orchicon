//go:build windows

package main

import (
	"fmt"
	"os"
)

// discoverReapPoint is a NO-OP on Windows, and the build tag is what makes that true rather than a
// comment claiming it.
func discoverReapPoint() int { return 0 }

// killOrphans is a NO-OP on Windows — CONFIRMED INTENTIONAL, and now LOUD instead of silent.
func killOrphans() {
	fmt.Fprintf(os.Stderr, "warning: orphan sweep is inert on this platform: a CRASHED plane's opencode and "+
		"`orchicon mcp` children are left running\n")
	fmt.Fprintln(os.Stderr, "  why: no /proc and no SIGTERM — see cmd/orchicon/serve_reap_windows.go")
	fmt.Fprintln(os.Stderr, "  cleanup: stop the plane with `orchicon plane stop`, then look in Task Manager → "+
		"Details for `opencode` and `orchicon mcp` processes and end them from there")
}
