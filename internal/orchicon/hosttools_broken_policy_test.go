package orchicon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

// brokenPolicySuite is policySuite with a MALFORMED policy file — the state the
// operator's live config reached, which locked them out of every action including
// reading the file that would explain it.
func brokenPolicySuite(t *testing.T) (*HostTools, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(dir, "permission-policy.yaml")
	// The exact shape that broke the operator's file: a key indented one space, so it
	// is neither flush left nor inside the sequence above it.
	if err := os.WriteFile(policyPath, []byte("deny:\n    - ~/.ssh/**\n accept: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHostTools(dir, "")
	h.SetPathPolicy(permpolicy.NewStore(policyPath).HostSuiteGuard())
	return h, dir
}

// TestABrokenPolicyDoesNotBlockARead is the operator's lockout as a test.
//
// When the policy cannot be parsed there is NO decision to respect — yet every action
// was refused, including reads, so the only thing that would explain the problem (the
// file itself) was the one thing they could not look at from inside Orchicon. They had
// to fix it from a shell outside and paste it into chat for me to read.
func TestABrokenPolicyDoesNotBlockARead(t *testing.T) {
	h, dir := brokenPolicySuite(t)
	target := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := h.Execute(context.Background(), "read", `{"path":"`+target+`"}`)
	if err != nil {
		t.Fatalf("a READ must survive a broken policy — otherwise the operator cannot look at the file that is broken: %v", err)
	}
	if out == "" {
		t.Error("the read returned nothing")
	}
}

// TestABrokenPolicyStillBlocksAWrite is the security half, and it is the reason the
// exemption is limited to reads: a broken policy means no decision was made, so an
// action that can CHANGE something must not proceed on silence.
func TestABrokenPolicyStillBlocksAWrite(t *testing.T) {
	h, dir := brokenPolicySuite(t)
	target := filepath.Join(dir, "victim.txt")

	_, err := h.Execute(context.Background(), "write", `{"filePath":"`+target+`","content":"x"}`)
	if err == nil {
		t.Fatal("a WRITE must still refuse when the policy cannot be read — no decision is not permission")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("the refused write created its target anyway")
	}
}

// TestABrokenPolicyStillBlocksAnExecution: the same rule for bash, the other
// world-changing tool.
func TestABrokenPolicyStillBlocksAnExecution(t *testing.T) {
	h, _ := brokenPolicySuite(t)
	if _, err := h.Execute(context.Background(), "bash", `{"command":"touch /tmp/orchicon-broken-policy-probe"}`); err == nil {
		t.Fatal("a shell command must still refuse when the policy cannot be read")
	}
	if _, statErr := os.Stat("/tmp/orchicon-broken-policy-probe"); statErr == nil {
		t.Fatal("the refused command ran anyway")
	}
}
