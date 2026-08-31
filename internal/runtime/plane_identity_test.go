package runtime

import "testing"

// TestAutomationIdentitySubjectStability pins the plane-channel identity
// contract: the minted API key's identity_id must reference a REAL
// identities row provisioned under the run-scoped automation subject. A
// worker ID is NOT an identity — stamping it made every plane-channel WRITE
// fail with SQLSTATE 23503 (audit_events_actor_identity_fk) because
// recordAudit stamps auth.ActorFromContext(ctx) into audit_events, whose
// actor_identity_id is FK'd to identities (reads worked; writes did not).
//
// The subject is run-scoped and deterministic so concurrent steps of one run
// share a single identity row (unique (tenant_id, subject)) and the audit
// trail attributes writes to the run that made them.
func TestAutomationIdentitySubjectStability(t *testing.T) {
	if got := automationIdentitySubject("01M1AFKT8NR8F1M9JWYKEAWV9Q"); got != "run:01M1AFKT8NR8F1M9JWYKEAWV9Q" {
		t.Fatalf("subject = %q, want run-scoped deterministic subject", got)
	}
	if automationIdentitySubject("run-a") == automationIdentitySubject("run-b") {
		t.Fatalf("distinct runs must map to distinct identity subjects")
	}
}