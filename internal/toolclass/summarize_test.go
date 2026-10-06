package toolclass

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

// fixtureCase is one row of the SHARED cross-client fixture. It is deliberately the same schema a
// Vitest test (child 5) reads, so the two clients assert over identical bytes.
type fixtureCase struct {
	Name   string          `json:"name"`
	Ledger json.RawMessage `json:"ledger"`
	Now    int64           `json:"now"`
	Window int64           `json:"window"`
	Want   string          `json:"want"`
}

type fixture struct {
	Cases []fixtureCase `json:"cases"`
}

// TestSummarizeAgainstSharedFixture drives Summarize from internal/toolclass/testdata/
// rollup_fixture.json — the SAME file the GUI's Vitest test reads (child 5), so the terminal and
// the browser cannot render different numbers from one ledger JSON. Precedent: the TUI's
// grouping_test.go:5-6 ports sessionItems.test.ts's cases verbatim for the same reason.
//
// issued_at_unix_ms/now/window in the fixture are epoch MILLISECONDS.
func TestSummarizeAgainstSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/rollup_fixture.json")
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse shared fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("shared fixture has no cases — the Go/TS parity contract would be vacuous")
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := Summarize(c.Ledger, time.UnixMilli(c.Now), time.Duration(c.Window)*time.Millisecond)
			if got != c.Want {
				t.Errorf("Summarize(...) = %q, want %q", got, c.Want)
			}
		})
	}
}

// TestSummarizeAcceptanceExample pins AC6's literal target string, independent of the fixture, so
// a regeneration of the fixture cannot silently move the format both clients promise.
func TestSummarizeAcceptanceExample(t *testing.T) {
	now := time.UnixMilli(65000)
	ledger := `[
		{"function_name":"write","issued_at_unix_ms":35000},
		{"function_name":"edit","issued_at_unix_ms":35000},
		{"function_name":"batch_write","issued_at_unix_ms":35000},
		{"function_name":"orchicon_write","issued_at_unix_ms":35000},
		{"function_name":"write","issued_at_unix_ms":35000},
		{"function_name":"read","issued_at_unix_ms":35000},
		{"function_name":"batch_grep","issued_at_unix_ms":35000},
		{"function_name":"bash","issued_at_unix_ms":35000},
		{"function_name":"shell","issued_at_unix_ms":35000},
		{"function_name":"bash","issued_at_unix_ms":35000}
	]`
	const want = "5 modifies · 2 reads · 3 bash · newest call 30s ago"
	if got := Summarize([]byte(ledger), now, DefaultWindow); got != want {
		t.Fatalf("Summarize = %q, want %q", got, want)
	}
}

