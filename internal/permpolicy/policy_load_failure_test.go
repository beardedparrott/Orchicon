package permpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestABrokenPolicyIsNotADecision is the distinction whose absence cost the operator two rounds of
// diagnosis.
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

// TestADenyIsNotALoadFailure is the CONTROL: a deny is a decision the policy successfully made, so
// it must NOT be mistaken for a broken file.
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
