package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolved_ports_test.go — THE INSTALL RECORDS THE PORTS IT BOUND.

// TestWriteResolvedPortsRecordsWhatWasBound is the contract the PowerShell side reads: a JSON object keyed
// by the ORCHICON_*_PORT names, in the instance's state dir.
func TestWriteResolvedPortsRecordsWhatWasBound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ports := map[string]int{
		"ORCHICON_CONTROL_PORT":  8092, // NOT the dev default (8080)
		"ORCHICON_POSTGRES_PORT": 5444, // NOT the dev default (5432)
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

	writeResolvedPorts("dev", map[string]int{"ORCHICON_POSTGRES_PORT": 5433})
	writeResolvedPorts("test", map[string]int{"ORCHICON_POSTGRES_PORT": 5434})

	for inst, want := range map[string]int{"dev": 5433, "test": 5434} {
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
		if got["ORCHICON_POSTGRES_PORT"] != want {
			t.Errorf("instance %s recorded postgres %d, want %d", inst, got["ORCHICON_POSTGRES_PORT"], want)
		}
	}
}

// TestWriteResolvedPortsIsBestEffort — a record that cannot be written must not fail an install
// that has already resolved everything and pulled its images.
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

// TestTheInstallerNeverTreatsAPresentTagAsCurrent — the regression that made an instance
// un-upgradable.
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
	// THE PLANE-IMAGE FALLBACK IS GONE WITH THE PLANE IMAGE.
}
