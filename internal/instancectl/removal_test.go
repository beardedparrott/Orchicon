package instancectl

// removal_test.go — "I removed the server and it is still on the screen."
//
// THE OPERATOR'S DIAGNOSIS WAS EXACT: "When you delete a server in the TUI it does actually remove the
// server and if you restart the desktop app it is gone from there as well. So something in how the desktop
// is sending the delete and how it refreshs its screen is causing the issue."
//
// The deletion was never broken. TWO things made the LIST keep showing it:
//
//  1. `isInstanceDir` counted `postgres` and `blobs` as proof of an installation. `orchicon uninstall`
//     keeps the data by default and removes only infrastructure, so a keep-data uninstall left a directory
//     that still passed the test — a server with no binaries, no config, no ports and no plane, listed with
//     a Start button that could not work.
//  2. the setup record (setup-progress.json) was never removed, so the row went on saying
//     "Setup complete" for an instance that no longer existed.
//
// These assert the LIST, because that is what the operator was looking at.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLeftoverDataIsNotAnInstalledServer is defect (1).
func TestLeftoverDataIsNotAnInstalledServer(t *testing.T) {
	// A directory shaped exactly like the aftermath of `orchicon uninstall <name>` (keep-data): the
	// operator's data survives, every piece of infrastructure is gone.
	dataOnly := t.TempDir()
	for _, d := range []string{"postgres", "blobs", "secrets", "backups"} {
		if err := os.MkdirAll(filepath.Join(dataOnly, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if isInstanceDir(dataOnly) {
		t.Error("a directory holding ONLY the operator's data is being listed as an installed server. " +
			"That is what an uninstall leaves behind, so the server stays on the screen after it is " +
			"removed — and its Start button can never work because there are no binaries")
	}

	// EVERY STAGE OF A REAL INSTALL MUST STILL BE VISIBLE, which is what the old "any marker" rule was
	// protecting. One directory per stage, each with only the marker that stage produces.
	stages := map[string]string{
		"resolver unpacked the binaries": "bin",
		"install recorded its ports":     "ports.json",
		"install wrote the config":       "config",
		"supervisor wrote its pid/logs":  "serve",
		"install recorded the manifests": "infra-binaries.json",
	}
	for why, marker := range stages {
		dir := t.TempDir()
		// A file, not a directory: ports.json and infra-binaries.json are files, and the check must not
		// care which it is.
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !isInstanceDir(dir) {
			t.Errorf("a partially-created instance is INVISIBLE: the directory has %q (%s) and "+
				"isInstanceDir said no. An instance must never be hidden while it is still being built",
				marker, why)
		}
	}
}

// TestTheSetupRecordDoesNotOutliveTheInstance is defect (2).
//
// It is asserted against the uninstall list rather than by running an uninstall, because the list is the
// contract: everything named here is removed on EVERY uninstall, and this is the one entry whose absence
// produced a screen that contradicted the filesystem.
func TestTheSetupRecordDoesNotOutliveTheInstance(t *testing.T) {
	found := false
	for _, f := range uninstallInfraFilesOfCmdOrchicon(t) {
		if f == "setup-progress.json" {
			found = true
			break
		}
	}
	if !found {
		t.Error("setup-progress.json is not in the uninstall list, so the row keeps saying " +
			"\"Setup complete\" for a server that has been removed — which is exactly what was reported")
	}
}

// uninstallInfraFilesOfCmdOrchicon reads the list from the CLI's uninstall.go.
//
// A SOURCE READ RATHER THAN AN IMPORT, because the list lives in `package main` under cmd/orchicon and
// cannot be imported. The alternative — duplicating the list here — would be a second copy that agrees
// today and drifts later, which is the failure these guards exist to prevent.
func uninstallInfraFilesOfCmdOrchicon(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "orchicon", "uninstall.go"))
	if err != nil {
		t.Fatalf("cannot read cmd/orchicon/uninstall.go: %v — if it moved, update this guard", err)
	}
	var out []string
	inList := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inList {
			if strings.HasPrefix(trimmed, "var uninstallInfraFiles = []string{") {
				inList = true
			}
			continue
		}
		if trimmed == "}" {
			break
		}
		// Entries are quoted strings, possibly several on one line, possibly followed by a comment.
		if i := strings.IndexByte(trimmed, '"'); i >= 0 {
			rest := trimmed[i+1:]
			if j := strings.IndexByte(rest, '"'); j > 0 {
				out = append(out, rest[:j])
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no entries in uninstallInfraFiles — the list moved or changed shape, and this " +
			"guard would otherwise pass vacuously")
	}
	return out
}
