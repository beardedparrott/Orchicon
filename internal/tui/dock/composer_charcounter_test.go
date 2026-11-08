package dock

// composer_charcounter_test.go — THE COMPOSER WARNS AS THE MESSAGE BOUND IS APPROACHED.
//
// The bound was 10,000 and the composer said nothing about it, so the operator met it mid-paste with the text
// already in the box. The counter must show over the last fifth, not from the first character, and it must be
// counted by NoticeRows or the composer's height budget silently overflows the rows the shell reserved.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/chatlimits"
)

// newLimitedDock is a composer that knows the message bound, with a buffer of n runes.
func newLimitedDock(n int) Model {
	m := New()
	m.SetCharLimit(chatlimits.MaxMessageChars)
	m.SetValue(strings.Repeat("a", n))
	return m
}

// TestTheCounterIsSilentForOrdinaryInput is the requirement, not a nicety: a count under every sentence is
// noise, and noise is ignored by the time it matters.
func TestTheCounterIsSilentForOrdinaryInput(t *testing.T) {
	m := newLimitedDock(chatlimits.WarnAt - 1)
	if got := m.charCounter(); got != "" {
		t.Fatalf("the counter shows at %d characters: %q — it must appear only as the bound is approached",
			chatlimits.WarnAt-1, got)
	}
	if got := m.NoticeRows(); got != 0 {
		t.Fatalf("NoticeRows = %d with a silent counter, so the composer reserves a row it does not draw", got)
	}
}

// TestTheCounterAppearsAtTheThreshold pins where it starts, and that it names both numbers.
func TestTheCounterAppearsAtTheThreshold(t *testing.T) {
	m := newLimitedDock(chatlimits.WarnAt)
	got := m.charCounter()
	if got == "" {
		t.Fatalf("no counter at WarnAt (%d), so the threshold is off by one in the wrong direction", chatlimits.WarnAt)
	}
	if !strings.Contains(got, "80,000") || !strings.Contains(got, "100,000") {
		t.Fatalf("counter = %q, want the length and the bound with digit grouping", got)
	}
}

// TestTheCounterSharesTheHeightBudget: the row must be counted wherever the shell asks how tall the composer
// is, or the block grows past its reserved rows and eats the pane above it.
func TestTheCounterSharesTheHeightBudget(t *testing.T) {
	withCounter := newLimitedDock(chatlimits.WarnAt)
	without := newLimitedDock(chatlimits.WarnAt - 1)
	if got, want := withCounter.Lines(), without.Lines()+1; got != want {
		t.Fatalf("Lines() with the counter = %d, want %d (one more than without) — a row drawn but not "+
			"reserved is how the composer overflows", got, want)
	}
}

// TestTheCounterSaysWhenASendWillBeRefused: past the bound the server refuses, so the counter must say so
// before the operator presses Enter rather than after.
func TestTheCounterSaysWhenASendWillBeRefused(t *testing.T) {
	m := newLimitedDock(chatlimits.MaxMessageChars + 1)
	got := m.charCounter()
	if !strings.Contains(got, "over the limit") {
		t.Fatalf("counter = %q past the bound, want it to say the send will be refused", got)
	}
	if !m.overLimit() {
		t.Fatal("overLimit is false with a buffer past the bound")
	}
}

// TestTheCounterIsOffUnlessABoundIsInstalled: the dock is shared, so the counter must be opt-in.
func TestTheCounterIsOffUnlessABoundIsInstalled(t *testing.T) {
	m := New()
	m.SetValue(strings.Repeat("a", chatlimits.MaxMessageChars+1))
	if got := m.charCounter(); got != "" {
		t.Fatalf("counter = %q on a dock with no installed bound", got)
	}
}

// TestTheCounterRendersInTheComposer is the outward half: the row the operator reads.
func TestTheCounterRendersInTheComposer(t *testing.T) {
	m := newLimitedDock(chatlimits.WarnAt + 5)
	m.Width = 80
	out := m.View()
	if !strings.Contains(out, "80,005 / 100,000 characters") {
		t.Fatalf("the counter is not in the drawn composer:\n%s", out)
	}
	// And the block is exactly as tall as it claims, which is what the shell reserves.
	if got, want := strings.Count(out, "\n")+1, m.Lines(); got != want {
		t.Fatalf("View drew %d rows but Lines() says %d — the shell reserves Lines()", got, want)
	}
}
