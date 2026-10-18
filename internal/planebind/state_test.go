package planebind

// state_test.go — AC6 at its source: the FOUR states are distinguishable, and
// plane-error is NEVER offline. A reachable-but-unhealthy plane and one running a
// superseded binary are each their own finding, because each needs its own fix.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

func TestClassifyFourStates(t *testing.T) {
	const url = "http://localhost:8080/healthz"

	base := planecontrol.Status{Instance: "dev", HealthURL: url, ControlPort: 8080}

	tests := []struct {
		name       string
		status     planecontrol.Status
		health     planecontrol.Health
		wantState  string
		wantDetail []string
	}{
		{
			name:       "nothing answers is offline and names the URL it tried",
			status:     base,
			health:     planecontrol.Health{Reachable: false},
			wantState:  StateOffline,
			wantDetail: []string{"nothing answers", url},
		},
		{
			name:       "answers 503 is plane-error, naming the status",
			status:     base,
			health:     planecontrol.Health{Reachable: true, StatusCode: 503},
			wantState:  StatePlaneError,
			wantDetail: []string{"HTTP 503", url},
		},
		{
			name:       "healthy but superseded is plane-error, naming the drift",
			status:     planecontrol.Status{Instance: "dev", HealthURL: url, ControlPort: 8080, Drift: planecontrol.Drifted, Binary: "/usr/local/bin/orchicon (deleted)"},
			health:     planecontrol.Health{Reachable: true, StatusCode: 200},
			wantState:  StatePlaneError,
			wantDetail: []string{"superseded", "deleted"},
		},
		{
			name:       "healthy and current is connected, naming the server",
			status:     planecontrol.Status{Instance: "dev", HealthURL: url, ControlPort: 8080, PID: 4242, Binary: "/usr/local/bin/orchicon", Residency: planecontrol.ResidencyHost, Drift: planecontrol.Current},
			health:     planecontrol.Health{Reachable: true, StatusCode: 200},
			wantState:  StateConnected,
			wantDetail: []string{url, "4242", "/usr/local/bin/orchicon"},
		},
		{
			name:       "healthy with unknown identity is still connected but says so",
			status:     planecontrol.Status{Instance: "dev", HealthURL: url, ControlPort: 8080, Drift: planecontrol.Unknown},
			health:     planecontrol.Health{Reachable: true, StatusCode: 200},
			wantState:  StateConnected,
			wantDetail: []string{"identity unknown"},
		},
		{
			name:       "no control port recorded is offline, naming the instance",
			status:     planecontrol.Status{Instance: "prod"},
			health:     planecontrol.Health{},
			wantState:  StateOffline,
			wantDetail: []string{"prod", "no control port"},
		},
	}

	seen := map[string]bool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, detail := Classify(tc.status, tc.health)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q (detail %q)", state, tc.wantState, detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(detail, want) {
					t.Errorf("detail %q does not name %q — the screen must say what it FOUND", detail, want)
				}
			}
			seen[state] = true
		})
	}

	// All three observable states must appear; `connecting` is client-only.
	for _, want := range []string{StateOffline, StatePlaneError, StateConnected} {
		if !seen[want] {
			t.Errorf("no case produced %q — the state set is incomplete", want)
		}
	}
}

// TestClassifyNeverBlursPlaneErrorIntoOffline is the one equivalence the brief
// forbids, asserted on its own so a future edit cannot merge the two by accident.
func TestClassifyNeverBlursPlaneErrorIntoOffline(t *testing.T) {
	url := "http://localhost:8091/healthz"
	st := planecontrol.Status{Instance: "prod", HealthURL: url, ControlPort: 8091}

	offline, _ := Classify(st, planecontrol.Health{Reachable: false})
	sick, _ := Classify(st, planecontrol.Health{Reachable: true, StatusCode: 500})
	drifted, _ := Classify(planecontrol.Status{Instance: "prod", HealthURL: url, ControlPort: 8091, Drift: planecontrol.Drifted},
		planecontrol.Health{Reachable: true, StatusCode: 200})

	if offline != StateOffline {
		t.Fatalf("nothing-answers classified as %q", offline)
	}
	for _, got := range []string{sick, drifted} {
		if got != StatePlaneError {
			t.Fatalf("a reachable-but-unwell plane classified as %q, want %q — same posture as the installer: "+
				"a plane that answers is not necessarily the plane we want", got, StatePlaneError)
		}
		if got == offline {
			t.Fatal("plane-error collapsed into offline")
		}
	}
}

func TestConnectingIsNeverProducedByTheGoSide(t *testing.T) {
	// The classifier only ever reports what it OBSERVED. `connecting` is a client state
	// (an action in flight), so the binding can never claim progress it has not seen.
	st := planecontrol.Status{Instance: "dev", HealthURL: "http://localhost:8080/healthz", ControlPort: 8080}
	for _, h := range []planecontrol.Health{{}, {Reachable: true, StatusCode: 503}, {Reachable: true, StatusCode: 200}} {
		if got, _ := Classify(st, h); got == StateConnecting {
			t.Fatalf("Classify produced %q — that state belongs to the client", StateConnecting)
		}
	}
}
