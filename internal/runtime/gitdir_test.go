package runtime

// gitdir_test.go — the host-provisioned worktree pointer must name the CONTAINER's path.
//
// REPORTED as "git.exe is absent from the runtime WSL PATH". Git is not absent — the image installs it, no host
// git is mounted, and the supervisor does not allowlist it. The pointer is wrong: the plane provisions the
// worktree on the host, git writes `gitdir: C:/projects/weatherapp/.git/worktrees/<run>`, and the project is
// mounted into the container at `/mnt/c/projects/weatherapp`. Linux git then cannot resolve the path and reports
// "not a git repository".
//
// These tests are pure filesystem work — no container, no git binary — because the fix is a string rewrite of a
// file the host wrote.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietSink() *sinkLogger { return &sinkLogger{} }

type sinkLogger struct{ infos, warns int }

func (s *sinkLogger) Info(string, ...any) { s.infos++ }
func (s *sinkLogger) Warn(string, ...any) { s.warns++ }

// THE REPORTED SHAPE, end to end: a worktree whose `.git` names the host path becomes usable in-container.
func TestHostGitdirPointerIsRepointedToTheGuestPath(t *testing.T) {
	root := t.TempDir() // stands in for /mnt/c/projects/weatherapp
	worktree := filepath.Join(root, ".orchicon-worktrees", "01M6EFBYBRJWPF7ET3FNDGQ5X8")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitFile := filepath.Join(worktree, ".git")
	// EXACTLY what Windows git writes: forward slashes, absolute host path.
	const hostGitdir = "C:/projects/weatherapp/.git/worktrees/01M6EFBYBRJWPF7ET3FNDGQ5X8"
	if err := os.WriteFile(gitFile, []byte("gitdir: "+hostGitdir+"\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	maps := []pathMapping{{Host: `C:\projects\weatherapp`, Guest: root}}
	if n := rewriteGuestGitdirs(root, maps, quietSink()); n != 1 {
		t.Fatalf("rewrote %d pointers, want 1 — the container's git would still read a host path", n)
	}

	raw, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatalf("read .git: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	want := "gitdir: " + root + "/.git/worktrees/01M6EFBYBRJWPF7ET3FNDGQ5X8"
	if got != want {
		t.Fatalf(".git = %q\n  want %q\n  a host path here is what makes the container's git report "+
			"'not a git repository'", got, want)
	}
	if strings.Contains(got, "C:/projects") {
		t.Fatal("the host path survived the rewrite")
	}
}

// A BACKSLASH-form pointer is the same place, and must be recognised too — which git wrote the file decides
// which separator is in it, and comparing literally would miss one of them.
func TestBackslashFormPointerIsAlsoRepointed(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitFile := filepath.Join(worktree, ".git")
	if err := os.WriteFile(gitFile, []byte(`gitdir: C:\projects\weatherapp\.git\worktrees\wt`+"\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	maps := []pathMapping{{Host: `C:\projects\weatherapp`, Guest: root}}
	if n := rewriteGuestGitdirs(root, maps, quietSink()); n != 1 {
		t.Fatalf("rewrote %d pointers, want 1", n)
	}
	raw, _ := os.ReadFile(gitFile)
	if strings.Contains(string(raw), `C:\projects`) || strings.Contains(string(raw), "C:/projects") {
		t.Fatalf("a backslash-form host path survived: %q", string(raw))
	}
}

// IDEMPOTENT: a file already naming the guest path is left byte-identical, so every container start is safe.
func TestAlreadyGuestFormPointerIsUntouched(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitFile := filepath.Join(worktree, ".git")
	content := "gitdir: " + root + "/.git/worktrees/wt\n"
	if err := os.WriteFile(gitFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	maps := []pathMapping{{Host: `C:\projects\weatherapp`, Guest: root}}
	if n := rewriteGuestGitdirs(root, maps, quietSink()); n != 0 {
		t.Fatalf("rewrote %d already-correct pointers, want 0", n)
	}
	raw, _ := os.ReadFile(gitFile)
	if string(raw) != content {
		t.Fatalf("an already-correct pointer was modified: %q", string(raw))
	}
}

// A pointer into a repository OUTSIDE the mounted tree matches no prefix and is left alone — guessing would be
// worse than leaving it, and the operator can see it.
func TestUnrelatedPointerIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitFile := filepath.Join(worktree, ".git")
	content := "gitdir: D:/elsewhere/.git/worktrees/wt\n"
	if err := os.WriteFile(gitFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	maps := []pathMapping{{Host: `C:\projects\weatherapp`, Guest: root}}
	if n := rewriteGuestGitdirs(root, maps, quietSink()); n != 0 {
		t.Fatalf("rewrote a pointer outside the mapped root (%d)", n)
	}
	raw, _ := os.ReadFile(gitFile)
	if string(raw) != content {
		t.Fatalf("an unrelated pointer was modified: %q", string(raw))
	}
}

// A plain file named `.git` that is not a pointer (a stray) is left alone.
func TestNonPointerDotGitIsIgnored(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := filepath.Join(dir, ".git")
	if err := os.WriteFile(f, []byte("not a pointer\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	maps := []pathMapping{{Host: `C:\projects\weatherapp`, Guest: root}}
	if n := rewriteGuestGitdirs(root, maps, quietSink()); n != 0 {
		t.Fatalf("rewrote a non-pointer .git file (%d)", n)
	}
}

// The env format is "<host>|<guest>" joined by ';' — chosen because a Windows host path contains ':' and a
// POSIX guest path cannot contain '|'.
func TestPathMappingFormat(t *testing.T) {
	got := parsePathMappings(`C:\projects\weatherapp|/mnt/c/projects/weatherapp;D:\work|/mnt/d/work`)
	if len(got) != 2 {
		t.Fatalf("parsed %d mappings, want 2: %+v", len(got), got)
	}
	if got[0].Host != `C:\projects\weatherapp` || got[0].Guest != "/mnt/c/projects/weatherapp" {
		t.Fatalf("mapping 0 = %+v", got[0])
	}
	// Malformed and identity entries are skipped rather than fatal — a bad mapping must not stop a container.
	for _, raw := range []string{"", "nopipe", "/a|/a", "|x", "x|"} {
		if m := parsePathMappings(raw); len(m) != 0 {
			t.Errorf("parsePathMappings(%q) = %+v, want none", raw, m)
		}
	}
}

// Unmap is Map's inverse for the drive form, and refuses anything it cannot translate confidently.
func TestUnmapRoundTripsTheDriveForm(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	host := `C:\projects\weatherapp`
	guest := m.Map(host)
	if guest != "/mnt/c/projects/weatherapp" {
		t.Fatalf("Map(%q) = %q", host, guest)
	}
	back, ok := m.Unmap(guest)
	if !ok || back != host {
		t.Fatalf("Unmap(%q) = %q, %v; want %q", guest, back, ok, host)
	}
	// Identity mode has nothing to reverse.
	id := NewPathMapper("linux", "")
	if _, ok := id.Unmap("/mnt/c/x"); ok {
		t.Error("identity mode claimed to reverse a guest path")
	}
	// A non-/mnt path is not reversible.
	for _, p := range []string{"/usr/local/bin", "/mnt/1/x", "C:/x"} {
		if _, ok := m.Unmap(p); ok {
			t.Errorf("Unmap(%q) claimed success", p)
		}
	}
}

// THE CREATE FLAGS must carry the mapping, and only when host and guest actually differ — on Linux they do not,
// so the whole mechanism is a no-op there instead of a traversal that always finds nothing.
func TestCreateFlagsCarryTheGitdirMapping(t *testing.T) {
	wsl := NewPathMapper("windows", "wsl")
	req := CreateRequest{Mounts: []MountSpec{{Source: `C:\projects\weatherapp`, Dest: `C:\projects\weatherapp`}}}
	got := gitdirMappingEnv(req.Mounts, wsl)
	if got == "" {
		t.Fatal("no mapping emitted for a Windows mount — the supervisor would never repoint the worktrees")
	}
	if !strings.Contains(got, `C:\projects\weatherapp|/mnt/c/projects/weatherapp`) {
		t.Fatalf("mapping = %q, want the host|guest pair", got)
	}
	if !strings.HasPrefix(got, "ORCHICON_GITDIR_MAPS=") {
		t.Fatalf("mapping = %q, want the env assignment form", got)
	}

	// Identity mode: nothing to map, so nothing is emitted.
	identity := NewPathMapper("linux", "")
	if got := gitdirMappingEnv(req.Mounts, identity); got != "" {
		t.Fatalf("identity mode emitted %q — Linux mounts need no rewrite", got)
	}
	// A nil mapper must not panic.
	if got := gitdirMappingEnv(req.Mounts, nil); got != "" {
		t.Fatalf("nil mapper emitted %q", got)
	}
}

// The mapping must reach the CREATE ARGV, not merely the helper — a helper that is never called is the class of
// bug this whole exercise is about.
func TestCreateFlagsEmitTheGitdirMappingForAWSLPlane(t *testing.T) {
	d, _ := podmanCreateDaemon(t)
	d.Paths = NewPathMapper("windows", "wsl")
	// The FULL argv: createFlagArgs + standardHostMountArgs, the same composition the create path assembles
	// (daemon.go) and the same one daemon_createflags_test.go uses. The env block lives in the latter.
	req := CreateRequest{
		WorkflowID: "wf-1",
		InstanceID: "dev",
		Mounts:     []MountSpec{{Source: `C:\projects\weatherapp`, Dest: `C:\projects\weatherapp`}},
	}
	args := append(d.createFlagArgs("orchicon-rt-1", req), d.standardHostMountArgs(req)...)
	var found string
	for _, a := range args {
		if strings.HasPrefix(a, envGitdirMaps+"=") {
			found = a
		}
	}
	if found == "" {
		t.Fatalf("no %s in the create argv — the supervisor would never repoint the worktrees: %v",
			envGitdirMaps, args)
	}
	if !strings.Contains(found, `C:\projects\weatherapp|/mnt/c/projects/weatherapp`) {
		t.Fatalf("mapping = %q, want the host|guest pair", found)
	}
}

// ...and it must NOT be emitted on a Linux plane, where host and guest are the same path.
func TestCreateFlagsOmitTheMappingOnLinux(t *testing.T) {
	d, _ := podmanCreateDaemon(t)
	d.Paths = NewPathMapper("linux", "")
	req := CreateRequest{
		WorkflowID: "wf-1",
		InstanceID: "dev",
		Mounts:     []MountSpec{{Source: "/home/x/app", Dest: "/home/x/app"}},
	}
	args := append(d.createFlagArgs("orchicon-rt-1", req), d.standardHostMountArgs(req)...)
	for _, a := range args {
		if strings.HasPrefix(a, envGitdirMaps+"=") {
			t.Fatalf("a Linux plane emitted %q — identity mounts need no rewrite", a)
		}
	}
}
