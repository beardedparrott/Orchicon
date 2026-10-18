package planecontrol

// planebinary_test.go — WHICH BINARY RUNS THE PLANE.
//
// This is the regression gate for the thin-client case: `orch` (the TUI) and the desktop app call this
// package but are NOT the plane's binary, so re-execing SELF launched a second TUI instead of the plane.
// Every lifecycle verb failed from the TUI — start timed out after 60s waiting for a /healthz nothing was
// going to serve, and stop re-execed the shell (whose `serve --stop` is not a subcommand) and reported a
// stop that never reached the plane.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExe writes an executable shim that records the fact it ran, and returns its path.
func fakeExe(t *testing.T, dir, name, marker string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\necho ran > " + marker + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write shim %s: %v", name, err)
	}
	return path
}

// pinSelf makes executable() answer with a specific path for the test's lifetime.
func pinSelf(t *testing.T, self string) {
	t.Helper()
	old := executable
	executable = func() (string, error) { return self, nil }
	t.Cleanup(func() { executable = old })
}

// A THIN CLIENT RESOLVES THE INSTALLED `orchicon`, never itself.
func TestPlaneBinaryForAThinClientIsTheInstalledOrchicon(t *testing.T) {
	dir := t.TempDir()
	installed := fakeExe(t, dir, "orchicon", filepath.Join(dir, "unused"))
	client := fakeExe(t, dir, "orch", filepath.Join(dir, "unused-client"))
	t.Setenv("PATH", dir)
	pinSelf(t, client)

	got, err := resolvePlaneBinary()
	if err != nil {
		t.Fatalf("resolvePlaneBinary from a thin client: %v", err)
	}
	if got != installed {
		t.Errorf("resolvePlaneBinary = %q, want the installed %q — re-execing the client is the bug", got, installed)
	}
}

// THE CLI IS THE PLANE BINARY: `orchicon` resolves to itself, even when a different `orchicon` is on PATH
// first. An install/build path must never be redirected to somebody else's binary.
func TestPlaneBinaryForOrchiconIsItself(t *testing.T) {
	dir := t.TempDir()
	self := fakeExe(t, dir, "orchicon", filepath.Join(dir, "unused-self"))
	// A DIFFERENT directory on PATH, so the only `orchicon` found there is the decoy.
	pathDir := t.TempDir()
	decoyOnPath := fakeExe(t, pathDir, "orchicon", filepath.Join(dir, "unused2"))
	t.Setenv("PATH", pathDir)
	pinSelf(t, self)

	got, err := resolvePlaneBinary()
	if err != nil {
		t.Fatalf("resolvePlaneBinary when this IS orchicon: %v", err)
	}
	if got != self {
		t.Errorf("resolvePlaneBinary = %q, want self %q (decoy on PATH: %q)", got, self, decoyOnPath)
	}
}

// NO INSTALLED `orchicon` IS AN ERROR THAT NAMES THE REMEDY — never a silent fallback to the caller.
func TestPlaneBinaryUnresolvableNamesTheRemedy(t *testing.T) {
	dir := t.TempDir()
	client := fakeExe(t, dir, "orch", filepath.Join(dir, "unused"))
	t.Setenv("PATH", t.TempDir()) // nothing named orchicon anywhere on it
	pinSelf(t, client)
	oldHome := userHomeDir
	userHomeDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { userHomeDir = oldHome })

	got, err := resolvePlaneBinary()
	if err == nil {
		t.Fatalf("resolvePlaneBinary with no orchicon = %q, want an error naming the remedy", got)
	}
	for _, want := range []string{"orchicon", "client"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "did not answer") {
		t.Errorf("the error must not look like a health timeout — nothing was ever run: %v", err)
	}
}

// THE WHOLE POINT, OBSERVED: the re-exec runs `orchicon`, not the calling client.
//
// runSelf is called directly (not the runCommand seam) because the question is which PROCESS it starts,
// not what arguments it is handed.
func TestRunSelfReExecsThePlaneBinaryNotTheCaller(t *testing.T) {
	dir := t.TempDir()
	planeMarker := filepath.Join(dir, "plane-ran")
	clientMarker := filepath.Join(dir, "client-ran")
	fakeExe(t, dir, "orchicon", planeMarker)
	client := fakeExe(t, dir, "orch", clientMarker)
	t.Setenv("PATH", dir)
	pinSelf(t, client)

	if err := runSelf([]string{"serve", "--detach"}, os.Environ(), io.Discard); err != nil {
		t.Fatalf("runSelf: %v", err)
	}
	if _, err := os.Stat(planeMarker); err != nil {
		t.Errorf("`orchicon` never ran — the re-exec did not reach the plane binary: %v", err)
	}
	if _, err := os.Stat(clientMarker); err == nil {
		t.Error("the CALLER was re-execed — `orch serve --detach` launches a second TUI, which is the bug")
	}
}
