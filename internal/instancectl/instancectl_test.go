package instancectl

// instancectl_test.go — the contract's own behaviour: what it lists, what it refuses, and what it records.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/planebind"
)

// withInstance lays out one instance dir under a temp home and returns the home.
func withInstance(t *testing.T, names ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Every port pinned to something free, so a status read on this machine cannot be influenced by a
	// REAL instance the developer happens to be running.
	for _, env := range []string{
		"ORCHICON_CONTROL_PORT", "ORCHICON_POSTGRES_PORT", "ORCHICON_NATS_PORT",
		"ORCHICON_NATS_MONITOR_PORT", "ORCHICON_TEMPO_PORT", "ORCHICON_LOKI_PORT",
		"ORCHICON_VM_PORT", "ORCHICON_OTLP_GRPC_PORT", "ORCHICON_OTLP_HTTP_PORT",
		"ORCHICON_OTEL_HEALTH_PORT",
	} {
		t.Setenv(env, "")
	}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(home, ".local", "share", "orchicon-"+n, "serve"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// LIST FINDS EVERY INSTANCE, INCLUDING ONE THE PORT TABLE WOULD NOT NAME.
func TestListFindsEveryInstanceOnTheMachine(t *testing.T) {
	withInstance(t, "dev", "prod", "mine")

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, i := range got {
		names = append(names, i.Name)
	}
	want := []string{"dev", "mine", "prod"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("List() = %v, want %v (sorted, and including the hand-created one)", names, want)
	}
	// `mine` is manageable but not one of the pre-booked three — the UI needs that distinction so it does
	// not claim a port default that does not exist for it.
	for _, i := range got {
		if i.Name == "mine" && i.Known {
			t.Error("`mine` was reported as a known instance; only dev/prod/test are pre-booked")
		}
		if i.Name == "dev" && !i.Known {
			t.Error("`dev` was not reported as a known instance")
		}
	}
}

// A MACHINE WITH NOTHING INSTALLED IS AN EMPTY LIST, NOT AN ERROR — that is the "empty" state the
// Servers page renders on a fresh desktop install.
func TestListOnAFreshMachine(t *testing.T) {
	withInstance(t)
	got, err := List()
	if err != nil {
		t.Fatalf("List on a fresh machine must not fail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no instances, got %v", got)
	}
}

// A NAME THAT CANNOT BE A DIRECTORY NAME IS REFUSED, not turned into a path.
func TestStatusRefusesAnInvalidName(t *testing.T) {
	withInstance(t, "dev")
	for _, bad := range []string{"", "../etc", "a/b", ".."} {
		if _, err := Status(bad); err == nil {
			t.Errorf("Status(%q) accepted an invalid instance name", bad)
		}
	}
}

// STATUS ON A NAME THAT IS NOT INSTALLED IS A CLEAR ERROR naming where it looked.
func TestStatusOnAMissingInstance(t *testing.T) {
	withInstance(t, "dev")
	_, err := Status("prod")
	if err == nil {
		t.Fatal("Status(prod) succeeded on a machine where prod is not installed")
	}
	if !strings.Contains(err.Error(), "no instance named") {
		t.Errorf("the error must say the instance is not there: %v", err)
	}
}

// STATUS CARRIES THE SETUP RECORD WHEN ONE EXISTS, because the Servers page renders progress from the
// same call it renders state from — two calls would let the row disagree with its own progress bar.
func TestStatusCarriesTheSetupRecord(t *testing.T) {
	withInstance(t, "dev")
	if err := WriteSetupProgress("dev", SetupProgress{
		State: SetupRunning, Phase: "downloading-postgres", Label: "Downloading PostgreSQL", Percent: 62,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := Status("dev")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Setup == nil {
		t.Fatal("Status did not carry the setup record")
	}
	if got.Setup.Percent != 62 || got.Setup.Phase != "downloading-postgres" {
		t.Errorf("the carried record lost fields: %+v", got.Setup)
	}
	// And with no record, the field is absent rather than a zero-valued struct the UI would render as
	// "0% — starting".
	withInstance(t, "test")
	got2, err := Status("test")
	if err != nil {
		t.Fatalf("Status(test): %v", err)
	}
	if got2.Setup != nil {
		t.Errorf("Setup should be nil when no record exists, got %+v", got2.Setup)
	}
}

// THE SHORT VERBS FLATTEN planebind's OUTCOME, so the contract cannot invent a word the desktop has never
// seen — and every result carries a word at all.
func TestActFlattensPlanebindOutcomes(t *testing.T) {
	withInstance(t, "dev")
	orig := newPlaneService
	defer func() { newPlaneService = orig }()

	for _, tc := range []struct {
		outcome string
		ok      bool
	}{
		{"started", true},
		{"already-stopped", true},
	} {
		newPlaneService = func(out interface{ Write([]byte) (int, error) }) planeOps {
			return &stubPlane{action: planebind.ActionDTO{
				Instance: "dev", Action: "start", Outcome: tc.outcome, OK: tc.ok}}
		}
		res, err := Start("dev", Options{})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if res.Outcome != tc.outcome || res.OK != tc.ok {
			t.Errorf("flattened result = %+v, want outcome %q ok=%v", res, tc.outcome, tc.ok)
		}
	}
}

// AN EMPTY OUTCOME IS REPORTED AS A FAILURE, not as a blank row.
func TestActTreatsAnEmptyOutcomeAsFailure(t *testing.T) {
	withInstance(t, "dev")
	orig := newPlaneService
	defer func() { newPlaneService = orig }()
	newPlaneService = func(out interface{ Write([]byte) (int, error) }) planeOps {
		return &stubPlane{action: planebind.ActionDTO{Instance: "dev", Action: "start"}} // no Outcome
	}
	res, err := Start("dev", Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.OK || res.Outcome != OutcomeFailed {
		t.Errorf("a result with no outcome must read as failed, got %+v", res)
	}
	if res.Message == "" {
		t.Error("the failure carries no message, so a UI has nothing to show")
	}
}

// INSTALL AND UNINSTALL BUILD THE CLI'S OWN COMMAND LINE, with pins in a stable order.
//
// They spawn (the work is minutes long), so this asserts the ARGS rather than a result: the args are the
// contract with the single implementation.
func TestInstallAndUninstallArgs(t *testing.T) {
	withInstance(t, "dev")

	install := installArgs("prod", Options{Ports: map[string]int{
		"ORCHICON_POSTGRES_PORT": 5433, "ORCHICON_CONTROL_PORT": 8091,
	}, RuntimeImages: "none"})
	want := []string{"install", "--non-interactive", "--instance", "prod",
		"--port", "ORCHICON_CONTROL_PORT=8091", "--port", "ORCHICON_POSTGRES_PORT=5433",
		"--runtime-images", "none"}
	if strings.Join(install, " ") != strings.Join(want, " ") {
		t.Errorf("install args =\n  %v\nwant\n  %v", install, want)
	}

	// NO PORTS PINNED is the important case: a reinstall must leave the instance's own recorded ports
	// alone, or it renumbers its database (the 5433 bug).
	if got := installArgs("dev", Options{}); strings.Contains(strings.Join(got, " "), "--port") {
		t.Errorf("a pinless install must pass no --port at all, got %v", got)
	}

	if got := uninstallArgs("dev", Options{}); strings.Join(got, " ") != "uninstall dev" {
		t.Errorf("uninstall args = %v, want [uninstall dev]", got)
	}
	if got := uninstallArgs("dev", Options{Purge: true}); strings.Join(got, " ") != "uninstall dev --purge" {
		t.Errorf("purge args = %v", got)
	}
	if got := uninstallArgs("", Options{}); strings.Join(got, " ") != "uninstall --all" {
		t.Errorf("all-instances args = %v, want [uninstall --all]", got)
	}
}

// A STALE, UNFINISHED SETUP IS VISIBLE THROUGH THE CONTRACT, which is what makes the Servers page able to
// offer "retry" instead of a spinner.
func TestStatusExposesAStaleSetup(t *testing.T) {
	home := withInstance(t, "dev")
	path := filepath.Join(home, ".local", "share", "orchicon-dev", "setup-progress.json")
	old := time.Now().Add(-2 * setupProgressStaleAfter).UTC().Format(time.RFC3339)
	raw := `{"instance":"dev","state":"running","phase":"x","started_at":"` + old + `","updated_at":"` + old + `"}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Status("dev")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Setup == nil || got.Setup.EffectiveState() != SetupStale {
		t.Fatalf("a stale setup must be visible as stale through Status, got %+v", got.Setup)
	}
}

// A DIRECTORY IS NOT AN INSTANCE.
//
// REPORTED: "on the TUI there is some weird desktop option showing up like it's a different server which is
// wrong." It was real — `~/.local/share/orchicon-desktop` is where the DESKTOP APP keeps its WebView state
// (hsts-storage.sqlite, localstorage, mediakeys, storage). Listing every `orchicon-*` directory made that a
// server called "desktop": not startable, not stoppable, and wrong on a screen whose whole job is naming what
// is running.
//
// The predicate is INSTANCE MARKERS, not the name, and any one is enough so a partially-created instance is
// still visible.
func TestADirectoryWithoutInstanceStateIsNotAServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// The desktop app's own directory, exactly as it appears on disk.
	desktop := filepath.Join(home, ".local", "share", "orchicon-desktop")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"hsts-storage.sqlite"} {
		if err := os.WriteFile(filepath.Join(desktop, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"localstorage", "mediakeys", "storage"} {
		if err := os.MkdirAll(filepath.Join(desktop, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A real instance beside it.
	dev := filepath.Join(home, ".local", "share", "orchicon-dev")
	if err := os.MkdirAll(filepath.Join(dev, "serve"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "dev" {
		var names []string
		for _, i := range got {
			names = append(names, i.Name)
		}
		t.Fatalf("List() = %v, want exactly [dev] — the desktop app's WebView directory is NOT a server", names)
	}
}

// A PARTIALLY-CREATED INSTANCE IS STILL AN INSTANCE: any ONE marker is enough, so an install caught mid-flight
// does not vanish from the list the operator is watching.
func TestOneMarkerIsEnoughToBeAnInstance(t *testing.T) {
	for _, marker := range instanceMarkers {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		dir := filepath.Join(home, ".local", "share", "orchicon-half")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, marker)
		if marker == "ports.json" {
			if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := List()
		if err != nil {
			t.Fatalf("List with marker %q: %v", marker, err)
		}
		if len(got) != 1 || got[0].Name != "half" {
			t.Errorf("a directory holding only %q was not listed as an instance", marker)
		}
	}
}
