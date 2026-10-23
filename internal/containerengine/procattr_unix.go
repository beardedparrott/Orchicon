//go:build !windows

package containerengine

import "os/exec"

// setProcAttrNoWindow is a NO-OP on Unix, and that is not laziness: a Unix process has no console to
// allocate, so there is nothing to suppress. It exists so quietCommand has one definition on every platform
// rather than a build-tagged call site.
func setProcAttrNoWindow(*exec.Cmd) {}
