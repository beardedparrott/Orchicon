package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/permpolicy"
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

// TestExecutionGuardAllowsWorktreeScopedDelete pins AC3: a delete resolving
// inside the worker's directory (the run worktree when provisioned — the
// guard's projectDir IS the worktree) is allowed; a delete escaping the
// worktree/project root stays blocked.
func TestExecutionGuardAllowsWorktreeScopedDelete(t *testing.T) {
	proj := t.TempDir()
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatalf("NewExecutionGuard: %v", err)
	}
	defer g.Close()

	// Recursive delete INSIDE the worktree (project dir) resolves and succeeds.
	sub := filepath.Join(proj, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-rf", sub)
	if exit != 0 {
		t.Fatalf("rm -rf <worktree>/sub: expected exit 0, got %d: %s", exit, out)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("worktree subdir should have been deleted")
	}

	// A delete resolving OUTSIDE the worktree/project root stays blocked.
	other := t.TempDir()
	exit, out = runGuard(t, g, "rm", "-rf", other)
	if exit == 0 {
		t.Fatalf("rm -rf outside the worktree: expected blocked (non-zero exit), got exit 0: %s", out)
	}
	if !strings.Contains(out, "ORCHICON GUARD") {
		t.Fatalf("expected guard message, got: %s", out)
	}
}

// writePolicyFile writes a policy file with the given deny entries.
func writePolicyFile(t *testing.T, deny ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("deny:\n")
	for _, d := range deny {
		b.WriteString("  - " + d + "\n")
	}
	b.WriteString("accept: []\n")
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

// TestPolicyDenyOutranksNeverAllowAndProjectScope pins the guard half of the
// precedence chain: the never-allow binary class is absolute and comes FIRST,
// the deny list outranks the project-scope allow (which is what the shim
// would otherwise grant for an in-project target).
func TestPolicyDenyBlocksInProjectTargetAndNeverAllowStaysAbsolutestFirst(t *testing.T) {
	proj := t.TempDir()
	policy := writePolicyFile(t, filepath.Join(proj, "secret")+"/**")
	g, err := NewExecutionGuardWithPolicy(proj, policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	// In-project target: blocked_path ALLOWS it (inside PROJECT_DIR — the
	// shim's normal verdict), the policy deny list refuses it. Without the
	// policy hook this command would have run.
	if err := os.MkdirAll(filepath.Join(proj, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(proj, "secret", "key.pem")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-rf", victim)
	if exit == 0 {
		t.Fatalf("policy-denied in-project delete ran: %s", out)
	}
	if !strings.Contains(out, "denied by entry") || !strings.Contains(out, filepath.Join(proj, "secret")+"/**") {
		t.Fatalf("refusal must name the deny entry: %s", out)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("policy-denied target was deleted: %v", err)
	}

	// The never-allow class is FIRST and cannot be reached past by the deny
	// list: even with a deny entry that covers the argument, sudo reports the
	// destructive-command refusal (a command denied by policy is a DIFFERENT
	// outcome from a command that can never run at all).
	pol2 := writePolicyFile(t, "**")
	g2, err := NewExecutionGuardWithPolicy(proj, pol2)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g2.Close()
	exit, out = runGuard(t, g2, "sudo", "rm", "-rf", "/")
	if exit == 0 {
		t.Fatal("sudo ran")
	}
	if strings.Contains(out, "denied by entry") {
		t.Fatalf("never-allow binaries must be refused by the case block, not the policy: %s", out)
	}
	if !strings.Contains(out, "destructive") {
		t.Fatalf("want the never-allow refusal, got: %s", out)
	}
}

// TestPolicyEditTakesEffectOnTheNextCommand pins the live-read semantics for
// the bash path: ONE guard, no rebuild, and the deny disappears when the
// entry is removed from the file (the shim re-reads per invocation).
func TestPolicyEditTakesEffectOnTheNextCommand(t *testing.T) {
	proj := t.TempDir()
	deny := filepath.Join(proj, "sub") + "/**"
	policy := writePolicyFile(t, deny)
	g, err := NewExecutionGuardWithPolicy(proj, policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	mk := func() string {
		if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(proj, "sub", "a.txt")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	if exit, out := runGuard(t, g, "rm", "-f", mk()); exit == 0 {
		t.Fatalf("first invocation should be denied: %s", out)
	}

	// The operator deletes the entry (a hand-edit is the same bytes as a UI
	// write). Same guard, same process, no restart.
	if err := os.WriteFile(policy, []byte("deny: []\naccept: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-f", mk())
	if exit != 0 {
		t.Fatalf("after the entry was deleted the command must succeed, got exit %d: %s", exit, out)
	}
}

// TestPolicyTildeEntryMatchesHome pins the preset's own spelling: `~/.ssh/**`
// must match an absolute $HOME/.ssh/... target even though blocked_path would
// already refuse it — the entry must be recognised as the reason.
func TestPolicyTildeEntryExpandsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The project dir IS the home here, so blocked_path ALLOWS the target
	// (in-project) and only the policy entry can refuse it.
	policy := writePolicyFile(t, "~/.ssh/**")
	g, err := NewExecutionGuardWithPolicy(home, policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, ".ssh", "id_rsa")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-f", key)
	if exit == 0 {
		t.Fatalf("rm on the denied credential store ran: %s", out)
	}
	if !strings.Contains(out, "denied by entry") || !strings.Contains(out, "~/.ssh/**") {
		t.Fatalf("the ~/.ssh/** preset entry must be named as the reason: %s", out)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("denied credential store was deleted: %v", err)
	}
}

// TestShippedPolicyFileFormatIsReadableByTheShim pins the seam between the two
// halves of the persistent permission policy: the shim's denied_target() parses
// the file BY HAND, so the exact bytes permpolicy.WriteFile emits (the
// documented header block plus yaml.v3's rendering of the two lists) must be
// readable by it. The other guard tests use a hand-written fixture, which
// cannot catch a writer change the reader cannot follow — a preset that the
// shim silently parses to nothing is precisely the failure the feature's
// "malformed input fails loudly" rule exists to prevent.
func TestShippedPolicyFileFormatIsReadableByTheShim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := permpolicy.WriteFile(path, permpolicy.MustParsePreset()); err != nil {
		t.Fatalf("write the shipped preset: %v", err)
	}

	// home IS the project dir here, so blocked_path ALLOWS the target (it is
	// in-project) and only a parsed policy entry can refuse it — which is what
	// makes this a test of the parser, not of containment.
	g, err := NewExecutionGuardWithPolicy(home, path)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, ".ssh", "id_rsa")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-f", key)
	if exit == 0 {
		t.Fatalf("the shipped preset did not refuse %s — the shim cannot read what permpolicy.WriteFile writes: %s", key, out)
	}
	if !strings.Contains(out, "denied by entry") || !strings.Contains(out, "~/.ssh/**") {
		t.Fatalf("the refusal must name the preset entry: %s", out)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("the denied credential store was deleted: %v", err)
	}
}
