package project

// directory_probe_test.go — THE DIRECTORY PROBE MUST NOT CREATE WHAT IT PROBES.
//
// ListProjectFiles(dir_path) is how the frontend browses a raw path before a
// project exists, how the TUI's project-dir form validates a path, and how
// orch's launch prompt asks whether the CONTROL PLANE can see a directory
// (internal/tui/launch.go). All three depend on the same property: a path that is
// absent from the plane's view must come back as an ERROR.
//
// The comment in the TUI used to claim the opposite — "the server materializes the
// directory when the files endpoint resolves it" — which, if it were true, would
// make the directory probe useless for the third caller: it would answer its own
// question with a yes, and the warning that an unmounted/unreachable directory is
// about to make a project unusable would be dead code that never fires.
//
// So this pins the property at the function that implements it, rather than
// trusting a comment in another package.

import (
	"os"
	"path/filepath"
	"testing"
)

// A MISSING PATH IS AN ERROR AND STAYS MISSING.
func TestListDirectoryDoesNotCreateTheDirectoryItProbes(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "definitely-not-here")

	if _, _, _, err := listDirectory(missing, ""); err == nil {
		t.Fatal("listDirectory succeeded on a directory that does not exist, so it cannot be used to ask " +
			"whether a path is reachable — every caller that relies on the error would silently pass")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("listDirectory CREATED %q (stat err = %v). The probe then answers its own question: it can "+
			"never report a directory as unreachable, so the launch prompt's warning for a path the plane "+
			"cannot see would never fire.", missing, err)
	}

	// The parent is untouched too: no scaffolding was assembled along the way.
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read the temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("probing a missing directory left %d entr(ies) behind in its parent", len(entries))
	}
}

// AN EXISTING DIRECTORY LISTS, and reports itself as the directory it listed —
// the positive half of the same call, so a probe that failed for everything would
// not pass the test above by accident.
func TestListDirectoryListsAnExistingDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	parentPath, dirName, entries, err := listDirectory(base, "")
	if err != nil {
		t.Fatalf("listDirectory on an existing directory failed: %v", err)
	}
	if dirName != filepath.Base(base) {
		t.Errorf("dirName = %q, want %q", dirName, filepath.Base(base))
	}
	// AT THE ROOT THERE IS NO PARENT TO GO UP TO, so parentPath is empty — it is
	// the UI's "go up" target, and the root is the end of that chain.
	if parentPath != "" {
		t.Errorf("parentPath at the listed root = %q, want \"\" — the root has nowhere up to go, and a "+
			"non-empty value would offer the operator a step above the directory they opened", parentPath)
	}
	if len(entries) != 1 || entries[0].GetName() != "a.txt" {
		t.Errorf("entries = %v, want just a.txt", entries)
	}
}
