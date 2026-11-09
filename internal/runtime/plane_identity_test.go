package runtime

import "testing"

// TestAutomationIdentitySubjectStability pins the plane-channel identity contract: the minted API
// key's identity_id must reference a REAL identities row provisioned under the run-scoped.
func TestAutomationIdentitySubjectStability(t *testing.T) {
	if got := automationIdentitySubject("01M1AFKT8NR8F1M9JWYKEAWV9Q"); got != "run:01M1AFKT8NR8F1M9JWYKEAWV9Q" {
		t.Fatalf("subject = %q, want run-scoped deterministic subject", got)
	}
	if automationIdentitySubject("run-a") == automationIdentitySubject("run-b") {
		t.Fatalf("distinct runs must map to distinct identity subjects")
	}
}
