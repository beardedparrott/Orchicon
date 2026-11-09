package adapter

// container_host_preference_test.go — WHO SUPPLIES THE CLI A CONTAINER EXECS.
//
// A container ran a workflow with the adapter plainly installed on the host and died with
//
//	exec: "claude": executable file not found in $PATH
//
// because #898 sent the BARE name on every platform and trusted the image to provide it. It did not: the
// image was three weeks older than the adapter install, so six executions burned 70ms each and 0 tokens.
// The operator's own `claude` was bind-mounted into that container the whole time, at its host path, and
// nothing named it.
//
// The preference is now the host's, and THESE ARE THE GATES. They live in one function because the two
// consumers — the adapter's argv[0] and the daemon's PATH prefix — disagreed when each derived its own.

import (
	"strings"
	"testing"
)

// THE INVERSION: a Linux host install wins over the image's.
func TestALinuxHostInstallIsPreferredInAContainer(t *testing.T) {
	ok, why := PreferHostAdapterInContainer("linux", "/home/op/.local/bin/claude")
	if !ok {
		t.Fatalf("a Linux host install was not preferred, so the container falls back to the image: %s", why)
	}
}

// A WSL PATH IS A LINUX PATH — /mnt/c is a mount on a Linux kernel, not a Windows binary.
func TestAWSLPathIsPreferred(t *testing.T) {
	if ok, why := PreferHostAdapterInContainer("linux", "/mnt/c/Users/op/.local/bin/claude"); !ok {
		t.Fatalf("a /mnt path on a Linux host was refused: %s", why)
	}
}

// THE ARM THAT MUST NOT REGRESS, AND CANNOT BE SEEN ON A LINUX CI HOST: macOS. A Darwin build is a Mach-O
// binary with no ".exe" suffix, so ContainerUsable's shape check passes it — only the host GOOS stops it.
// Modelled rather than skipped, so the defect fails here too.
func TestADarwinHostInstallIsNotPreferred(t *testing.T) {
	ok, why := PreferHostAdapterInContainer("darwin", "/Users/op/.local/bin/claude")
	if ok {
		t.Fatal("a darwin host install was preferred for a LINUX container — the container would exec a " +
			"Mach-O binary and die with an exec format error, which is the macOS testing case")
	}
	if !strings.Contains(why, "darwin") || !strings.Contains(why, "Linux") {
		t.Errorf("the refusal must name both platforms so the operator can see why the image is used: %s", why)
	}
}

func TestAWindowsHostInstallIsNotPreferred(t *testing.T) {
	// Both shapes: the GOOS alone, and a .exe path on a non-linux GOOS.
	for _, c := range []struct{ goos, bin string }{
		{"windows", `C:\Users\op\.local\bin\claude.exe`},
		{"windows", "/c/Users/op/.local/bin/claude"},
		{"linux", `C:\Users\op\AppData\Local\claude.exe`}, // a PE reached from a Linux host (an odd but real shape)
	} {
		if ok, _ := PreferHostAdapterInContainer(c.goos, c.bin); ok {
			t.Errorf("(%s, %s) was preferred — a Windows binary cannot exec in a Linux container", c.goos, c.bin)
		}
	}
}

// NOTHING RESOLVED IS ITS OWN ANSWER, and it must not read as "the wrong kind of binary": the remedies differ.
func TestNoHostInstallIsItsOwnReason(t *testing.T) {
	ok, why := PreferHostAdapterInContainer("linux", "")
	if ok {
		t.Fatal("an empty host path was preferred")
	}
	if strings.Contains(why, "Windows") || strings.Contains(why, "darwin") {
		t.Errorf("an absent install was described as the wrong platform: %s", why)
	}
	if !strings.Contains(why, "no host install") {
		t.Errorf("the reason should say nothing was resolved: %s", why)
	}
}

// AN UNKNOWN HOST GOOS IS NOT TREATED AS LINUX. A build for a target this code has never heard of must fall
// back to the image rather than assume a kernel it cannot see.
func TestAnUnknownHostGOOSFallsBackToTheImage(t *testing.T) {
	if ok, why := PreferHostAdapterInContainer("plan9", "/usr/local/bin/claude"); ok {
		t.Fatalf("an unknown host GOOS was treated as Linux, so a binary of unknown format crossed the "+
			"container boundary: %s", why)
	}
}
