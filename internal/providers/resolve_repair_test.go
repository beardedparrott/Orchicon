package providers

import (
	"strings"
	"testing"
)

// THE PATHOLOGY THIS PINS (observed in production, from the plane's log):
//
//	sourcing: probe https://api.anthropic.com/v1/models → HTTP 401
//	sourcing: probe https://api.anthropic.com:8095/v1/v1/models ... i/o timeout
//	sourcing: probe https://api.anthropic.com:11434/v1/models ... deadline
//
// 08:49:01 → 08:49:46. Forty-five seconds, which is EXACTLY the model picker's
// read budget, so the picker timed out and the offline catalog seed below the
// sweep never ran — a catalog-covered provider listed nothing while its authored
// models sat one line away in the same function.
//
// The cause is the port fallback: the common local-inference ports are for an
// endpoint on THIS network, and applying them to a PUBLIC host manufactures
// addresses that cannot exist. Repairing anthropic's base URL produced
// api.anthropic.com:11434.
func TestRepairCandidatesDoesNotInventPortsForPublicHosts(t *testing.T) {
	got := repairCandidates("https://api.anthropic.com/v1")
	for _, c := range got {
		for _, port := range []string{"8080", "8095", "8000", "11434"} {
			if strings.Contains(c, ":"+port) {
				t.Errorf("repairCandidates(%q) produced %q — a local-inference port on a public host. "+
					"Each of these dials blocks for a timeout and the sweep is sequential; this is the 45s stall.",
					"https://api.anthropic.com/v1", c)
			}
		}
	}
	// A public host with a version root has nothing to repair: the URL is correct
	// by construction and a 401 is a TOKEN problem, not an endpoint one.
	if len(got) != 0 {
		t.Errorf("repairCandidates(public host with %q path) = %v, want none", "/v1", got)
	}
}

// The self-heal still works for what it was written for: a LOCAL endpoint that
// moved or lost its port. The port fallback is the local case's whole value.
func TestRepairCandidatesKeepsLocalPortFallback(t *testing.T) {
	cases := []string{
		"http://localhost",    // loopback, no port
		"http://127.0.0.1",    // loopback
		"http://192.168.1.50", // private range
		"http://ollama",       // bare docker service name
	}
	for _, base := range cases {
		got := repairCandidates(base)
		if len(got) == 0 {
			t.Errorf("repairCandidates(%q) = none, want the local-inference port candidates", base)
			continue
		}
		found := false
		for _, c := range got {
			if strings.Contains(c, ":11434") || strings.Contains(c, ":8000") {
				found = true
			}
		}
		if !found {
			t.Errorf("repairCandidates(%q) = %v, want the common local-inference ports", base, got)
		}
	}
}

// mustTryLocalPorts is the gate itself, so it is pinned directly. A false
// NEGATIVE here would re-introduce the stall; a false POSITIVE would only cost a
// pointless probe for a genuinely local-looking name.
func TestMustTryLocalPorts(t *testing.T) {
	local := map[string]bool{
		"":                                  true, // portless parse: historical behaviour
		"localhost":                         true,
		"0.0.0.0":                           true,
		"::1":                               true,
		"host.docker.internal":              true,
		"gateway.docker.internal":           true,
		"ollama":                            true, // bare service name
		"vllm":                              true,
		"127.0.0.1":                         true,
		"192.168.1.50":                      true,
		"10.0.0.5":                          true,
		"172.17.0.1":                        true,
		"169.254.1.1":                       true, // link-local
		"api.anthropic.com":                 false,
		"api.openai.com":                    false,
		"generativelanguage.googleapis.com": false,
		"openrouter.ai":                     false,
	}
	for host, want := range local {
		if got := mustTryLocalPorts(host); got != want {
			t.Errorf("mustTryLocalPorts(%q) = %v, want %v", host, got, want)
		}
	}
}

// The budget must be small enough to leave room for the catalog seed inside the
// picker's read budget. The picker uses 45s; anything approaching that would
// re-create the bug this bound exists to prevent.
func TestRepairBudgetLeavesRoomForTheSeed(t *testing.T) {
	const pickerBudget = 45 * 1e9 // 45s, internal/tui/models.go's model-load timeout
	if int64(repairBudget) >= pickerBudget/3 {
		t.Fatalf("repairBudget = %s, too large a share of the picker's %s budget — the seed could still be starved",
			repairBudget, "45s")
	}
}
