package claude

// container_argv0_host_test.go — THE CONTAINER'S argv[0] IS THE HOST'S INSTALL WHEN IT CAN RUN THERE.
//
// #898 sent the bare name on every platform. On Windows that was the only usable route (a PE cannot exec in
// a Linux container), but on POSIX it discarded a working one: the daemon mounts the operator's `claude` at
// its identical host path and the supervisor allowlists argv[0] by BASENAME, so the absolute path is
// accepted. A container whose image happened to be missing the CLI then died with
//
//	exec: "claude": executable file not found in $PATH
//
// while the operator's own install sat mounted and unreferenced.
//
// These pin the decision at the seam, with the platform INJECTED so the non-Linux arms are reachable from a
// Linux gate — the macOS case is the one this cannot be tried on by running the suite.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// THE INVERSION, at the seam the session actually uses.
func TestContainerArgv0UsesTheHostInstallOnLinux(t *testing.T) {
	const host = "/home/op/.local/bin/claude"
	if got := claudeContainerArgv0("linux", host); got != host {
		t.Fatalf("container argv[0] = %q, want the host install %q — the image is not the only route on a "+
			"Linux host, and preferring it is what made a stale image fatal", got, host)
	}
}

// THE MACOS CASE, modelled: a Mach-O binary must never cross into a Linux container, and ContainerUsable's
// ".exe" shape check does not catch it — only the GOOS does.
func TestContainerArgv0FallsBackToTheImageOnDarwin(t *testing.T) {
	got := claudeContainerArgv0("darwin", "/Users/op/.local/bin/claude")
	if got != ClaudeBinaryName {
		t.Fatalf("container argv[0] = %q for a darwin host, want %q — a Mach-O binary cannot exec in a Linux "+
			"container, so the image's CLI is the route", got, ClaudeBinaryName)
	}
}

// AND THE WINDOWS CASE, which is why the bare name existed at all.
func TestContainerArgv0FallsBackToTheImageOnWindows(t *testing.T) {
	for _, bin := range []string{`C:\Users\op\.local\bin\claude.exe`, "/c/Users/op/.local/bin/claude"} {
		if got := claudeContainerArgv0("windows", bin); got != ClaudeBinaryName {
			t.Errorf("container argv[0] = %q for a windows host, want %q", got, ClaudeBinaryName)
		}
	}
}

// NOTHING TO PREFER — an image-only host still gets the bare name, which is the behaviour that must survive.
func TestContainerArgv0FallsBackWhenNoHostInstallExists(t *testing.T) {
	if got := claudeContainerArgv0("linux", ""); got != ClaudeBinaryName {
		t.Fatalf("container argv[0] = %q with no host install, want the bare name", got)
	}
}

// WHATEVER THE DECISION, THE SUPERVISOR MUST ACCEPT IT. This is the boundary #898 broke, and it is asserted
// here in the claude package so a future change to the decision cannot leave the two disagreeing.
func TestEveryContainerArgv0IsAcceptedByTheSupervisor(t *testing.T) {
	for _, c := range []struct{ goos, bin string }{
		{"linux", "/home/op/.local/bin/claude"},
		{"linux", "/mnt/c/Users/op/.local/bin/claude"},
		{"darwin", "/Users/op/.local/bin/claude"},
		{"windows", `C:\Users\op\.local\bin\claude.exe`},
		{"linux", ""},
	} {
		got := claudeContainerArgv0(c.goos, c.bin)
		if _, ok := acceptedBySupervisor(got); !ok {
			t.Errorf("(%s, %q) -> argv[0] %q is NOT allowlisted — the daemon answers {event:error} and spawns "+
				"nothing, which is the 0-token failure with no stderr", c.goos, c.bin, got)
		}
	}
}

// END TO END, on the real resolver: a host with a claude installed resolves to THAT ABSOLUTE PATH for a
// container. ClaudeBinaryPath's bare-name fallback must not be mistaken for a host install here.
func TestClaudeBinaryForContainerFindsTheInstalledHostCLI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the host install only runs in a Linux container")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(adapter.BinEnv(adapter.KindClaude), "")

	got := ClaudeBinaryFor(true)
	if !strings.HasPrefix(got, home) {
		t.Fatalf("ClaudeBinaryFor(container) = %q, want the install under %q — a container on a Linux host "+
			"has a usable host CLI and must not depend on the image", got, home)
	}
	if _, ok := acceptedBySupervisor(got); !ok {
		t.Fatalf("the resolved host path %q is not allowlisted by basename", got)
	}
}

// THE FIXTURE CANNOT MANUFACTURE ABSENCE BY MOVING HOME: the resolver also asks the OS's PATH (read
// from its SOURCE) and the known-install lists.
func TestClaudeBinaryForContainerWithoutAHostInstallUsesTheBareName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the container route only prefers the host on Linux")
	}
	if host := claudeHostInstall(); host != "" {
		t.Skipf("this host has a resolvable claude (%s), so absence cannot be simulated here — the decision "+
			"with no host install is pinned by TestContainerArgv0FallsBackWhenNoHostInstallExists", host)
	}
	if got := ClaudeBinaryFor(true); got != ClaudeBinaryName {
		t.Fatalf("ClaudeBinaryFor(container) = %q with no host install, want %q", got, ClaudeBinaryName)
	}
}
