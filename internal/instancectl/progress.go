package instancectl

// progress.go — the machine-readable SETUP PROGRESS record (design of record, D4).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// Setup states. A closed set, because a GUI branching on free-form strings is how one client stops
// rendering a case the other handles.
const (
	// SetupRunning means a setup process is working.
	SetupRunning = "running"
	// SetupDone means it finished cleanly. The record is kept (not deleted) so the app can show
	// "setup complete" once after a relaunch instead of an empty page.
	SetupDone = "done"
	// SetupFailed means it finished with an error; Error says what.
	SetupFailed = "failed"
	// SetupCancelled means an operator stopped it.
	SetupCancelled = "cancelled"
	// SetupStale is COMPUTED, never written: a "running" record whose process is gone, or whose
	// updated_at has not moved.
	SetupStale = "stale"
)

// SetupProgress is one instance's setup, as the GUI renders it.
//
// EVERY FIELD IS THERE BECAUSE ITS ABSENCE PRODUCED A BUG WE ALREADY PAID FOR:
//
//	State       without it, a failure looke like a slow download
//	Phase/Label without them, the bar moved with no indication of WHAT was happening
//	Percent     without it, only an indeterminate spinner was possible
//	PID         "is setup actually running?" answered by the PROCESS TABLE, not by the file's age
//	UpdatedAt   the staleness signal (see setupProgressStaleAfter)
//	Error       a failed setup must say why, not just that it failed
type SetupProgress struct {
	Instance  string `json:"instance"`
	State     string `json:"state"`
	Phase     string `json:"phase"`
	Label     string `json:"label"`
	Percent   int    `json:"percent"`
	Detail    string `json:"detail"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`
	PID       int    `json:"pid"`
	Error     string `json:"error"`

	// Stale is computed on read (the JSON field above is only what was written). A caller that wants to
	// render "setup stopped responding" asks EffectiveState, not State.
	Stale bool `json:"-"`
}

// SetupProgressPath is where one instance's setup record lives: beside its ports.json.
func SetupProgressPath(name string) string {
	return filepath.Join(planecontrol.HostDataDirFor(name), "setup-progress.json")
}

// ReadSetupProgress reads an instance's record.
func ReadSetupProgress(name string) (*SetupProgress, error) {
	b, err := os.ReadFile(SetupProgressPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p SetupProgress
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("the setup record at %s is unreadable: %w", SetupProgressPath(name), err)
	}
	p.Stale = progressIsStale(&p)
	return &p, nil
}

// EffectiveState returns the state to RENDER: the written state, unless the record claims to be
// running while nothing is.
func (p *SetupProgress) EffectiveState() string {
	if p == nil {
		return ""
	}
	if p.State == SetupRunning && (p.Stale || !pidAlive(p.PID)) {
		return SetupStale
	}
	return p.State
}

// Finished reports whether the setup reached a terminal state (by its effective state).
func (p *SetupProgress) Finished() bool {
	switch p.EffectiveState() {
	case SetupDone, SetupFailed, SetupCancelled, SetupStale:
		return true
	}
	return false
}

// progressIsStale applies the timestamp rule only. The PID test lives in EffectiveState so that a record
// read back by a test (with no real process) is still judged on its timestamp alone.
func progressIsStale(p *SetupProgress) bool {
	if p.State != SetupRunning || p.UpdatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, p.UpdatedAt)
	if err != nil {
		return false // an unparseable timestamp is not evidence of staleness
	}
	return time.Since(t) > setupProgressStaleAfter
}

// pidAlive reports whether a PID is a running process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	return processAlive(pid)
}

// WriteSetupProgress writes a record atomically, so a reader never sees a half-written file.
func WriteSetupProgress(name string, p SetupProgress) error {
	p.Instance = name
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if p.StartedAt == "" {
		p.StartedAt = p.UpdatedAt
	}
	if p.State == "" {
		p.State = SetupRunning
	}
	path := SetupProgressPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// WRITE-THEN-RENAME. A reader polling every second would otherwise be able to read a truncated file
	// mid-write and conclude the record is corrupt, which is indistinguishable from a real failure.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ClearSetupProgress removes the record. Used before a fresh install so a finished, older record cannot
// be mistaken for this run's progress.
func ClearSetupProgress(name string) error {
	err := os.Remove(SetupProgressPath(name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
