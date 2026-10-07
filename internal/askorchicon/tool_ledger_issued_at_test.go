package askorchicon

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// Issue-time stamping for the live tool ledger.
//
// The rolling-window summary ("5 modifies · 2 reads · 3 bash · newest call 30s ago") needs
// to know WHEN each tool call was issued, and the ledger had no time at all.
// These tests pin the new `issued_at_unix_ms` field at every place it can be
// written, preserved, or silently lost:
//
//	1. recordStart stamps it .......................... the actual feature
//	2. snapshot() copies it into the JSON the mirror writes
//	3. neither resolve path re-stamps or drops it ...... (opencode + typed)
//	4. both synthesized-call branches stamp "now"
//	5. recordPermission stamps its decision record
//	6. the terminal sanitizer re-marshal keeps it
//	7. the wire readers carry the stamp WITHOUT disturbing the other four fields
//
// Every timestamp assertion is on the SNAPSHOT JSON — the bytes the mirror
// writes — not on an internal field, because the internal field persisting
// while the JSON drops it is exactly the failure mode this feature risks.

// withLedgerClock installs a fixed clock for the duration of one test and
// restores the real one afterwards. The ledger's clock seam is a package-level
// var (askorchicon has no injectable clock), so this is only safe while no test
// in the package calls t.Parallel().
func withLedgerClock(t *testing.T, ms int64) {
	t.Helper()
	prev := ledgerNowMillis
	ledgerNowMillis = func() int64 { return ms }
	t.Cleanup(func() { ledgerNowMillis = prev })
}

// ledgerCallJSON is the snapshot's per-call shape INCLUDING the stamp. It is
// deliberately a separate local type from the production entry: the assertion
// is about the JSON, and a reader of the column sees only JSON.
type ledgerCallJSON struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	FunctionName   string `json:"function_name"`
	Arguments      string `json:"arguments"`
	IssuedAtUnixMs int64  `json:"issued_at_unix_ms"`
}

func decodeLedgerCalls(t *testing.T, calls []byte) []ledgerCallJSON {
	t.Helper()
	var out []ledgerCallJSON
	if err := json.Unmarshal(calls, &out); err != nil {
		t.Fatalf("unmarshal tool_calls: %v (%s)", err, calls)
	}
	return out
}

// opencodePart is the opencode-shaped `message.part.updated` Payload that
// recordResolve reads (tool + state.status + state.input + state.output) — the
// same shape busAskCompleted builds in the loop tests.
func opencodePart(tool, status, command, output string) map[string]any {
	return map[string]any{
		"tool": tool,
		"state": map[string]any{
			"status": status,
			"input":  map[string]any{"command": command},
			"output": output,
		},
	}
}

// AC 1: a call issued through recordStart carries the wall-clock time at which
// recordStart ran, asserted on the snapshot JSON the mirror writes.
func TestToolLedgerRecordStartStampsIssueTime(t *testing.T) {
	withLedgerClock(t, 1_700_000_000_123)
	led := newToolLedger()
	led.recordStart("bash")

	calls, _ := led.snapshot()
	got := decodeLedgerCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("want exactly one recorded call, got %d (%s)", len(got), calls)
	}
	if got[0].IssuedAtUnixMs != 1_700_000_000_123 {
		t.Errorf("issued_at_unix_ms = %d, want the time recordStart ran (1700000000123): %s",
			got[0].IssuedAtUnixMs, calls)
	}
	if !strings.Contains(string(calls), "\"issued_at_unix_ms\"") {
		t.Fatalf("the snapshot JSON does not carry the key at all — the mirror writes zero time: %s", calls)
	}
}

// AC 6: the wire shape gains EXACTLY one field. snapshot() re-copies only the
// exported fields deliberately, so `resolved` must stay off the wire while the
// new timestamp is deliberately ON it.
func TestToolLedgerSnapshotWireShapeGainsExactlyOneField(t *testing.T) {
	withLedgerClock(t, 1000)
	led := newToolLedger()
	led.recordStart("bash")
	led.recordResolve(opencodePart("bash", "completed", "ls", "file.txt"))

	calls, _ := led.snapshot()
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(calls, &rows); err != nil {
		t.Fatalf("unmarshal tool_calls: %v (%s)", err, calls)
	}
	if len(rows) != 1 {
		t.Fatalf("want one call, got %d", len(rows))
	}
	var keys []string
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"arguments", "function_name", "id", "issued_at_unix_ms", "type"}
	if len(keys) != len(want) {
		t.Fatalf("wire key set = %v, want exactly %v (resolved must not leak; the stamp must be present)", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("wire key set = %v, want exactly %v", keys, want)
		}
	}
}

