package adapter

import "testing"

// TestWorkerAgentNameIsDerivedAndStable pins the contract the plane and the
// execution both depend on: the name is COMPUTED from the worker id, so the
// writer (which bakes the agent into the serve config) and the selector (which
// switches a session to it) agree without the name ever being transmitted.
func TestWorkerAgentNameIsDerivedAndStable(t *testing.T) {
	const id = "01M1ERH9921YS9S1QWGZV8D1VM"
	got := WorkerAgentName(id)
	if got != WorkerAgentName(id) {
		t.Error("WorkerAgentName must be deterministic")
	}
	if got != "orchicon-worker-01m1erh9921ys9s1qwgzv8d1vm" {
		t.Errorf("WorkerAgentName(%q) = %q", id, got)
	}
}

// TestWorkerAgentNameSurvivesALookupKey pins the sanitisation: an agent id is a
// lookup key, so a worker id carrying a separator (or any character outside the
// id grammar) must not produce a name that cannot be addressed.
func TestWorkerAgentNameSurvivesALookupKey(t *testing.T) {
	for _, in := range []string{"Weird/ID With Spaces", "a.b:c", "tab\there"} {
		got := WorkerAgentName(in)
		for i := 0; i < len(got); i++ {
			ch := got[i]
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				continue
			}
			t.Errorf("WorkerAgentName(%q) = %q contains an unaddressable byte %q", in, got, ch)
		}
	}
}

// TestWorkerAgentNameIsPrefixedAndEmptySafe pins the boundary behaviours the
// caller relies on: the name is always prefixed, so it cannot collide with
// opencode's own built-in agents, and an empty id still yields a usable string
// rather than a bare prefix a caller would have to special-case.
func TestWorkerAgentNameIsPrefixedAndEmptySafe(t *testing.T) {
	if got := WorkerAgentName(""); got == "" {
		t.Error("an empty worker id must still yield a non-empty name")
	}
	for _, in := range []string{"", "x", "01M1ERH9921YS9S1QWGZV8D1VM"} {
		if got := WorkerAgentName(in); len(got) < len("orchicon-worker-") {
			t.Errorf("WorkerAgentName(%q) = %q is missing the prefix", in, got)
		}
	}
}
