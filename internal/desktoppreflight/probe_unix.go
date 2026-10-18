//go:build !windows

package desktoppreflight

import "os/exec"

// runCommand is the exec capability shared by every non-Windows probe.
func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
