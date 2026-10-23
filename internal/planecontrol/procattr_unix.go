//go:build !windows

package planecontrol

// procattr_unix.go — there is no console to allocate on Unix, so this is a no-op by construction rather
// than by omission. The Windows half is where the behaviour lives.

import "os/exec"

func setProcAttrNoWindow(*exec.Cmd) {}
