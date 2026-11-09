package askorchicon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/toolclass"
)

// The LEDGER -> SUMMARIZER seam.

// TestSummarizeCountsTheRealLedgerShape is the cross-package contract: a real ledger snapshot,
// rendered by the real Summarize, produces the real counts.
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
	const want = "2 modifies · 1 read · 1 bash · newest call 0s ago"
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
	const want = "1 modify · newest call 0s ago"
	if got := toolclass.Summarize(mixed, now, toolclass.DefaultWindow); got != want {
		t.Errorf("Summarize(real ledger + an untimestamped pre-change row) = %q, want %q "+
			"(the old row is skipped; the stamped one still counts)", got, want)
	}
}

// TestSummarizeEmptyRealLedgerIsTheEmptyString pins the load-bearing AC6 rule on the shape the
// clients actually hand it: snapshot() encodes an empty ledger as "[]" (never nil).
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

// TestTheIssueStampSurvivesTheWire is the SECOND half of the same seam: the ledger's stamp has to
// survive `messageRowToProto` to reach a CLIENT at all.
func TestTheIssueStampSurvivesTheWire(t *testing.T) {
	const stamp = int64(1_700_000_000_123)
	withLedgerClock(t, stamp)
	led := newToolLedger()
	led.recordStart("write")
	led.recordStart("read")
	calls, _ := led.snapshot()

	wire := messageRowToProto(db.MessageRow{
		ID: "a1", ConversationID: "c1", Role: "assistant", Content: "done",
		ToolCalls: calls,
	})

	got := wire.GetToolCalls()
	if len(got) != 2 {
		t.Fatalf("the wire carries %d tool calls, want 2 — messageRowToProto dropped the ledger rows: %v",
			len(got), got)
	}
	for i, c := range got {
		if c.GetIssuedAtUnixMs() != stamp {
			t.Errorf("tool call %d reaches the client with issued_at_unix_ms = %d, want %d — the stamp did not "+
				"survive toolCallsFromJSON, so the activity line's rolling counter would see every row as "+
				"unstamped and render nothing", i, c.GetIssuedAtUnixMs(), stamp)
		}
	}
	// AND THE CLIENT-SIDE SUMMARIZER COUNTS THEM, which is the point of carrying it: the wire shape is what
	// toolclass.SummarizeCalls (the TUI's entry point) is handed.
	callsForLine := make([]toolclass.Call, 0, len(got))
	for _, c := range got {
		callsForLine = append(callsForLine, toolclass.Call{ToolName: c.GetFunctionName(), AtMs: c.GetIssuedAtUnixMs()})
	}
	if summary := toolclass.SummarizeCalls(callsForLine, time.UnixMilli(stamp), toolclass.DefaultWindow); summary == "" {
		t.Error("SummarizeCalls over the wire tool calls rendered nothing — the counter the TUI draws would be " +
			"empty for a turn that just made two calls")
	}
}

// TestTheOperatorTurnIsFullyCounted is AC1 (the measurement) and AC2 (the fix) on a REAL ledger.
func TestTheOperatorTurnIsFullyCounted(t *testing.T) {
	const nowMs int64 = 1_700_000_000_000
	withLedgerClock(t, nowMs)
	led := newToolLedger()
	for _, name := range []string{
		"bash", "bash", "bash",
		"list_projects", "list_work_items", "get_work_item", "read_project_file",
		"list_executions", "get_execution", "list_ideas", "get_project", "list_audit_events",
	} {
		led.recordStart(name)
	}
	calls, _ := led.snapshot()

	// The raw ledger: 12 rows, each stamped, each carrying the BARE product name (so the prefix
	// strip is not in play and the unknown-name default is the only thing that could drop them).
	rows := decodeLedgerCalls(t, calls)
	if len(rows) != 12 {
		t.Fatalf("the real ledger has %d rows, want 12: %s", len(rows), calls)
	}
	for i, r := range rows {
		if r.IssuedAtUnixMs != nowMs {
			t.Errorf("row %d (%s) is unstamped (%d) — the summarizer would skip it", i, r.FunctionName, r.IssuedAtUnixMs)
		}
	}

	const want = "3 bash · 9 other tools · newest call 0s ago"
	if got := toolclass.Summarize(calls, time.UnixMilli(nowMs), toolclass.DefaultWindow); got != want {
		t.Errorf("Summarize(operator turn) = %q, want %q\n"+
			"the turn made 12 calls in the window; the line must account for all 12", got, want)
	}
}
