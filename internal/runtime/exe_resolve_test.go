package runtime

// exe_resolve_test.go — THE CONTAINER NEEDS A BINARY ITS KERNEL CAN EXECUTE.
//
// The runtime container is Linux (deploy/runtime/Dockerfile: FROM debian:bookworm-slim) and runs
// `orchicon runtime-supervisor`. The image deliberately does not bake the binary in; the daemon mounts its own
// executable in at create time. On a Linux host those are the same file. On Windows the daemon's file is a PE
// image, and mounting it into a Linux container produces a container whose supervisor cannot start — the
// runtime never becomes ready, which is the shape of the Windows runtime failure these tests cover.
//
// No path mapping can repair that: the problem is WHICH FILE, not how the engine addresses it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates a file with the given content and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLinuxUsesTheDaemonsOwnBinary pins the unchanged behaviour: on Linux the original design stands.
func TestLinuxUsesTheDaemonsOwnBinary(t *testing.T) {
	dir := t.TempDir()
	self := writeFile(t, dir, "orchicon", "ELF")
	got, err := ResolveContainerExe("linux", self, "", "")
	if err != nil {
		t.Fatalf("linux resolution must not fail: %v", err)
	}
	if got != self {
		t.Errorf("linux container exe = %q, want the daemon's own binary %q", got, self)
	}
}

// TestWindowsRequiresALinuxBinary is the reported class of failure, at the resolver.
func TestWindowsRequiresALinuxBinary(t *testing.T) {
	dir := t.TempDir()
	self := writeFile(t, dir, "orchicon.exe", "MZ") // a PE header: what Windows would otherwise mount

	_, err := ResolveContainerExe("windows", self, "", "")
	if err == nil {
		t.Fatal("on Windows the daemon's own PE binary must NOT be accepted for a Linux container")
	}
	for _, want := range []string{"linux", "cannot run inside a Linux container", ContainerExeEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must explain %q; got: %v", want, err)
		}
	}
	// It must name the file to place, because that is the operator's action.
	if !strings.Contains(err.Error(), "orchicon-linux-amd64") {
		t.Errorf("the failure must name the file the installer places; got: %v", err)
	}
}

// TestWindowsFindsTheInstalledLinuxSibling pins the zero-configuration case: the installer places the linux build
// beside the Windows binary, so the daemon needs no environment at all.
func TestWindowsFindsTheInstalledLinuxSibling(t *testing.T) {
	dir := t.TempDir()
	self := writeFile(t, dir, "orchicon.exe", "MZ")
	want := writeFile(t, dir, "orchicon-linux-amd64", "ELF")

	got, err := ResolveContainerExe("windows", self, "", "")
	if err != nil {
		t.Fatalf("a sibling linux build must be found without configuration: %v", err)
	}
	if got != want {
		t.Errorf("container exe = %q, want %q", got, want)
	}
}

// TestOverrideWins keeps the escape hatch honest — and validates it, because a bad override is worse than none.
func TestOverrideWins(t *testing.T) {
	dir := t.TempDir()
	self := writeFile(t, dir, "orchicon.exe", "MZ")
	writeFile(t, dir, "orchicon-linux-amd64", "ELF")
	other := writeFile(t, t.TempDir(), "custom-linux", "ELF")

	got, err := ResolveContainerExe("windows", self, other, "")
	if err != nil {
		t.Fatalf("a valid override must be accepted: %v", err)
	}
	if got != other {
		t.Errorf("container exe = %q, want the override %q", got, other)
	}

	// A missing override is an error, not a silent fallback to something that cannot work.
	if _, err := ResolveContainerExe("windows", self, filepath.Join(dir, "nope"), ""); err == nil {
		t.Error("a non-existent override must be reported")
	}
	// A DIRECTORY is not a binary.
	if _, err := ResolveContainerExe("windows", self, dir, ""); err == nil {
		t.Error("an override pointing at a directory must be reported")
	}
}

// TestUnknownHostExeIsReported keeps the defensive branch honest: without a host executable path there is nothing
// to look beside, and the error must say so rather than mounting an empty path.
func TestUnknownHostExeIsReported(t *testing.T) {
	if _, err := ResolveContainerExe("windows", "", "", ""); err == nil {
		t.Fatal("an unknown host executable must be reported on Windows")
	}
}

// TestDarwinAlsoNeedsALinuxBinary pins the platform that was silently broken.
//
// The resolver used to branch on `goos != "windows"`, so a macOS daemon handed its own MACH-O binary to a Linux
// container's entrypoint. ContainerExeRequired is true on darwin, so the create path's ELF check then refused
// every container: container mode could not work on a Mac at all, and nothing said why.
func TestDarwinAlsoNeedsALinuxBinary(t *testing.T) {
	dir := t.TempDir()
	self := writeFile(t, dir, "orchicon", "\xcf\xfa\xed\xfe") // Mach-O magic: the macOS build

	if _, err := ResolveContainerExe("darwin", self, "", ""); err == nil {
		t.Fatal("a macOS Mach-O binary must NOT be accepted as a Linux container's entrypoint")
	}
	want := writeFile(t, dir, "orchicon-linux-amd64", "\x7fELF")
	got, err := ResolveContainerExe("darwin", self, "", "")
	if err != nil {
		t.Fatalf("a sibling linux build must be found on darwin too: %v", err)
	}
	if got != want {
		t.Errorf("container exe = %q, want %q", got, want)
	}
}

// TestTheDataDirIsSearchedToo: the daemon self-copies to %TEMP%, which a temp cleaner can delete, so the durable
// state dir has to be a second place to look.
func TestTheDataDirIsSearchedToo(t *testing.T) {
	bin := t.TempDir()
	self := writeFile(t, bin, "orchicon.exe", "MZ") // nothing usable beside the executable

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeFile(t, filepath.Join(dataDir, "bin"), "orchicon-linux-amd64", "\x7fELF")

	got, err := ResolveContainerExe("windows", self, "", dataDir)
	if err != nil {
		t.Fatalf("the data dir bin must be searched: %v", err)
	}
	if got != want {
		t.Errorf("container exe = %q, want %q", got, want)
	}

	// Beside the executable still WINS — that is where the installer has always put it.
	beside := writeFile(t, bin, "orchicon-linux-amd64", "\x7fELF")
	if got, _ := ResolveContainerExe("windows", self, "", dataDir); got != beside {
		t.Errorf("container exe = %q, want the sibling %q (search order changed)", got, beside)
	}
}

// TestTheRemedyNamesADurablePath: the operator was told to place the file beside the daemon, and on Windows the
// daemon's own path had already been rewritten to the %TEMP% self-copy — a directory Storage Sense deletes.
func TestTheRemedyNamesADurablePath(t *testing.T) {
	dataDir := filepath.Join("home", "operator", "orchicon")
	msg := containerExeRemedy(dataDir)

	if !strings.Contains(msg, filepath.Join(dataDir, "bin", "orchicon-linux-amd64")) {
		t.Errorf("the remedy must name the durable destination; got: %s", msg)
	}
	if strings.Contains(msg, os.TempDir()) {
		t.Errorf("the remedy must never point into the temp dir; got: %s", msg)
	}
	for _, want := range []string{ContainerExeEnv, "install.ps1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the remedy must mention %q; got: %s", want, msg)
		}
	}
}
