//go:build !windows

package procwin

import "os/exec"

// applyNoWindow is a NO-OP on Unix, and that is not laziness: a Unix process has no console to
// allocate, so there is nothing to suppress and nothing for a caller to opt out of.
func applyNoWindow(*exec.Cmd) {}
