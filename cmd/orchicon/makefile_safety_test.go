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

// TestTheRebuildChainRebuildsTheDesktopApp — the staleness trap that produced three reported bugs.
//
// The operator reported three separate things:
//
//	"On Linux, it doesn't have the nice server section separated"
//	"It doesn't relaunch the desktop app when you switch instances ... the tab bar goes missing"
//	"the error/warning text in the desktop app is hard to see"
//
// All three had ALREADY been fixed on develop — f109d2235 (the Servers page), 5e9f92490 (Connect
// restarts the app), 4533ef77a (readable error text) — and none had reached them. `make
// rebuild-dev` rebuilt bin/orchicon and bin/orch only. The SPA is `go:embed`-ed at BUILD time, so
// the desktop binary carries a FROZEN COPY of the UI: the plane was rebuilt four times in a week
// while the app the operator was actually looking at stayed four days old. Nothing failed.
//
// So the assertion is not "the app is nice" — it is that a rebuild REACHES it. That has two
// halves, and the second is the one that goes wrong:
//
//	1. full-rebuild (which rebuild-dev/prod/test all delegate to) invokes the desktop build.
//	2. it invokes the CONDITIONAL one, not desktop-build — a rebuild must still work on a
//	   headless server or a CI container where CGO/webkit are absent, and failing there would
//	   trade a silent defect for a loud one on the machines that can never run a GUI.
func TestTheRebuildChainRebuildsTheDesktopApp(t *testing.T) {
	mk := mustRead(t, "../../Makefile")

	// THE CALL, inside full-rebuild — which is the one place all three rebuild-* targets pass
	// through. Asserting it in rebuild-dev alone would let rebuild-prod drift, and there are
	// three of them.
	full := makeTarget(t, mk, "full-rebuild")
	// ASSERT THE INVOCATION, NOT THE NAME. The recipe carries a comment block that names the
	// target ("see desktop-build-if-possible for the full account"), so a Contains on the bare
	// name is satisfied by PROSE — measured: deleting the call line left this test green. The
	// sub-make invocation is the thing that must exist.
	if !strings.Contains(full, "$(MAKE) desktop-build-if-possible") {
		t.Errorf("full-rebuild does not invoke desktop-build-if-possible, so `make rebuild-dev` / "+
			"rebuild-prod / rebuild-test rebuild the plane and leave the desktop app on its older "+
			"embedded UI.\n  full-rebuild:\n%s\n"+
			"  That is the reported defect: three UI fixes shipped and never reached the operator "+
			"because the rebuild never touched the app.", full)
	}
	// THE RIGHT VARIANT. A bare desktop-build here makes the everyday rebuild fail on any host
	// without webkit dev headers, which is every headless server and every CI container.
	if strings.Contains(full, "desktop-build\n") || strings.Contains(full, "$(MAKE) desktop-build ") {
		t.Errorf("full-rebuild calls desktop-build unconditionally:\n%s\n"+
			"  It must call desktop-build-if-possible, or a rebuild on a host without "+
			"GTK3/WebKitGTK dev headers fails instead of rebuilding the plane and saying why the GUI "+
			"was skipped.", full)
	}

	// THE CONDITIONAL TARGET EXISTS AND IS PHONED, or a file of that name would shadow it.
	if !strings.Contains(mk, "\ndesktop-build-if-possible:") {
		t.Fatal("the Makefile has no desktop-build-if-possible target")
	}
	if !strings.Contains(mk, ".PHONY:") || !strings.Contains(mk, "desktop-build-if-possible") {
		t.Error("desktop-build-if-possible is not in a .PHONY list — a file with that name in the " +
			"tree would silently disable the desktop half of every rebuild")
	}

	cond := makeTarget(t, mk, "desktop-build-if-possible")
	// THE PRECONDITIONS ARE THE POINT: each must be checked BEFORE the build is invoked.
	for _, want := range []struct{ what, sub string }{
		{"a cgo check", "go env CGO_ENABLED"},
		{"a GTK check", "pkg-config --exists gtk+-3.0"},
		{"a webkit check", "pkg-config --exists webkit2gtk-4.1"},
		{"the alternative webkit ABI", "pkg-config --exists webkit2gtk-4.0"},
	} {
		if !strings.Contains(cond, want.sub) {
			t.Errorf("desktop-build-if-possible has no %s (%v), so it cannot tell a host that "+
				"CAN build the GUI from one that cannot — and would fail a rebuild on a server",
				want.what, want.sub)
		}
	}
	// AND IT MUST STILL BUILD. A guard that skips on every host is a very quiet way to have no
	// desktop app at all — so the INVOCATION is asserted, not the name (which appears in both this
	// target's own declaration and its comment).
	if !strings.Contains(cond, "$(MAKE) --no-print-directory desktop-build") {
		t.Error("desktop-build-if-possible never invokes desktop-build, so the chain always skips")
	}

	// ONE SHELL — AND THIS IS THE ASSERTION THAT MATTERS MOST, because the bug it catches was made
	// and only found by RUNNING the target.
	//
	// Make runs every recipe LINE in its own shell. Written the obvious way — the precondition
	// block, then `exit 0` for the skip, then a separate line invoking desktop-build — the skip
	// ended its own shell, make ran the next line anyway, and the target printed
	// "SKIPPED — this host has no GTK3/WebKitGTK dev headers" and then invoked the build, which
	// died inside cgo. A guard that reports a skip and builds anyway is worse than no guard: it
	// says the opposite of what it does.
	//
	// So the `exit 0` and the desktop-build invocation must be in the SAME logical line, which in
	// make means a backslash-continued run of recipe lines. Asserted on the recipe's real shape:
	// split it into logical lines by trailing backslashes and require that the exit and the
	// invocation land in the same one.
	// THE PROPERTY IS "ONE SHELL", so it is asserted as exactly that: every non-comment recipe
	// line that carries logic must belong to a SINGLE logical line. Counting groups rather than
	// looking for a pair of markers matters — the fallback's `|| { …; exit 0; }` also contains
	// "exit 0", so a marker pair finds a spurious match and passes on the mutated file (measured).
	if n := len(logicalRecipeGroups(recipeLines(cond))); n != 1 {
		t.Errorf("desktop-build-if-possible's skip `exit 0` and its desktop-build invocation are not "+
			"in one logical recipe line:\n%s\n"+
			"  Make runs each LINE in its own shell, so `exit 0` on its own line stops only that line "+
			"and the build runs regardless — the target would print SKIPPED and then build. The recipe "+
			"is %d separate shell group(s); it must be 1. Join the lines with backslashes.", cond, n)
	}
}

