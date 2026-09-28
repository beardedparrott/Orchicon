package askorchicon

// protected_path_test.go — the CONSENT half of the protected-path rule.
//
// The guard shim refuses a target that would destroy the scope, but the shim only runs AFTER a
// decision has been made — and the decision layer is where the operator is OFFERED a card. A card is
// a button, and this is the one case where the button must not be offered at all: `rm -rf /home`
// presented as "allow once / never ask again / deny" invites an approval nobody can take back.
//
// So the same rule runs here, from the same declaration (internal/protectedpath), above FULLSEND and
// above the policy loop — the position of the never-allow class, because it is the same kind of thing.
//
// The operator, after a guard test deleted their home: "The fact that a permission accept and FULLSEND
// can do a damaging rm -rf on /home on other directories may be a bit concerning."

import (
	"context"
	"testing"
)

func TestDestroyingTheScopeIsRefusedNotAsked(t *testing.T) {
	for _, fullsend := range []bool{false, true} {
		isolatedPolicy(t, "")
		svc := testConsentService()
		// FULLSEND ON is the case that matters: it is the mode whose whole purpose is to skip the
		// sanctioned set, so the rule has to sit above it or it does not exist.
		if fullsend {
			svc.fullsend.Set("conv-1", true)
		}
		ct := newTestConsentTurn(svc, "/p/proj", true, nil)

		for _, cmd := range []string{
			`cd /p/proj && rm -rf /p`,         // the project's PARENT — takes the project with it
			`cd /p/proj && rm -rf /p/proj/..`, // the same directory by traversal
		} {
			resp, ask, refusal := ct.decide(context.Background(), "ses_1", bashAskEvent("per_1", cmd))
			if fullsend {
				t.Logf("fullsend=%v  %-34s resp=%q ask=%v", fullsend, cmd, resp, ask != nil)
			}
			if ask != nil {
				t.Errorf("fullsend=%v: %q raised a CARD — a target that destroys the scope must be "+
					"refused, never offered as a button", fullsend, cmd)
			}
			if resp != "reject" {
				t.Errorf("fullsend=%v: %q resp=%q, want reject", fullsend, cmd, resp)
			}
			if refusal == "" {
				t.Errorf("fullsend=%v: %q must say WHY, or the model cannot choose another path",
					fullsend, cmd)
			}
		}
	}
}

// THE FALSE POSITIVE THAT MUST NOT EXIST. Acting ON or INSIDE the project is ordinary work, and a rule
// that refused it would make the feature unusable — this is what the two-list split in protectedpath
// is for.
func TestWorkingInsideTheProjectIsStillAllowed(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	svc.fullsend.Set("conv-1", true) // fullsend: nothing else would refuse these anyway
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	for _, cmd := range []string{
		`cd /p/proj && rm -rf /p/proj/dist`, // INSIDE the project
		`cd /p/proj && rm -rf dist`,         // relative, resolves inside
	} {
		resp, ask, refusal := ct.decide(context.Background(), "ses_1", bashAskEvent("per_2", cmd))
		if resp != "once" || ask != nil || refusal != "" {
			t.Errorf("%q must proceed under fullsend: resp=%q ask=%v refusal=%q", cmd, resp, ask != nil, refusal)
		}
	}
}