// TestSummarizeFormatEdgeCases pins the format rules that are easy to regress: singular/plural,
// the omission of a zero class, the absence of a dangling separator, the fixed ordering, and the
// load-bearing empty string when nothing was counted (AC6, AC7).
func TestSummarizeFormatEdgeCases(t *testing.T) {
	now := time.UnixMilli(100_000)
	cases := []struct {
		name   string
		ledger string
		want   string
	}{
		{
			"a single entry is singular",
			`[{"function_name":"write","issued_at_unix_ms":100000}]`,
			"1 modify · newest call 0s ago",
		},
		{
			"two entries are plural",
			`[{"function_name":"write","issued_at_unix_ms":100000},{"function_name":"edit","issued_at_unix_ms":100000}]`,
			"2 modifies · newest call 0s ago",
		},
		{
			"a zero class is omitted, and the separator never dangles",
			`[{"function_name":"bash","issued_at_unix_ms":100000}]`,
			"1 bash · newest call 0s ago",
		},
		{
			"the order is modify, read, bash however the ledger is ordered",
			`[{"function_name":"bash","issued_at_unix_ms":100000},{"function_name":"glob","issued_at_unix_ms":100000},{"function_name":"write","issued_at_unix_ms":100000}]`,
			"1 modify · 1 read · 1 bash · newest call 0s ago",
		},
		{
			"nothing counted is the EMPTY string, never 0 modifies",
			`[{"function_name":"ask_user","issued_at_unix_ms":100000},{"function_name":"permission.allow_once","issued_at_unix_ms":100000}]`,
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Summarize([]byte(c.ledger), now, DefaultWindow); got != c.want {
				t.Errorf("Summarize(...) = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSummarizeWindowIsReal is AC5: the window actually filters, and the boundary is stated and
// tested rather than left to an off-by-one. The rule implemented is INCLUSIVE on the left — an
// entry exactly `window` old is counted — because the window is "the last 30s" and the canonical
// example sits exactly on that edge.
func TestSummarizeWindowIsReal(t *testing.T) {
	now := time.UnixMilli(40_000)
	ledger := `[
		{"function_name":"write","issued_at_unix_ms":0},
		{"function_name":"read","issued_at_unix_ms":10000},
		{"function_name":"bash","issued_at_unix_ms":40000}
	]`
	// issued_at_unix_ms=0 is skipped (no window can place it); 10000 is 30s old and IS counted; 40000 is now.
	if got, want := Summarize([]byte(ledger), now, DefaultWindow), "1 read · 1 bash · newest call 0s ago"; got != want {
		t.Errorf("Summarize = %q, want %q", got, want)
	}

	oneMsOver := `[{"function_name":"read","issued_at_unix_ms":9999}]`
	if got := Summarize([]byte(oneMsOver), now, DefaultWindow); got != "" {
		t.Errorf("an entry 1ms past the window must not be counted, got %q", got)
	}
	exactBoundary := `[{"function_name":"read","issued_at_unix_ms":10000}]`
	if got, want := Summarize([]byte(exactBoundary), now, DefaultWindow), "1 read · newest call 30s ago"; got != want {
		t.Errorf("an entry exactly window-old is counted (inclusive rule): got %q, want %q", got, want)
	}
}

// TestSummarizeIsPureAndTotal is AC8: no I/O, no clock of its own, and NO PANIC on any input a
// caller can hand it — malformed, empty, nil, a JSON object, a ledger of only ignored tools, an
// entry with a missing or zero timestamp, a future timestamp, and a window of zero.
func TestSummarizeIsPureAndTotal(t *testing.T) {
	now := time.UnixMilli(50_000)
	cases := []struct {
		name   string
		in     []byte
		window time.Duration
		want   string
	}{
		{"nil input", nil, DefaultWindow, ""},
		{"empty input", []byte(""), DefaultWindow, ""},
		{"whitespace input", []byte("   "), DefaultWindow, ""},
		{"JSON null", []byte("null"), DefaultWindow, ""},
		{"JSON object, not an array", []byte("{\"function_name\":\"write\"}"), DefaultWindow, ""},
		{"unparseable JSON", []byte("not json"), DefaultWindow, ""},
		{"truncated array", []byte(`[{"function_name":"write"`), DefaultWindow, ""},
		{"empty array", []byte("[]"), DefaultWindow, ""},
		{"entry with no timestamp", []byte(`[{"function_name":"write"}]`), DefaultWindow, ""},
		{"entry with a zero timestamp", []byte(`[{"function_name":"write","issued_at_unix_ms":0}]`), DefaultWindow, ""},
		{"entry with no function name", []byte(`[{"issued_at_unix_ms":50000}]`), DefaultWindow, ""},
		{"unrecognised names COUNT as other, but the empty name is still skipped", []byte(
			`[{"function_name":"todoread_typo","issued_at_unix_ms":50000},{"function_name":"mcp__x__y","issued_at_unix_ms":50000},{"function_name":"","issued_at_unix_ms":50000}]`), DefaultWindow, "2 other tools · newest call 0s ago"},
		{"a future timestamp clamps rather than dropping the work", []byte(
			`[{"function_name":"write","issued_at_unix_ms":90000}]`), DefaultWindow, "1 modify · newest call 0s ago"},
		{"window of zero falls back to the named default", []byte(
			`[{"function_name":"write","issued_at_unix_ms":40000}]`), 0, "1 modify · newest call 10s ago"},
		{"a zero timestamp is untouched by a long window", []byte(
			`[{"function_name":"write","issued_at_unix_ms":0}]`), time.Hour, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Summarize panicked: %v", r)
				}
			}()
			if g := Summarize(c.in, now, c.window); g != c.want {
				t.Errorf("Summarize(%q, window=%v) = %q, want %q", c.in, c.window, g, c.want)
			}
		})
	}
}

// TestSummarizeIsIdempotentAndClockless pins purity: the same inputs give the same output twice,
// and the ONLY time input is `now` — the function owns no clock, so two calls a second apart over
// the same `now` agree.
func TestSummarizeIsIdempotentAndClockless(t *testing.T) {
	in := []byte(`[{"function_name":"write","issued_at_unix_ms":10000},{"function_name":"read","issued_at_unix_ms":15000}]`)
	now := time.UnixMilli(40_000)
	first := Summarize(in, now, DefaultWindow)
	second := Summarize(in, now, DefaultWindow)
	if first != second {
		t.Fatalf("Summarize is not deterministic: %q then %q", first, second)
	}
	if want := "1 modify · 1 read · newest call 25s ago"; first != want {
		t.Fatalf("Summarize = %q, want %q", first, want)
	}
	// The input is not mutated: the same bytes must still parse to the same ledger.
	var back []ledgerCall
	if err := json.Unmarshal(in, &back); err != nil {
		t.Fatalf("input was mutated: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("input was mutated: %d entries", len(back))
	}
}

// TestSummarizeRoundsTheAgeLikeTheActivityLine pins the rounding rule against the line the string
// is appended to: internal/tui/app.go computes `int(d.Round(time.Second)/time.Second)`, so the two
// numbers must agree at the halfway point. 500ms rounds UP here, exactly as it does there.
func TestSummarizeRoundsTheAgeLikeTheActivityLine(t *testing.T) {
	now := time.UnixMilli(10_500)
	ledger := `[{"function_name":"write","issued_at_unix_ms":10000}]`
	want := "1 modify · newest call " + itoaLikeAppDotGo(now.UnixMilli()-10_000) + "s ago"
	if got := Summarize([]byte(ledger), now, DefaultWindow); got != want {
		t.Errorf("Summarize = %q, want %q (the activity line's own rounding)", got, want)
	}
}

// itoaLikeAppDotGo is the activity line's rule, spelled once, in the test: seconds of d, rounded to
// the nearest second. Kept here so the expectation is derived, not a literal that could drift.
func itoaLikeAppDotGo(deltaMs int64) string {
	secs := int((time.Duration(deltaMs) * time.Millisecond).Round(time.Second) / time.Second)
	return strconv.Itoa(secs)
}

// TestSummarizeAgeIsTheNewestCountedEntryNotZero pins the invariant QA found broken: the trailing
// age is the age of the NEWEST COUNTED entry, and it must be derived from that entry even when its
// stamp is a malformed NEGATIVE value. Seeding `newest` at 0 and taking the max left the age
// measured from the EPOCH (here `now - 0` = an age of 10s) instead of from the entry that was
// actually counted (an age of 15s) — the "who is newest" sentinel must be the first counted entry, not 0.
// Unreachable from the real producer (time.UnixMilli is always positive), but a total function
// must not violate its own stated contract on malformed input.
func TestSummarizeAgeIsTheNewestCountedEntryNotZero(t *testing.T) {
	now := time.UnixMilli(10_000)
	ledger := `[{"function_name":"write","issued_at_unix_ms":-5000}]`
	if got, want := Summarize([]byte(ledger), now, DefaultWindow), "1 modify · newest call 15s ago"; got != want {
		t.Errorf("Summarize(negative-stamped entry) = %q, want %q "+
			"(the age is measured from the counted entry, not from a 0 sentinel)", got, want)
	}
	// With one real and one negative entry, the newest is still the real one.
	both := `[{"function_name":"write","issued_at_unix_ms":-5000},{"function_name":"read","issued_at_unix_ms":9000}]`
	if got, want := Summarize([]byte(both), now, DefaultWindow), "1 modify · 1 read · newest call 1s ago"; got != want {
		t.Errorf("Summarize(negative + real) = %q, want %q", got, want)
	}
}
