package askorchicon

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// TestConsentClassificationPartitionsTheHostSuite is the guard that makes the
// native consent gate FAIL-CLOSED across a package boundary.
//
// The gate used to be a denylist — write/edit/batch_write/bash — so a mutating
// host-suite tool added later would have run with NO consent at all, silently.
// It is now derived from an explicit classification in the adapter (which cannot
// import this package: the arrow points the other way), and this test is what
// closes the loop: every host-suite tool must be classified as read-only or
// mutating, deliberately.
//
// A new tool in hostSuiteToolNames therefore BREAKS this test until someone
// decides which it is — which is the whole point. Silently ungated is the failure
// this exists to prevent.
func TestConsentClassificationPartitionsTheHostSuite(t *testing.T) {
	readOnly := map[string]bool{}
	for _, n := range orchicon.ConsentReadOnlyTools {
		if readOnly[n] {
			t.Errorf("%q appears twice in ConsentReadOnlyTools", n)
		}
		readOnly[n] = true
	}
	mutating := map[string]bool{}
	for _, n := range orchicon.ConsentMutatingTools {
		if mutating[n] {
			t.Errorf("%q appears twice in ConsentMutatingTools", n)
		}
		mutating[n] = true
	}

	// Disjoint: a tool cannot both ask and not ask.
	for n := range readOnly {
		if mutating[n] {
			t.Errorf("%q is classified BOTH read-only and mutating", n)
		}
	}

	// Total over the host suite: every tool is classified one way or the other.
	for _, n := range hostSuiteToolNames {
		if !readOnly[n] && !mutating[n] {
			t.Errorf("host-suite tool %q is UNCLASSIFIED — the native consent gate would treat it as a product tool and let it run with no approval. Add it to orchicon.ConsentReadOnlyTools or orchicon.ConsentMutatingTools.", n)
		}
	}

	// And nothing beyond it: a name here that is not a host-suite tool would mean
	// the lists have drifted from the suite they claim to describe.
	for _, n := range orchicon.ConsentReadOnlyTools {
		if !contains(hostSuiteToolNames, n) {
			t.Errorf("ConsentReadOnlyTools names %q, which is not in hostSuiteToolNames", n)
		}
	}
	for _, n := range orchicon.ConsentMutatingTools {
		if !contains(hostSuiteToolNames, n) {
			t.Errorf("ConsentMutatingTools names %q, which is not in hostSuiteToolNames", n)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
