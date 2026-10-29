package runtime

// daemon_project_roots_test.go — A PROJECT'S OWN DIRECTORY IS ALWAYS MOUNTABLE.
//
// Reported from Windows, where the default allowlist is %USERPROFILE%\projects:
//
//	mount source outside allowed roots: C:\projects\weatherapp
//	    (as the container engine sees it: /mnt/c/projects/weatherapp;
//	     allowed roots: C:\Users\justi\projects)
//
// The engine was right and the policy was arbitrary: a runtime container exists to work on a project, and the
// project says where it is. Requiring it to live under the user's home is a restriction with no security
// meaning — the boundary that matters is "trees this plane was told to work on", and a project_dir IS that.
//
// The roots are read PER CHECK (AllowedRootsSource) because "as soon as a project_dir is set" is the
// requirement: a list captured at daemon start would refuse a project created a minute later.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAProjectDirIsMountableWithoutBeingConfigured is the reported case, end to end: the engine's view of a
// project outside %USERPROFILE%\projects must be accepted because it is a project directory.
func TestAProjectDirIsMountableWithoutBeingConfigured(t *testing.T) {
	// A Windows-shaped daemon: the project is on C:, the configured root is elsewhere in the user profile.
	d := &Daemon{
		Paths:        NewPathMapper("windows", "wsl"),
		AllowedRoots: []string{`C:\Users\justi\projects`},
		AllowedRootsSource: func(context.Context) ([]string, error) {
			return []string{`C:\projects\weatherapp`}, nil
		},
	}
	got := d.effectiveAllowedRoots(context.Background())
	if !withinRootsIn(d.Paths, `C:\projects\weatherapp`, got) {
		t.Fatalf("the project's own directory was refused; roots = %v (engine view %q)",
			got, d.Paths.Map(`C:\projects\weatherapp`))
	}
	// And a directory that is NEITHER configured NOR a project is still refused: the boundary is real.
	if withinRootsIn(d.Paths, `C:\elsewhere\notmine`, got) {
		t.Fatalf("an unrelated directory was allowed: %v", got)
	}
}

// TestProjectRootsAreReadLive is the "as soon as it is set" requirement, stated as a sequence.
func TestProjectRootsAreReadLive(t *testing.T) {
	var projectDirs []string
	d := &Daemon{
		Paths:        NewPathMapper("windows", "wsl"),
		AllowedRoots: []string{`C:\Users\justi\projects`},
		AllowedRootsSource: func(context.Context) ([]string, error) {
			return projectDirs, nil // read on every call, like the real source
		},
	}
	ctx := context.Background()

	if withinRootsIn(d.Paths, `C:\projects\new`, d.effectiveAllowedRoots(ctx)) {
		t.Fatal("the directory was mountable before any project claimed it")
	}
	// The operator sets the project_dir. No restart, no reconfigure.
	projectDirs = []string{`C:\projects\new`}
	if !withinRootsIn(d.Paths, `C:\projects\new`, d.effectiveAllowedRoots(ctx)) {
		t.Fatal("a project_dir set after the daemon started was NOT honoured — roots must be read per check, " +
			"not captured at start-up")
	}
}

// TestTheSourceFailsClosed: a database problem must never widen the boundary.
func TestTheSourceFailsClosed(t *testing.T) {
	d := &Daemon{
		Paths:        NewPathMapper("windows", "wsl"),
		AllowedRoots: []string{`C:\Users\justi\projects`},
		AllowedRootsSource: func(context.Context) ([]string, error) {
			return []string{`C:\everything`}, errors.New("database is down")
		},
	}
	got := d.effectiveAllowedRoots(context.Background())
	if len(got) != 1 || filepath.Clean(got[0]) != filepath.Clean(`C:\Users\justi\projects`) {
		t.Fatalf("a failing source must fall back to EXACTLY the configured roots, got %v", got)
	}
}

// TestNoSourceKeepsTheOldBehaviour: without a source the daemon is exactly as strict as before.
func TestNoSourceKeepsTheOldBehaviour(t *testing.T) {
	d := &Daemon{Paths: NewPathMapper("linux", ""), AllowedRoots: []string{"/home/me/projects"}}
	if d.AllowedRootsSource != nil {
		t.Fatal("test setup: expected no source")
	}
	got := d.effectiveAllowedRoots(context.Background())
	if len(got) != 1 {
		t.Fatalf("roots = %v, want only the configured one", got)
	}
	if withinRootsIn(d.Paths, "/opt/other", got) {
		t.Fatal("an unconfigured directory was allowed with no project source present")
	}
}

// TestWithinRootsInMapsBothSides pins the Windows comparison directly, because it is the part that has no
// string relationship and therefore cannot be verified by reading the host forms.
func TestWithinRootsInMapsBothSides(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	roots := []string{`C:\projects\weatherapp`}

	if !withinRootsIn(m, `C:\projects\weatherapp`, roots) {
		t.Error("the project directory itself must be inside its own root")
	}
	if !withinRootsIn(m, `C:\projects\weatherapp\src\deep`, roots) {
		t.Error("a path UNDER the root must be allowed")
	}
	// The prefix trap: /mnt/c/projects/weatherapp-OTHER shares a string prefix with the root but is not
	// inside it. A naive HasPrefix would allow it.
	if withinRootsIn(m, `C:\projects\weatherapp-OTHER`, roots) {
		t.Error("a sibling that merely SHARES A PREFIX was allowed — the comparison must be on path segments")
	}
	if withinRootsIn(m, `C:\projects`, roots) {
		t.Error("the PARENT of the root must not be allowed")
	}
}

// TestEmptyRootsAllowNothing keeps the deny-by-default posture explicit.
func TestEmptyRootsAllowNothing(t *testing.T) {
	if withinRootsIn(NewPathMapper("linux", ""), "/opt/x", nil) {
		t.Fatal("no roots must allow nothing")
	}
	if withinRootsIn(nil, "/opt/x", []string{"/opt"}) != true {
		// A nil mapper is identity, which is the Linux/macOS case and must keep working.
		t.Fatal("a nil PathMapper must behave as identity, not as a refusal")
	}
	_ = os.PathSeparator
}
