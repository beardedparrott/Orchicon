package main

// install_engine_broken_docker_test.go — A BROKEN DOCKER MUST NOT STOP PODMAN BEING INSTALLED.
//
// The reported failure, verbatim from the operator's Windows host:
//
//	== checking the container engine
//	== checking the podman machine
//	== verifying the container engine — running a trivial container
//	orchicon: the container engine is installed but cannot run a container: failed to connect to
//	          the docker API at npipe:////./pipe/dockerDesktopLinuxEngine; ... The system cannot
//	          find the file specified.
//	orchicon: runtime containers are unavailable — ... work runs in-process
//
// Podman was never offered: provisioning was gated on "nothing was DETECTED", and a docker CLI with a
// dead daemon is still detected. The install then had no usable engine, started no runtime daemon, and
// every container fetch failed on a socket that would never exist — identically on every reinstall,
// which is why a newer canary changed nothing.
//
// WHY THIS IS A STRUCTURAL TEST. Driving the ladder end to end would make it exec a real package
// manager (winget/brew/apt), so the suite would try to install software on whatever machine ran it.
// That is not an acceptable side effect for a unit test, and stubbing the exec layer would mean the
// test asserted a stub rather than the code. So this pins the SEAM — the verify failure must reach
// provisioning for a non-podman engine — and says plainly that it is a structural assertion.

import (
	"os"
	"strings"
	"testing"
)

// readInstallEngine returns the ladder's source.
func readInstallEngine(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("install_engine.go")
	if err != nil {
		t.Fatalf("read install_engine.go: %v", err)
	}
	return string(b)
}

// TestBrokenEngineFallsThroughToProvisioning pins the fall-through that makes a dead Docker Desktop
// recoverable.
func TestBrokenEngineFallsThroughToProvisioning(t *testing.T) {
	src := readInstallEngine(t)

	// The verify failure must not be an unconditional return any more.
	if strings.Contains(src, `if err := containerengine.Verify(context.Background(), p, e, verifyImage, 0); err != nil {
		return unavailable("%v", err)
	}`) {
		t.Fatal("a failed verify still returns immediately — a host whose only engine is a stopped " +
			"Docker Desktop is left with no engine, no runtime daemon, and a container fetch that " +
			"fails on a socket that will never exist")
	}

	// It must instead branch on the engine kind and provision.
	for _, want := range []string{
		"if e.Name != containerengine.EnginePodman {",
		"provisionPodman(",
		"installing podman instead",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the fall-through is missing %q — a detected-but-broken engine must reach "+
				"provisioning, or the operator is stuck reinstalling forever", want)
		}
	}
}

// TestPodmanFailingVerifyIsNotReinstalled is the guard on the other side: podman is the engine
// provisioning would install, so sending it back through provisioning could only loop.
func TestPodmanFailingVerifyIsNotReinstalled(t *testing.T) {
	src := readInstallEngine(t)
	if !strings.Contains(src, "if e.Name != containerengine.EnginePodman {") {
		t.Fatal("the fall-through does not exclude podman — provisioning would reinstall the engine " +
			"that just failed to verify")
	}
	// And the original error survives for the podman case: it names what is actually broken.
	if !strings.Contains(src, `return unavailable("%v", verr)`) {
		t.Error("a broken podman must report its own verification failure, not a provisioning story")
	}
}

// TestProvisioningRungIsShared states the coupling: one provisioning rung, two callers. A second
// copy is how the absent path and the broken path would drift.
func TestProvisioningRungIsShared(t *testing.T) {
	src := readInstallEngine(t)
	// One DEFINITION plus two CALL SITES. The definition is a func literal (`provisionPodman := func`),
	// which is why it is counted separately rather than by the call syntax.
	if n := strings.Count(src, "provisionPodman := func("); n != 1 {
		t.Errorf("provisionPodman is defined %d time(s), want exactly 1 — a second rung is how the "+
			"absent path and the broken path drift in prompting and in reported reasons", n)
	}
	// TWO CALL SITES, and the definition is not one of them: it is `provisionPodman := func(why string)`,
	// which does not match the call pattern. (An earlier version of this assertion expected three and
	// failed on its own arithmetic, not on the code.)
	if n := strings.Count(src, "provisionPodman("); n != 2 {
		t.Errorf("provisionPodman is CALLED from %d place(s), want 2 — the absent-engine rung and the "+
			"broken-engine rung", n)
	}
	if n := strings.Count(src, "containerengine.Provision("); n != 1 {
		t.Errorf("containerengine.Provision is called from %d places; the two rungs must share ONE "+
			"call, or they can drift in prompting and in reported reasons", n)
	}
}
