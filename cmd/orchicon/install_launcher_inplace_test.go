package main

// install_launcher_inplace_test.go — WHEN orch AND orchicon SHARE A DIRECTORY, THE LAUNCHER *IS* THE BINARY.
//
// THE REGRESSION THIS PINS. Reported from macOS after the ownership fix: a fresh install left
//
//	orch version → Too many levels of symbolic links
//
// and the 70 MB binary gone. The release layout puts `orchicon` and `orch` side by side, and the shell
// installer installs them into the same directory — so link and sibling are the SAME PATH. The clobber guard
// recognised the existing `orch` as ours, removed it, and then symlinked `sibling` (that very path) to
// `link`: a symlink pointing at itself, in place of the only copy of the binary.
//
// WHY THE EXISTING TESTS MISSED IT: their fixture puts the build dir and the install dir in different
// directories, so link != sibling and the remove-and-relink is correct. The default layout was never
// exercised. That is the whole bug — not a subtle edge, the ordinary case.
//
// These tests EXECUTE the launcher rather than inspecting it: "orch runs" is the property, and a symlink
// that resolves is indistinguishable from a regular file to every check short of running it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// inPlaceFixture is the DEFAULT layout: orchicon and a real orch in the same install dir, exactly as the
// release archive and scripts/install.sh leave them.
func inPlaceFixture(t *testing.T) (exe, sibling, link, marker string) {
	t.Helper()
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(installDir, "orchicon"+exeSuffix())
	sibling = filepath.Join(installDir, "orch"+exeSuffix())
	link = filepath.Join(installDir, "orch"+exeSuffix())

	marker = "orch ran from " + installDir
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real, runnable stand-in for the 70 MB binary: if it survives, running it prints the marker.
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\necho '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, sibling, link, marker
}

// runLauncher executes the launcher and returns its stdout — the only assertion that proves it works.
func runLauncher(t *testing.T, path string) (string, error) {
	t.Helper()
	out, err := exec.Command(path).Output()
	return strings.TrimSpace(string(out)), err
}

// THE REPORTED BUG: a fresh install over the default layout must leave a runnable orch.
func TestLauncherInPlaceStaysRunnable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a shell script as the stand-in binary")
	}
	exe, _, link, marker := inPlaceFixture(t)

	var got orchLauncher
	captureStdout(t, func() { got = installOrchLauncherFrom(exe, filepath.Dir(link)) })

	if !got.Installed {
		t.Fatalf("the launcher was reported missing when orch was already in place: %s", got.Problem)
	}
	// The self-referential symlink is the exact damage: it replaces the binary with a link to itself.
	if st, err := os.Lstat(link); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if target, _ := os.Readlink(link); filepath.Clean(target) == filepath.Clean(link) {
			t.Fatalf("orch was replaced by a symlink TO ITSELF (%s -> %s) — the binary is gone and `orch` "+
				"fails with \"Too many levels of symbolic links\"", link, target)
		}
	}
	out, err := runLauncher(t, link)
	if err != nil {
		t.Fatalf("the launcher does not execute after install: %v", err)
	}
	if out != marker {
		t.Fatalf("the launcher ran but printed %q, want %q — it is no longer this install's orch", out, marker)
	}
}

// A REINSTALL over a real-file orch must converge, not churn: report already-installed, touch nothing.
func TestReinstallOverARealFileOrchReportsAlreadyInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a shell script as the stand-in binary")
	}
	exe, sibling, link, _ := inPlaceFixture(t)
	before, err := os.Stat(sibling)
	if err != nil {
		t.Fatal(err)
	}

	var got orchLauncher
	out := captureStdout(t, func() { got = installOrchLauncherFrom(exe, filepath.Dir(link)) })

	if !got.Installed {
		t.Fatalf("a reinstall over our own in-place orch must report it installed: %s", got.Problem)
	}
	if !strings.Contains(out, "already installed") {
		t.Errorf("a reinstall should say the launcher is already installed:\n%s", out)
	}
	// Nothing was rewritten: same inode, and still not a symlink.
	after, err := os.Stat(sibling)
	if err != nil {
		t.Fatalf("the binary was removed: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the binary was replaced — a no-op refresh must not rewrite the file it found")
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink != 0 {
		t.Error("a second install turned our own binary into a symlink")
	}
}

