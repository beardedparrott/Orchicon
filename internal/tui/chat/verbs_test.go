package chat

// verbs_test.go — THE ROTATION IS A PURE FUNCTION OF SERVER TIME, PINNED ACROSS CLIENTS.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// askVerbsFixturePath is the ONE file both languages enumerate. From internal/tui/chat, three levels up is
// the repo root.
var askVerbsFixturePath = filepath.Join("..", "..", "..", "frontend", "src", "lib", "ask-verbs.json")

// TestVerbAtIsPureAndDeterministic — the same timestamp always yields the same word, on any client, in any
// process (AC2). Called twice, and from a fresh scope, to make the point that there is no hidden state.
func TestVerbAtIsPureAndDeterministic(t *testing.T) {
	stamps := []int64{1, 3999, 4000, 4001, 1_700_000_000_000, 1_700_000_123_456}
	for _, ts := range stamps {
		first := VerbAt(ts)
		if second := VerbAt(ts); second != first {
			t.Errorf("VerbAt(%d) returned %q then %q — the selector is not pure", ts, first, second)
		}
		if !isVerbatimMember(first) {
			t.Errorf("VerbAt(%d) = %q, which is not in AskVerbs", ts, first)
		}
	}
}

// TestVerbAtFallbackIsNeverEmpty — the moment before the first heartbeat (AC5).
func TestVerbAtFallbackIsNeverEmpty(t *testing.T) {
	for _, ts := range []int64{0, -1, -1_700_000_000_000, 1} {
		got := VerbAt(ts)
		if got == "" {
			t.Fatalf("VerbAt(%d) is empty — the line would vanish at the moment it matters most", ts)
		}
		if !isVerbatimMember(got) {
			t.Errorf("VerbAt(%d) = %q, which is not in AskVerbs", ts, got)
		}
	}
	// And the pre-heartbeat sentinel is specifically the LIST's first entry, so the fallback word is stable
	// and known rather than an arbitrary draw.
	if got := VerbAt(0); got != AskVerbs[0] {
		t.Errorf("VerbAt(0) = %q, want the list's first entry %q", got, AskVerbs[0])
	}
}

// TestVerbAtRotatesOnThePeriod — the word advances once per VerbPeriodMS and wraps at the list's
// end (AC1/AC2 boundary).
func TestVerbAtRotatesOnThePeriod(t *testing.T) {
	for k := int64(0); k < int64(len(AskVerbs))*2; k++ {
		want := AskVerbs[(k)%int64(len(AskVerbs))]
		if got := VerbAt(k * VerbPeriodMS); got != want {
			t.Errorf("VerbAt(%d * VerbPeriodMS) = %q, want %q", k, got, want)
		}
		// Within one period the word does not change.
		if got := VerbAt(k*VerbPeriodMS + VerbPeriodMS - 1); got != want {
			t.Errorf("VerbAt(%d * VerbPeriodMS + %d) = %q, want %q — the word flickered mid-period",
				k, VerbPeriodMS-1, got, want)
		}
	}
	// invariant (AC2): the operator's "changing WAY too often" is the boundary. 1s apart must NOT move the
	// word (a ~1s repaint would read as churn), a full 15s period apart must.
	if VerbAt(0) != VerbAt(1_000) {
		t.Errorf("VerbAt(0) = %q but VerbAt(1000) = %q — the word changed within a second",
			VerbAt(0), VerbAt(1_000))
	}
	if VerbAt(0) == VerbAt(15_000) {
		t.Errorf("VerbAt(0) == VerbAt(15000) = %q — 15s did not advance the word", VerbAt(0))
	}
	if VerbPeriodMS != 15_000 {
		t.Errorf("VerbPeriodMS = %d, want 15000 — the operator's heartbeat-aligned decision", VerbPeriodMS)
	}
}

