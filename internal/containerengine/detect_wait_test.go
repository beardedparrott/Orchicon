package containerengine

// detect_wait_test.go — AN ENGINE INSTALLED A MOMENT AGO IS NOT ON THIS PROCESS'S PATH.
//
// The failure this covers: `winget install RedHat.Podman` succeeds, `podman --version` works in a NEW shell, and
// the installer — still the process that ran winget — reports "no container engine". install.go then skips the
// runtime daemon (it is gated on the engine), and the operator's container fetch fails on a socket that will never
// exist, after an install that said it worked.
//
// The probe is deliberately narrow, and these tests pin both halves: it must FIND a real install, and it must NOT
// accept a name that merely resolves on PATH.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// newWaitProbe builds a Probe whose PATH and filesystem the test controls, with Sleep stubbed so the poll is
// instant — the same injection MachineStatus uses.
func newWaitProbe(goos string, onPath map[string]string, existing ...string) Probe {
	files := map[string]bool{}
	for _, f := range existing {
		files[f] = true
	}
	return Probe{
		GOOS: goos,
		LookPath: func(name string) (string, error) {
			if v, ok := onPath[name]; ok {
				return v, nil
			}
			return "", errors.New("not found")
		},
		Stat: func(path string) (os.FileInfo, error) {
			if files[path] {
				return fakeInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
		Run:    func(context.Context, string, ...string) (string, error) { return "", nil },
		Getenv: func(string) string { return "" },
		IsRoot: func() bool { return false },
		Sleep:  func(time.Duration) {},
	}
}

type fakeInfo struct{}

func (fakeInfo) Name() string       { return "f" }
func (fakeInfo) Size() int64        { return 1 }
func (fakeInfo) Mode() os.FileMode  { return 0o755 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }

// TestWaitForBinaryFindsAFreshlyInstalledEngine is the reported case: the engine was installed by this process,
// so PATH in this process does not have it, but its conventional location now does.
func TestWaitForBinaryFindsAFreshlyInstalledEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on the probe window")
	}
	goos := runtime.GOOS
	conventional := conventionalPaths(goos)[EnginePodman]
	if len(conventional) == 0 {
		t.Skipf("no conventional podman path on %s", goos)
	}
	want := conventional[0]

	p := newWaitProbe(goos, map[string]string{"podman": want}, want)
	got, ok := (Engine{Name: EngineNone}).WaitForBinary(p)
	if !ok {
		t.Fatalf("a podman at its conventional path %s was not found — the installer would report no engine "+
			"right after installing one", want)
	}
	if got.Name != EnginePodman || got.Bin != want {
		t.Errorf("found %+v, want podman at %s", got, want)
	}
}

// TestWaitForBinaryRejectsAPathOnlyHit is the safety half: a name that resolves on PATH but is NOT at the
// engine's conventional location must not satisfy the probe. That is how a winget App Execution Alias, or a stub
// left by a half-finished install, would otherwise masquerade as a working engine.
func TestWaitForBinaryRejectsAPathOnlyHit(t *testing.T) {
	goos := runtime.GOOS
	p := newWaitProbe(goos, map[string]string{"podman": filepath.Join(t.TempDir(), "podman")})
	// A SHORT BUDGET: the production window is 45s, and a unit test must not sit through it. The behaviour under
	// test is "a PATH-only hit is refused", not the length of the window.
	got, ok := (Engine{Name: EngineNone}).WaitForBinaryWithin(p, 50*time.Millisecond)
	if ok {
		t.Fatalf("a PATH-only hit was accepted as an engine (%+v) — a stub or an execution alias would then be "+
			"treated as a usable engine", got)
	}
	// AND IT BOUNDS ITSELF: the loop is driven by the injected Sleep, so the test finishing at all is the
	// assertion — an unbounded probe would hang here rather than pass.
}

// TestWaitForBinaryIsANoOpWhenAnEngineIsAlreadyKnown keeps the probe off the normal path.
func TestWaitForBinaryIsANoOpWhenAnEngineIsAlreadyKnown(t *testing.T) {
	known := Engine{Name: EngineDocker, Bin: "/usr/bin/docker"}
	got, ok := known.WaitForBinary(newWaitProbe(runtime.GOOS, nil))
	if ok || got.Name != EngineDocker {
		t.Errorf("the probe ran on a host that already has an engine: got %+v ok=%v", got, ok)
	}
}
