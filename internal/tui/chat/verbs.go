package chat

import (
	"os"
	"strings"
)

// verbs.go — THE ACTIVITY VERB ROTATES, INDEXED ON THE SERVER'S CLOCK.

// AskVerbs is the rotation.
var AskVerbs = []string{
	"thinking", "pondering", "contemplating", "considering", "deliberating",
	"musing", "ruminating", "cogitating", "reasoning", "reflecting",
	"meditating", "mulling", "brooding", "speculating", "hypothesizing",
	"theorizing", "analyzing", "scrutinizing", "examining", "inspecting",
	"investigating", "inquiring", "inquisiting", "questioning", "probing",
	"exploring", "researching", "studying", "surveying", "scouting",
	"mapping", "charting", "plotting", "planning", "scheming",
	"strategizing", "devising", "designing", "drafting", "sketching",
	"outlining", "organizing", "arranging", "sorting", "sifting",
	"parsing", "dissecting", "unraveling", "untangling", "synthesizing",
	"assembling", "composing", "crafting", "constructing", "shaping",
	"refining", "polishing", "honing", "pruning", "distilling",
	"conjuring", "calculating", "computing", "enumerating", "estimating",
	"weighing", "balancing", "verifying", "validating", "rehearsing",
	"preparing", "calibrating",
}

// VerbCellCap is the widest entry the one-row footer will accept.
const VerbCellCap = 14

// VerbPeriodMS is how long one word stays up: 15s, the server's heartbeat cadence
// (askHeartbeatInterval, internal/askorchicon/chat.go), so the word advances about once per
// heartbeat.
const VerbPeriodMS int64 = 15000

// VerbAt is the pure selector: the same timestamp always yields the same word, on any client, in any
// process. No clock, no RNG, no global mutable state — integer division and modulo only, so Go and JS
// cannot disagree on the result.
//
// t <= 0 IS THE "NO HEARTBEAT YET" SENTINEL (a real server stamp is Unix milliseconds and always
// positive), and it returns AskVerbs[0]. That is the fallback for the moment right after the operator
// sends — before-content, first second — which is exactly when they are most likely to be looking: the
// line must never be empty and must never panic.
func VerbAt(serverTimeUnixMs int64) string {
	if serverTimeUnixMs <= 0 {
		return AskVerbs[0]
	}
	return AskVerbs[(serverTimeUnixMs/VerbPeriodMS)%int64(len(AskVerbs))]
}

// VerbRotationOn reports whether the rotation may advance.
func VerbRotationOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ORCHICON_TUI_NO_ROTATE"))) {
	case "1", "true":
		return false
	default:
		return true
	}
}

// ActivityVerb is what the activity line asks for: the rotating word for this server time, or the
// still first entry when the operator has turned the rotation off.
func ActivityVerb(serverTimeUnixMs int64) string {
	if !VerbRotationOn() {
		return AskVerbs[0]
	}
	return VerbAt(serverTimeUnixMs)
}
