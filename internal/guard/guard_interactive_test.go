package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// guard_interactive_test.go pins the interactive (Ask) profile: reads free,
// gated writes, and the permanent never-allow class. The interactive profile is
// switched on by ORCHICON_GUARD_POLICY (only the Ask path sets it); every test
// here except the worker-parity one builds the env through InteractiveEnviron.

// runGuardEnv runs the shim for a binary with the interactive profile's
// environment appended to the ambient one. The shim is a symlink named after
// the binary pointing at the generated guard script, so argv[0] basename = name.
func runGuardEnv(t *testing.T, g *Guard, env []string, name string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(g.dir, name), args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run guard %s: %v", name, err)
	}
	return exit, string(out)
}

// writePolicyLists writes a policy file with explicit deny and accept halves
// (an empty half is rendered as `[]`, the bytes permpolicy.WriteFile emits).
func writePolicyLists(t *testing.T, deny, accept []string) string {
	t.Helper()
	var b strings.Builder
	if len(deny) == 0 {
		b.WriteString("deny: []\n")
	} else {
		b.WriteString("deny:\n")
		for _, d := range deny {
			b.WriteString("  - " + d + "\n")
		}
	}
	if len(accept) == 0 {
		b.WriteString("accept: []\n")
	} else {
		b.WriteString("accept:\n")
		for _, a := range accept {
			b.WriteString("  - " + a + "\n")
		}
	}
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

// TestInteractiveGrantAllowsInsideGrantedDir — AC-1a: after a session grant for
// a directory, `rm` inside it succeeds.
func TestInteractiveGrantAllowsInsideGrantedDir(t *testing.T) {
	proj := t.TempDir()
	grant := t.TempDir()
	policy := writePolicyLists(t, nil, nil)
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	victim := filepath.Join(grant, "x.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := InteractiveEnviron(policy, proj, []string{grant}, nil)
	exit, out := runGuardEnv(t, g, env, "rm", "-rf", victim)
	if exit != 0 {
		t.Fatalf("rm inside the granted directory: expected exit 0, got %d: %s", exit, out)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("the granted target should have been deleted")
	}
}

// TestInteractiveRefusesOutsideProjectAndGrants — AC-1b: a target outside both
// the project and the granted set is refused, naming the path and the reason.
func TestInteractiveRefusesOutsideProjectAndGrants(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()
	policy := writePolicyLists(t, nil, nil)
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	victim := filepath.Join(other, "keep.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, proj, nil, nil), "rm", "-rf", victim)
	if exit == 0 {
		t.Fatalf("rm outside project and grants ran: %s", out)
	}
	if !strings.Contains(out, victim) {
		t.Fatalf("the refusal must name the path: %s", out)
	}
	if !strings.Contains(out, "outside the conversation's project") {
		t.Fatalf("the refusal must state the reason: %s", out)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the refused target was deleted: %v", err)
	}
}

// TestInteractiveDenyOutranksGrant — AC-2: a persistent deny entry is refused
// even with a session grant, naming the deny entry.
func TestInteractiveDenyOutranksGrant(t *testing.T) {
	proj := t.TempDir()
	grant := t.TempDir()
	entry := grant + "/**"
	policy := writePolicyLists(t, []string{entry}, nil)
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	victim := filepath.Join(grant, "key.pem")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, proj, []string{grant}, nil), "rm", "-rf", victim)
	if exit == 0 {
		t.Fatalf("a deny entry must outrank a session grant, but the delete ran: %s", out)
	}
	if !strings.Contains(out, "denied by entry") || !strings.Contains(out, entry) {
		t.Fatalf("the refusal must name the deny entry %q: %s", entry, out)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the denied target was deleted: %v", err)
	}
}

// TestInteractiveFailsClosedOnUnreadablePolicy — AC-4: a policy the shim cannot
// read (missing, unreadable, malformed) REFUSES the path-scoped command.
func TestInteractiveFailsClosedOnUnreadablePolicy(t *testing.T) {
	target := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(t.TempDir(), "unreadable.yaml")
	if err := os.WriteFile(unreadable, []byte("deny: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(t.TempDir(), "absent.yaml")},
		{"a directory", t.TempDir()},
		{"unreadable", unreadable},
		{"malformed scalar", writePolicyFileRaw(t, "deny: 3\n")},
		{"malformed key", writePolicyFileRaw(t, "allow: [x]\n")},
		{"malformed tab", writePolicyFileRaw(t, "deny:\n\t- x\n")},
	}
	for _, tc := range cases {
		if tc.name == "unreadable" && os.Geteuid() == 0 {
			// root can read a 000 file, so the case cannot be set up.
			continue
		}
		g, err := NewExecutionGuardWithPolicy("", tc.path)
		if err != nil {
			t.Fatalf("NewExecutionGuardWithPolicy(%s): %v", tc.name, err)
		}
		env := InteractiveEnviron(tc.path, "", nil, nil)
		exit, out := runGuardEnv(t, g, env, "rm", "-f", target)
		g.Close()
		if exit == 0 {
			t.Errorf("%s policy: expected the shim to refuse, got exit 0: %s", tc.name, out)
		}
		if !strings.Contains(out, "fail-closed") {
			t.Errorf("%s policy: refusal must be the fail-closed one, got: %s", tc.name, out)
		}
	}
}

func writePolicyFileRaw(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInteractiveAcceptsAcceptEntry — D4: an `accept` entry means "never
// prompts", so the shim must not then refuse the command that proceeds on it.
func TestInteractiveAcceptsAcceptEntry(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()
	policy := writePolicyLists(t, nil, []string{other + "/**"})
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	victim := filepath.Join(other, "allowed.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, proj, nil, nil), "rm", "-f", victim)
	if exit != 0 {
		t.Fatalf("an accept-covered target must be allowed, got exit %d: %s", exit, out)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("the accepted target should have been deleted")
	}
}

// TestInteractiveOnceAllowsOnlyTheApprovedPath — D5: an operator-approved
// `once` target runs while a sibling in the same directory is still refused.
func TestInteractiveOnceAllowsOnlyTheApprovedPath(t *testing.T) {
	proj := t.TempDir()
	dir := t.TempDir()
	policy := writePolicyLists(t, nil, nil)
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	approved := filepath.Join(dir, "approved.txt")
	sibling := filepath.Join(dir, "sibling.txt")
	for _, f := range []string{approved, sibling} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := InteractiveEnviron(policy, proj, nil, []string{approved})

	if exit, out := runGuardEnv(t, g, env, "rm", "-f", approved); exit != 0 {
		t.Fatalf("the approved once-target must run, got exit %d: %s", exit, out)
	}
	if _, err := os.Stat(approved); !os.IsNotExist(err) {
		t.Fatalf("the approved target should have been deleted")
	}
	exit, out := runGuardEnv(t, g, env, "rm", "-f", sibling)
	if exit == 0 {
		t.Fatalf("a sibling of the approved once-target must still be refused: %s", out)
	}
	if !strings.Contains(out, sibling) {
		t.Fatalf("the refusal must name the sibling path: %s", out)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("the sibling was deleted: %v", err)
	}
}

// TestNeverAllowSaysPermanentlyBlocked — AC-3: the never-allow class is refused
// unconditionally, even after a grant, and the refusal says the class is
// permanently blocked rather than inviting another prompt.
func TestNeverAllowSaysPermanentlyBlocked(t *testing.T) {
	proj := t.TempDir()
	grant := t.TempDir()
	policy := writePolicyLists(t, nil, []string{grant + "/**"})
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	cases := []struct {
		name string
		args []string
	}{
		{"sudo", []string{"rm", "-rf", "/"}},
		{"dd", []string{"if=/dev/zero", "of=/dev/sda"}},
		{"mkfs.ext4", []string{"/dev/sdb"}},
		{"fdisk", []string{"-l"}},
		{"parted", []string{"/dev/sdb", "print"}},
		{"shred", []string{"/dev/sda"}},
		{"wipefs", []string{"/dev/sda"}},
		{"lvremove", []string{"/dev/vg0/lv0"}},
	}
	env := InteractiveEnviron(policy, proj, []string{grant}, nil)
	for _, tc := range cases {
		// The shim installs a symlink for EVERY never-allow member (scoped=false
		// links are created whether or not the host has the binary), so the
		// dispatch is exercised even where the tool is not installed.
		exit, out := runGuardEnv(t, g, env, tc.name, tc.args...)
		if exit == 0 {
			t.Errorf("%s: expected the never-allow class to refuse, got exit 0: %s", tc.name, out)
		}
		if !strings.Contains(out, "PERMANENTLY BLOCKED") {
			t.Errorf("%s: the refusal must say the class is permanently blocked: %s", tc.name, out)
		}
		if !strings.Contains(out, "never-allow") {
			t.Errorf("%s: the refusal must name the never-allow class: %s", tc.name, out)
		}
		if strings.Contains(out, "denied by entry") {
			t.Errorf("%s: the permanent class must not be reported as a policy deny (there is no prompt to answer): %s", tc.name, out)
		}
	}
}

// TestWorkerProfileIsUnchanged — AC-7: with no ORCHICON_GUARD_* the shim is the
// worker profile: same shims, same path-scoped policy, same always-block set,
// and the interactive switch cannot leak into it.
func TestWorkerProfileIsUnchanged(t *testing.T) {
	proj := t.TempDir()
	g, err := NewExecutionGuard(proj)
	if err != nil {
		t.Fatalf("NewExecutionGuard: %v", err)
	}
	defer g.Close()

	// A grant/project passed to InteractiveEnviron with NO policy path is the
	// worker profile: nothing is emitted.
	if got := InteractiveEnviron("", proj, []string{"/somewhere"}, []string{"/somewhere/x"}); got != nil {
		t.Fatalf("InteractiveEnviron with no policy path must emit nothing, got %v", got)
	}

	// In-project delete works (unchanged).
	victim := filepath.Join(proj, "file.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runGuard(t, g, "rm", "-rf", victim)
	if exit != 0 {
		t.Fatalf("worker in-project rm: expected exit 0, got %d: %s", exit, out)
	}

	// Out-of-project delete is blocked with the WORKER wording, never the
	// interactive one.
	other := t.TempDir()
	exit, out = runGuard(t, g, "rm", "-rf", other)
	if exit == 0 {
		t.Fatalf("worker out-of-project rm ran: %s", out)
	}
	if strings.Contains(out, "outside the conversation's project") {
		t.Fatalf("the interactive wording leaked into the worker profile: %s", out)
	}
	if !strings.Contains(out, "ORCHICON GUARD") {
		t.Fatalf("expected the worker refusal: %s", out)
	}

	// The always-block class is unchanged.
	exit, out = runGuard(t, g, "sudo", "true")
	if exit == 0 || !strings.Contains(out, "PERMANENTLY BLOCKED") {
		t.Fatalf("sudo must stay unconditionally blocked: exit=%d %s", exit, out)
	}
}
