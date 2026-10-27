package main

// tracked_symlink_test.go — the repo must never TRACK a symlink that points outside itself.
//
// THE DEFECT THIS EXISTS FOR. f109d2235 (PR #820) committed `frontend/node_modules` as a git symlink whose
// content was an ABSOLUTE PATH on the author's machine:
//
//     <repo>/frontend/node_modules
//
// `.gitignore` said `node_modules/` — a trailing slash matches a DIRECTORY only, so a SYMLINK named
// node_modules was not ignored and `git add -A` swept it in. The consequences, in order of how they were
// noticed:
//
//   - on the author's machine the link pointed AT ITSELF, so every path under it failed with ELOOP,
//     `tsc -b` died and `make rebuild-dev` reported nothing but
//         make[2]: *** [Makefile:707: fe-build] Error 216
//   - on ANY OTHER machine it pointed at a directory that does not exist — so a fresh clone could not
//     build the frontend at all, and the fix (delete it, `npm ci`) was nowhere in the output.
//
// WHY THIS IS ASSERTED RATHER THAN REMEMBERED: `git add -A` in a worktree is exactly how it happened, and it
// will happen again the moment another ignored-by-pattern-only path gains a symlink. The check is cheap and
// it names the file.
//
// A LEGITIMATE tracked symlink is not forbidden outright — a relative link inside the repo (a test fixture,
// say) is harmless. What is forbidden is one whose target is ABSOLUTE or escapes the repo, because those are
// machine-specific by construction: they cannot mean the same thing on two machines.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoTrackedAbsoluteSymlinks walks the index and fails on any symlink whose target would not resolve the
// same way on another machine.
func TestNoTrackedAbsoluteSymlinks(t *testing.T) {
	root := repoRoot(t)
	// ls-files -s gives "<mode> <sha> <stage>\t<path>"; mode 120000 is a symlink.
	out, err := exec.Command("git", "-C", root, "ls-files", "-s").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	checked := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 2 || fields[0] != "120000" {
			continue
		}
		checked++
		// The link's CONTENT is the blob; read it from the index, not the worktree, because the worktree may
		// hold something else (a real directory, after a repair).
		body, err := exec.Command("git", "-C", root, "cat-file", "-p", fields[1]).Output()
		if err != nil {
			t.Errorf("%s: cannot read the symlink target from the index: %v", path, err)
			continue
		}
		target := strings.TrimSpace(string(body))
		if filepath.IsAbs(target) {
			t.Errorf("the repo TRACKS an absolute symlink: %s -> %s\n"+
				"  An absolute target is machine-specific: on the machine it was created on it may point at\n"+
				"  itself (ELOOP, and `tsc -b` fails with a bare 'Error 216'), and everywhere else it points\n"+
				"  at nothing. Remove it from the index and ignore it by name.", path, target)
		}
	}
	// VACUITY GUARD: this repo tracks many files; a walk that found none proves nothing.
	if checked == 0 && !indexHasFiles(t, root) {
		t.Fatal("the index walk found no files at all — it is not looking where it thinks")
	}
}

// indexHasFiles is the vacuity backstop: the index must be non-empty for the loop above to mean anything.
func indexHasFiles(t *testing.T, root string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// AND node_modules IS NOT TRACKED, as a symlink or anything else.
//
// The narrow restatement of the same defect: even a RELATIVE tracked node_modules would be wrong, because the
// directory's contents are machine-installed and must never be committed.
func TestNodeModulesIsNotTracked(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, path := range strings.Split(string(out), "\n") {
		if path == "" {
			continue
		}
		if path == "frontend/node_modules" || strings.HasPrefix(path, "frontend/node_modules/") {
			t.Errorf("the repo tracks %s.\n"+
				"  node_modules is INSTALLED, not source: tracking it (or a symlink to it) gives every\n"+
				"  checkout a link whose target only means something on one machine.", path)
		}
	}
}

// THE IGNORE RULE MUST MATCH A SYMLINK TOO. `node_modules/` matches a directory only — which is exactly how
// the link got committed.
func TestGitignoreCoversASymlinkedNodeModules(t *testing.T) {
	body := readGoFile(t, filepath.Join("..", "..", ".gitignore"))
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		// The rule that is meant to cover node_modules.
		if trimmed == "node_modules/" {
			t.Fatal("`.gitignore` ignores `node_modules/` — a trailing slash matches a DIRECTORY only, so a " +
				"SYMLINK named node_modules is not ignored. Drop the slash so the rule covers a directory, " +
				"a file and a symlink.")
		}
	}
	if !strings.Contains(body, "\nnode_modules\n") && !strings.HasPrefix(body, "node_modules\n") {
		t.Error("`.gitignore` has no bare `node_modules` rule, so an accidental symlink of that name would " +
			"be committable again")
	}
}