// AC 3 (opencode path): recordResolve backfills the arguments onto the call
// recordStart already stamped — it must neither re-stamp nor drop the time.
func TestToolLedgerResolveKeepsTheIssueTime(t *testing.T) {
	withLedgerClock(t, 1000)
	led := newToolLedger()
	led.recordStart("bash")

	// The clock advances before the resolution arrives.
	ledgerNowMillis = func() int64 { return 2000 }
	led.recordResolve(opencodePart("bash", "completed", "ls -la", "file.txt"))

	calls, _ := led.snapshot()
	got := decodeLedgerCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("want one call (backfilled, not synthesized), got %d (%s)", len(got), calls)
	}
	if got[0].IssuedAtUnixMs != 1000 {
		t.Errorf("issued_at_unix_ms = %d, want the ISSUE time 1000 — recordResolve re-stamped or dropped it: %s",
			got[0].IssuedAtUnixMs, calls)
	}
	if !strings.Contains(got[0].Arguments, "ls -la") {
		t.Errorf("arguments = %q, want the resolution's arguments backfilled", got[0].Arguments)
	}
}

// AC 3 (typed/adapter-neutral path): recordToolResolution's backfill likewise
// keeps the issue time taken at recordStart.
func TestToolLedgerToolResolutionKeepsTheIssueTime(t *testing.T) {
	withLedgerClock(t, 1000)
	led := newToolLedger()
	led.recordStart("ask_user")

	ledgerNowMillis = func() int64 { return 2000 }
	led.recordToolResolution("ask_user", `{"question":"which?"}`, `{"recorded":true}`, false)

	calls, _ := led.snapshot()
	got := decodeLedgerCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("want one call (backfilled, not synthesized), got %d (%s)", len(got), calls)
	}
	if got[0].IssuedAtUnixMs != 1000 {
		t.Errorf("issued_at_unix_ms = %d, want the ISSUE time 1000 — the typed backfill disturbed the stamp: %s",
			got[0].IssuedAtUnixMs, calls)
	}
	if got[0].Arguments != `{"question":"which?"}` {
		t.Errorf("arguments = %q, want the typed resolution's arguments backfilled", got[0].Arguments)
	}
}

// AC 3 (both synthesized branches): when no start was ever observed, the honest
// issue time is "now" — the call is known to have happened by the time its
// result arrived. Both resolve paths must stamp it, or a re-attached client's
// calls would carry a phantom zero.
func TestToolLedgerSynthesizedCallsAreStampedNow(t *testing.T) {
	withLedgerClock(t, 5555)

	// (a) typed path, no start.
	typed := newToolLedger()
	typed.recordToolResolution("bash", `{"command":"ls"}`, "file.txt", false)
	calls, _ := typed.snapshot()
	got := decodeLedgerCalls(t, calls)
	if len(got) != 1 || got[0].IssuedAtUnixMs != 5555 {
		t.Errorf("typed synthesized call = %+v, want issued_at_unix_ms 5555", got)
	}

	// (b) opencode path, no start.
	opencode := newToolLedger()
	opencode.recordResolve(opencodePart("bash", "completed", "ls", "file.txt"))
	calls, _ = opencode.snapshot()
	got = decodeLedgerCalls(t, calls)
	if len(got) != 1 || got[0].IssuedAtUnixMs != 5555 {
		t.Errorf("opencode synthesized call = %+v, want issued_at_unix_ms 5555", got)
	}
}

// AC 5: a permission record is stamped. The choice is pinned here so a later
// reader cannot be surprised either way. It is a real event with a real time,
// and the window classifier excludes `permission.*` by name, so the stamp is
// free.
func TestToolLedgerPermissionRecordIsStamped(t *testing.T) {
	withLedgerClock(t, 7777)
	led := newToolLedger()
	led.recordPermission("bash", "make ci", "deny", "nope", "")

	calls, _ := led.snapshot()
	got := decodeLedgerCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("want one permission record, got %d (%s)", len(got), calls)
	}
	if got[0].IssuedAtUnixMs != 7777 {
		t.Errorf("permission record issued_at_unix_ms = %d, want 7777 (the decision's own time)",
			got[0].IssuedAtUnixMs)
	}
}

