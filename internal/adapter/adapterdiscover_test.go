package adapter

// adapterdiscover_test.go — ASK THE OS, AND SURFACE AMBIGUITY INSTEAD OF RESOLVING IT.
//
// The reported failures that motivate this:
//
//	"I reinstalled on windows and I am still getting this error ... opencode CLI should be installed on PATH
//	 now. I can run it from anywhere."
//
// Both halves are true: the shell finds it, the plane does not, because a long-lived plane's PATH is the
// snapshot it started with. And separately: an install by ANY means we did not anticipate (Chocolatey, nix, a
// portable zip) has no entry in our candidate list, so it is "not found" through no fault of the operator.
//
// These tests pin the two answers — a PATH read from the SOURCE rather than a snapshot, and LookPath against it
// — plus the two traps: a shim must never be offered, and two installs must be SURFACED rather than silently
// resolved.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withPathDirs makes freshPathDirs return a controlled set, restoring the real one afterwards.
//
// It swaps the platform hook rather than the filesystem, so the POSIX and Windows seams are both testable from
// one host — which is the point of the seam existing.
func withPathDirs(t *testing.T, dirs ...string) {
	t.Helper()
	prev := freshPathDirsHook
	freshPathDirsHook = func() []string { return dirs }
	t.Cleanup(func() { freshPathDirsHook = prev })
}

// TestDiscoversWhereverItIsOnThePath is the agnostic claim: an install the candidate lists have never heard of,
// found because the OS's PATH names its directory.
func TestDiscoversWhereverItIsOnThePath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, binaryName(runtime.GOOS, "claude"))
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, dir)

	env := func(string) string { return "" }
	got := DiscoverKind(runtime.GOOS, t.TempDir(), "claude", env)
	if len(got) != 1 || got[0] != bin {
		t.Fatalf("DiscoverKind = %v, want the PATH-only install %q — a location no candidate list knows about "+
			"must still be found", got, bin)
	}
}

// TestDiscoveryFindsSomethingTheSnapshotMissed is the reported bug, directly: the PROCESS PATH does not have the
// directory, the SOURCE says it does, and discovery must use the source.
func TestDiscoveryFindsSomethingTheSnapshotMissed(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, binaryName(runtime.GOOS, "opencode"))
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, dir)

	// The process's own PATH deliberately does NOT contain dir — that is the plane-started-before-install case.
	env := func(k string) string {
		if k == "PATH" {
			return t.TempDir() // a directory that holds nothing
		}
		return ""
	}
	got := DiscoverKind(runtime.GOOS, t.TempDir(), "opencode", env)
	if len(got) != 1 || got[0] != bin {
		t.Fatalf("DiscoverKind = %v, want %q — the SOURCE path must be searched, not only the process snapshot",
			got, bin)
	}
}

// TestDiscoveryNeverOffersAShim is the trap LookPath has and we must not inherit: it returns .cmd/.bat/.ps1
// because the CMD shell can run them, and Go cannot.
func TestDiscoveryNeverOffersAShim(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"opencode.cmd", "opencode.ps1", "opencode.bat"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("@echo off"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	withPathDirs(t, dir)
	if got := DiscoverKind(runtime.GOOS, t.TempDir(), "opencode", func(string) string { return "" }); len(got) != 0 {
		t.Fatalf("DiscoverKind offered %v — a shim cannot be executed by Go, so offering it moves the failure "+
			"to the first dispatch", got)
	}
}

// TestOverrideIsNotADiscovery keeps provenance honest: ORCHICON_*_BIN is a decision, and mixing it into "what
// did the OS find" makes that question unanswerable.
func TestOverrideIsNotADiscovery(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, dir) // empty of adapters
	env := func(k string) string {
		if k == BinEnv("claude") {
			return other
		}
		return ""
	}
	if got := DiscoverKind(runtime.GOOS, t.TempDir(), "claude", env); len(got) != 0 {
		t.Fatalf("DiscoverKind = %v, want none — the override is reported by Candidates with its own source", got)
	}
	// …and Candidates DOES report it, first.
	cands := Candidates(runtime.GOOS, t.TempDir(), "claude", env)
	if len(cands) == 0 || cands[0].Source != "override" {
		t.Fatalf("Candidates = %+v, want the override first", cands)
	}
}

