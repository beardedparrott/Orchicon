package main

// install_launcher_copy_test.go — OWNERSHIP OF A COPIED `orch` MUST BE RECOGNISED ON EVERY PLATFORM.
//
// Reported from macOS, and BOTH claims in the installer's warning were false: <home>/.local/bin/orch
// "exists and was not placed there by this install", and "the TUI is missing". The file was from that very
// install and it ran.
//
// WHY IT HAPPENED. scripts/install.sh `mv`s the archive's `orch` into the install dir as a REGULAR FILE. The
// ownership test then recognised a symlink (POSIX) or a byte-identical copy (Windows only) — so on macOS the
// installer could not recognise the file it had just placed, and its clobber guard fired against itself.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// copyFixture lays out a build directory (orchicon + its sibling orch) and an install dir already holding a
// copy of orch, exactly as scripts/install.sh leaves a first install.
func copyFixture(t *testing.T) (exe, sibling, link string) {
	t.Helper()
	dir := t.TempDir()
	buildDir := filepath.Join(dir, "build")
	installDir := filepath.Join(dir, "install")
	for _, d := range []string{buildDir, installDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	exe = filepath.Join(buildDir, "orchicon"+exeSuffix())
	sibling = filepath.Join(buildDir, "orch"+exeSuffix())
	link = filepath.Join(installDir, "orch"+exeSuffix())
	if err := os.WriteFile(sibling, []byte("the orch binary of THIS build"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, sibling, link
}

// TestASameBuildCopyIsRefreshedOnEveryPlatform is the reported bug, in both of its false claims.
func TestASameBuildCopyIsRefreshedOnEveryPlatform(t *testing.T) {
	exe, sibling, link := copyFixture(t)
	if err := copyLauncher(sibling, link); err != nil {
		t.Fatalf("place the copy the shell installer would leave: %v", err)
	}

	var got orchLauncher
	out := captureStdout(t, func() { got = installOrchLauncherFrom(exe, filepath.Dir(link)) })

	if strings.Contains(out, "was not placed there by this install") {
		t.Fatalf("the installer accused its own launcher of being foreign:\n%s", out)
	}
	if !got.Installed {
		t.Fatalf("a byte-identical copy of this build's orch was not recognised: %s", got.Problem)
	}
	if _, err := os.Stat(link); err != nil {
		t.Fatalf("the launcher is not there after the refresh: %v", err)
	}
	// On POSIX the refresh replaces the copy with a symlink, which is the self-updating mechanism; on Windows
	// it stays a copy because symlinks need a privilege an ordinary account may not have.
	if runtime.GOOS != "windows" {
		st, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the copy was not refreshed into a symlink — a launcher that stays a stale file is the "+
				"failure this arm exists to prevent: mode %v", st.Mode())
		}
		if target, _ := os.Readlink(link); target != sibling {
			t.Fatalf("symlink points at %q, want %q", target, sibling)
		}
	}
}

// TestAForeignLauncherIsStillRefused is the OTHER direction, and the reason the guard exists: a build-dir
// install must never hijack a launcher another install — or another build — put there.
func TestAForeignLauncherIsStillRefused(t *testing.T) {
	exe, _, link := copyFixture(t)
	if err := os.WriteFile(link, []byte("a DIFFERENT orch entirely, at a different length"), 0o755); err != nil {
		t.Fatal(err)
	}

	var got orchLauncher
	out := captureStdout(t, func() { got = installOrchLauncherFrom(exe, filepath.Dir(link)) })

	if got.Installed {
		t.Fatal("a foreign launcher was replaced without ORCHICON_FORCE_LAUNCHER — installing must never " +
			"relocate someone else's binary")
	}
	if !strings.Contains(out, "not placed there by this install") {
		t.Fatalf("a genuinely foreign launcher was not named as foreign:\n%s", out)
	}
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the foreign file was removed: %v", err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the foreign file was turned into our symlink")
	}

	// The escape hatch must keep working.
	t.Setenv("ORCHICON_FORCE_LAUNCHER", "1")
	captureStdout(t, func() { got = installOrchLauncherFrom(exe, filepath.Dir(link)) })
	if !got.Installed {
		t.Fatalf("ORCHICON_FORCE_LAUNCHER=1 did not replace the foreign launcher: %s", got.Problem)
	}
}

// TestASameBuildCopyIsRecognisedByContentNotByPlatform pins the defect at its own granularity: the ownership
// test itself, on a copied file, on whatever host this runs.
func TestASameBuildCopyIsRecognisedByContentNotByPlatform(t *testing.T) {
	_, sibling, link := copyFixture(t)
	if err := copyLauncher(sibling, link); err != nil {
		t.Fatal(err)
	}
	if !isOurCopy(link, sibling) {
		t.Fatal("a byte-identical copy of this build's orch was not recognised as ours — the ownership test " +
			"is what decides, and gating it by platform is the bug")
	}
}
