package instancectl

// spawndir_test.go — a child must never inherit the app's working directory.
//
// REPORTED: "When trying to create a test environment in the desktop in windows (with the desktop launched
// from the installer), it complained about not being able to create it due to the directory the desktop ran
// from. If I closed out of the desktop and launched it from the start menu, then it did allow me to create
// test with no issues."
//
// Nothing in `orchicon install` reads its cwd; every child it started INHERITED the app's, because no spawn
// site set cmd.Dir. On Windows a process cannot be created when the inherited directory no longer exists
// (ERROR_DIRECTORY, "The directory name is invalid") — so a GUI launched out of an installer's staging area
// could not start ANY child, while the same app from the Start menu could. That is the whole difference.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpawnDirSurvivesADeletedWorkingDirectory is the reported condition, reproduced.
//
// The process is put into a directory and that directory is then removed — which is what a launch from an
// installer's cleaned-up staging area leaves behind. os.Getwd then fails, and on Windows every spawn from
// here fails too. spawnDir must still answer with a real directory, so the child starts.
func TestSpawnDirSurvivesADeletedWorkingDirectory(t *testing.T) {
	gone := t.TempDir()
	// t.Chdir restores the previous directory at cleanup, and makes this test non-parallel-safe by design:
	// the working directory is process-wide, so pretending otherwise would be the flaky kind of test.
	t.Chdir(gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("could not remove the working directory: %v", err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Skip("this platform still resolves a removed working directory; nothing to prove here")
	}

	dir := spawnDir()
	if dir == "" {
		t.Fatal("spawnDir returned nothing with an unusable working directory, so every child inherits it — " +
			"on Windows that means none of them starts, which is the reported failure")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("spawnDir returned %q, which is not an existing directory (err=%v); a child given a "+
			"non-existent working directory fails exactly as if it had inherited one", dir, err)
	}
}

// TestUnusableCwdExplainsTheWin32Error pins the diagnostic, because the message it replaces is useless:
// "The directory name is invalid" names nothing an operator can act on, and the reporter went looking at the
// INSTANCE rather than at how they had launched the app.
func TestUnusableCwdExplainsTheWin32Error(t *testing.T) {
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("could not remove the working directory: %v", err)
	}

	msg := unusableCwd()
	if msg == "" {
		t.Fatal("unusableCwd reported nothing while the working directory was demonstrably gone; the spawn " +
			"failure would then read only \"The directory name is invalid\"")
	}
	for _, want := range []string{"working directory", "Start menu"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the explanation does not mention %q: %s", want, msg)
		}
	}
}

func TestUnusableCwdIsQuietWhenTheCwdIsFine(t *testing.T) {
	// VACUITY, and the important half: this must not decorate EVERY spawn failure with a guess about the
	// working directory, or the real cause gets buried.
	t.Chdir(t.TempDir())
	if msg := unusableCwd(); msg != "" {
		t.Errorf("unusableCwd invented a cause with a perfectly good working directory: %s", msg)
	}
}

// TestSpawnSetsTheChildsWorkingDirectory pins the wiring. A source read because spawn starts a real detached
// process; asserting the effect would mean launching an install in a test.
func TestSpawnSetsTheChildsWorkingDirectory(t *testing.T) {
	src, err := os.ReadFile("instancectl.go")
	if err != nil {
		t.Fatalf("cannot read instancectl.go: %v", err)
	}
	body := string(src)
	i := strings.Index(body, "func spawn(")
	if i < 0 {
		t.Fatal("spawn moved; this guard must move with it")
	}
	spawn := body[i:]
	if end := strings.Index(spawn, "\nfunc "); end > 0 {
		spawn = spawn[:end]
	}
	if !strings.Contains(spawn, "cmd.Dir = spawnDir()") {
		t.Error("spawn does not set cmd.Dir, so the child inherits the app's working directory — which is " +
			"the reported failure when the app was launched from the installer")
	}
	// AND IT MUST BE THE HELPER, not a literal: a hardcoded "." is the same inheritance by another name.
	if strings.Contains(spawn, `cmd.Dir = "."`) || strings.Contains(spawn, `cmd.Dir = ".\\"`) {
		t.Error("spawn sets a relative working directory, which resolves against the very cwd this fixes")
	}
}

func TestSpawnDirPrefersTheHomeDirectory(t *testing.T) {
	// The home directory is where install/uninstall already keep per-user state, so a child that resolves a
	// relative path lands somewhere it already owns. Pinned because the fallback order is the design.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this host")
	}
	if got := spawnDir(); got != filepath.Clean(home) && got != home {
		t.Logf("spawnDir = %q while the home directory is %q — acceptable only if the home directory was "+
			"unusable", got, home)
	}
}
