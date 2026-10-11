package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolved_ports_test.go — THE INSTALL RECORDS THE PORTS IT BOUND.
//
// Why this exists, and it is a real report rather than a hypothetical: installing on Windows, the operator
// answered 3001 for Grafana. `orchicon install` printed 3001, and then scripts/install.ps1 — which prints
// the "Open from Windows" URLs after the setup step — printed 3002. One run, two contradicting numbers, and
// the second one was a port nothing had bound.
//
// The cause is that a PROMPT cannot be predicted by a wrapper: the resolved ports existed only as output on
// a stream PowerShell had already consumed, so it re-derived them from the per-instance defaults. These
// tests pin the record that makes the wrapper's job reading rather than guessing.

// TestWriteResolvedPortsRecordsWhatWasBound is the contract the PowerShell side reads: a JSON object keyed
// by the ORCHICON_*_PORT names, in the instance's state dir.
func TestWriteResolvedPortsRecordsWhatWasBound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ports := map[string]int{
		"ORCHICON_CONTROL_PORT": 8092, // NOT the dev default (8080)
		"ORCHICON_GRAFANA_PORT": 3001, // NOT the dev default (3002)
	}
	writeResolvedPorts("test", ports)

	path, err := resolvedPortsFile("test")
	if err != nil {
		t.Fatalf("resolvedPortsFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the record must exist at %s: %v", path, err)
	}

	// Parse it the way the wrapper does, rather than string-matching: the contract is the JSON shape.
	var got map[string]int
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the record must be valid JSON (the wrapper parses it): %v\n%s", err, b)
	}
	for k, want := range ports {
		if got[k] != want {
			t.Errorf("%s = %d, want %d — the record must carry the port that was BOUND, not a default", k, got[k], want)
		}
	}

	// The instance's own directory, because that is what the port belongs to.
	if want := filepath.Join(home, ".local", "share", "orchicon-test", "ports.json"); path != want {
		t.Errorf("record at %s, want %s", path, want)
	}
}

// TestResolvedPortsArePerInstance keeps two instances' records apart — the whole point of the instance
// model, and the reason the wrapper reads by instance rather than by a single well-known path.
func TestResolvedPortsArePerInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeResolvedPorts("dev", map[string]int{"ORCHICON_GRAFANA_PORT": 3002})
	writeResolvedPorts("test", map[string]int{"ORCHICON_GRAFANA_PORT": 3001})

	for inst, want := range map[string]int{"dev": 3002, "test": 3001} {
		path, err := resolvedPortsFile(inst)
		if err != nil {
			t.Fatalf("resolvedPortsFile(%s): %v", inst, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", inst, err)
		}
		var got map[string]int
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("parse %s: %v", inst, err)
		}
		if got["ORCHICON_GRAFANA_PORT"] != want {
			t.Errorf("instance %s recorded grafana %d, want %d", inst, got["ORCHICON_GRAFANA_PORT"], want)
		}
	}
}

// TestWriteResolvedPortsIsBestEffort — a record that cannot be written must not fail an install that has
// already resolved everything and pulled its images. The wrapper's fallback is the documented default, so
// the cost of losing the file is a stale URL, not a broken install.
func TestWriteResolvedPortsIsBestEffort(t *testing.T) {
	// HOME pointing at a path that cannot be created as a directory (a FILE stands where the dir must go).
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f)

	// Must not panic and must not fail: it returns nothing, and prints a warning.
	writeResolvedPorts("dev", map[string]int{"ORCHICON_CONTROL_PORT": 8080})
}

// TestTheInstallerNeverTreatsAPresentTagAsCurrent — the regression that made an instance un-upgradable.
//
// `pullImageIfAbsent` skipped the pull whenever the tag existed locally, and `:latest` is a MOVABLE pointer:
// the host kept the layers it first downloaded for ever, the install compared the container against that
// same stale local image, found them equal, and reported success. The operator's evidence was a container
// "Up 6 seconds" with "CREATED 11 hours ago" — restarted, never recreated, because nothing newer was ever
// fetched.
//
// It is asserted as an ABSENCE, because that is the shape of the defect: no code path in the installer may
// decide that a present tag means a current one.
func TestTheInstallerNeverTreatsAPresentTagAsCurrent(t *testing.T) {
	b, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatalf("read install.go: %v", err)
	}
	src := string(b)
	if strings.Contains(src, "func pullImageIfAbsent(") {
		t.Error("the installer has a pullImageIfAbsent again — a present tag is not evidence of currency, so the registry must be asked")
	}
	for _, want := range []string{"func ensureImage(", "func pullImage("} {
		if !strings.Contains(src, want) {
			t.Errorf("install.go is missing %q — the replace-or-refresh path is what keeps an instance upgradeable", want)
		}
	}
	// And the fallback must never be silent: an install that runs a different image than the one asked for
	// has to say so.
	if !strings.Contains(src, "falling back to") {
		t.Error("the image fallback is not announced; running a different image than the operator asked for must never be silent")
	}
}
