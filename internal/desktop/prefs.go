package desktop

// prefs.go — WHAT THE APP REMEMBERS BETWEEN RUNS.
//
// Deliberately one field, and deliberately a separate file from window.json: a
// corrupt preference must never cost the operator their window geometry, and
// vice versa. Both load best-effort — losing a preference must never stop the app
// opening.
//
// THIS IS NOT A CACHE OF INSTANCE STATE. It records only which instance the
// person last CHOSE. It is never used to decide whether that instance is running
// (internal/planebind answers that, live), so a stale file can point the window at
// a stopped plane — which the Manage Server page then shows, correctly, as
// offline.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// DefaultInstance is where the app points when nothing else says otherwise. Dev,
// because that is the instance a developer is working against, and because it is
// the one the tree can rebuild.
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

// LoadChosenInstance returns the remembered instance, or "" when there is no
// usable record. A corrupt file is NOT an error: the caller falls back to the
// default, because a bad preference must not be fatal.
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

// NewControlToken returns a per-run credential for the shell's plane-control
// endpoints. It is injected into the SPA only (see spa.go) and is never written
// to disk, so a token from a previous run is worthless.
func NewControlToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A token we cannot generate is worse than none only if we pretend it is
		// secret. Returning "" makes the endpoints reply 404/403 rather than
		// accepting an empty header, which is the safe direction.
		return ""
	}
	return hex.EncodeToString(b[:])
}
