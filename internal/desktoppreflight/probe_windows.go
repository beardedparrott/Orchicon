//go:build windows

package desktoppreflight

import (
	"os"
	"os/exec"

	"golang.org/x/sys/windows/registry"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// osProbe is the real Windows probe. WebView2 is best detected through the
// Evergreen registry keys, because that is what the runtime's own installer
// writes — a filesystem check alone misses a per-user install under HKCU.
func osProbe() Probe {
	return Probe{
		Lookup: exec.LookPath,
		Run: func(name string, args ...string) (string, error) {
			out, err := procwin.Command(name, args...).Output()
			return string(out), err
		},
		Env: os.Getenv,
		Stat: func(path string) (bool, error) {
			_, err := os.Stat(path)
			return err == nil, err
		},
		Registry: readRegistryString,
	}
}

// readRegistryString reads one string value, reporting false when the key or the
// value is absent (the normal case on a machine without WebView2, not an error).
func readRegistryString(root, path, value string) (string, bool) {
	var key registry.Key
	switch root {
	case "HKLM":
		key = registry.LOCAL_MACHINE
	case "HKCU":
		key = registry.CURRENT_USER
	default:
		return "", false
	}
	k, err := registry.OpenKey(key, path, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	s, _, err := k.GetStringValue(value)
	if err != nil {
		return "", false
	}
	return s, true
}
