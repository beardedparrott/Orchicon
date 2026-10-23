package main

// makefile_safety_test.go — the build must not break the operator's environment, and must not report a
// failure as a bare exit code when it can name the cause.
//
// TWO REPORTED DEFECTS, both from `make rebuild-dev`:
//
//	1. "Also my orch binary has been deleted!" — the launcher targets symlinked
//	   "$(CURDIR)/$(BIN_DIR)/orch". Run from a git worktree, CURDIR is the WORKTREE, so ~/.local/bin/orch
//	   was repointed into `.orchicon-worktrees/<id>/bin/` and deleting that worktree took the launcher with
//	   it. `orch` was never deleted; its symlink pointed at a directory that no longer existed, which is
//	   the same thing to every command that runs it.
//
//	2. "make[2]: *** [Makefile:707: fe-build] Error 216" — frontend/node_modules was a symlink pointing at
//	   ITSELF, so every path under it failed with ELOOP, `tsc -b` died, and npm exited 216. The number was
//	   the symptom and nothing said so.
//
// The behaviour lives in scripts/dev (testable) rather than in make syntax (not), and these assertions are
// about the SCRIPT's decisions plus the Makefile's WIRING — because fixing the script and leaving a raw
// `ln -sfn` beside it is how this comes back.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is two levels up from cmd/orchicon.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// runScript runs a scripts/dev helper and returns combined output + exit code.
func runScript(t *testing.T, script string, args ...string) (string, int) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "dev", script)
	cmd := exec.Command(path, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", script, err)
	}
	return string(out), code
}

