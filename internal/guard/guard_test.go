package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGuard runs the guard shim for a given binary with the given args and
// returns (exitCode, combinedOutput). The shim is a symlink named after the
// binary pointing at the generated guard script, so argv[0] basename = name.
func runGuard(t *testing.T, g *Guard, name string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(g.dir, name), args...)
	out, err := cmd.CombinedOutput()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run guard %s: %v", name, err)
	}
	return exit, string(out)
}

func TestExecutionGuardBlocksDestructive(t *testing.T) {
	proj := t.TempDir()
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatalf("newExecutionGuard: %v", err)
	}
	defer g.Close()

	cases := []struct {
		name string
		args []string
	}{
		{"rm", []string{"-rf", "/"}},
		{"rm", []string{"-rf", "/home"}},
		{"rm", []string{"-fr", "/"}},
		{"rm", []string{"-r", "/"}},
		{"rm", []string{"-rf", "/home/user/outside-project"}},
		{"rm", []string{"-rf", "~"}},
		{"rm", []string{"-rf", "~/stuff"}},
		{"rm", []string{"-rf", "$HOME"}},
		{"rm", []string{"-rf", "$HOME/things"}},
		{"rm", []string{"-rf", ".."}},
		{"rm", []string{"-rf", "../../escape"}},
		{"rm", []string{"-rf", "/*"}},
		{"sudo", []string{"rm", "-rf", "/"}},
		{"sudo", []string{"true"}},
		{"dd", []string{"if=/dev/zero", "of=/dev/sda"}},
		{"mkfs", []string{"-t", "ext4", "/dev/sdb"}},
		{"shred", []string{"/dev/sda"}},
	}
	for _, tc := range cases {
		exit, out := runGuard(t, g, tc.name, tc.args...)
		if exit == 0 {
			t.Errorf("%s %v: expected blocked (non-zero exit), got exit 0: %s", tc.name, tc.args, out)
		}
		if !strings.Contains(out, "ORCHICON GUARD") {
			t.Errorf("%s %v: expected guard message, got: %s", tc.name, tc.args, out)
		}
	}
}

func TestExecutionGuardAllowsInProject(t *testing.T) {
	proj := t.TempDir()
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatalf("newExecutionGuard: %v", err)
	}
	defer g.Close()

	// rm inside the project works.
	target := filepath.Join(proj, "file.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-rf", target)
	if exit != 0 {
		t.Errorf("rm in project: expected exit 0, got %d: %s", exit, out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("rm in project: file should be deleted")
	}

	// A relative path from the project cwd also resolves inside the project.
	sub := filepath.Join(proj, "sub")
	os.MkdirAll(sub, 0o755)
	cmd := exec.Command(filepath.Join(g.dir, "rm"), "-rf", "./sub")
	cmd.Dir = proj
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("rm ./sub: expected success, got %v: %s", err, out)
	}

	// cp within the project works.
	src := filepath.Join(proj, "a.txt")
	dst := filepath.Join(proj, "b.txt")
	os.WriteFile(src, []byte("a"), 0o644)
	exit, out = runGuard(t, g, "cp", src, dst)
	if exit != 0 {
		t.Errorf("cp in project: expected exit 0, got %d: %s", exit, out)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("cp in project: dest should exist")
	}

	// cp from OUTSIDE the project is blocked.
	exit, out = runGuard(t, g, "cp", "/etc/hostname", filepath.Join(proj, "x.txt"))
	if exit == 0 {
		t.Errorf("cp from outside project: expected blocked, got exit 0: %s", out)
	}
}

func TestExecutionGuardNoProjectMode(t *testing.T) {
	// Empty PROJECT_DIR is the shared host-serve mode (a single serve
	// process hosting sessions in many project dirs). EVERY absolute target
	// is outside scope and blocked — including `rm /`, which an empty-dir
	// guard would otherwise leak through the "$PROJECT_DIR"/* glob.
	g, err := NewExecutionGuard("")
	if err != nil {
		t.Fatalf("NewExecutionGuard(''): %v", err)
	}
	defer g.Close()

	blocked := []struct {
		name string
		args []string
	}{
		{"rm", []string{"-rf", "/"}},
		{"rm", []string{"-rf", "/home"}},
		{"rm", []string{"-rf", "/tmp/whatever"}},
		{"rm", []string{"-rf", "~"}},
		{"rm", []string{"-rf", "$HOME/x"}},
		{"rm", []string{"-rf", ".."}},
		{"cp", []string{"/etc/hostname", "/tmp/copy.txt"}},
	}
	for _, tc := range blocked {
		exit, out := runGuard(t, g, tc.name, tc.args...)
		if exit == 0 {
			t.Errorf("%s %v: expected blocked in no-project mode, got exit 0: %s", tc.name, tc.args, out)
		}
		if !strings.Contains(out, "ORCHICON GUARD") {
			t.Errorf("%s %v: expected guard message, got: %s", tc.name, tc.args, out)
		}
	}

	// Relative paths remain allowed (they resolve inside the session's own
	// directory, scoped by opencode per session).
	dir := t.TempDir()
	cmd := exec.Command(filepath.Join(g.dir, "rm"), "-rf", "junk")
	cmd.Dir = dir
	if err := os.MkdirAll(filepath.Join(dir, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("rm relative in no-project mode: expected success, got %v: %s", err, out)
	}
}

func TestExecutionGuardAppliesToPATH(t *testing.T) {
	proj := t.TempDir()
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatalf("newExecutionGuard: %v", err)
	}
	defer g.Close()

	env := g.Apply(os.Environ())
	cmd := exec.Command("bash", "-c", "rm -rf /")
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ORCHICON GUARD") {
		t.Errorf("bash -c 'rm -rf /' through guard PATH: expected guard message, got: %s", out)
	}

	// A safe command still works through the shimmed PATH.
	cmd = exec.Command("bash", "-c", "echo hi")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash -lc 'echo hi' through guard PATH: expected success, got %v: %s", err, out)
	}
}
