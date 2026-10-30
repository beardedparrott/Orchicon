package main

// install_launcher_outcome_test.go — THE LAUNCHER MUST NOT BE REPORTED WHEN IT IS NOT THERE.
//
// Reported from a real non-elevated Windows box, where the install finished healthy and the summary said:
//
//	warning: could not symlink orch launcher: A required privilege is not held by the client.
//	  orch launcher: C:\Users\operator\.local\bin/orch (remote TUI client)
//
// The warning was true and the summary was false, and the summary is what a person reads. os.Symlink needs
// SeCreateSymbolicLinkPrivilege, which an ordinary account without Developer Mode has not, so on stock Windows
// the launcher was NEVER created — and the one line that should have said so named it.
//
// These tests pin the two halves of the fix: the install REPORTS what it did, and it COPIES when it cannot link.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/containerengine"
)

// TestLauncherOutcomeIsReportedNotAssumed is the lie itself.
//
// With no sibling binary there is nothing to install, and the outcome must therefore say so. Before the fix a
// caller had no way to know: the function only printed, and the summary printed its own unconditional line.
func TestLauncherOutcomeIsReportedNotAssumed(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "orchicon")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(dir, "install")

	got := installOrchLauncherFrom(exe, installDir)
	if got.Installed {
		t.Fatal("Installed is true with no sibling binary — this is exactly the summary lie: a launcher that is " +
			"not there must never be reported as there")
	}
	if got.Problem == "" {
		t.Error("no Problem recorded — the summary would print NOT installed with an empty reason")
	}
	if _, err := os.Stat(filepath.Join(installDir, "orch")); err == nil {
		t.Fatal("a launcher was placed despite there being no sibling binary to place")
	}
}

// TestLauncherOutcomeNamesTheMechanism pins that a successful install says HOW, because on Windows the mechanism
// is a copy and the summary must not imply a symlink exists.
func TestLauncherOutcomeNamesTheMechanism(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	installDir := filepath.Join(dir, "install")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "orchicon")
	sibling := filepath.Join(binDir, "orch")
	if runtime.GOOS == "windows" {
		sibling += ".exe"
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := installOrchLauncherFrom(exe, installDir)
	if !got.Installed {
		t.Fatalf("launcher not installed: %s", got.Problem)
	}
	if got.Method != "symlink" && got.Method != "copy" {
		t.Fatalf("Method = %q, want symlink or copy — the summary prints it", got.Method)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Fatalf("Installed is true but nothing is at %s: %v", got.Path, err)
	}
}

// TestLauncherCopyFallbackWorksWhenSymlinkIsImpossible is the stock-Windows case, exercised on EVERY platform.
//
// It calls the copy path directly rather than trying to make os.Symlink fail, because the failure being defended
// against is a PRIVILEGE the test host may well have — so a test that waited for it would pass on CI and prove
// nothing about the machine the bug came from. What must hold everywhere is that a copy produces a runnable
// launcher and reports itself as one.
func TestLauncherCopyFallbackWorksWhenSymlinkIsImpossible(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "orch")
	body := []byte("#!/bin/sh\necho orch\n")
	if err := os.WriteFile(src, body, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "install", "orch")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyLauncher(src, dst); err != nil {
		t.Fatalf("copyLauncher: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read the copied launcher: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("copied content = %q, want %q", got, body)
	}
	// THE EXECUTE BIT IS THE POINT ON POSIX: a copy without it exists and cannot run, which is the same
	// user-facing outcome as no launcher, only quieter.
	if runtime.GOOS != "windows" {
		st, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Fatalf("the copied launcher is not executable: mode %v", st.Mode().Perm())
		}
	}
}

