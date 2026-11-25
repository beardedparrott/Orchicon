package gitenv_test

// A SOURCE GATE, because the unit tests only cover the paths they exercise.
//
// The failure mode this guards is silent by construction: a git invocation that inherits the ambient
// environment works perfectly on an operator's machine and corrupts a worker's shared repository only
// when GIT_DIR happens to be set. Nothing fails; the damage lands later and looks unrelated. So this
// test asserts the property at the source, for every package that runs git against a repository it
// reaches by path.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packages that must never spawn git with an inherited environment. Each of these reaches a repository
// by path (a project dir, a run worktree, a session dir) and would be redirected by a leaked pointer.
var gitSpawnPackages = []string{
	"scheduler",  // restoreWorkTree: reset --hard + clean -fd — the most destructive git in the codebase
	"project",    // git init + git config during project setup — the writer that caused the incident
	"fileedit",   // status/show/diff on a run worktree — reports the wrong tree if redirected
	"checkpoint", // commits the run's worktree on abort
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// This package lives at internal/gitenv.
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestNoGitSpawnInheritsTheAmbientEnvironment is the gate.
func TestNoGitSpawnInheritsTheAmbientEnvironment(t *testing.T) {
	root := repoRoot(t)
	for _, pkg := range gitSpawnPackages {
		dir := filepath.Join(root, "internal", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			src := stripComments(string(b))

			// Every function that builds a git command must set Env from gitenv, never os.Environ raw.
			for _, line := range strings.Split(src, "\n") {
				// procwin.CommandContext(ctx, "git", …) / exec.Command("git", …)
				if !strings.Contains(line, `"git"`) {
					continue
				}
				if !strings.Contains(line, "procwin.Command") && !strings.Contains(line, "exec.Command") {
					continue
				}
				// A raw os.Environ() handed straight to Env is the bug.
				if strings.Contains(src, "cmd.Env = append(os.Environ()") {
					t.Errorf("%s: a git command's environment is built from raw os.Environ(). Use "+
						"gitenv.Clean(os.Environ()) — an inherited GIT_DIR/GIT_WORK_TREE overrides -C and "+
						"redirects the command to a different repository.", path)
					break
				}
			}
		}
	}
}

// TestEveryGitHelperPackageImportsGitenv is the positive half: the packages above must actually depend on
// the sanitizer, or the gate above would be satisfied by a helper that simply never sets Env at all.
func TestEveryGitHelperPackageImportsGitenv(t *testing.T) {
	root := repoRoot(t)
	for _, pkg := range gitSpawnPackages {
		dir := filepath.Join(root, "internal", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		found := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "internal/gitenv") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("internal/%s spawns git but does not import internal/gitenv — its commands can be "+
				"redirected by an inherited repository pointer", pkg)
		}
	}
}

// stripComments removes line and block comments so a MENTION of a hazard is not read as the hazard.
func stripComments(src string) string {
	var b strings.Builder
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		if inBlock {
			if i := strings.Index(line, "*/"); i >= 0 {
				inBlock = false
				line = line[i+2:]
			} else {
				continue
			}
		}
		if i := strings.Index(line, "/*"); i >= 0 {
			if j := strings.Index(line[i:], "*/"); j >= 0 {
				line = line[:i] + line[i+j+2:]
			} else {
				line = line[:i]
				inBlock = true
			}
		}
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
