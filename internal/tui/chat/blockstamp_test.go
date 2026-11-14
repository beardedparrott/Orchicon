package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// THE OPERATOR: "Is there any way we can also print the timestamps of every block in both the GUI and TUI
// for both user and assistant messages?"
//
// Both clients already CARRIED the instant — every ChatItem has At, every row has createdAt — and neither
// client drew it. These tests pin the drawing, and they pin the ZONE too, because this repo has been here
// before: screenkit/format.go opens with the operator's own words about a bare clock reading being
// misread, so a stamp that renders without its zone is a regression rather than a style choice.

func stampFixture(t *testing.T) int64 {
	t.Helper()
	return time.Date(2026, 9, 18, 14, 30, 5, 0, time.Local).UnixMilli()
}

func wantStamp(atMs int64) string {
	return screenkit.FmtClock(time.UnixMilli(atMs))
}

func TestEveryMessageBlockDrawsItsStampWithItsZone(t *testing.T) {
	at := stampFixture(t)
	want := wantStamp(at)
	// ONE LAYOUT, ONE SOURCE: the stamp must be the SAME formatter the rest of the TUI uses. A second
	// Format() call here is how two screens end up disagreeing about what a timestamp looks like.
	// FmtClock always ends with a zone ABBREVIATION ("CDT", "UTC", "BST"…). If it ever stops doing that,
	// this fixture stops being meaningful — so say so rather than asserting a stamp nobody can interpret.
	if name, _ := time.UnixMilli(at).In(time.Local).Zone(); !strings.HasSuffix(want, name) {
		t.Fatalf("FmtClock produced %q, which does not end with the local zone %q — the formatter changed "+
			"under this test, and a stamp with no zone is the defect screenkit/format.go exists to prevent",
			want, name)
	}

	for _, tc := range []struct {
		name  string
		item  ChatItem
		label string
	}{
		{"operator", ChatItem{Kind: KindUser, Text: "hello", At: at, Key: "u"}, userBandLabel},
		{"assistant", ChatItem{Kind: KindText, Text: "reply", At: at, Key: "a"}, ""},
		{"reasoning", ChatItem{Kind: KindReasoning, Text: "thinking", At: at, Key: "r"}, "reasoning"},
		{"notice", ChatItem{Kind: KindNotice, Text: "compacted", At: at, Key: "n"}, "notice"},
	} {
		out, _ := RenderItemsSpans([]ChatItem{tc.item}, 100)
		if !strings.Contains(out, want) {
			t.Errorf("%s block did not draw its stamp %q:\n%s", tc.name, want, out)
		}
	}
}

func TestABlockWithNoInstantDrawsNoStamp(t *testing.T) {
	// A live fragment that has not been stamped, or a row written before timestamps. "00:00:00 UTC" would be
	// a FABRICATED time, which is worse than none — it reads as fact.
	out, _ := RenderItemsSpans([]ChatItem{
		{Kind: KindUser, Text: "hello", At: 0, Key: "u"},
		{Kind: KindText, Text: "reply", At: 0, Key: "a"},
	}, 100)
	if strings.Contains(out, "00:00:00") || strings.Contains(out, "1969") {
		t.Errorf("an unstamped block invented a time:\n%s", out)
	}
	// And the operator's label still renders, unwidened by a missing stamp.
	if !strings.Contains(out, userBandLabel) {
		t.Errorf("the operator's band label went missing:\n%s", out)
	}
}

func TestStampUsesLocalWallClockNotUTC(t *testing.T) {
	// THE DEFECT THE RULE EXISTS FOR: FmtTime used to print UTC, so an operator on the wrong coast read a
	// time that was hours out and had no cue to convert it. A stamp built from the instant must be LOCAL.
	at := stampFixture(t)
	got := blockStamp(at)
	if want := time.UnixMilli(at).In(time.Local).Format("15:04:05"); !strings.Contains(got, want) {
		t.Errorf("blockStamp(%d) = %q, want it to contain local %q", at, got, want)
	}
}
