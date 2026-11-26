package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeScratchFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ageTree sets every entry under root (and root) to `age` ago, so a directory looks idle.
func ageTree(t *testing.T, root string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
}

func TestSessionIDsAreOnePathElement(t *testing.T) {
	for in, want := range map[string]string{
		"ask-01ABC": "ask-01ABC",
		"../../etc": "etc",
		"a/b c":     "a-b-c",
		"":          "anon",
		"///":       "anon",
	} {
		if got := SanitizeSessionID(in); got != want {
			t.Errorf("SanitizeSessionID(%q) = %q, want %q", in, got, want)
		}
	}
	if long := SanitizeSessionID(strings.Repeat("x", 500)); len(long) > 64 {
		t.Errorf("a 500-byte id was not bounded: %d", len(long))
	}
	if NewSessionID() == NewSessionID() {
		t.Error("two session ids collided")
	}
}

func TestRemoveSessionDirRemovesASessionAndOnlyASession(t *testing.T) {
	scratch := t.TempDir()
	dir := SessionDir(scratch, "exec-1")
	writeScratchFile(t, filepath.Join(dir, "xdg", "containers", "storage", "layer", "f"), "x")

	if err := RemoveSessionDir(scratch, dir); err != nil {
		t.Fatalf("RemoveSessionDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the session directory survived: %v", err)
	}
	// Removing what is not there is not an error (an execution that never ran bash has no scratch).
	if err := RemoveSessionDir(scratch, dir); err != nil {
		t.Errorf("removing a missing session errored: %v", err)
	}

	// A WRONG ARGUMENT MUST NOT DELETE ANYTHING ELSE.
	project := t.TempDir()
	writeScratchFile(t, filepath.Join(project, "keep.go"), "package x")
	for _, bad := range []string{project, scratch, filepath.Join(scratch, "xdg"), SessionRoot(scratch), filepath.Join(dir, "xdg")} {
		if err := RemoveSessionDir(scratch, bad); err == nil {
			t.Errorf("RemoveSessionDir accepted %q, which is not a session directory", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(project, "keep.go")); err != nil {
		t.Errorf("a refused removal touched the project: %v", err)
	}
}

func TestSweepRemovesIdleSessionsAndKeepsLiveOnes(t *testing.T) {
	scratch := t.TempDir()
	idle := SessionDir(scratch, "idle")
	live := SessionDir(scratch, "live")
	writeScratchFile(t, filepath.Join(idle, "xdg", "containers", "storage", "a"), "1")
	writeScratchFile(t, filepath.Join(live, "xdg", "containers", "storage", "a"), "1")
	ageTree(t, idle, 24*time.Hour)
	ageTree(t, live, 24*time.Hour)
	// A BUILD IN PROGRESS: only a file deep in the live session was touched just now. The directories above
	// it are old, and the session must still read as active.
	if err := os.Chtimes(filepath.Join(live, "xdg", "containers", "storage", "a"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if n := SweepSessions(scratch, 6*time.Hour, time.Now(), nil); n != 1 {
		t.Errorf("swept %d directories, want 1 (the idle session)", n)
	}
	if _, err := os.Stat(idle); !os.IsNotExist(err) {
		t.Errorf("the idle session was not removed: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("a session with a fresh file inside was removed: %v", err)
	}
}

// The old SHARED layout (<scratch>/xdg, <scratch>/data) is what filled one machine with 34.7 GB. It is cleared
// when idle, and left alone when an older plane is still using it.
func TestSweepClearsTheLegacySharedLayout(t *testing.T) {
	scratch := t.TempDir()
	writeScratchFile(t, filepath.Join(scratch, "xdg", "containers", "storage", "layer"), "x")
	writeScratchFile(t, filepath.Join(scratch, "data", "d"), "x")
	writeScratchFile(t, filepath.Join(scratch, "tui-upcoming-queued.txt"), "unrelated")
	ageTree(t, filepath.Join(scratch, "xdg"), 48*time.Hour)

	if n := SweepSessions(scratch, 6*time.Hour, time.Now(), nil); n != 1 {
		t.Errorf("swept %d, want 1 (the idle legacy xdg; legacy data is fresh)", n)
	}
	if _, err := os.Stat(filepath.Join(scratch, "xdg")); !os.IsNotExist(err) {
		t.Error("the idle legacy xdg survived")
	}
	if _, err := os.Stat(filepath.Join(scratch, "data")); err != nil {
		t.Error("a FRESH legacy directory was removed — an older plane may still be using it")
	}
	if _, err := os.Stat(filepath.Join(scratch, "tui-upcoming-queued.txt")); err != nil {
		t.Error("the sweep touched a file that is not a session or legacy directory")
	}
}

func TestSweepOnAMissingScratchDirIsANoOp(t *testing.T) {
	if n := SweepSessions(filepath.Join(t.TempDir(), "nope"), time.Hour, time.Now(), nil); n != 0 {
		t.Errorf("swept %d from a directory that does not exist", n)
	}
}

// THE CASE THAT MATTERS: a real rootless podman store. After a build it holds files a plain os.RemoveAll
// cannot unlink (measured: 538 paths left behind with permission errors), so removal must go through
// `podman unshare`. Skipped where podman or a local alpine image is missing.
func TestRemoveSessionDirRemovesARealRootlessPodmanStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rootless podman store: not applicable on windows")
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not installed")
	}
	if exec.Command(podman, "image", "exists", "docker.io/library/alpine:3").Run() != nil {
		t.Skip("docker.io/library/alpine:3 is not in the local podman store")
	}
	scratch := t.TempDir()
	dir := SessionDir(scratch, "real")
	xdg := filepath.Join(dir, "xdg")
	ctxDir := filepath.Join(t.TempDir(), "ctx")
	writeScratchFile(t, filepath.Join(ctxDir, "f"), "x")
	writeScratchFile(t, filepath.Join(ctxDir, "Dockerfile"),
		"FROM docker.io/library/alpine:3\nCOPY f /f\nRUN touch /rootfile && chmod 600 /rootfile\n")
	if err := os.MkdirAll(xdg, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "XDG_DATA_HOME="+xdg)
	save := exec.Command(podman, "save", "docker.io/library/alpine:3")
	load := exec.Command(podman, "load", "-q")
	load.Env = env
	pipe, _ := save.StdoutPipe()
	load.Stdin = pipe
	if err := load.Start(); err != nil {
		t.Skipf("cannot start podman load: %v", err)
	}
	if err := save.Run(); err != nil {
		t.Skipf("podman save failed: %v", err)
	}
	if err := load.Wait(); err != nil {
		t.Skipf("podman load into the scratch store failed: %v", err)
	}
	build := exec.Command(podman, "build", "-q", "--pull=never", "-t", "scratchstoretest", ctxDir)
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("podman build in the scratch store failed (%v): %s", err, out)
	}

	if err := RemoveSessionDir(scratch, dir); err != nil {
		t.Fatalf("RemoveSessionDir left a real podman store behind: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the session directory survived: %v", err)
	}
}
