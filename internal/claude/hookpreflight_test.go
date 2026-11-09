package claude

// hookpreflight_test.go — A HOOK THAT CANNOT RUN IS A PERMISSION WALL, NOT A NO-OP.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestABareHookNameMustResolveOnPATH pins the fallback that made this possible.
func TestABareHookNameMustResolveOnPATH(t *testing.T) {
	t.Setenv(HookBinEnv, "definitely-not-on-path-orchicon-hook")
	err := PreflightHookExecutable(false)
	if err == nil {
		t.Fatal("a hook binary that does not exist was accepted — every gated tool call would be refused as " +
			"an unanswerable prompt, which is the reported permission wall")
	}
	for _, want := range []string{"not on PATH", HookBinEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must say %q so the operator has an action; got: %v", want, err)
		}
	}
}

// TestAnExplicitHookPathIsAccepted — the override must work.
func TestAnExplicitHookPathIsAccepted(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "orchicon")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(HookBinEnv, bin)
	if err := PreflightHookExecutable(false); err != nil {
		t.Fatalf("an existing hook binary was refused: %v", err)
	}
}

// TestAMissingHookPathIsNamed — os.Stat's failure must be reported, not swallowed.
func TestAMissingHookPathIsNamed(t *testing.T) {
	t.Setenv(HookBinEnv, filepath.Join(t.TempDir(), "gone", "orchicon"))
	err := PreflightHookExecutable(false)
	if err == nil {
		t.Fatal("a missing absolute hook path was accepted")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("the failure must say the binary is unreadable; got: %v", err)
	}
}

// TestADirectoryIsNotAHook — the shape a wrong ORCHICON_CLAUDE_HOOK_BIN most often takes.
func TestADirectoryIsNotAHook(t *testing.T) {
	t.Setenv(HookBinEnv, t.TempDir())
	err := PreflightHookExecutable(false)
	if err == nil {
		t.Fatal("a directory was accepted as the hook binary")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("the failure must say it is a directory; got: %v", err)
	}
}

// TestTheContainerHookMustBeTheMountedAbsolutePath — inside a container this process cannot stat the path, so
// the FORM is what is checked. A relative path would resolve against the tool process's cwd and be wrong.
func TestTheContainerHookMustBeTheMountedAbsolutePath(t *testing.T) {
	if err := PreflightHookExecutable(true); err != nil {
		t.Fatalf("the container hook path %s must be accepted (the daemon guarantees it, and this process "+
			"cannot stat inside the container): %v", HookBinaryContainerPath, err)
	}
	// THE OVERRIDE DOES NOT APPLY IN A CONTAINER, deliberately: HookBinaryFor ignores HookBinEnv for
	// the container transport, because an operator's host path is meaningless inside the image.
	t.Setenv(HookBinEnv, "orchicon")
	if got := HookBinaryFor(true); got != HookBinaryContainerPath {
		t.Errorf("a container session resolved the hook to %q; it must be the daemon's bind-mounted binary %s "+
			"— a host path or a bare name cannot run inside the container", got, HookBinaryContainerPath)
	}
	if !strings.HasPrefix(HookBinaryContainerPath, "/") {
		t.Error("the container hook path must be absolute")
	}
}
