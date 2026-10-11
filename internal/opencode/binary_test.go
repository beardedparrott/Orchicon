package opencode

// binary_test.go pins the ONE resolver's contract: the env override, PATH, the
// container-visible $HOME candidates, and the visibility check that pairs with
// 'present'.
//
// The cases are chosen from real reports rather than invented: the sudo/npm
// install that the plane can see but its containers cannot, the override the
// error message advertises but two of three call sites ignored, and the empty
// $HOME that must fail loudly rather than resolve to something unusable.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// emptyPathPointingHome makes every probe miss: no PATH, and a $HOME with none
// of the candidate locations in it.
func emptyPathPointingHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PATH", filepath.Join(home, "no-such-bin"))
	t.Setenv("HOME", home)
	t.Setenv(OpenCodeBinEnv, "")
	return home
}

// writeFakeOpenCode creates an executable-looking regular file at home/rel.
func writeFakeOpenCode(t *testing.T, home, rel string) string {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestResolveOpenCodeBinaryProbesTheContainerVisibleHomeLocations is the core
// contract: every $HOME candidate a runtime container can reach is probed, and
// the FIRST one wins.
func TestResolveOpenCodeBinaryProbesTheContainerVisibleHomeLocations(t *testing.T) {
	for _, rel := range openCodeRelCandidates {
		t.Run(rel, func(t *testing.T) {
			home := emptyPathPointingHome(t)
			want := writeFakeOpenCode(t, home, rel)

			got, err := resolveOpenCodeBinary()
			if err != nil {
				t.Fatalf("resolve failed for %s: %v", rel, err)
			}
			if got != want {
				t.Fatalf("resolved %q, want %q", got, want)
			}
		})
	}
}

// TestResolveOpenCodeBinaryPrefersTheEnvOverride pins the escape hatch the
// plane's error message advertises. Before this resolver existed, only the
// discovery call site honoured it, so the operator who followed that advice got
// working discovery and a broken spawn path.
func TestResolveOpenCodeBinaryPrefersTheEnvOverride(t *testing.T) {
	home := emptyPathPointingHome(t)
	// A perfectly good install that must LOSE to the explicit override.
	writeFakeOpenCode(t, home, filepath.Join(".opencode", "bin", "opencode"))
	override := writeFakeOpenCode(t, home, filepath.Join("elsewhere", "opencode"))
	t.Setenv(OpenCodeBinEnv, override)

	got, err := resolveOpenCodeBinary()
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got != override {
		t.Fatalf("resolved %q, want the explicit override %q", got, override)
	}
}

// TestResolveOpenCodeBinaryRejectsADeadOverride: an override that names nothing
// is an explicit instruction that is wrong, and must fail with the override
// named rather than being silently ignored in favour of a fallback. The silent
// version is what the discovery wiring did, and it reports 'not found' — sending
// the operator to install something they already have.
func TestResolveOpenCodeBinaryRejectsADeadOverride(t *testing.T) {
	home := emptyPathPointingHome(t)
	writeFakeOpenCode(t, home, filepath.Join(".opencode", "bin", "opencode"))
	t.Setenv(OpenCodeBinEnv, filepath.Join(home, "typo", "opencode"))

	bin, err := resolveOpenCodeBinary()
	if err == nil || bin != "" {
		t.Fatalf("expected ('', err) for a dead override; got bin=%q err=%v", bin, err)
	}
	if !strings.Contains(err.Error(), OpenCodeBinEnv) {
		t.Errorf("the error must name %s so the typo is visible; got: %v", OpenCodeBinEnv, err)
	}
}

// TestResolveOpenCodeBinaryFailsLoudlyWhenNothingIsUsable keeps the original
// contract (the adapter's fail-fast guard keys on (”, err) containing 'opencode
// binary not found') while adding the hint that names a system-wide install.
func TestResolveOpenCodeBinaryFailsLoudlyWhenNothingIsUsable(t *testing.T) {
	emptyPathPointingHome(t)

	bin, err := resolveOpenCodeBinary()
	if err == nil || bin != "" {
		t.Fatalf("expected ('', err) with no binary anywhere; got bin=%q err=%v", bin, err)
	}
	if !strings.Contains(err.Error(), "opencode binary not found") {
		t.Errorf("expected the loud 'binary not found' error, got: %v", err)
	}
	// The non-sudo instruction must survive: sudo is the cause of the class of
	// report this message exists for.
	if !strings.Contains(err.Error(), "never with sudo") {
		t.Errorf("the error must name the sudo trap, got: %v", err)
	}
}

// TestContainerVisibleFollowsTheRuntimeDeclaration is the half that was missing:
// a binary the plane can execute but its runtime containers cannot is the
// green-and-broken case, and it must be DETECTABLE rather than assumed fine.
func TestContainerVisibleFollowsTheRuntimeDeclaration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Every path the runtime daemon declares as an opencode mount must be visible.
	roots := runtimeAdapterMountRoots(home)
	if len(roots) == 0 {
		t.Fatal("the runtime declaration yielded no opencode mount roots — the visibility check would be vacuous")
	}
	for _, root := range roots {
		if !ContainerVisible(filepath.Join(root, "some", "binary")) {
			t.Errorf("a path under the declared mount root %s must be container-visible", root)
		}
	}

	// The canonical INVISIBLE install: the npm prefix under sudo.
	if ContainerVisible(filepath.Join("/usr", "local", "bin", "opencode")) {
		t.Error("/usr/local/bin/opencode must NOT be container-visible — no container mounts it, which is why a sudo install cannot run workflow executions")
	}
}

// TestOpenCodeBinDirsMatchesTheProbedCandidates pins the one-list rule: the
// directories handed to the plane's PATH augmentation are derived from the
// resolver's candidate list, so a prefix can never point somewhere the resolver
// does not look.
func TestOpenCodeBinDirsMatchesTheProbedCandidates(t *testing.T) {
	home := "/home/example"
	dirs := OpenCodeBinDirs(home)
	if len(dirs) == 0 {
		t.Fatal("OpenCodeBinDirs returned nothing — the PATH augmentation would be empty")
	}
	for _, rel := range openCodeRelCandidates {
		want := filepath.Dir(filepath.Join(home, rel))
		found := false
		for _, d := range dirs {
			if d == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("candidate %s has no bin dir in OpenCodeBinDirs — the PATH prefix and the resolver would drift", rel)
		}
	}
	if got := OpenCodeBinDirs(""); got != nil {
		t.Errorf("an empty home must yield no dirs (never /usr/local/bin-style absolutes); got %v", got)
	}
}

// runtimeAdapterMountRoots reads the same declaration ContainerVisible reads, so
// the test asserts against the contract rather than a copy of it.
func runtimeAdapterMountRoots(home string) []string {
	return runtime.AdapterHostMountRoots(home, adapter.DefaultAdapterKind)
}
