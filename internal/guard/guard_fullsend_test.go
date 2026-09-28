package guard

// guard_fullsend_test.go — the SHIM's half of FULLSEND.
//
// FULLSEND is the operator's per-conversation waiver of the permission PROMPT. It has
// TWO enforcement points, and a mode that reached only one of them would refuse what it
// had just approved: internal/askorchicon decides whether to raise a card, and this shim
// is what a shell subprocess actually runs under (rm / chmod / chown / mv / cp / ln).
//
// These tests pin the boundary of the waiver, which is the part worth being careful
// about, because the shim cannot ask and therefore cannot recover from being wrong:
//
//   - the SANCTIONED SET is waived (a target outside the project, the grants and the
//     accept list runs);
//   - the operator's DENY list is NOT waived, in EITHER spelling of home — the deny
//     check runs before the waiver, and pem_match_either is what makes "~/.x" and
//     "$HOME/.x" the same target;
//   - the never-allow binary class is NOT waived. It is a separate case arm that never
//     enters blocked_path, so no environment value can reach it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInteractiveEnvironEmitsFullsendOnlyWhenOn pins the ENV CONTRACT. Off must emit
// nothing at all rather than a "0": an unset variable is the absence of the mode, so a
// shim driven by an environment this code did not build cannot be read as fullsend by
// misreading a value it does not understand.
func TestInteractiveEnvironEmitsFullsendOnlyWhenOn(t *testing.T) {
	off := InteractiveEnviron("/tmp/policy.yaml", "/p", nil, nil, false)
	for _, kv := range off {
		if strings.HasPrefix(kv, FullsendEnvVar+"=") {
			t.Fatalf("fullsend OFF emitted %q — OFF must be the ABSENCE of the variable", kv)
		}
	}
	on := InteractiveEnviron("/tmp/policy.yaml", "/p", nil, nil, true)
	found := false
	for _, kv := range on {
		if kv == FullsendEnvVar+"=1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fullsend ON did not emit %s=1: %v", FullsendEnvVar, on)
	}
	// The worker profile is untouched: no policy path emits NOTHING, fullsend or not.
	if got := InteractiveEnviron("", "/p", nil, nil, true); got != nil {
		t.Fatalf("a fullsend worker-profile env emitted %v — the worker path is the frozen half", got)
	}
}

// TestInteractiveFullsendWaivesTheSanctionedSet is the mode's whole point: a target
// outside the project, the grants and the accept list RUNS instead of being refused.
func TestInteractiveFullsendWaivesTheSanctionedSet(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()
	policy := writePolicyLists(t, nil, nil) // no denies, no accepts
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	victim := filepath.Join(other, "fullsend.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The control: WITHOUT fullsend the same command is refused and the file survives.
	// Without this the test would pass on a shim that never guarded anything.
	exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, proj, nil, nil, false), "rm", "-f", victim)
	if exit == 0 {
		t.Fatalf("control: a target outside the sanctioned set ran without fullsend: %s", out)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("control: the refused target was removed anyway: %v", err)
	}

	// With fullsend the same command runs.
	exit, out = runGuardEnv(t, g, InteractiveEnviron(policy, proj, nil, nil, true), "rm", "-f", victim)
	if exit != 0 {
		t.Fatalf("fullsend must allow a target the sanctioned set does not cover, got exit %d: %s", exit, out)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatal("fullsend reported success but the file is still there")
	}
}

// TestInteractiveFullsendStillRefusesADenyEntry is THE safety test for this mode.
//
// A deny entry is a policy DECISION, not a permission request: no card is ever raised
// for it, so there is no prompt for fullsend to waive. If this fails, fullsend silently
// opens the operator's own exclusions — the presets among them — and the mode's name
// becomes a lie in the one direction that matters.
//
// BOTH SPELLINGS, because the model writes paths any way a shell accepts them: the shim
// expands home itself (pem_expand_home), and a waiver that only honoured the literal
// spelling would be bypassable by writing $HOME instead of ~.
func TestInteractiveFullsendStillRefusesADenyEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	secret := filepath.Join(home, ".secrets")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(secret, "key")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := writePolicyLists(t, []string{"~/.secrets/**"}, nil)
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	for _, spelling := range []string{"~/.secrets/key", "$HOME/.secrets/key", key} {
		exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, "", nil, nil, true), "rm", "-f", spelling)
		if exit == 0 {
			t.Fatalf("FULLSEND OPENED A DENIED PATH (%q): the shim ran rm on it", spelling)
		}
		if !strings.Contains(out, "denied by entry") {
			t.Fatalf("%q: the refusal must name the deny entry, not a generic block: %s", spelling, out)
		}
		if _, err := os.Stat(key); err != nil {
			t.Fatalf("%q: the denied file was removed despite the refusal: %v", spelling, err)
		}
	}
}

// TestInteractiveFullsendCannotReachTheNeverAllowClass — sudo / dd / mkfs* are refused
// before any permission decision exists, so fullsend has nothing to waive. The class is a
// separate case arm, which is what makes it unreachable from any environment value.
func TestInteractiveFullsendCannotReachTheNeverAllowClass(t *testing.T) {
	policy := writePolicyLists(t, nil, nil)
	g, err := NewExecutionGuardWithPolicy("", policy)
	if err != nil {
		t.Fatalf("NewExecutionGuardWithPolicy: %v", err)
	}
	defer g.Close()

	exit, out := runGuardEnv(t, g, InteractiveEnviron(policy, "", nil, nil, true), "sudo", "true")
	if exit == 0 {
		t.Fatalf("FULLSEND RAN sudo: the never-allow class is not a permission to waive")
	}
	if !strings.Contains(out, "never-allow") {
		t.Fatalf("the refusal must name the never-allow class: %s", out)
	}
}
