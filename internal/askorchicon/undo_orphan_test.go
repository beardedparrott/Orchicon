package askorchicon

// undo_orphan_test.go — UNDO LEAVES A REPLY WITH NO QUESTION, AND A REPLAY MAY NOT START WITH ONE.
//
// The operator asked for two verbs on a user message: "undo it (remove it from the screen and context)" and
// "revert to that message removing anything that has come after it". Undo removes ONE row, by their choice —
// which means the assistant reply it produced is left with no question in front of it.
//
// That reply must stay in the transcript (undoing your question should not silently take the answer with it),
// but a replayed history whose OLDEST turn is an assistant message is a shape providers reject: the first
// non-system turn has to be the user's.

import (
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

func row(id, role string, at time.Time) db.MessageRow {
	return db.MessageRow{ID: id, Role: role, Content: id, CreatedAt: at}
}

// TestUndoLeavesAnOrphanThatTheReplayDrops is the reported shape, in the order the turn path uses.
//
// prevMessages comes from ListMessages (ORDER BY created_at DESC), so the OLDEST row is LAST. This is the
// case an earlier version of the repair got backwards.
func TestUndoLeavesAnOrphanThatTheReplayDrops(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// The operator undid u1, so a1 is now the oldest surviving row.
	desc := []db.MessageRow{
		row("a2", "assistant", t0.Add(3*time.Second)),
		row("u2", "user", t0.Add(2*time.Second)),
		row("a1", "assistant", t0.Add(1*time.Second)),
	}
	got := sanitizeHistoryRows(desc)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (a1 dropped, a2 and u2 kept): %v", len(got), ids(got))
	}
	for _, m := range got {
		if m.Role == "assistant" && m.ID == "a1" {
			t.Error("the orphaned OLDEST reply survived the replay — a history that begins with an assistant " +
				"turn is refused by a provider")
		}
	}
	if last := got[len(got)-1]; last.ID != "u2" {
		t.Errorf("the oldest surviving row is %q, want u2 — the repair trimmed the wrong end", last.ID)
	}
}

// TestAnOrdinaryHistoryIsUntouched — the repair must be invisible when there is no orphan.
func TestAnOrdinaryHistoryIsUntouched(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// ASC, starting with a user row: nothing to drop.
	asc := []db.MessageRow{
		row("u1", "user", t0),
		row("a1", "assistant", t0.Add(time.Second)),
		row("u2", "user", t0.Add(2*time.Second)),
	}
	if got := sanitizeHistoryRows(asc); len(got) != 3 {
		t.Fatalf("an ordinary history was trimmed: %v", ids(got))
	}
	// DESC of the same conversation: also nothing to drop.
	desc := []db.MessageRow{
		row("u2", "user", t0.Add(2*time.Second)),
		row("a1", "assistant", t0.Add(time.Second)),
		row("u1", "user", t0),
	}
	if got := sanitizeHistoryRows(desc); len(got) != 3 {
		t.Fatalf("an ordinary DESC history was trimmed: %v", ids(got))
	}
}

// TestEveryUserMessageUndoneLeavesNothingToReplay — the end state of undoing everything.
func TestEveryUserMessageUndoneLeavesNothingToReplay(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	desc := []db.MessageRow{
		row("a2", "assistant", t0.Add(2*time.Second)),
		row("a1", "assistant", t0.Add(time.Second)),
	}
	if got := sanitizeHistoryRows(desc); len(got) != 0 {
		t.Fatalf("a history of nothing but replies still replays %d rows: %v", len(got), ids(got))
	}
}

// TestRowsWithoutATimestampKeepTheCallersOrder — the fixtures and single-transaction case.
//
// Two rows written in one transaction share a timestamp, so there is no evidence of order and guessing would be
// arbitrary. The caller's order stands, which is what keeps an ordinary history untouched and is why an
// earlier id-comparison version was wrong.
func TestRowsWithoutATimestampKeepTheCallersOrder(t *testing.T) {
	var zero time.Time
	asc := []db.MessageRow{row("u1", "user", zero), row("a1", "assistant", zero)}
	if got := sanitizeHistoryRows(asc); len(got) != 2 {
		t.Fatalf("a history with no timestamps was trimmed: %v", ids(got))
	}
}

func ids(rows []db.MessageRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID + ":" + r.Role
	}
	return out
}
