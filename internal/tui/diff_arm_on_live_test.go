package tui

// diff_arm_on_live_test.go — THE PANE MUST RE-ENGAGE WHEN ITS OWNER BECOMES LIVE.
//
// The operator: "In the TUI it worked but not while I was actively watching it.
// I had to hit ctrl+d again to refresh the screen."
//
// The pane was armed EXACTLY ONCE — when it was opened. At that moment the turn
// had not started, so diffOwnerLive said false and no live stream was ever
// subscribed. Nothing re-evaluated it afterwards: refreshDiffOwner runs on a tab
// switch and on conversation selection, and it additionally REFUSES while the
// COMPOSER holds focus — which is precisely where the operator is sitting while
// watching a turn run. So the pane kept whatever its open-time fetch saw, and
// only closing and reopening it re-armed the stream and revealed the files.

import "testing"

// TestDiffPaneArmsWhenItsOwnerBecomesLive is the fix.
func TestDiffPaneArmsWhenItsOwnerBecomesLive(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// The composer has focus: the operator is watching a turn run, not reading
	// the transcript. This is the state the old gate refused in.
	m.setFocus(focusComposer)
	m.diffOpen = true
	if m.diffPane == nil {
		t.Fatal("fixture: the shell must own a diff pane")
	}

	// Nothing live yet — there is no stream to subscribe to, and arming here
	// would be a lie.
	if cmd := m.ensureDiffStreamArmed(); cmd != nil {
		t.Error("the pane engaged while no turn was running — a durable-only owner has no live stream")
	}

	// The turn starts NOW, after the pane was already open.
	if cmd := m.chat.Send("c1", "hi", ""); cmd == nil {
		t.Fatal("fixture: the send must arm a stream slot")
	}
	if !m.chat.IsStreaming("c1") {
		t.Fatal("fixture: this client must hold a live slot for the turn")
	}

	if cmd := m.ensureDiffStreamArmed(); cmd == nil {
		t.Fatal("the pane did NOT re-engage when its owner became live — the stream is never subscribed, so the " +
			"diff stays empty until the operator closes and reopens it (the reported ctrl+d workaround)")
	}

	// IDEMPOTENT, because the wake path calls this on every chat event and
	// SetOwner fetches: acting unconditionally would re-fetch the ledger
	// continuously.
	if again := m.ensureDiffStreamArmed(); again != nil {
		t.Error("a second call acted again — the wake path would re-fetch the ledger on every event")
	}
}

// TestAWakeEngagesThePane pins the WIRING, not just the helper: the wake path
// is what runs while a turn is in flight, so if it stops calling the helper the
// fix is inert however correct the helper is.
func TestAWakeEngagesThePane(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.setFocus(focusComposer)
	m.diffOpen = true
	if cmd := m.chat.Send("c1", "hi", ""); cmd == nil {
		t.Fatal("fixture: the send must arm a stream slot")
	}
	// A wake is the per-event repaint the shell runs during a turn.
	if cmd := m.onChatWake(); cmd == nil {
		t.Error("the wake path did not engage the diff pane — a pane opened before the turn started would stay " +
			"empty for the whole turn, which is the reported ctrl+d workaround")
	}
}

// TestTheOldRepointPathCouldNotDoThis is the CONTRAST: it pins the reason the
// wake-path helper exists, so a future reader does not "simplify" it back into
// refreshDiffOwner and reintroduce the bug.
func TestTheOldRepointPathCouldNotDoThis(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.setFocus(focusComposer)
	m.diffOpen = true
	if cmd := m.chat.Send("c1", "hi", ""); cmd == nil {
		t.Fatal("fixture: the send must arm a stream slot")
	}
	if !m.chat.IsStreaming("c1") {
		t.Fatal("fixture: the client must hold a live slot")
	}

	// refreshDiffOwner REFUSES while composing — which is where the operator
	// sits while watching a turn. That refusal is why the pane never re-armed.
	if cmd := m.refreshDiffOwner(); cmd != nil {
		t.Error("refreshDiffOwner acted while the composer held focus — if its gate has changed, this file's " +
			"premise is gone and the wake-path helper needs re-justifying rather than silently duplicating it")
	}

	// The new helper is NOT focus-gated: whether the pane subscribes to its
	// owner's ledger is data plumbing, not input routing.
	if cmd := m.ensureDiffStreamArmed(); cmd == nil {
		t.Error("the wake-path helper must not care which surface owns the keystrokes")
	}
}

// TestDiffArmingIsSkippedWhenThePaneIsClosed keeps the helper from doing work
// nobody asked for.
func TestDiffArmingIsSkippedWhenThePaneIsClosed(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.diffOpen = false
	if cmd := m.chat.Send("c1", "hi", ""); cmd == nil {
		t.Fatal("fixture: the send must arm a stream slot")
	}
	if cmd := m.ensureDiffStreamArmed(); cmd != nil {
		t.Error("a closed pane must not be armed — there is nothing rendering it")
	}
}
