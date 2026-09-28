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


// THE FALSE POSITIVE THE FIRST VERSION HAD, and it is the awk-regex class of bug again: this layer
// judges what a command TEXT MENTIONS, so a command that merely NAMED a protected path was refused as
// though it were deleting it. Setting `HOME=/home/me` for a child process was read as a target that
// contains the home directory, and the whole command was rejected.
//
// The shim would never look at that command — it intercepts rm/mv/cp/ln/chmod/chown and nothing else —
// so the consent layer was refusing something the enforcement layer would have allowed, and stating
// something false about what the rule prohibits.
func TestMentioningAProtectedPathIsNotDestroyingIt(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	for _, cmd := range []string{
		`cd /p/proj && HOME=/p/other some-tool --flag`,   // an assignment mentioning an ancestor
		`cd /p/proj && cat /p/proj/../README.md`,          // a read that traverses upward
		`cd /p/proj && echo "see /p for details"`,         // a mention inside a string
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
	// applies. The last one matters because a bare `mv` is easy to forget: moving something UP into
	// the project's parent takes the same directory with it as deleting it would.
	//
	// NOT INCLUDED, deliberately: `mv /p/proj /p/elsewhere` moves the scope root ITSELF, which is
	// equality rather than containment — that is left to the ordinary consent chain (it raises a card,
	// because the destination is outside the scope), which is the split the rule is built on.
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