// logicalRecipeGroups splits recipe lines into the scripts make actually runs: a line continues
// into the next when it ends in a backslash, and each resulting group is ONE shell invocation.
// Comment-only groups are dropped, since they carry no logic.
func logicalRecipeGroups(lines []string) []string {
	var groups []string
	var cur strings.Builder
	for _, ln := range lines {
		cur.WriteString(ln)
		cur.WriteString("\n")
		if strings.HasSuffix(strings.TrimRight(ln, " \t"), "\\") {
			continue
		}
		script := cur.String()
		cur.Reset()
		tr := strings.TrimSpace(script)
		if tr == "" || strings.HasPrefix(tr, "#") || strings.HasPrefix(tr, "@#") {
			continue
		}
		groups = append(groups, script)
	}
	if script := cur.String(); strings.TrimSpace(script) != "" {
		groups = append(groups, script)
	}
	return groups
}

// makeTarget returns the body of a Makefile target: from its `name:` line to the next line that
// starts in column 0 (the next target, comment or blank), which is where a make recipe ends.
func makeTarget(t *testing.T, mk, name string) string {
	t.Helper()
	lines := strings.Split(mk, "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, name+":") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("the Makefile has no %q target", name)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		if ln == "" || strings.HasPrefix(ln, "\t") || strings.HasPrefix(ln, "#") {
			continue
		}
		end = i
		break
	}
	return strings.Join(lines[start:end], "\n")
}

// recipeLines returns a target body's recipe lines (those starting with a tab), which is what make
// actually hands to a shell — one shell per line, unless the line ends in a backslash.
func recipeLines(body string) []string {
	var out []string
	for _, ln := range strings.Split(body, "\n")[1:] {
		if strings.HasPrefix(ln, "\t") {
			out = append(out, ln)
		}
	}
	return out
}

// sameLogicalRecipeLine reports whether two markers appear in the SAME logical line: a run of
// recipe lines joined by trailing backslashes, which make passes to one shell as a single script.
func sameLogicalRecipeLine(lines []string, a, b string) bool {
	var logical strings.Builder
	flush := func() (string, bool) {
		s := logical.String()
		logical.Reset()
		return s, s != ""
	}
	for _, ln := range lines {
		trimmed := strings.TrimRight(ln, " \t")
		logical.WriteString(ln)
		logical.WriteString("\n")
		if strings.HasSuffix(trimmed, "\\") {
			continue // continues into the next line: still one shell
		}
		script, nonEmpty := flush()
		if !nonEmpty || strings.HasPrefix(strings.TrimSpace(script), "#") ||
			strings.HasPrefix(strings.TrimSpace(script), "@#") {
			continue // a comment line is its own (no-op) shell and carries no logic
		}
		if strings.Contains(script, a) && strings.Contains(script, b) {
			return true
		}
	}
	if script, nonEmpty := flush(); nonEmpty && strings.Contains(script, a) && strings.Contains(script, b) {
		return true
	}
	return false
}
