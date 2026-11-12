package desktop

// prefs.go — WHAT THE APP REMEMBERS BETWEEN RUNS.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// DefaultInstance is where the app points when nothing else says otherwise.
const DefaultInstance = "dev"

// PrefsPath is the remembered-choice file, beside the window geometry.
func PrefsPath() string {
	g := ConfigPath() // <config>/orchicon-desktop/window.json
	if g == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(g), "prefs.json")
}

type prefs struct {
	Instance string `json:"instance"`
}

// LoadChosenInstance returns the remembered instance, or "" when there is no usable record.
func LoadChosenInstance(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var p prefs
	if err := json.Unmarshal(b, &p); err != nil {
		return ""
	}
	return p.Instance
}

// SaveChosenInstance records the choice, best-effort: a read-only config dir must
// not stop the switch the operator just asked for.
func SaveChosenInstance(path, instance string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(prefs{Instance: instance})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// NewControlToken returns a per-run credential for the shell's plane-control endpoints.
func NewControlToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A token we cannot generate is worse than none only if we pretend it is secret.
		return ""
	}
	return hex.EncodeToString(b[:])
}
