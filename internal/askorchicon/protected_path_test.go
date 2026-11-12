package askorchicon

// protected_path_test.go — the CONSENT half of the protected-path rule.

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

// THE FALSE POSITIVE THAT MUST NOT EXIST.
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

// THE FALSE POSITIVE THE FIRST VERSION HAD, and it is the awk-regex class of bug again: this layer
// judges what a command TEXT MENTIONS.
func TestMentioningAProtectedPathIsNotDestroyingIt(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	for _, cmd := range []string{
		`cd /p/proj && HOME=/p/other some-tool --flag`, // an assignment mentioning an ancestor
		`cd /p/proj && cat /p/proj/../README.md`,       // a read that traverses upward
		`cd /p/proj && echo "see /p for details"`,      // a mention inside a string
	} {
		resp, ask, refusal := ct.decide(context.Background(), "ses_1", bashAskEvent("per_1", cmd))
		if refusal != "" {
			t.Errorf("%q was REFUSED (%s) — it does not invoke a command the shim judges at all, so "+
				"this is over-refusal and a false statement about the rule", cmd, refusal)
		}
		// It may still ASK (that is the ordinary path for an outside-scope command); what it must not
		// do is claim the protected-path rule refused it.
		_ = resp
		_ = ask
	}
}

// And the rule still fires where the shim WOULD judge the target — the gate narrows the rule to the
// operations it is about, rather than weakening it.
func TestTheRuleStillFiresForAnInvocationTheShimJudges(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	// Each of these INVOKES a judged binary with a target that CONTAINS the project, so the rule
	// applies.
	for _, cmd := range []string{
		`cd /p/proj && rm -rf /p`,
		`cd /p/proj && /bin/rm -rf /p`,
		`cd /p/proj && mv /p/proj/build /p`,
		`cd /p/proj && cp /p/proj/x /p/`,
	} {
		resp, _, refusal := ct.decide(context.Background(), "ses_1", bashAskEvent("per_2", cmd))
		if resp != "reject" || refusal == "" {
			t.Errorf("%q must be refused with a reason: resp=%q refusal=%q", cmd, resp, refusal)
		}
	}
}
