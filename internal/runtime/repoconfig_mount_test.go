package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepoConfigGuardMountIsReadOnlyAndNamesTheConfigFile(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(proj, ".git", "config")
	if err := os.WriteFile(cfg, []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, ok := repoConfigGuardMount(proj)
	if !ok {
		t.Fatal("no guard mount for an ordinary checkout")
	}
	if !m.RO || m.Source != cfg || m.Dest != cfg {
		t.Errorf("mount = %+v, want a READ-ONLY bind of %s onto itself", m, cfg)
	}
}

func TestRepoConfigGuardMountIsAbsentWhereThereIsNothingToProtect(t *testing.T) {
	// no .git at all
	if _, ok := repoConfigGuardMount(t.TempDir()); ok {
		t.Error("mounted a config for a directory with no .git")
	}
	// a .git FILE: the project dir is itself a linked worktree
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x/.git/worktrees/y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := repoConfigGuardMount(wt); ok {
		t.Error("mounted a config for a linked worktree")
	}
	// a .git dir with no config
	nc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(nc, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := repoConfigGuardMount(nc); ok {
		t.Error("mounted a config that does not exist (the engine would create a DIRECTORY there)")
	}
	if _, ok := repoConfigGuardMount(""); ok {
		t.Error("mounted a config for an empty project dir")
	}
}

func TestRepoConfigGuardMountHonoursTheOptOut(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "true", "TRUE"} {
		t.Setenv(envGitConfigRW, v)
		if _, ok := repoConfigGuardMount(proj); ok {
			t.Errorf("%s=%q still mounted the config read-only", envGitConfigRW, v)
		}
	}
	t.Setenv(envGitConfigRW, "")
	if _, ok := repoConfigGuardMount(proj); !ok {
		t.Error("an empty opt-out variable disabled the guard")
	}
}

// The mount must reach the engine as `:ro` on a FILE, and the lifecycle must actually add it: a helper nothing
// calls is not a guard.
func TestTheLifecycleAddsTheConfigGuardMount(t *testing.T) {
	raw, err := os.ReadFile("lifecycle.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "repoConfigGuardMount(projectDir)") {
		t.Error("lifecycle.go no longer adds the read-only .git/config mount")
	}
}

// END TO END against a real engine when one is available: the poisoning `git init` fails and the config is
// byte-identical afterwards; ordinary worker git still works. Skipped without podman or the dev image.
func TestReadOnlyConfigBlocksThePoisonInARealContainer(t *testing.T) {
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not installed")
	}
	const img = "orchicon-runtime:dev"
	if exec.Command(podman, "image", "exists", img).Run() != nil {
		t.Skip(img + " not present")
	}
	root := t.TempDir()
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	proj := filepath.Join(root, "proj")
	_ = os.MkdirAll(proj, 0o755)
	sh(proj, "init", "-q", "-b", "develop")
	sh(proj, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i")
	wt := filepath.Join(proj, ".orchicon-worktrees", "run1")
	sh(proj, "worktree", "add", "-q", "-b", "feature", wt, "develop")
	cfg := filepath.Join(proj, ".git", "config")
	before, _ := os.ReadFile(cfg)

	m, ok := repoConfigGuardMount(proj)
	if !ok {
		t.Fatal("no guard mount")
	}
	script := `cd ` + wt + `
git -c user.name=w -c user.email=w@w commit -q --allow-empty -m w || { echo COMMIT-FAILED; exit 1; }
env GIT_DIR=` + proj + `/.git/worktrees/run1 GIT_WORK_TREE=` + wt + ` git init -q /tmp/fixture && echo POISON-SUCCEEDED || echo POISON-BLOCKED`
	out, err := exec.Command(podman, "run", "--rm", "--userns=keep-id", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()),
		"-v", root+":"+root, "-v", m.Source+":"+m.Dest+":ro", "--entrypoint", "bash", img, "-c", script).CombinedOutput()
	if err != nil {
		t.Skipf("container run failed (%v): %s", err, out)
	}
	if !strings.Contains(string(out), "POISON-BLOCKED") || strings.Contains(string(out), "POISON-SUCCEEDED") {
		t.Errorf("the poisoning git init was not blocked: %s", out)
	}
	if strings.Contains(string(out), "COMMIT-FAILED") {
		t.Errorf("an ordinary commit in the worktree failed under the read-only config: %s", out)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Errorf("the shared config changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The guard mount reaches the engine as exactly `-v <cfg>:<cfg>:ro`, after the project's read-write bind (the
// engine nests by path depth, so a file under the project mount is mounted over it).
func TestTheConfigGuardMountRendersAsAReadOnlyFileBind(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(proj, ".git", "config")
	if err := os.WriteFile(cfg, []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := repoConfigGuardMount(proj)
	got := mountArgv([]MountSpec{{Source: proj, Dest: proj}, m})
	want := []string{"-v", proj + ":" + proj, "-v", cfg + ":" + cfg + ":ro"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("mountArgv = %v, want %v", got, want)
	}
}
