package askorchicon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/toolclass"
)

// The LEDGER -> SUMMARIZER seam.
//
// internal/toolclass.Summarize reads the `tool_calls` column by its JSON FIELD
// NAMES, and internal/askorchicon is the only producer of those bytes. Nothing
// else in either package's tests crosses that boundary: the toolclass fixture
// is written in toolclass's own vocabulary, so if the ledger's tag drifts from
// Summarize's tag, json.Unmarshal leaves the timestamp 0, every entry is
// skipped as "no timestamp", and the activity line renders "" over a turn that
// is plainly working — with NO error, NO failing fixture and NO failing ledger
// test. These tests drive Summarize over a REAL snapshot, so that silent
// divergence fails loudly here instead.
//
// This is the regression guard for exactly the bug the shared-tool-classifier
// work item's own architect flagged as a risk (plan D5 "if child 1 names the
// field differently …"), and it is the reason `issued_at_unix_ms` — not `ts` —
// is pinned on both sides.

// TestSummarizeCountsTheRealLedgerShape is the cross-package contract: a real
// ledger snapshot, rendered by the real Summarize, produces the real counts.
// The ledger's own field tag and the summarizer's reader tag must agree, or
// the want below becomes "".
func TestSummarizeCountsTheRealLedgerShape(t *testing.T) {
	withLedgerClock(t, 1_700_000_000_000)
	led := newToolLedger()
	led.recordStart("write")
	led.recordStart("edit")
	led.recordStart("read")
	led.recordStart("bash")
	led.recordStart("ask_user")                               // a card, must not count
	led.recordPermission("bash", "make ci", "deny", "no", "") // a decision, must not count

	calls, _ := led.snapshot()

	// The wire bytes really do carry the key the summarizer reads. This is the
	// assertion that fails the moment either tag drifts.
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(calls, &rows); err != nil {
		t.Fatalf("unmarshal tool_calls: %v (%s)", err, calls)
	}
	if _, ok := rows[0]["issued_at_unix_ms"]; !ok {
		t.Fatalf("the ledger snapshot has no `issued_at_unix_ms` key — "+
			"toolclass.Summarize would silently read 0 and render \"\": %s", calls)
	}

	now := time.UnixMilli(1_700_000_000_000)
	const want = "2 modifies · 1 read · 1 bash · last 0s"
	if got := toolclass.Summarize(calls, now, toolclass.DefaultWindow); got != want {
		t.Errorf("toolclass.Summarize(real ledger) = %q, want %q\n"+
			"the ledger's field tag and the summarizer's reader tag have diverged", got, want)
	}
}

// TestSummarizeSkipsAnUnstampedPreChangeRowOverRealLedgerBytes pins the
// "no timestamp -> skipped" rule (AC8) against the wire bytes a pre-change row
// actually has: the key simply absent. A row with no time cannot be placed in
// a window, so it must not contribute — and must not, on its own, blank the
// line a stamped sibling still earns.
func TestSummarizeSkipsAnUnstampedPreChangeRowOverRealLedgerBytes(t *testing.T) {
	withLedgerClock(t, 9_000_000_000_000)
	led := newToolLedger()
	led.recordStart("write")

	calls, _ := led.snapshot()

	// Splice in a pre-change row: no issued_at_unix_ms key at all.
	var rows []json.RawMessage
	if err := json.Unmarshal(calls, &rows); err != nil {
		t.Fatalf("unmarshal tool_calls: %v (%s)", err, calls)
	}
	old := json.RawMessage(`{"id":"legacy","type":"function","function_name":"write","arguments":"{}"}`)
	mixed, err := json.Marshal(append(append([]json.RawMessage{}, rows...), old))
	if err != nil {
		t.Fatalf("re-marshal mixed ledger: %v", err)
	}

	now := time.UnixMilli(9_000_000_000_000)
	const want = "1 modify · last 0s"
	if got := toolclass.Summarize(mixed, now, toolclass.DefaultWindow); got != want {
		t.Errorf("Summarize(real ledger + an untimestamped pre-change row) = %q, want %q "+
			"(the old row is skipped; the stamped one still counts)", got, want)
	}
}

// TestSummarizeEmptyRealLedgerIsTheEmptyString pins the load-bearing AC6 rule
// on the shape the clients actually hand it: snapshot() encodes an empty ledger
// as "[]" (never nil), and that must render "" — never "0 modifies".
func TestSummarizeEmptyRealLedgerIsTheEmptyString(t *testing.T) {
	led := newToolLedger()
	calls, _ := led.snapshot()
	if string(calls) != "[]" {
		t.Fatalf("empty ledger snapshot = %s, want []", calls)
	}
	if got := toolclass.Summarize(calls, time.UnixMilli(1), toolclass.DefaultWindow); got != "" {
		t.Errorf("Summarize(empty ledger) = %q, want the empty string", got)
	}
}
