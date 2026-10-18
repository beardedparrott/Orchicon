//go:build linux

package desktoppreflight

import (
	"os"
	"os/exec"
)

// osProbe is the real Linux probe: the dynamic loader's own view of WebKitGTK,
// with pkg-config as a second opinion (a dev machine can have the .pc files from
// the -dev package without a complete runtime, but the ldconfig hit is what counts
// in practice).
func osProbe() Probe {
	return Probe{
		Lookup: exec.LookPath,
		Run:    runCommand,
		Env:    os.Getenv,
		Stat: func(path string) (bool, error) {
			_, err := os.Stat(path)
			return err == nil, err
		},
	}
}
