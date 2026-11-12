//go:build !windows

package desktoppreflight

import (

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// runCommand is the exec capability shared by every non-Windows probe.
func runCommand(name string, args ...string) (string, error) {
	out, err := procwin.Command(name, args...).Output()
	return string(out), err
}
