//go:build !windows

package procwin

import "os/exec"

// applyNoWindow is a NO-OP on Unix, and that is not laziness: a Unix process has no console to allocate, so
// there is nothing to suppress and nothing for a caller to opt out of. The definition exists so the package
// has one API on every platform rather than a build-tagged call site in every caller.
func applyNoWindow(*exec.Cmd) {}