// TestReinstallRefreshesOurOwnCopy is the converge rule for the copy mechanism.
//
// The foreign-file guard is important (a build-dir install must never hijack a release launcher), but a guard that
// cannot tell OUR copy from someone else's makes the second install refuse to refresh what the first install put
// there. isOurCopy is what separates the two, and this pins it in both directions.
func TestReinstallRefreshesOurOwnCopy(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "bin", "orch")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("the real orch binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	ourCopy := filepath.Join(dir, "install", "orch")
	if err := os.MkdirAll(filepath.Dir(ourCopy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyLauncher(sibling, ourCopy); err != nil {
		t.Fatal(err)
	}
	if !isOurCopy(ourCopy, sibling) {
		t.Fatal("a byte-identical copy of the sibling was not recognised as ours — a reinstall would refuse to " +
			"refresh the launcher it placed itself")
	}

	// Someone ELSE's file of the same name is not ours, even at the same path.
	foreign := filepath.Join(dir, "install-other", "orch")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("a completely different binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isOurCopy(foreign, sibling) {
		t.Fatal("a DIFFERENT file was claimed as ours — installing must never relocate someone else's launcher")
	}
	// And a different NAME at identical content is not ours either: the launcher is a specific file.
	if isOurCopy(filepath.Join(dir, "install", "somethingelse"), sibling) {
		t.Fatal("matched on content alone despite a different file name")
	}
}

// TestLauncherOnlyIsAConsumedFlag keeps --install-launcher from becoming the next `unknown flag "--console"`.
//
// `rest` reaches parseInstanceTarget, which rejects every flag it does not recognize, so a flag the install acts
// on but does not consume kills the run it was added to save.
func TestLauncherOnlyIsAConsumedFlag(t *testing.T) {
	args := []string{"install", "--install-launcher", "--instance", "dev"}
	flags, rest, err := parseInstallFlags(args)
	if err != nil {
		t.Fatalf("parseInstallFlags: %v", err)
	}
	if !flags.LauncherOnly {
		t.Error("--install-launcher was not recorded")
	}
	for _, a := range rest {
		if strings.Contains(a, "install-launcher") {
			t.Fatalf("--install-launcher reached the instance parser, which will reject it: rest=%v", rest)
		}
	}
	if !hasLauncherOnlyFlag(args) {
		t.Error("hasLauncherOnlyFlag missed it, so a launcher-only run would relaunch itself into a console that " +
			"flashes and disappears")
	}
}

// TestLauncherProblemIsWhatTheSummaryPrints guards the two places against drifting apart.
//
// The summary refuses to print a launcher line when the outcome is not installed, and falls through to the
// problem. A reason is what makes that refusal useful rather than mysterious.
func TestLauncherProblemIsWhatTheSummaryPrints(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "orchicon") // no sibling anywhere
	got := installOrchLauncherFrom(exe, filepath.Join(dir, "install"))
	if got.Installed || got.Problem == "" {
		t.Fatalf("want not-installed WITH a problem, got installed=%v problem=%q", got.Installed, got.Problem)
	}
}

// TestSummaryReportsTheLauncherItActuallyInstalled is the OUTWARD-FACING claim, in both directions: the box a
// person reads must name the launcher when it is there, and must NOT name it when it is not.
//
// This is the assertion the whole change exists for. Everything above tests the decision; this tests what the
// decision turns into on screen, which is the only part an operator ever sees.
func TestSummaryReportsTheLauncherItActuallyInstalled(t *testing.T) {
	summary := func(l orchLauncher) string {
		return captureStdout(t, func() {
			printInstallInfo("dev", residencyHost, "n", "v", "/tmp/s", "http://localhost:8080/healthz",
				"img", "/home/u/.local/bin", l, map[string]int{}, false, true, containerengine.Status{})
		})
	}

	present := summary(orchLauncher{Path: "/home/u/.local/bin/orch", Installed: true, Method: "copy"})
	if !strings.Contains(present, "orch launcher: /home/u/.local/bin/orch (remote TUI client, copy)") {
		t.Fatalf("a launcher that WAS installed is not named, with its mechanism:\n%s", present)
	}
	if strings.Contains(present, "NOT installed") {
		t.Fatalf("the summary says a present launcher is missing:\n%s", present)
	}

	// THE REPORTED BUG, verbatim: the launcher could not be placed and the summary named it anyway.
	missing := summary(orchLauncher{Path: "/home/u/.local/bin/orch", Problem: "could not place it"})
	if strings.Contains(missing, "orch launcher: /home/u/.local/bin/orch (remote TUI client") {
		t.Fatalf("the summary claimed a launcher that was not installed:\n%s", missing)
	}
	if !strings.Contains(missing, "NOT installed") {
		t.Fatalf("the summary did not say the launcher is missing:\n%s", missing)
	}
	if !strings.Contains(missing, "could not place it") {
		t.Fatalf("the summary did not carry the reason, so the operator has a gap and no cause:\n%s", missing)
	}
}
