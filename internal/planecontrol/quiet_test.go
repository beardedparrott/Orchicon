package planecontrol

// quiet_test.go — no helper tool in this package may open a console window.
//
// THE COMPLAINT CAME BACK AFTER THE FIRST FIX, and the reason was SCOPE, not disagreement: the fix went
// into cmd/orchicon, and the operator was still seeing windows — "No, it's happening in the desktop app
// when you install a new server until it says 'setup complete'." An install runs DETACHED (a minutes-long
// download must not die with the GUI), a detached process has no console, and Win32 gives a console-
// subsystem child of a console-less process a BRAND NEW WINDOW. Every helper ANYWHERE in that subtree that
// forgot the attribute put a window on the screen — this package's `docker network inspect` and `taskkill`
// among them.
//
// THE TWO COPIES OF THE HELPER ARE DELIBERATE — see quiet.go: exported, either would make one package
// depend on the other, and this package's imports test forbids that. So the guard asserts BOTH copies exist
// and that neither has lost the attribute, which is the thing that would silently regress.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helperToolFiles are this package's files that run host tools FOR THEMSELVES. A bare exec in any of them
// is a window on Windows.
var helperToolFiles = []string{
	"env.go",
	"hostproc_windows.go",
}

func TestEveryHelperToolInThisPackageIsSpawnedWindowless(t *testing.T) {
	for _, name := range helperToolFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("cannot read %s: %v — if it was renamed, update helperToolFiles", name, err)
		}
		body := string(src)
		for _, banned := range []string{"exec.Command(", "exec.CommandContext("} {
			if i := strings.Index(body, banned); i >= 0 {
				line := 1 + strings.Count(body[:i], "\n")
				t.Errorf("%s:%d spawns a helper tool with a bare %s. This package runs inside a DETACHED "+
					"install (no console), so Win32 gives every console child a new window. Use "+
					"quietCommand/quietCommandContext", name, line, banned)
			}
		}
	}
}

func TestTheQuietHelperStillSetsTheAttribute(t *testing.T) {
	src, err := os.ReadFile("quiet.go")
	if err != nil {
		t.Fatalf("cannot read quiet.go: %v", err)
	}
	// BOTH helpers, counted rather than grepped once: a version that set the attribute on only the
	// context variant would leave every plain call unguarded.
	if got := strings.Count(string(src), "setProcAttrNoWindow(cmd)"); got != 2 {
		t.Errorf("quiet.go applies the attribute %d times, want 2. A helper that does not set it is a "+
			"bare exec.Command wearing a better name", got)
	}

	win, err := os.ReadFile(filepath.Join("procattr_windows.go"))
	if err != nil {
		t.Fatalf("cannot read procattr_windows.go: %v", err)
	}
	if !strings.Contains(string(win), "createNoWindow = 0x08000000") {
		t.Error("CREATE_NO_WINDOW is gone from this package's Windows half, so quietCommand is only a " +
			"rename and every helper tool opens a window again")
	}

	unix, err := os.ReadFile("procattr_unix.go")
	if err != nil {
		t.Fatalf("cannot read procattr_unix.go: %v", err)
	}
	if !strings.Contains(string(unix), "func setProcAttrNoWindow(*exec.Cmd)") {
		t.Error("the Unix half is missing, so quietCommand would not compile there")
	}
}
