package askorchicon

// guard_consent_test.go — AC-6 across the two layers: the guard shim the Ask
// path execs and the consent core read the SAME policy accessor.
//
// The guard package's own equivalence test pins the READER (shim verdict =
// permpolicy.Decide verdict). This one pins the WIRING: the environment the
// real Service hands to bash carries the policy path, the conversation's project
// and its session grants, and the shim's refusal names the same deny entry the
// consent layer's Refusal would name.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

func runBashWithEnv(t *testing.T, env []string, script string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run bash: %v", err)
	}
	return exit, string(out)
}

func TestGuardShimAndConsentCoreReadTheSamePolicy(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "permission-policy.yaml")
	deniedDir := filepath.Join(dir, "denied")
	grantedDir := filepath.Join(dir, "granted")
	for _, d := range []string{deniedDir, grantedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deniedTarget := filepath.Join(deniedDir, "secret.txt")
	grantedTarget := filepath.Join(grantedDir, "ok.txt")
	for _, f := range []string{deniedTarget, grantedTarget} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := deniedDir + "/**"
	if err := os.WriteFile(policy, []byte("deny:\n  - "+entry+"\naccept: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The Service resolves the policy path through permpolicy.DefaultPath(), so
	// the shim and the consent core read the file the env names.
	t.Setenv(permpolicy.PolicyEnv, policy)

	svc := &Service{grants: newGrantStore(), once: newOnceStore(), log: slog.Default()}
	const conv = "conv-1"
	svc.grants.Grant(conv, grantedDir)

	scope := AskFileScope{Dir: grantedDir, FromConversation: true}
	env := svc.askGuardEnviron(scope, conv)()
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		guard.PolicyEnvVar + "=" + policy,
		guard.ProjectEnvVar + "=" + grantedDir,
		guard.GrantsEnvVar + "=" + grantedDir,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the env the shim runs with is missing %q:\n%s", want, joined)
		}
	}

	store := permpolicy.NewStore(policy)
	want, err := store.Decide(deniedTarget, permpolicy.Inputs{SessionGranted: svc.grants.Has(conv, filepath.Dir(deniedTarget))})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if want.Verdict != permpolicy.VerdictDeny || want.Entry != entry {
		t.Fatalf("the consent core's verdict for the denied path = %s/%q, want deny/%q", want.Verdict, want.Entry, entry)
	}

	// A granted path runs through the shim (the grant rides the env).
	if exit, out := runBashWithEnv(t, env, "rm -f "+grantedTarget); exit != 0 {
		t.Fatalf("the granted path must run through the shim, got exit %d: %s", exit, out)
	}
	if _, err := os.Stat(grantedTarget); !os.IsNotExist(err) {
		t.Fatalf("the granted target should have been deleted")
	}

	// The denied path is refused by the SAME entry the consent core names.
	exit, out := runBashWithEnv(t, env, "rm -f "+deniedTarget)
	if exit == 0 {
		t.Fatalf("the denied path ran through the shim: %s", out)
	}
	ref := store.Refusal(deniedTarget, want.Entry).Error()
	if !strings.Contains(out, want.Entry) || !strings.Contains(ref, want.Entry) {
		t.Fatalf("the shim and the consent core must name the SAME deny entry %q\nshim: %s\nconsent: %s", want.Entry, out, ref)
	}
	if !strings.Contains(out, deniedTarget) {
		t.Fatalf("the shim refusal must name the target: %s", out)
	}
	if _, err := os.Stat(deniedTarget); err != nil {
		t.Fatalf("the denied target was deleted: %v", err)
	}
}

// TestAskBashInteractiveProfileThroughTheRealPlumbing pins the interactive
// profile at the WIDEST layer: the environment the real Service hands to Ask
// bash decides whether a path-scoped command runs or is refused, and the refusal
// comes back as the tool's RESULT (not a transport error) so the model can
// course-correct. AC-1 (allow in scope / refuse out of scope), AC-4 (fail closed
// when the policy cannot be read) and AC-5 (refusal = tool result).
//
// The existing TestNativeAskBashGuardBlocksDestructive cannot assert this: it
// runs with no policy file at permpolicy.DefaultPath(), so the shim fails CLOSED
// (AC-4, correct) and its `rm /etc/passwd` case would pass without the scope
// check ever firing. This test pins the policy file first, so the scope rule is
// the thing under test — and then removes it again to pin the failing-closed
// behaviour on the same plumbing.
func TestAskBashInteractiveProfileThroughTheRealPlumbing(t *testing.T) {
	project := t.TempDir()
	policy := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(policy, []byte("deny: []\naccept: []\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	t.Setenv("ORCHICON_PERMISSION_POLICY", policy)

	restore := stubAskFileRoot(project)
	defer restore()
	p := (&Service{toolRegistry: testToolRegistry()}).NativeAskTools()

	// Readable policy + in-project target: the shim's project default allows it.
	victim := filepath.Join(project, "gone.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := p.ExecuteAskTool(context.Background(), "bash", `{"command":"rm -f `+victim+`"}`)
	if err != nil {
		t.Fatalf("in-project rm returned a transport error: %v", err)
	}
	if strings.Contains(out, "ORCHICON GUARD") {
		t.Fatalf("an in-project rm must run under a readable policy, got: %s", out)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("the in-project target was not deleted: %v", err)
	}

	// Readable policy + out-of-project target: refused, naming the path, and the
	// refusal is the scope rule — not the fail-closed message.
	out, err = p.ExecuteAskTool(context.Background(), "bash", `{"command":"rm /etc/passwd"}`)
	if err != nil {
		t.Fatalf("out-of-project rm returned a transport error (want refusal as result): %v", err)
	}
	if !strings.Contains(out, "ORCHICON GUARD") || !strings.Contains(out, "/etc/passwd") {
		t.Fatalf("the out-of-project refusal must name the path: %s", out)
	}
	if strings.Contains(out, "fail-closed") {
		t.Fatalf("the scope check must be what refused, not the fail-closed policy read: %s", out)
	}

	// Unreadable policy: the shim REFUSES even an in-project target rather than
	// running it unguarded (a guard that silently stops guarding is the failure
	// this component exists to prevent).
	t.Setenv("ORCHICON_PERMISSION_POLICY", filepath.Join(t.TempDir(), "absent.yaml"))
	survivor := filepath.Join(project, "survivor.txt")
	if err := os.WriteFile(survivor, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = p.ExecuteAskTool(context.Background(), "bash", `{"command":"rm -f `+survivor+`"}`)
	if err != nil {
		t.Fatalf("fail-closed probe returned a transport error: %v", err)
	}
	if !strings.Contains(out, "fail-closed") {
		t.Fatalf("an unreadable policy must refuse via the fail-closed message: %s", out)
	}
	if _, err := os.Stat(survivor); err != nil {
		t.Fatalf("the command ran despite the unreadable policy: %v", err)
	}
}