// TestTwoInstallsAreSurfacedNotChosen is the case that must never resolve silently: two installs are two
// versions and possibly two sets of credentials.
func TestTwoInstallsAreSurfacedNotChosen(t *testing.T) {
	discovered := t.TempDir()
	a := filepath.Join(discovered, binaryName(runtime.GOOS, "claude"))
	if err := os.WriteFile(a, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, discovered)

	// A SECOND install, in the known-install shape for the fake home.
	home := t.TempDir()
	declared := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(declared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(declared, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	cands := Candidates(runtime.GOOS, home, "claude", func(string) string { return "" })
	if len(cands) != 2 {
		t.Fatalf("Candidates = %+v, want BOTH installs", cands)
	}
	if !Ambiguous(cands) {
		t.Fatal("two installs were not reported as ambiguous — picking one for the operator is how a session " +
			"signs in as the wrong account")
	}
	// Provenance differs, so the operator can tell them apart.
	sources := map[string]bool{}
	for _, c := range cands {
		sources[c.Source] = true
	}
	if !sources["discovered"] || !sources["known"] {
		t.Fatalf("sources = %v, want one discovered and one known", sources)
	}
}

// TestSingleInstallIsNotAmbiguous keeps the common case frictionless.
func TestSingleInstallIsNotAmbiguous(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, binaryName(runtime.GOOS, "opencode")), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, dir)
	cands := Candidates(runtime.GOOS, t.TempDir(), "opencode", func(string) string { return "" })
	if len(cands) != 1 || Ambiguous(cands) {
		t.Fatalf("Candidates = %+v, want exactly one and not ambiguous", cands)
	}
}

// TestCandidatesCarryContainerReadiness is what the explicit surface needs to warn: a Windows .exe resolves
// perfectly and cannot run in a Linux container.
func TestCandidatesCarryContainerReadiness(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, dir)
	cands := Candidates("windows", t.TempDir(), "claude", func(k string) string {
		if k == "PATH" {
			return dir
		}
		return ""
	})
	if len(cands) == 0 {
		t.Fatal("no candidate found")
	}
	if cands[0].ContainerOK {
		t.Fatal("a .exe was reported container-ready")
	}
	if !strings.Contains(cands[0].Why, "Linux") {
		t.Errorf("the candidate must carry the reason: %q", cands[0].Why)
	}
}

// TestSplitPersistedPathHandlesTheRegistryForms guards the lifted parser, including the case its own comment
// records: a variable spelled differently in the registry than in the process.
func TestSplitPersistedPathHandlesTheRegistryForms(t *testing.T) {
	environ := []string{`SystemRoot=C:\Windows`, `USERPROFILE=C:\Users\op`}
	got := SplitPersistedPath(`%SystemRoot%\system32;C:\bin;%NOT_SET%\x;%USERPROFILE%\.local\bin`, environ)
	want := []string{`C:\Windows\system32`, `C:\bin`, `C:\Users\op\.local\bin`}
	if len(got) != len(want) {
		t.Fatalf("SplitPersistedPath = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SplitPersistedPath = %v, want %v", got, want)
		}
	}
}

// TestTheNativeKindIsNeverDiscovered pins a bug the runtime tests caught: the native adapter IS this product,
// so "discovery" found the operator's own orchicon binary on PATH and derived an adapter mount for it.
func TestTheNativeKindIsNeverDiscovered(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orchicon"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPathDirs(t, dir)
	env := func(k string) string {
		if k == "PATH" {
			return dir
		}
		return ""
	}
	if got := DiscoverKind(runtime.GOOS, t.TempDir(), "orchicon", env); len(got) != 0 {
		t.Fatalf("DiscoverKind(native) = %v — the native adapter is the product binary, mounted by the daemon "+
			"from its own path, never discovered as an adapter install", got)
	}
}
