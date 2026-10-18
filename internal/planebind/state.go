package planebind

// state.go — THE ONE place the four connection states are decided.
//
// The whole point of separating them is that they have DIFFERENT REMEDIES:
//
//	connecting  an action or the first status round-trip is in flight
//	connected   /healthz answers 200 on the configured instance
//	offline     nothing answers at the configured URL   → start the plane
//	plane-error something answers and is not well       → a different problem:
//	            unhealthy, or running a superseded binary
//
// `plane-error` MUST NOT collapse into `offline`. A plane that answers is not
// necessarily the plane we want — planeDrift exists for exactly that — and telling
// an operator "nothing is running" about a process that is plainly listening sends
// them to the wrong fix.
//
// `connecting` is NOT produced here. It is a CLIENT state (an action is in flight,
// or the first Status has not returned), so the Go side can never claim progress it
// has not observed; its zero value is the pessimistic one.

import (
	"fmt"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// The four states, named once so the Go and TS sides cannot disagree.
const (
	StateConnecting = "connecting"
	StateConnected  = "connected"
	StateOffline    = "offline"
	StatePlaneError = "plane-error"
)

// Classify maps a status + a health probe onto a state and the detail that names
// WHAT WAS FOUND (the URL, the status code, the drift verdict) — never a generic
// "error".
func Classify(st planecontrol.Status, h planecontrol.Health) (state, detail string) {
	url := st.HealthURL
	if url == "" {
		url = fmt.Sprintf("instance %s (no control port recorded)", st.Instance)
	}

	// 1. Nothing answered. This is the state the page exists for.
	if !h.Reachable {
		return StateOffline, fmt.Sprintf("nothing answers %s", url)
	}

	// 2. Something answered and is not healthy. A DIFFERENT problem from 1.
	if !h.Healthy() {
		return StatePlaneError, fmt.Sprintf("answers HTTP %d at %s (expected 200)", h.StatusCode, url)
	}

	// 3. It answers 200 — but WHICH binary? An install replaces the binary under a
	// running process, so a healthy plane can be serving superseded code. That is a
	// plane-error, not a connection success.
	if st.Drift == planecontrol.Drifted {
		who := st.Binary
		if who == "" {
			who = "an unidentified binary"
		}
		return StatePlaneError, fmt.Sprintf("answers 200 at %s but is running a superseded binary (%s)", url, who)
	}

	// 4. Serving, healthy, and (as far as can be established) current.
	return StateConnected, identity(st, url)
}

// identity is the connected state's detail: server identity, not merely "up".
func identity(st planecontrol.Status, url string) string {
	s := fmt.Sprintf("serving %s", url)
	if st.PID > 0 {
		s += fmt.Sprintf(" as pid %d", st.PID)
	}
	if st.Binary != "" {
		s += fmt.Sprintf(" (%s)", st.Binary)
	}
	if st.Residency != "" {
		s += fmt.Sprintf(", %s-resident", st.Residency)
	}
	if st.Drift == planecontrol.Unknown {
		// Honest about what could not be established rather than implying currency.
		s += " — serving process identity unknown"
	}
	return s
}
