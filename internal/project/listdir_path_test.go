package project

// listDirectory must accept the paths its own callers echo back to it.
//
// REPORTED from the filesystem picker: expanding a directory inside a project failed with
//
//	Error: ConnectError: [internal] list directory: stat
//	"<repo>/<repo>/skills": no such file or directory
//
// The root is in there TWICE. FileTreeEntry.Path is absolute (this file hands out the absolute
// path for every child), and the picker echoes that path straight back as the next subpath —
// so the server was doing filepath.Join(root, "/root/skills"), which concatenates rather than
// replacing. Expanding any directory a second level deep could never work.
//
// These assertions call listDirectory directly: the doubling and the traversal escapes are both
// decidable without a service, a tenant or a database.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newProjectDir builds <tmp>/proj with a nested skills directory containing one file.
func newProjectDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(root, "skills", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "a.md"), []byte("# a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "top.md"), []byte("# t"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root
}

func TestListDirectoryAcceptsTheAbsolutePathItJustHandedOut(t *testing.T) {
	root := newProjectDir(t)

	// First level: the picker lists the project root and receives absolute child paths.
	_, _, top, err := listDirectory(root, "")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	var skillsEntry string
	for _, e := range top {
		if e.Name == "skills" {
			skillsEntry = e.Path
		}
	}
	if skillsEntry == "" {
		t.Fatalf("no skills entry in %v", top)
	}
	if !filepath.IsAbs(skillsEntry) {
		t.Fatalf("entry path %q should be absolute — the picker echoes it back as a subpath", skillsEntry)
	}

	// THE REPORTED FAILURE: expand by echoing that absolute path back unchanged.
	parent, dirName, entries, err := listDirectory(root, skillsEntry)
	if err != nil {
		t.Fatalf("expanding %q failed: %v — the root is being joined twice", skillsEntry, err)
	}
	if dirName != "skills" {
		t.Errorf("dirName = %q, want %q", dirName, "skills")
	}
	if parent != "skills" {
		t.Errorf("parentPath = %q, want the path RELATIVE to project_dir (%q)", parent, "skills")
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (a.md, nested): %v", len(entries), entries)
	}
	for _, e := range entries {
		if !filepath.IsAbs(e.Path) {
			t.Errorf("entry %q path %q is not absolute", e.Name, e.Path)
		}
		if !strings.HasPrefix(e.Path+string(filepath.Separator), root+string(filepath.Separator)) {
			t.Errorf("entry %q path %q escaped the project dir %q", e.Name, e.Path, root)
		}
	}

	// A RELATIVE subpath still works — the documented contract, unchanged.
	if _, _, relEntries, relErr := listDirectory(root, "skills"); relErr != nil || len(relEntries) != len(entries) {
		t.Errorf("relative subpath: err=%v entries=%d want %d", relErr, len(relEntries), len(entries))
	}
}

func TestListDirectoryRejectsPathsOutsideTheProject(t *testing.T) {
	root := newProjectDir(t)

	for _, sub := range []string{"..", "../..", "skills/../../..", "/etc", filepath.Join(root, "..", "elsewhere")} {
		if _, _, _, err := listDirectory(root, sub); err == nil {
			t.Errorf("subpath %q was accepted — it is outside %q", sub, root)
		}
	}
}
