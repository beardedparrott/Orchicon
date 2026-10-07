package tui

// activity_summary_ingest_test.go — THE DURABLE TOOL CALLS REACH THE LINE THROUGH onTranscript.
//
// The AC tests in activity_summary_test.go seed chatStore directly, which proves the STORE -> LINE half
// and says nothing about how the data gets INTO the store. This closes the other half: a TranscriptMsg
// carrying the page's tool calls must land in the store, and the line must then report them on the
// painted frame. Without it, a regression that stopped onTranscript from handing msg.ToolCalls to the
// store — or that APPENDED instead of replacing — would leave every other test green while the counter
// never appeared, or climbed without bound.
//
// The returned command is APPLIED, the way the router does (router.go: chat.TranscriptMsg ->
// tea.Batch(m.onTranscript(msg), m.waitChat())), because the footer is set by onChatWake — which
// onTranscript RETURNS rather than calls. Asserting on the store alone would miss that.

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestTheTranscriptIngestLandsTheToolCalls — the wire -> store seam, asserted at the painted frame the
// operator reads.
func TestTheTranscriptIngestLandsTheToolCalls(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m := summaryTurnApp(t)
	m.chat.SetSilenceForTest("c1", 4*time.Second)
	if strings.Contains(m.askStatusLine(), "modifies") {
		t.Fatal("fixture: the counter is already showing before any page carried work")
	}
	calls := summaryFixture(now)

	applyCmds(m, m.onTranscript(chat.TranscriptMsg{ConvID: "c1", ToolCalls: calls}))

	if got := m.chatStore.snapshotToolCalls("c1"); len(got) != len(calls) {
		t.Fatalf("onTranscript landed %d tool calls, want %d — the page's work never reached the store: %+v",
			len(got), len(calls), got)
	}
	if frame := stripANSI(m.View()); !strings.Contains(frame, "modifies") {
		t.Errorf("the ingested page's work is not on the painted frame:\n%s", tailOf(frame, 1200))
	}
}

// TestTheTranscriptIngestReplacesRatherThanAppends pins the WHOLESALE rule the field comment states:
// the ledger column is cumulative for the turn and each landing page is its authority, so a second page
// REPLACES the first. Appending would double-count the same call on every 1s poll and the counter would
// climb forever over a turn that made three calls.
func TestTheTranscriptIngestReplacesRatherThanAppends(t *testing.T) {
	now := time.UnixMilli(1_700_000_130_000)
	freezeNoticeNow(t, now)

	m := summaryTurnApp(t)
	m.chat.SetSilenceForTest("c1", 4*time.Second)

	page := summaryFixture(now) // three calls
	applyCmds(m, m.onTranscript(chat.TranscriptMsg{ConvID: "c1", ToolCalls: page}))
	applyCmds(m, m.onTranscript(chat.TranscriptMsg{ConvID: "c1", ToolCalls: page}))
	if got := m.chatStore.snapshotToolCalls("c1"); len(got) != len(page) {
		t.Fatalf("a second identical page left %d calls, want %d — the store APPENDED, so the counter would "+
			"double-count on every poll: %+v", len(got), len(page), got)
	}

	// AND A PAGE THAT CARRIES NO WORK CLEARS IT, rather than leaving a stale count standing as a claim
	// that work is still happening.
	applyCmds(m, m.onTranscript(chat.TranscriptMsg{ConvID: "c1", ToolCalls: nil}))
	if got := m.chatStore.snapshotToolCalls("c1"); len(got) != 0 {
		t.Fatalf("a page with no tool calls left %d behind — a stale counter outlived its work: %+v", len(got), got)
	}
	if frame := stripANSI(m.View()); strings.Contains(frame, "modifies") {
		t.Errorf("the cleared page still shows a counter:\n%s", tailOf(frame, 1200))
	}
}
