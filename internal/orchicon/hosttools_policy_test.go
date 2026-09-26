package orchicon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

// hosttools_policy_test.go pins the persistent-permission-policy enforcement
// at the REAL seam: HostTools.Execute, the one function every host-suite call
// passes through. permpolicy's own tests prove Decide's precedence in
// isolation; this file proves the suite actually consults that accessor, that
// a refusal reaches the caller verbatim, and that the file is re-read per call
// (so an edit needs no restart).

// policySuite builds a host suite scoped to dir with the preset policy
// installed at a temp policy file, plus a HOME override so `~/.ssh/**`
// expands into the test's temp tree and never touches the real home.
func policySuite(t *testing.T) (*HostTools, *permpolicy.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	policyPath := filepath.Join(dir, "permission-policy.yaml")
	if err := permpolicy.WriteFile(policyPath, permpolicy.MustParsePreset()); err != nil {
		t.Fatalf("write preset: %v", err)
	}
	store := permpolicy.NewStore(policyPath)
	h := NewHostTools(dir, "")
	h.SetPathPolicy(store.HostSuiteGuard())
	return h, store, policyPath, filepath.Join(home, ".ssh", "id_rsa")
}

func writeCall(t *testing.T, target string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"filePath": target, "content": "stolen"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHostSuitePolicyDenyBlocksAWriteEvenAfterASessionGrant is AC1 at the
// enforcement seam: the preset `~/.ssh/**` entry refuses a write, the refusal
// NAMES the entry, and a session grant cannot open it (Decide reads the deny
// list before the grant field, so the suite — which holds no grant of its own
// — cannot be talked past the entry).
func TestHostSuitePolicyDenyBlocksAWriteEvenAfterASessionGrant(t *testing.T) {
	h, store, _, target := policySuite(t)
	ctx := context.Background()

	_, err := h.Execute(ctx, "write", writeCall(t, target))
	if err == nil {
		t.Fatalf("write to %s succeeded; the preset deny entry must refuse it", target)
	}
	if !strings.Contains(err.Error(), "~/.ssh/**") {
		t.Errorf("refusal does not name the denied entry: %v", err)
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("refusal is not recognisable as a policy denial: %v", err)
	}
	// The denied file must not exist: a refusal that still wrote is worse
	// than none.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("denied write still created %s", target)
	}

	// A read of the same credential store is refused too (reads never ask,
	// which is exactly why the preset denies them).
	if _, err := h.Execute(ctx, "read", `{"path":`+jsonString(target)+`}`); err == nil {
		t.Errorf("read of a denied path succeeded")
	}

	// And the deny outranks a session grant by evaluation order.
	if d, err := store.Decide(target, permpolicy.Inputs{SessionGranted: true}); err != nil {
		t.Fatal(err)
	} else if d.Verdict != permpolicy.VerdictDeny {
		t.Errorf("Decide with a session grant = %v, want deny (the deny list sits above the grant)", d.Verdict)
	}
}

// TestHostSuitePolicyEditTakesEffectOnTheNextCall is AC2: deleting the entry
// from the FILE — no restart, no reopen — makes the same call succeed on the
// very next consult.
func TestHostSuitePolicyEditTakesEffectOnTheNextCall(t *testing.T) {
	h, store, _, target := policySuite(t)
	ctx := context.Background()

	if _, err := h.Execute(ctx, "write", writeCall(t, target)); err == nil {
		t.Fatalf("precondition failed: the preset entry did not refuse the write")
	}

	// The operator deletes the entry by hand (here: an equivalent write of an
	// empty policy to the same file). Nothing signals the suite.
	if err := store.Write(permpolicy.Policy{}); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}

	if _, err := h.Execute(ctx, "write", writeCall(t, target)); err != nil {
		t.Fatalf("write failed after the entry was deleted (live-read violated): %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("write succeeded but %s is absent: %v", target, err)
	}
}

// TestHostSuiteNoPolicyHookLeavesTheSuiteUnchanged pins that the worker path
// (no hook installed) is untouched: with no policy accessor wired, a write to
// a ~/.ssh-shaped path is not refused by this layer.
func TestHostSuiteNoPolicyHookLeavesTheSuiteUnchanged(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	h := NewHostTools(dir, "")
	target := filepath.Join(home, ".ssh", "id_rsa")
	if _, err := h.Execute(context.Background(), "write", writeCall(t, target)); err != nil {
		t.Fatalf("no hook installed: write should not be refused by the policy layer: %v", err)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
