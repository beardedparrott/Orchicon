package runtime

// exe_resolve_wiring_test.go — THE CONTAINER'S ENTRYPOINT MUST BE A BINARY THE CONTAINER CAN RUN.
//
// Reported from Windows: the container started and its supervisor never came up.
//
//	"runtime orchicon-runtime-ce766de7… started but supervisor socket not ready"
//
// The daemon had bind-mounted its OWN executable — the Windows PE build — at /usr/local/bin/orchicon and run
// `orchicon runtime-supervisor` as PID 1. A Linux kernel cannot exec a PE, so the supervisor died instantly,
// never opened its socket, and the readiness probe timed out. The operator got a timeout with no cause and a
// run wedged at pending.
//
// ResolveContainerExe existed for exactly this, with a sibling-name list and an actionable error — AND HAD NO
// CALLER. These tests pin the wiring, because the resolver being correct is what made the gap invisible.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveContainerExeIsActuallyCalled is the wiring assertion.
//
// The resolver was written, documented, tested by hand, and never invoked — so the daemon mounted the PE. A
// test of the resolver alone cannot catch that; only one that asserts the CALL exists can.
func TestResolveContainerExeIsActuallyCalled(t *testing.T) {
	b, err := os.ReadFile("../../cmd/orchicon/runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "runtime.ResolveContainerExe(") {
		t.Fatal("the runtime daemon never calls ResolveContainerExe — it would mount its own executable, " +
			"which on Windows is a PE file a Linux container cannot exec (the reported symptom: the " +
			"supervisor socket never becomes ready)")
	}
	if !strings.Contains(src, "ContainerExe:") {
		t.Fatal("the daemon is never given a ContainerExe — the resolved linux binary would be discarded")
	}
}

// TestAPEReachingTheEntrypointIsRefused: resolution can be right and still be overridden wrongly (a stale copy,
// a misconfigured ORCHICON_RUNTIME_CONTAINER_EXE). The create path checks the FORMAT, not the filename.
func TestAPEReachingTheEntrypointIsRefused(t *testing.T) {
	dir := t.TempDir()
	pe := filepath.Join(dir, "orchicon")
	if err := os.WriteFile(pe, []byte("MZ\x90\x00\x03\x00\x00\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isELFFile(pe) {
		t.Fatal("a PE file was accepted as an ELF binary — the container would exec-format-error")
	}

	elf := filepath.Join(dir, "orchicon-linux-amd64")
	if err := os.WriteFile(elf, []byte("\x7fELF\x02\x01\x01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isELFFile(elf) {
		t.Fatal("an ELF file was not recognised")
	}

	// And a missing file is not an ELF either — never a silent pass.
	if isELFFile(filepath.Join(dir, "nope")) {
		t.Fatal("a missing file was accepted as an ELF binary")
	}
}

// TestTheEntrypointRefusalIsActionable names the two things an operator must be told.
func TestTheEntrypointRefusalIsActionable(t *testing.T) {
	msg := containerExeRemedy("/opt/orchicon/orchicon.exe")
	for _, want := range []string{"/opt/orchicon/orchicon.exe", ContainerExeEnv, "install.ps1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the remedy does not mention %q, so the operator has a refusal but no next step: %s",
				want, msg)
		}
	}
}

// TestNoFallbackToTheHostBinaryOnAWindowsShapedHost pins the change from fallback to refusal.
//
// Before, an empty ContainerExe fell back to d.ExePath — which is exactly how the PE got mounted. The fallback
// is only safe when the host IS the container's platform.
func TestNoFallbackToTheHostBinaryOnAWindowsShapedHost(t *testing.T) {
	b, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "if d.ContainerExeRequired {") {
		t.Fatal("the create path has no ContainerExeRequired branch — an empty ContainerExe would silently " +
			"fall back to the daemon's own executable, which is the reported failure")
	}
	i := strings.Index(src, "containerExe := d.ContainerExe")
	j := strings.Index(src[i:], "containerExe = d.ExePath")
	if j < 0 {
		t.Fatal("could not find the fallback to assert about")
	}
	// The refusal must come BEFORE the fallback.
	if !strings.Contains(src[i:i+j], "ContainerExeRequired") {
		t.Error("the fallback to d.ExePath happens before the platform check — the PE would still be mounted")
	}
}
