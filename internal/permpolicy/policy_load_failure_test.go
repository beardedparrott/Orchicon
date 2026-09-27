package permpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestABrokenPolicyIsNotADecision is the distinction whose absence cost the operator
// two rounds of diagnosis.
//
// A malformed policy file and a policy that DENIES both surfaced as a bare error, so
// nothing could tell "this file is denied" (a decision — respect it) from "the policy
// is broken" (no decision was ever made). The native adapter reported the parse
// failure to the model as "the operator denied you — do not retry", which was untrue
// and the opposite of the right advice once the file was fixed.
func TestABrokenPolicyIsNotADecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n accept: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)

	_, err := store.Decide("/tmp/x", Inputs{})
	if err == nil {
		t.Fatal("a malformed policy must fail, not silently allow")
	}
	if !IsPolicyLoadFailure(err) {
		t.Fatalf("err = %v, want a POLICY LOAD failure — a parse error must not read as a decision", err)
	}
	var pe *PolicyLoadError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *PolicyLoadError so a caller can name the broken file", err)
	}
	if pe.Path != path {
		t.Errorf("PolicyLoadError.Path = %q, want %q — the message must name the file", pe.Path, path)
	}
}

// TestADenyIsNotALoadFailure is the CONTROL: a deny is a decision the policy
// successfully made, so it must NOT be mistaken for a broken file — otherwise the
// read exemption would let a denied read through.
func TestADenyIsNotALoadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n  - /secret/**\naccept: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)

	d, err := store.Decide("/secret/key.pem", Inputs{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %v, want deny", d.Verdict)
	}
	if IsPolicyLoadFailure(store.Refusal("/secret/key.pem", "/secret/**")) {
		t.Fatal("a refusal is a DECISION — it must not be reported as a load failure, or a denied read would be let through")
	}
	if IsPolicyLoadFailure(nil) {
		t.Fatal("nil is not a load failure")
	}
	if IsPolicyLoadFailure(errors.New("something else")) {
		t.Fatal("an unrelated error is not a load failure")
	}
}
