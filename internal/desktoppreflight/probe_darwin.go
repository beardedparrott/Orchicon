//go:build darwin

package desktoppreflight

import (
	"os"
	"os/exec"
)

// osProbe on macOS has nothing to look for: WKWebView is part of the OS. The
// capability set is still populated so a future check (e.g. "is the app
// quarantined") has somewhere to live.
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
