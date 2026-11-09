package main

// install_path_test.go — the installer must PUT THE DIR ON THE PATH, not print advice about it.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHost points HOME and SHELL at a throwaway host, so a test can never touch the operator's own profile.
func fakeHost(t *testing.T, shell string) (home, installDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", shell)
	// USERPROFILE too: os.UserHomeDir reads it on Windows, and this file must compile and behave there.
	t.Setenv("USERPROFILE", home)
	installDir = filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}
	// The install dir must NOT be on this process's PATH, or the whole path is a no-op.
	t.Setenv("PATH", "/usr/bin:/bin")
	return home, installDir
}

// TestTheInstallWritesTheEntryToTheZshLoginProfile is the reported macOS case, end to end.
func TestTheInstallWritesTheEntryToTheZshLoginProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home, installDir := fakeHost(t, "/bin/zsh")

	got := ensureLoginProfilePATH(installDir)
	if !got.Changed {
		t.Fatalf("no entry written (problem=%q) — the operator is left with an unrunnable install and advice", got.Problem)
	}
	if filepath.Base(got.Profile) != ".zprofile" {
		t.Fatalf("wrote %s, want ~/.zprofile — a login shell never reads .zshrc", got.Profile)
	}
	body := readFile(t, filepath.Join(home, ".zprofile"))
	if !strings.Contains(body, `export PATH="`+installDir+`:$PATH"`) {
		t.Fatalf(".zprofile does not export the install dir:\n%s", body)
	}
}

// TestTheEntryIsWrittenOnlyOnce is the idempotency the install needs: a second run must not grow PATH.
func TestTheEntryIsWrittenOnlyOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home, installDir := fakeHost(t, "/bin/zsh")

	if got := ensureLoginProfilePATH(installDir); !got.Changed {
		t.Fatalf("first run wrote nothing (problem=%q)", got.Problem)
	}
	once := readFile(t, filepath.Join(home, ".zprofile"))

	got := ensureLoginProfilePATH(installDir)
	if got.Changed {
		t.Fatal("the second run appended again — repeated installs would grow PATH without bound")
	}
	if got.Profile == "" {
		t.Error("the second run did not report where the entry already lives, so the summary cannot say it converged")
	}
	if twice := readFile(t, filepath.Join(home, ".zprofile")); twice != once {
		t.Fatalf("the profile changed on a no-op run:\n--- first ---\n%s\n--- second ---\n%s", once, twice)
	}
	if n := strings.Count(once, installDir); n != 1 {
		t.Fatalf("the install dir appears %d times, want exactly 1:\n%s", n, once)
	}
}

// TestAnExistingProfileIsAppendedNotOverwritten: the operator's own profile is theirs.
func TestAnExistingProfileIsAppendedNotOverwritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home, installDir := fakeHost(t, "/bin/zsh")
	const theirs = "# my settings\nexport EDITOR=vim"
	if err := os.WriteFile(filepath.Join(home, ".zprofile"), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ensureLoginProfilePATH(installDir); !got.Changed {
		t.Fatalf("nothing written (problem=%q)", got.Problem)
	}
	body := readFile(t, filepath.Join(home, ".zprofile"))
	if !strings.Contains(body, "export EDITOR=vim") || !strings.Contains(body, "# my settings") {
		t.Fatalf("the operator's own profile content was lost:\n%s", body)
	}
	if !strings.Contains(body, installDir) {
		t.Fatalf("the entry was not added:\n%s", body)
	}
}

// TestBashPrefersTheProfileItAlreadyReads is the SHADOWING guard: bash login reads the FIRST existing file of
// .bash_profile, .bash_login, .profile. Creating .bash_profile over an existing .profile would hide it.
func TestBashPrefersTheProfileItAlreadyReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	for _, existing := range []string{".profile", ".bash_login", ".bash_profile"} {
		t.Run(existing, func(t *testing.T) {
			home, installDir := fakeHost(t, "/bin/bash")
			if err := os.WriteFile(filepath.Join(home, existing), []byte("# existing\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			got := ensureLoginProfilePATH(installDir)
			if !got.Changed {
				t.Fatalf("nothing written (problem=%q)", got.Problem)
			}
			if filepath.Base(got.Profile) != existing {
				t.Fatalf("wrote %s, but bash login reads %s first — the operator's own file would be shadowed",
					filepath.Base(got.Profile), existing)
			}
			// And no shadowing file was created alongside it.
			for _, other := range []string{".profile", ".bash_login", ".bash_profile"} {
				if other == existing {
					continue
				}
				if _, err := os.Stat(filepath.Join(home, other)); err == nil {
					t.Fatalf("created %s, which shadows the %s bash would otherwise read", other, existing)
				}
			}
		})
	}
}

// TestBashCreatesBashProfileOnABareHost covers the fresh machine: none of the chain exists.
func TestBashCreatesBashProfileOnABareHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home, installDir := fakeHost(t, "/bin/bash")

	got := ensureLoginProfilePATH(installDir)
	if !got.Changed || filepath.Base(got.Profile) != ".bash_profile" {
		t.Fatalf("want a new ~/.bash_profile written, got changed=%v profile=%q problem=%q",
			got.Changed, got.Profile, got.Problem)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".bash_profile")), installDir) {
		t.Fatal("~/.bash_profile does not carry the entry")
	}
}

