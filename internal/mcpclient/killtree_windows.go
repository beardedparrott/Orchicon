//go:build windows

package mcpclient

import "os/exec"

// killProcessTree terminates a stdio child on Windows.
//
// No process-group signal: cmd.Process.Kill already terminates the process. A child that forked further
// processes is not reaped as a tree here, which is a Windows limitation rather than a choice — the boot
// sweep (sweep_windows.go) is a no-op there too, so there is no group machinery to reuse.
func killProcessTree(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