// A LAUNCHER INSTALLED FROM A WORKTREE MUST POINT AT THE MAIN CHECKOUT.
//
// This is the reported "orch binary has been deleted" reproduced as a unit: the synthetic layout has a main
// checkout, a worktree inside it, and a .git FILE in the worktree pointing at <main>/.git/worktrees/<name>
// — exactly what git creates. The script must resolve the main root from that and point there.
func TestLauncherFromAWorktreePointsAtTheMainCheckout(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	wt := filepath.Join(main, ".orchicon-worktrees", "abc123")
	launchers := filepath.Join(base, "bin")

	if err := os.MkdirAll(filepath.Join(main, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Both checkouts have the binary, so the test can tell WHICH one was chosen.
	for _, p := range []string{
		filepath.Join(main, "bin", "orch"),
		filepath.Join(wt, "bin", "orch"),
	} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The worktree's .git is a FILE, as git writes it.
	if err := os.MkdirAll(filepath.Join(main, ".git", "worktrees", "abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "abc123")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --dry-run, because this sandbox blocks `ln` outright (an ORCHICON GUARD refuses it as destructive
	// tooling for every child of the test binary). The DECISION is what this test is about: which checkout a
	// launcher may point at. The link itself is one command whose argument is asserted below.
	out, code := runScript(t, "install-launcher.sh", "--root", wt, "--launcher-dir", launchers, "--dry-run", "orch")
	if code != 0 {
		t.Fatalf("install-launcher.sh failed (%d):\n%s", code, out)
	}
	want := "-> " + filepath.Join(main, "bin", "orch")
	if !strings.Contains(out, want) {
		t.Fatalf("the launcher would not point at the main checkout (%s):\n%s", want, out)
	}
	if strings.Contains(out, "-> "+filepath.Join(wt, "bin")) {
		t.Fatalf("the launcher points INTO A WORKTREE.\n"+
			"  Removing that worktree then deletes the operator's launcher — the reported\n"+
			"  \"my orch binary has been deleted\".\n%s", out)
	}
	if !strings.Contains(out, "launchers track the MAIN checkout") {
		t.Errorf("the script did not explain what it is doing:\n%s", out)
	}
}

// AND FROM THE MAIN CHECKOUT IT POINTS AT ITSELF, which is the ordinary case.
func TestLauncherFromTheMainCheckoutPointsAtIt(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	launchers := filepath.Join(base, "bin")
	if err := os.MkdirAll(filepath.Join(main, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(main, "bin", "orchicon")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runScript(t, "install-launcher.sh", "--root", main, "--launcher-dir", launchers, "--dry-run", "orchicon")
	if code != 0 {
		t.Fatalf("install-launcher.sh failed (%d):\n%s", code, out)
	}
	if want := "-> " + bin; !strings.Contains(out, want) {
		t.Errorf("a main-checkout launcher must point at its own bin (%s):\n%s", want, out)
	}
}

// A SELF-REFERENTIAL node_modules IS REPORTED AS WHAT IT IS, not as an exit code.
func TestFeDepsNamesASelfReferentialNodeModules(t *testing.T) {
	frontend := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(frontend, 0o755); err != nil {
		t.Fatal(err)
	}
	nm := filepath.Join(frontend, "node_modules")
	// The exact broken shape: a link to itself.
	if err := os.Symlink(nm, nm); err != nil {
		t.Skipf("cannot create a self-referential symlink here: %v", err)
	}

	out, code := runScript(t, "fe-deps.sh", "--frontend", frontend, "--check-only")
	if code == 0 {
		t.Fatalf("a self-referential node_modules passed the check:\n%s", out)
	}
	for _, want := range []string{"BROKEN", "ITSELF"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnosis does not mention %q, so the operator still has only \"Error 216\":\n%s", want, out)
		}
	}
	if !strings.Contains(out, "npm ci") {
		t.Errorf("the diagnosis does not give the fix:\n%s", out)
	}
}

// A HEALTHY node_modules — including a WORKTREE'S SYMLINK TO THE MAIN CHECKOUT — passes.
//
// The worktree case is the one that must NOT be "repaired": linking the main checkout's dependencies is how
// a worktree avoids a second install.
func TestFeDepsAcceptsRealAndLegitimatelyLinkedTrees(t *testing.T) {
	base := t.TempDir()

	// (a) a real directory
	real := filepath.Join(base, "real", "frontend")
	if err := os.MkdirAll(filepath.Join(real, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := runScript(t, "fe-deps.sh", "--frontend", real, "--check-only"); code != 0 {
		t.Errorf("a real node_modules was rejected (%d):\n%s", code, out)
	}

	// (b) a worktree-shaped link to a real directory
	mainFE := filepath.Join(base, "main", "frontend")
	if err := os.MkdirAll(filepath.Join(mainFE, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	wtFE := filepath.Join(base, "main", ".orchicon-worktrees", "x", "frontend")
	if err := os.MkdirAll(wtFE, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(mainFE, "node_modules"), filepath.Join(wtFE, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if out, code := runScript(t, "fe-deps.sh", "--frontend", wtFE, "--check-only"); code != 0 {
		t.Errorf("a worktree's link to the main checkout's node_modules was rejected (%d):\n%s", code, out)
	}
}

// THE MAKEFILE MUST GO THROUGH THE SCRIPTS. Fixing the script and leaving a raw `ln -sfn "$(CURDIR)…"`
// beside it is exactly how the worktree bug comes back.
func TestMakefileUsesTheGuardedHelpers(t *testing.T) {
	body := readGoFile(t, filepath.Join("..", "..", "Makefile"))

	if strings.Contains(body, `ln -sfn "$(CURDIR)/$(BIN_DIR)`) {
		t.Error("the Makefile still symlinks a launcher at $(CURDIR)/$(BIN_DIR) directly.\n" +
			"  From a worktree that points the operator's launcher into a throwaway directory, and\n" +
			"  removing the worktree deletes it. Use scripts/dev/install-launcher.sh.")
	}
	if !strings.Contains(body, "scripts/dev/install-launcher.sh") {
		t.Error("the Makefile does not use scripts/dev/install-launcher.sh")
	}
	if !strings.Contains(body, "scripts/dev/fe-deps.sh") {
		t.Error("fe-build does not check frontend/node_modules first, so a broken link is still reported " +
			"as a bare \"Error 216\"")
	}
}

// THE REPAIR PATH REMOVES THE LINK AND RUNS `npm ci` — asserted from the script, not by running it.
//
// This sandbox blocks `rm` and `ln` for every child of the test binary (an ORCHICON GUARD refuses them as
// destructive tooling), so the repair cannot be EXECUTED here. Saying that plainly is better than a test
// that quietly proves nothing. What is pinned instead:
//
//   - the DIAGNOSIS and its fix line are asserted by running the script with --check-only (above), which
//     needs no destructive command;
//   - the repair SEQUENCE is asserted here against the script's own text;
//   - and the repair was exercised BY HAND on the operator's real checkout while restoring it — the actual
//     damage this fix was written for: `rm -f node_modules && npm ci` then `tsc -b` clean and `vite build`
//     green.
func TestFeDepsRepairSequenceIsPresent(t *testing.T) {
	body := readGoFile(t, filepath.Join("..", "..", "scripts", "dev", "fe-deps.sh"))

	for _, want := range []struct{ what, sub string }{
		{"the broken link is removed", `rm -f "$nm"`},
		{"a lockfile-pinned tree installs with ci", `"$npm_bin" ci`},
		{"a tree without a lockfile falls back to install", `"$npm_bin" install`},
	} {
		if !strings.Contains(body, want.sub) {
			t.Errorf("fe-deps.sh does not do %s (%q), so a broken node_modules is only reported and never "+
				"fixed — which leaves the operator the same manual repair the script exists to avoid",
				want.what, want.sub)
		}
	}
	// And the diagnosis must come BEFORE any destructive step, so the operator reads why first.
	diag := strings.Index(body, "is BROKEN")
	rm := strings.Index(body, `rm -f "$nm"`)
	if diag < 0 || rm < 0 {
		t.Fatal("could not locate the diagnosis and the removal")
	}
	if diag > rm {
		t.Error("the removal happens before the explanation — the operator's output would show a file " +
			"disappearing with no reason given")
	}
}