// TestALoginShellsDashPrefixedNameIsRecognised: a login shell reports itself as "-zsh", not "zsh".
func TestALoginShellsDashPrefixedNameIsRecognised(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	_, installDir := fakeHost(t, "-zsh")
	got := ensureLoginProfilePATH(installDir)
	if !got.Changed || filepath.Base(got.Profile) != ".zprofile" {
		t.Fatalf("a dash-prefixed login shell name was not recognised: changed=%v profile=%q problem=%q",
			got.Changed, got.Profile, got.Problem)
	}
}

// TestAnUnrecognisedShellIsToldSoRatherThanGuessed: fish and everything else must not get a stray file.
func TestAnUnrecognisedShellIsToldSoRatherThanGuessed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home, installDir := fakeHost(t, "/usr/bin/fish")

	got := ensureLoginProfilePATH(installDir)
	if got.Changed {
		t.Fatal("a file was written for a shell whose profile this build does not know")
	}
	if got.Problem == "" {
		t.Fatal("no problem recorded, so the summary would silently say nothing about an install dir that is not on PATH")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && e.Name() != ".local" {
			t.Fatalf("wrote %s for an unrecognised shell — guessing at a profile is worse than saying so", e.Name())
		}
	}
}

// TestTheEntryIsQuotedBecauseADirectoryMayHaveASpace pins the quoting: an unquoted entry would export two
// broken fragments instead of one usable path.
func TestTheEntryIsQuotedBecauseADirectoryMayHaveASpace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	dir := filepath.Join(home, "My Apps", ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := ensureLoginProfilePATH(dir); !got.Changed {
		t.Fatalf("nothing written (problem=%q)", got.Problem)
	}
	body := readFile(t, filepath.Join(home, ".zprofile"))
	if !strings.Contains(body, `export PATH="`+dir+`:$PATH"`) {
		t.Fatalf("a directory with a space was not quoted:\n%s", body)
	}
}

// TestADirectoryAlreadyOnPathIsLeftAlone: the common reinstall on a configured host writes nothing.
func TestADirectoryAlreadyOnPathIsLeftAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	// PATH entries are compared EXACTLY, so build a realistic one rather than a substring.
	t.Setenv("PATH", "/usr/bin:/bin:"+filepath.Join(home, ".local", "bin"))

	got := ensureLoginProfilePATH(filepath.Join(home, ".local", "bin"))
	if !got.OnPath || got.Changed {
		t.Fatalf("an install dir already on PATH was rewritten: onPath=%v changed=%v", got.OnPath, got.Changed)
	}
	if _, err := os.Stat(filepath.Join(home, ".zprofile")); !os.IsNotExist(err) {
		t.Fatal("wrote a profile for a directory that was already on PATH")
	}
}

// TestTheSummaryReportsTheEntryAsDone is the OUTWARD claim: what a person reads must say it happened,
// and must not fall back to advice it has already acted on.
func TestTheSummaryReportsTheEntryAsDone(t *testing.T) {
	out := captureStdout(t, func() {
		printPathHint(pathHint{Dir: "/home/u/.local/bin", Profile: "/home/u/.zprofile", Changed: true}, "/home/u/.local/bin")
	})
	if !strings.Contains(out, "Added /home/u/.local/bin to PATH in /home/u/.zprofile") {
		t.Fatalf("the summary does not say the entry was added:\n%s", out)
	}
	if strings.Contains(out, "export PATH=") {
		t.Fatalf("the summary still prints the manual line after doing the work itself:\n%s", out)
	}

	converged := captureStdout(t, func() {
		printPathHint(pathHint{Dir: "/home/u/.local/bin", Profile: "/home/u/.zprofile"}, "/home/u/.local/bin")
	})
	if !strings.Contains(converged, "already on your PATH via /home/u/.zprofile") {
		t.Fatalf("a converged install does not say where the entry lives:\n%s", converged)
	}

	// The fallback must SURVIVE: a host we could not handle stays actionable, and carries the reason.
	failed := captureStdout(t, func() {
		printPathHint(pathHint{Dir: "/home/u/.local/bin", Problem: "unrecognised shell \"fish\""}, "/home/u/.local/bin")
	})
	if !strings.Contains(failed, "export PATH=") {
		t.Fatalf("the advice fallback was lost, so an unhandleable host is left with no instruction:\n%s", failed)
	}
	if !strings.Contains(failed, "unrecognised shell") {
		t.Fatalf("the reason was dropped, so the operator cannot tell why the installer did not do it:\n%s", failed)
	}
}

// TestTheLauncherOnlyRemedyAlsoFixesThePath closes the seam the summary's own advice points at.
func TestTheLauncherOnlyRemedyAlsoFixesThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login profiles only")
	}
	home, installDir := fakeHost(t, "/bin/zsh")
	link := filepath.Join(installDir, "orch")
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := reportLauncherOnly(orchLauncher{Path: link, Installed: true, Method: "symlink"}, installDir); err != nil {
			t.Fatalf("--install-launcher failed: %v", err)
		}
	})

	body, err := os.ReadFile(filepath.Join(home, ".zprofile"))
	if err != nil {
		t.Fatalf("no profile was written by the remedy: %v", err)
	}
	if !strings.Contains(string(body), installDir) {
		t.Fatalf("--install-launcher placed the launcher and left the dir off PATH — the remedy the summary "+
			"names does not fix what it is for. Output:\n%s", out)
	}
	if !strings.Contains(out, "Added "+installDir+" to PATH") {
		t.Fatalf("the remedy did not say it had fixed the PATH:\n%s", out)
	}
}