// IN THE IN-PLACE LAYOUT "FOREIGN" IS NOT A DISTINGUISHABLE STATE, and the safe action is the same either
// way. The ownership test compares the candidate with sibling — which here is the same path, so it always
// matches. That is not a hole: the installer places nothing at that path, because whatever is there already
// occupies the location its launcher would. What must hold is that it is left EXACTLY as found.
//
// The genuine foreign case — a launcher this install did not place, in a DIFFERENT directory — is the one
// where a wrong choice can hijack someone else's binary, and it is covered by
// TestAForeignLauncherIsStillRefused.
func TestAnOrchAlreadyInPlaceIsLeftByteForByte(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a shell script as the stand-in binary")
	}
	exe, sibling, link, _ := inPlaceFixture(t)
	foreign := []byte("#!/bin/sh\necho 'an orch this installer did not place'\n")
	if err := os.WriteFile(sibling, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sibling)
	if err != nil {
		t.Fatal(err)
	}

	var got orchLauncher
	out := captureStdout(t, func() { got = installOrchLauncherFrom(exe, filepath.Dir(link)) })

	if !got.Installed {
		t.Fatalf("an orch already at the launcher path is a runnable launcher: %s", got.Problem)
	}
	after, err := os.Stat(sibling)
	if err != nil {
		t.Fatalf("the file was removed: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the file was replaced — in this layout it IS the launcher, so replacing it destroys the only copy")
	}
	onDisk, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read the launcher: %v", err)
	}
	if string(onDisk) != string(foreign) {
		t.Fatalf("the file's content changed by an install that should have placed nothing")
	}
	if !strings.Contains(out, "already installed") {
		t.Errorf("the operator should be told it is already there:\n%s", out)
	}
}

// --- claim 1: the PATH entry -------------------------------------------------

// A SUBSTRING MATCH ON THE DIRECTORY IS NOT "THE ENTRY IS THERE". The guard has to recognise the LINE it
// would write, or a profile that merely mentions the directory — a commented-out line, a sibling path like
// ~/.local/bin-old, a comment naming the install — suppresses the entry entirely and the operator gets the
// silent no-op this whole mechanism exists to remove.
func TestProfileGuardMatchesTheLineNotASubstring(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		want    bool
	}{
		{"empty", "", false},
		{"the exact line", "export PATH=\"/h/u/.local/bin:$PATH\"\n", true},
		{"the exact line, among others", "alias x=y\nexport PATH=\"/h/u/.local/bin:$PATH\"\n", true},
		{"commented out", "# export PATH=\"/h/u/.local/bin:$PATH\"\n", false},
		{"a sibling path that contains it", "export PATH=\"/h/u/.local/bin-old:$PATH\"\n", false},
		{"mentioned in prose", "# I keep tools in /h/u/.local/bin\n", false},
		{"quoted differently but equivalent", "export PATH=\"/h/u/.local/bin:${PATH}\"\n", true},
	}
	for _, c := range cases {
		if got := mentionsDir(c.profile, "/h/u/.local/bin"); got != c.want {
			t.Errorf("%s: mentionsDir = %v, want %v — profile %q", c.name, got, c.want, c.profile)
		}
	}
}

// THE ENTRY IS APPENDED ONCE, AND A SECOND RUN SAYS NOTHING CHANGED.
func TestPATHEntryIsWrittenOnceAndIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	// The dir must not look already-exported to this process, or nothing is written.
	t.Setenv("PATH", "/usr/bin:/bin")

	first := ensureLoginProfilePATH(dir)
	if !first.Changed {
		t.Fatalf("the first run must add the entry: %+v", first)
	}
	second := ensureLoginProfilePATH(dir)
	if second.Changed {
		t.Fatal("a second run appended the entry again — reinstall churn is the bug this guard prevents")
	}
	body, err := os.ReadFile(filepath.Join(home, ".zprofile"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), dir); n != 1 {
		t.Fatalf("the profile carries the install dir %d times, want exactly 1:\n%s", n, body)
	}
}

// A DRY RUN CLAIMS NOTHING AND WRITES NOTHING. The installer's --dry-run must not create the profile entry
// it is only describing, and must not say it did.
func TestPATHEntryIsNotWrittenWhenTheDirIsAlreadyOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", dir+":/usr/bin:/bin")

	got := ensureLoginProfilePATH(dir)
	if !got.OnPath {
		t.Fatalf("a dir already on PATH must be reported OnPath, not written: %+v", got)
	}
	if got.Changed {
		t.Fatal("a dir already on PATH was written to the profile")
	}
	if _, err := os.Stat(filepath.Join(home, ".zprofile")); err == nil {
		t.Fatal("the profile was created for a directory that is already on PATH")
	}
}

// SHELL IS NOT ALWAYS SET, and on macOS a non-interactive context (a Finder-launched terminal, some CI) has
// no SHELL at all — which used to leave the entry unwritten and fall back to the advice that caused this
// report. The login shell is resolvable from the directory service regardless.
func TestLoginShellIsResolvedWithoutSHELL(t *testing.T) {
	t.Setenv("SHELL", "")
	shell := resolveLoginShell()
	if shell == "" {
		if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
			t.Fatalf("could not resolve a login shell on %s with SHELL unset — the PATH entry would silently "+
				"go unwritten and the operator gets advice instead", runtime.GOOS)
		}
		t.Skip("no passwd entry to read on this platform")
	}
	if _, _, ok := loginProfileFor(shell, t.TempDir()); !ok {
		t.Errorf("resolved login shell %q has no known profile — SHELL-empty hosts would still be left "+
			"without the entry", shell)
	}
}
