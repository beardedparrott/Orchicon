package secretcrypto

// kek_location_test.go — the KEK's LOCATION must never depend on a process's working directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REPORTED FAILURE, at its exact point of harm: a relative data dir must not produce a CWD-relative KEK.
func TestRelativeDataDirIsRefusedRatherThanWrittenIntoTheWorkingDirectory(t *testing.T) {
	// RUN THE WHOLE TEST FROM A DIRECTORY THAT STANDS IN FOR THE WORKTREE the worker reported the file
	// in.
	worktree := t.TempDir()
	t.Chdir(worktree)
	// Prove the hazard is real before asserting it cannot happen: this is where a relative data dir points.
	if got := filepath.Join("", "secrets", "kek"); !strings.HasSuffix(got, "secrets/kek") {
		t.Fatalf("test premise changed: filepath.Join(\"\", \"secrets\", \"kek\") = %q", got)
	}

	for _, dataDir := range []string{"", ".", "secrets-here", "./state"} {
		_, err := LoadOrCreateKEK(dataDir)
		if err == nil {
			t.Fatalf("LoadOrCreateKEK(%q) succeeded — a relative data dir resolved the KEK against the "+
				"process's working directory, which is exactly how one reached a worker's bind-mounted worktree", dataDir)
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("LoadOrCreateKEK(%q) error does not say the location must be absolute: %v", dataDir, err)
		}
	}

	// And nothing was written anywhere in the stand-in worktree.
	if entries, _ := os.ReadDir(worktree); len(entries) != 0 {
		t.Fatalf("a refused KEK still wrote %d entries into the working directory", len(entries))
	}
}

// The guard must not break the legitimate case.
func TestAbsoluteDataDirStillCreatesAndReloadsTheKEK(t *testing.T) {
	dataDir := t.TempDir()

	first, err := LoadOrCreateKEK(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateKEK(absolute): %v", err)
	}
	if len(first) != 32 {
		t.Fatalf("KEK is %d bytes, want 32", len(first))
	}
	if _, err := os.Stat(KEKPath(dataDir)); err != nil {
		t.Fatalf("KEK file was not created at %s: %v", KEKPath(dataDir), err)
	}
	// Reload returns the SAME key — the whole persistence invariant.
	again, err := LoadOrCreateKEK(dataDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(first) != string(again) {
		t.Fatal("reload returned a DIFFERENT key — stored secrets would become undecryptable")
	}
}

// The key lands inside the data dir, never beside it.
func TestKEKPathStaysUnderTheDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "instance")
	if got := KEKPath(dataDir); got != filepath.Join(dataDir, "secrets", "kek") {
		t.Fatalf("KEKPath = %q, want it under the data dir", got)
	}
}
