package main

// quiet_test.go — the guard for "no helper tool opens a console window".
//
// THE BUG THIS EXISTS FOR CAME BACK ONCE, so the point of this file is that it cannot come back a third
// time. The first report ("a ton of command prompt windows") was fixed at the single call site it named;
// the second report was the same sentence again, because every OTHER console tool the program spawns was
// still free to open a window and several did — including one inside a sixty-iteration wait loop.
//
// The fix is quietCommand/quietCommandContext (quiet.go). These tests assert two things about it:
//
//   - the helper actually applies the attribute, on every platform (the Unix half is a no-op by design,
//     so what is checked there is that the wiring exists rather than that it does something);
//   - the files that spawn host tools contain NO bare exec.Command, so a newly added probe is caught
//     here rather than by the next person installing on Windows.

import (
	"os"
	"strings"
	"testing"
)

// helperToolFiles are the files that spawn tools this program runs FOR ITSELF. Every exec in them must go
// through the quiet helper: they run from the services supervisor (detached), from an install the desktop
// spawned detached, or from the MSI's console-less custom action — so a bare exec there is a window.
//
// Files deliberately NOT listed, because a console is what they are for and quieting them would be wrong:
// console_windows.go (creates the setup's own console) and install_detach_windows.go (the visible setup
// relaunch). serve.go is the detached plane, which has no console by design.
var helperToolFiles = []string{
	"services_health.go",
	"port_owner_windows.go",
	"host_services.go",
	"install_engine.go",
	"postgres_legacy_volume.go",
	"reclaim.go",
	"binaries.go",
}

func TestEveryHelperToolIsSpawnedWindowless(t *testing.T) {
	checked := 0
	for _, name := range helperToolFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			// A RENAMED OR DELETED FILE MUST FAIL, not silently shrink the guard: the vacuity check at
			// the end would still pass if half this list stopped existing.
			t.Fatalf("cannot read %s: %v — if it was renamed, update helperToolFiles to match", name, err)
		}
		body := string(src)
		for _, banned := range []string{"exec.Command(", "exec.CommandContext("} {
			if i := strings.Index(body, banned); i >= 0 {
				line := 1 + strings.Count(body[:i], "\n")
				t.Errorf("%s:%d spawns a helper tool with a bare %s. On Windows a console-subsystem "+
					"child of a process with no console gets a NEW WINDOW, and this file's callers run "+
					"with none (the supervisor is detached, the install is detached, the MSI action has "+
					"no console). Use quietCommand/quietCommandContext — see quiet.go",
					name, line, banned)
			}
		}
		checked++
	}
	// VACUITY: a guard that inspected nothing proves nothing. This is the same failure these tests
	// already caught twice elsewhere — a matcher that quietly stops matching.
	if checked != len(helperToolFiles) || checked < 5 {
		t.Fatalf("only %d of %d helper-tool files were inspected; the guard is not doing its job",
			checked, len(helperToolFiles))
	}
}

// TestQuietCommandAppliesTheNoWindowAttribute pins the helper's own contract, so a refactor cannot quietly
// drop the attribute it exists to apply.
func TestQuietCommandAppliesTheNoWindowAttribute(t *testing.T) {
	src, err := os.ReadFile("quiet.go")
	if err != nil {
		t.Fatalf("cannot read quiet.go: %v", err)
	}
	body := string(src)

	for _, want := range []string{
		"func quietCommand(name string, args ...string) *exec.Cmd",
		"func quietCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("quiet.go no longer defines %q", want)
		}
	}
	// BOTH halves must set the attribute. Counting rather than just grepping for one occurrence: a helper
	// that applied it to only the context variant would leave every plain call unguarded.
	if got := strings.Count(body, "setProcAttrNoWindow(cmd)"); got != 2 {
		t.Errorf("quiet.go calls setProcAttrNoWindow %d times, want 2 (one per helper). A helper that "+
			"does not set the attribute is a bare exec.Command wearing a better name", got)
	}
}

// TestNoWindowAttributeIsRealOnWindows keeps the Windows half honest: the Unix half is a no-op on purpose,
// so the attribute must be verifiably present where it matters.
func TestNoWindowAttributeIsRealOnWindows(t *testing.T) {
	win, err := os.ReadFile("procattr_windows.go")
	if err != nil {
		t.Fatalf("cannot read procattr_windows.go: %v", err)
	}
	if !strings.Contains(string(win), "createNoWindow = 0x08000000") {
		t.Error("CREATE_NO_WINDOW is gone from procattr_windows.go. Without it, quietCommand is only a " +
			"rename and every helper tool opens a window again")
	}
	if !strings.Contains(string(win), "func setProcAttrNoWindow(cmd *exec.Cmd)") {
		t.Error("setProcAttrNoWindow is missing from the Windows half")
	}
	unix, err := os.ReadFile("procattr_unix.go")
	if err != nil {
		t.Fatalf("cannot read procattr_unix.go: %v", err)
	}
	if !strings.Contains(string(unix), "func setProcAttrNoWindow(*exec.Cmd)") {
		t.Error("setProcAttrNoWindow is missing from the Unix half, so quietCommand would not compile there")
	}
}
