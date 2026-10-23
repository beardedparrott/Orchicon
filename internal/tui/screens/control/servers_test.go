package control

// servers_test.go — Control → Servers: present only on a local host, and honest about state.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/instancectl"
	"github.com/beardedparrott/orchicon/internal/planebind"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// THE SOURCE IS ABSENT WHEN MANAGEMENT IS NOT AVAILABLE.
//
// `orch` is a REMOTE client by design. Offering install/remove against a plane on another machine would
// either fail per button or act on the wrong host, so the section must be absent — not present-and-broken.
func TestServersSourceIsAbsentUnlessEnabled(t *testing.T) {
	m := New(nil, nil) // no WithServers
	for _, src := range m.Base.Sources() {
		if src.Name == serversSourceName {
			t.Fatal("the servers source is present without WithServers(true) — management is a host " +
				"operation and this shell may be pointed at a plane on another machine")
		}
	}

	withServers := New(nil, nil, WithServers(true))
	found := false
	for _, src := range withServers.Base.Sources() {
		if src.Name == serversSourceName {
			found = true
		}
	}
	if !found {
		t.Fatal("WithServers(true) did not add the servers source")
	}
}

// A STALE SETUP MUST NEVER READ AS PROGRESS. This is the state that exists so a killed detached install
// cannot render as an eternal spinner.
func TestServerMetaSaysWhenASetupStoppedResponding(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   instancectl.Instance
		want string
	}{
		{"running", instancectl.Instance{Plane: planeAlive(8080, true)}, "running :8080"},
		{"stopped", instancectl.Instance{}, "stopped"},
		{"not answering", instancectl.Instance{Plane: planeAlive(8080, false)}, "not answering"},
		{"setting up", instancectl.Instance{Setup: &instancectl.SetupProgress{State: instancectl.SetupRunning}}, "setting up"},
		{"progress pct", instancectl.Instance{Setup: &instancectl.SetupProgress{State: instancectl.SetupRunning, Percent: 62}}, "setting up 62%"},
		{"failed", instancectl.Instance{Setup: &instancectl.SetupProgress{State: instancectl.SetupFailed}}, "setup FAILED"},
		{"stale", instancectl.Instance{Setup: &instancectl.SetupProgress{State: instancectl.SetupStale}}, "setup stopped responding"},
	} {
		if got := serverMeta(tc.in); got != tc.want {
			t.Errorf("%s: serverMeta = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A SETUP IN FLIGHT OWNS THE ROW: the verbs would fight a database being initialised, so only re-apply
// (which is the retry) is offered.
func TestServersActionsWhileASetupIsActive(t *testing.T) {
	// Point the contract at a temp home with one instance so Status is deterministic.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("ORCHICON_POSTGRES_PORT", "")
	if err := mkInstance(t, dir, "dev", instancectl.SetupProgress{State: instancectl.SetupRunning}); err != nil {
		t.Fatal(err)
	}

	m := New(nil, nil, WithServers(true))
	acts := m.serversActions(kitItem("dev"))
	if len(acts) == 0 {
		t.Fatal("no actions while a setup is active — the operator cannot retry")
	}
	for _, a := range acts {
		if a.Label != "re-apply" {
			t.Errorf("action %q is offered while a setup is in flight; only re-apply (the retry) is safe", a.Label)
		}
	}
}

// REMOVE ASKS TWO QUESTIONS, and the destroy one names what it destroys.
func TestServersRemoveOffersBothDataAnswers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("ORCHICON_POSTGRES_PORT", "")
	if err := mkInstance(t, dir, "dev", instancectl.SetupProgress{}); err != nil {
		t.Fatal(err)
	}
	m := New(nil, nil, WithServers(true))
	var keep, destroy bool
	for _, a := range m.serversActions(kitItem("dev")) {
		switch {
		case strings.Contains(a.Label, "keep data"):
			keep = a.Confirm != ""
		case strings.Contains(a.Label, "DESTROY"):
			destroy = a.Confirm != "" && strings.Contains(a.Confirm, "secrets")
		}
	}
	if !keep {
		t.Error("there is no remove-and-keep-data action with a confirmation")
	}
	if !destroy {
		t.Error("the destroy-data action is missing, or its confirmation does not say that the secrets " +
			"key goes with it — deleting it makes the secrets permanently undecryptable")
	}
}

// --- helpers -------------------------------------------------------------

func planeAlive(port int, healthy bool) planebind.StatusDTO {
	return planebind.StatusDTO{ControlPort: port, PID: 1234, Healthy: healthy, Detail: "test"}
}

func kitItem(id string) kit2.Item { return kit2.Item{ID: id, Title: id} }

// mkInstance lays out one instance dir (and optionally a setup record) under home.
func mkInstance(t *testing.T, home, name string, setup instancectl.SetupProgress) error {
	t.Helper()
	dir := filepath.Join(home, ".local", "share", "orchicon-"+name)
	if err := os.MkdirAll(filepath.Join(dir, "serve"), 0o755); err != nil {
		return err
	}
	if setup.State != "" {
		return instancectl.WriteSetupProgress(name, setup)
	}
	return nil
}
