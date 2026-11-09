//go:build windows

package mcpclient

import "os/exec"

// killProcessTree terminates a stdio child on Windows.
func killProcessTree(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