// TestAskVerbsMatchTheSharedFixture — THE CROSS-CLIENT PARITY (AC3).
func TestAskVerbsMatchTheSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(askVerbsFixturePath)
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fixture struct {
		PeriodMs int64    `json:"periodMs"`
		Verbs    []string `json:"verbs"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse the shared fixture: %v", err)
	}
	if fixture.PeriodMs != VerbPeriodMS {
		t.Errorf("fixture periodMs = %d, but VerbPeriodMS = %d — the two clients would rotate at different "+
			"speeds for the same server time", fixture.PeriodMs, VerbPeriodMS)
	}
	if len(fixture.Verbs) != len(AskVerbs) {
		t.Fatalf("fixture has %d verbs, AskVerbs has %d — the lists have drifted",
			len(fixture.Verbs), len(AskVerbs))
	}
	for i := range fixture.Verbs {
		if fixture.Verbs[i] != AskVerbs[i] {
			t.Errorf("verb %d: fixture %q != Go %q — the two clients would draw different words for the "+
				"same server time", i, fixture.Verbs[i], AskVerbs[i])
		}
	}
}

// TestAskVerbsAreAsciiShortAndUnique — the SHAPE of the list (AC1/AC8).
func TestAskVerbsAreAsciiShortAndUnique(t *testing.T) {
	if len(AskVerbs) < 60 {
		t.Errorf("AskVerbs has %d entries — the operator asked for \"a ton of them\" (target 60+)", len(AskVerbs))
	}
	word := regexp.MustCompile(`^[a-z]+$`)
	seen := map[string]bool{}
	for i, v := range AskVerbs {
		if !word.MatchString(v) {
			t.Errorf("entry %d (%q) is not a single plain ASCII a-z word — the TUI's `ascii` theme must be able "+
				"to draw it", i, v)
		}
		if len(v) > VerbCellCap {
			t.Errorf("entry %d (%q) is %d cells, over the %d-cell cap — the one-row footer would clip it",
				i, v, len(v), VerbCellCap)
		}
		if seen[v] {
			t.Errorf("entry %d (%q) duplicates an earlier entry", i, v)
		}
		seen[v] = true
	}
}

// TestAskVerbsNeverClaimLiveness — NO WORD MAY BE A CLAIM THE STREAM CANNOT ALWAYS BACK (AC6).
func TestAskVerbsNeverClaimLiveness(t *testing.T) {
	banned := []string{"receiving", "progressing", "streaming", "completing", "advancing", "succeeding", "finishing"}
	for _, v := range AskVerbs {
		for _, b := range banned {
			if strings.Contains(v, b) {
				t.Errorf("entry %q contains %q — a liveness claim a stalled stream would make falsely", v, b)
			}
		}
	}
}

// TestRotationCanBeDisabled — the accessibility off-switch (AC7). With ORCHICON_TUI_NO_ROTATE set, the chosen
// word is the list's first entry regardless of the server stamp, so the line is present and true but still.
func TestRotationCanBeDisabled(t *testing.T) {
	t.Setenv("ORCHICON_TUI_NO_ROTATE", "1")
	if VerbRotationOn() {
		t.Fatal("ORCHICON_TUI_NO_ROTATE=1 did not switch the rotation off")
	}
	for _, ts := range []int64{0, 4000, 1_700_000_000_000} {
		if got := ActivityVerb(ts); got != AskVerbs[0] {
			t.Errorf("ActivityVerb(%d) = %q with rotation off, want the still first entry %q", ts, got, AskVerbs[0])
		}
	}

	// And the default is ON when the knob is absent, or the feature would ship disabled.
	t.Setenv("ORCHICON_TUI_NO_ROTATE", "")
	if !VerbRotationOn() {
		t.Error("rotation is off with no ORCHICON_TUI_NO_ROTATE set — the feature would never rotate")
	}
}

// isVerbatimMember reports whether s is one of the rotation's entries, so a test never has to restate the list
// to assert membership.
func isVerbatimMember(s string) bool {
	for _, v := range AskVerbs {
		if v == s {
			return true
		}
	}
	return false
}
