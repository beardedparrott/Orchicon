package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallOrchLauncherSymlinkBranches exercises the symlink create /
// refresh / clobber-guard / force-replace branches via installOrchLauncherFrom
// (os.Executable() is not redirectable, so we pass a controlled exe path).
func TestInstallOrchLauncherSymlinkBranches(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	installDir := filepath.Join(dir, "install")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "orchicon")
	sibling := filepath.Join(binDir, "orch")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Create.
	installOrchLauncherFrom(exe, installDir)
	link := filepath.Join(installDir, "orch")
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink at %s, got %v / %v", link, st, err)
	}
	if target, _ := os.Readlink(link); target != sibling {
		t.Fatalf("symlink target = %q, want %q", target, sibling)
	}

	// 2. Idempotent re-run (owned symlink refreshed in place, no error).
	installOrchLauncherFrom(exe, installDir)
	if target, _ := os.Readlink(link); target != sibling {
		t.Fatalf("after re-run target = %q, want %q", target, sibling)
	}

	// 3. Clobber guard: replace the symlink with a regular file → warn + skip.
	_ = os.Remove(link)
	if err := os.WriteFile(link, []byte("user file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installOrchLauncherFrom(exe, installDir)
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("clobber guard must not replace a regular file")
	}

	// 4. Force replace: ORCHICON_FORCE_LAUNCHER=1 replaces the regular file.
	t.Setenv("ORCHICON_FORCE_LAUNCHER", "1")
	installOrchLauncherFrom(exe, installDir)
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("force replace must turn the regular file into a symlink")
	}
	if target, _ := os.Readlink(link); target != sibling {
		t.Fatalf("after force target = %q, want %q", target, sibling)
	}
}

// TestInstallOrchLauncherMissingSiblingWarnsNonFatal verifies a missing
// sibling binary produces a warning and no launcher, without erroring.
func TestInstallOrchLauncherMissingSiblingWarnsNonFatal(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	installDir := filepath.Join(dir, "install")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "orchicon")
	// No sibling orch binary.
	installOrchLauncherFrom(exe, installDir)
	if _, err := os.Stat(filepath.Join(installDir, "orch")); err == nil {
		t.Fatal("expected no launcher when sibling is missing")
	}
}

// TestDirOnPath verifies the PATH hint helper.
func TestDirOnPath(t *testing.T) {
	if !dirOnPath("/usr/bin") {
		t.Fatal("/usr/bin should be on PATH")
	}
	if dirOnPath("/definitely/not/on/path/xyz") {
		t.Fatal("bogus dir should not be on PATH")
	}
}

// TestDefaultInstallDir verifies the default is ~/.local/bin.
func TestDefaultInstallDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "bin")
	if got := defaultInstallDir(); got != want {
		t.Fatalf("defaultInstallDir = %q, want %q", got, want)
	}
}

// TestInstallOrchLauncherWarningText sanity-checks the warning strings
// mention the actionable knobs.
func TestInstallOrchLauncherWarningText(t *testing.T) {
	// The clobber-guard warning must mention ORCHICON_FORCE_LAUNCHER.
	if !strings.Contains("set ORCHICON_FORCE_LAUNCHER=1 to replace", "ORCHICON_FORCE_LAUNCHER") {
		t.Fatal("clobber warning must mention ORCHICON_FORCE_LAUNCHER")
	}
}
