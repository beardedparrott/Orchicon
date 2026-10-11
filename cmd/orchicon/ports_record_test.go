package main

// ports_record_test.go — the reader `orchicon restart` depends on.
//
// The ports are READ BACK from the record an install wrote, never re-resolved: re-resolving prompts
// on a terminal when the spec is ambiguous, and a command that restarts existing work must not ask
// a question. So the reader and its failure modes are what make `restart` safe to run on an
// instance it did not create — including one installed by an older version, which is the case the
// remedy message exists for.

import (
	"os"
	"strings"
	"testing"
)

// scratchHome points the instance state dir at a temp HOME, so a test never reads or writes the
// operator's real ~/.local/share/orchicon-*.
func scratchHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestReadResolvedPortsRoundTripsWhatTheInstallRecorded(t *testing.T) {
	scratchHome(t)
	const instance = "dev"

	// Written through the PRODUCTION writer, so this pins the pair rather than a JSON shape
	// invented here — a reader that agrees only with the test's own marshal would not notice the
	// writer changing.
	written := map[string]int{"ORCHICON_CONTROL_PORT": 8080, "ORCHICON_GRAFANA_PORT": 3002}
	writeResolvedPorts(instance, written)

	got, err := readResolvedPorts(instance)
	if err != nil {
		t.Fatalf("read the record just written: %v", err)
	}
	if len(got) != len(written) {
		t.Fatalf("read %d ports, want %d: %v", len(got), len(written), got)
	}
	for k, v := range written {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}

	// It must be where `restart` looks, which is the point of the record living in the instance
	// state dir.
	path, err := resolvedPortsFile(instance)
	if err != nil {
		t.Fatalf("resolvedPortsFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the record is not where a restart looks for it (%s): %v", path, err)
	}
	if !strings.Contains(path, "orchicon-"+instance) {
		t.Errorf("the record must live in the instance's own state dir, got %s", path)
	}
}

func TestReadResolvedPortsSaysWhatToDoWhenTheRecordIsMissing(t *testing.T) {
	scratchHome(t)

	_, err := readResolvedPorts("dev")
	if err == nil {
		t.Fatal("a missing record must be an error, not an empty port set — restart would bind nothing " +
			"and report a health failure that looks like the restart's fault")
	}
	// The remedy has to be IN the message: this is the error an operator hits on an instance
	// installed before the record existed, and the fix is not something they can guess.
	if !strings.Contains(err.Error(), "no port record") {
		t.Errorf("the error must name what is missing, got: %v", err)
	}
	if !strings.Contains(err.Error(), "install") {
		t.Errorf("the error must name the remedy (re-running the install records the ports), got: %v", err)
	}
}

func TestReadResolvedPortsRejectsAnEmptyRecord(t *testing.T) {
	scratchHome(t)
	const instance = "dev"

	path, err := resolvedPortsFile(instance)
	if err != nil {
		t.Fatalf("resolvedPortsFile: %v", err)
	}
	if err := os.MkdirAll(strings.TrimSuffix(path, "/ports.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := readResolvedPorts(instance); err == nil {
		t.Error("an empty record must be an error: a restart driven by it would bind no ports at all, " +
			"and the resulting health failure would look like the restart's fault rather than the record's")
	}
}