// AC 6, terminal path: the field must survive sanitizeAssistantToolLedger,
// which DECODES into a fixed struct and RE-MARSHALS — a pass-through it is not.
// This is the regression test for the trap: omit the field from askToolCallJSON
// and the stamp is silently stripped from every persisted row while every
// snapshot-level test above still passes.
func TestToolLedgerSurvivesTheTerminalSanitizer(t *testing.T) {
	withLedgerClock(t, 4242)
	led := newToolLedger()
	led.recordStart("bash") // issued, never resolved — the interrupted turn

	calls, _ := led.repairedSnapshot()
	got := decodeLedgerCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("want one repaired call, got %d (%s)", len(got), calls)
	}
	if got[0].IssuedAtUnixMs != 4242 {
		t.Errorf("issued_at_unix_ms = %d after the terminal repair, want 4242 — "+
			"the sanitizer re-marshals and stripped the unknown key: %s", got[0].IssuedAtUnixMs, calls)
	}
}

// AC 2, RE-POINTED: the stamp RIDES THE WIRE, and the four original fields are
// still byte-identical.
//
// THIS TEST USED TO ASSERT THE OPPOSITE — that a stamped payload and its
// pre-change predecessor yielded byte-identical proto ToolCalls, i.e. that the
// stamp DIED at the wire boundary. That was child 1's deliberate scope ("never
// a wire contract"). It is no longer true, and the change is the whole point of
// the activity line's rolling counter: a client never sees the ledger COLUMN,
// only the ChatMessage wire field, so a stamp that stops at the boundary leaves
// every client rendering "" over a turn that is plainly working.
//
// WHAT IS STILL PINNED HERE, unchanged: the four pre-existing fields. A reader
// that lost `arguments`, or renamed `function_name`, would be a wire
// regression — so the decode is asserted field by field, and the unstamped
// predecessor (a row persisted before the stamp existed) still decodes to the
// same four fields with a ZERO stamp, which the summarizer reads as "no
// timestamp" rather than as the epoch.
func TestToolCallsFromJSONCarriesTheStamp(t *testing.T) {
	unstamped := []byte(`[{"id":"tc-1","type":"function","function_name":"bash","arguments":"{\"command\":\"ls\"}"}]`)
	stamped := []byte(`[{"id":"tc-1","type":"function","function_name":"bash","arguments":"{\"command\":\"ls\"}","issued_at_unix_ms":1700000000123}]`)

	before := toolCallsFromJSON(unstamped)
	after := toolCallsFromJSON(stamped)
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("reader returned %d and %d calls, want 1 and 1", len(before), len(after))
	}

	// THE FOUR ORIGINAL FIELDS ARE UNTOUCHED BY THE STAMP, before and after.
	for i, c := range []*apiv1.ToolCall{before[0], after[0]} {
		if c.GetId() != "tc-1" || c.GetType() != "function" ||
			c.GetFunctionName() != "bash" || c.GetArguments() != `{"command":"ls"}` {
			t.Errorf("decode %d = %v, want id/type/function_name/arguments byte-identical", i, c)
		}
	}

	// AND THE STAMP SURVIVES — the half that makes the counter possible.
	if after[0].GetIssuedAtUnixMs() != 1_700_000_000_123 {
		t.Errorf("the stamped row reaches the client with issued_at_unix_ms = %d, want 1700000000123 — "+
			"toolCallsFromJSON is dropping the stamp, so every client's rolling counter would see the row "+
			"as unstamped and render nothing: %v", after[0].GetIssuedAtUnixMs(), after[0])
	}
	// A PRE-CHANGE ROW IS ZERO, NOT THE EPOCH. The absent key must decode to 0 so the summarizer SKIPS the
	// entry; the only other reading, the epoch, would place a genuinely old call inside the window.
	if before[0].GetIssuedAtUnixMs() != 0 {
		t.Errorf("an unstamped legacy row decoded to issued_at_unix_ms = %d, want 0 (\"no timestamp\") — "+
			"the summarizer must SKIP it, never count it as issued in 1970", before[0].GetIssuedAtUnixMs())
	}
	// proto.Equal must now DIFFER on the stamp alone, which is the mechanical statement of "it rides the
	// wire". Asserted so a future refactor that silently strips it again fails HERE rather than in a
	// client that renders an empty line.
	if proto.Equal(before[0], after[0]) {
		t.Error("the stamped and unstamped wire ToolCalls are identical — the stamp is being stripped at " +
			"the wire boundary again, which is what the rolling counter depends on")
	}
}
