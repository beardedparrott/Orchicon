// This file is an EXTERNAL test package (toolclass_test) on purpose: it may import askmode and
// orchicon to CHECK the classifier against them, while neither ever becomes a production
// dependency of internal/toolclass. That is how AC9's reconciliation is achieved — by test, not by
// a fourth copy of the vocabulary and not by a package that drags the world into every client.
package toolclass_test

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/toolclass"
)

// TestClassifierAgreesWithExistingMutatingDefinitions is AC9: the four mutating names are already
// defined TWICE in this codebase — askmode's theWorkTools() (askmode.go:90-94, unexported; its
// exported proxy is PolicyFor(Brainstorm).Denied, set from theWorkTools() at askmode.go:120) and
// orchicon.ConsentMutatingTools (chatturn.go:1600-1602). This classifier must be their
// RECONCILIATION, never a third opinion, so every name in either set is asserted to be counted as
// WORK — Modify OR Bash — and never as Read and never as Ignore.
//
// `bash` is why the predicate is "is a mutating class" rather than "== Modify": the consent
// policy groups bash with the writers (it can write), while this display line counts commands
// separately (askmode.go:86-89 explains the same judgement).
func TestClassifierAgreesWithExistingMutatingDefinitions(t *testing.T) {
	policy, ok := askmode.PolicyFor(askmode.Brainstorm)
	if !ok {
		t.Fatal("askmode.PolicyFor(Brainstorm) is not a known mode — the reconcile test has gone stale")
	}
	work := policy.Denied
	if len(work) == 0 {
		t.Fatal("askmode's Brainstorm denial set is empty — this test would pass vacuously")
	}

	for name := range work {
		switch c := toolclass.Classify(name); c {
		case toolclass.Modify, toolclass.Bash:
		default:
			t.Errorf("Classify(%q) = %v, want Modify or Bash (askmode.theWorkTools counts it as work)", name, c)
		}
	}
	for _, name := range orchicon.ConsentMutatingTools {
		switch c := toolclass.Classify(name); c {
		case toolclass.Modify, toolclass.Bash:
		default:
			t.Errorf("Classify(%q) = %v, want Modify or Bash (orchicon.ConsentMutatingTools counts it as work)", name, c)
		}
	}

	// And the two EXISTING definitions must agree with each other on those names, or the three-way
	// reconciliation is meaningless.
	for name := range work {
		if !contains(orchicon.ConsentMutatingTools, name) {
			t.Errorf("%q is work in askmode.theWorkTools but not in orchicon.ConsentMutatingTools", name)
		}
	}
	for _, name := range orchicon.ConsentMutatingTools {
		if !work[name] {
			t.Errorf("%q is mutating in orchicon.ConsentMutatingTools but not in askmode.theWorkTools", name)
		}
	}
}

// TestClassifierReadBucketAgreesWithConsentReadOnly is the rest of AC9's "reconciled, not
// duplicated": the read names are already defined by orchicon.ConsentReadOnlyTools
// (chatturn.go:1589-1591). Every name in it is this line's Read — EXCEPT todowrite, which is a
// DELIBERATE divergence and not a bug (plan D7): `todowrite` writes session state, is not a read of
// the repository (which is what "reads" means on this line) and is not work a user asked for, so
// it is Ignore. Naming the exception here keeps the divergence a decision rather than a mystery.
func TestClassifierReadBucketAgreesWithConsentReadOnly(t *testing.T) {
	if len(orchicon.ConsentReadOnlyTools) == 0 {
		t.Fatal("orchicon.ConsentReadOnlyTools is empty — this test would pass vacuously")
	}
	for _, name := range orchicon.ConsentReadOnlyTools {
		switch name {
		case "todowrite":
			if got := toolclass.Classify(name); got != toolclass.Ignore {
				t.Errorf("Classify(%q) = %v, want Ignore: the D7 divergence decided todowrite counts for nothing", name, got)
			}
			continue
		default:
			if got := toolclass.Classify(name); got != toolclass.Read {
				t.Errorf("Classify(%q) = %v, want Read (orchicon.ConsentReadOnlyTools)", name, got)
			}
		}
	}
}

// TestClassifierCoarserThanToolIntentVerbAndThatIsDeliberate documents the OTHER existing
// definition, internal/askorchicon/consent.go's toolIntentVerb — which is a VERB for one call
// ("modify", "delete", "run a shell command"), not a class for a counter. They are deliberately
// NOT unified, and this test makes their disagreement impossible to overlook: the names that carry
// a "modify" verb in the consent path must be counted as work here, and bash's shell verb must
// land in the Bash bucket. It cannot call toolIntentVerb (unexported, and importing askorchicon to
// reach it would be a heavy price for one assertion), so it pins the AGREEMENT as the shared
// literal the two definitions share: the mutating shell and write names.
func TestClassifierCoarserThanToolIntentVerbAndThatIsDeliberate(t *testing.T) {
	// The names toolIntentVerb maps onto "modify" because they contain write/edit/create...
	// intersected with the host suite's real vocabulary (native_tools.go:54-58).
	for _, name := range []string{"write", "edit", "batch_write", "orchicon_write"} {
		if got := toolclass.Classify(name); got != toolclass.Modify {
			t.Errorf("Classify(%q) = %v, want Modify (toolIntentVerb's \"modify\" bucket)", name, got)
		}
	}
	// The names toolIntentVerb maps onto "run a shell command" (isBashAsk) — one bucket here.
	for _, name := range []string{"bash", "shell"} {
		if got := toolclass.Classify(name); got != toolclass.Bash {
			t.Errorf("Classify(%q) = %v, want Bash (toolIntentVerb's shell bucket)", name, got)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
