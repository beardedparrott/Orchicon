package dock

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The operator: "/models ... Then it should show the current ask orchicon model
// in text inside the text box on the bottom right side. We should also add
// context count, token count, and cache hit ratio and current cost ... before
// the brainstorm dropdown".
//
// So the stat row is right-aligned, the stats come BEFORE the mode pill, and the
// pill (the CONTROL) always survives a narrow box.
func TestStatRowRendersRightAlignedBeforeTheModePill(t *testing.T) {
	m := New()
	// Wide enough for the WHOLE strip plus the pill (the narrow case is its own
	// test below); the strip is ~103 cells and the box chrome takes 6.
	m.Width = 140
	m.Stats = "orchicon/anthropic/claude-sonnet-4 · ctx 124K/200K · 1.2M tok · cache 78% (940K) · $1.2345"
	m.Mode = "brainstorm"

	if m.StatsRows() != 1 {
		t.Fatalf("StatsRows = %d, want 1 when there is a strip to draw", m.StatsRows())
	}
	blank := New()
	if m.Lines() != blank.Lines()+1 {
		t.Fatalf("the stat row must be reserved: Lines() = %d, blank composer = %d",
			m.Lines(), blank.Lines())
	}

	v := ansi.Strip(m.View())
	row := ""
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "[brainstorm]") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("no stat row rendered:\n%s", v)
	}
	// Every metric the operator asked for is present.
	for _, want := range []string{"orchicon/anthropic/claude-sonnet-4", "ctx 124K/200K",
		"1.2M tok", "cache 78%", "(940K)", "$1.2345"} {
		if !strings.Contains(row, want) {
			t.Errorf("the stat row must show %q, got %q", want, row)
		}
	}
	// The stats sit BEFORE the pill, and the pill is flush against the edge.
	if strings.Index(row, "$1.2345") > strings.Index(row, "[brainstorm]") {
		t.Errorf("the stats must precede the mode pill:\n%s", row)
	}
	if got := strings.TrimRight(row, "│ "); !strings.HasSuffix(got, "[brainstorm]") {
		t.Errorf("the mode pill must be right-aligned, got:\n%s", row)
	}
}

// A narrow composer trims the STATS first: the mode pill is a control and must
// never be the thing that gets cut.
func TestStatRowKeepsTheModePillWhenNarrow(t *testing.T) {
	m := New()
	m.Width = 34
	m.Stats = "orchicon/commandcode/deepseek/deepseek-v4-flash · ctx 124K/200K · 1.2M tok"
	m.Mode = "brainstorm"

	v := ansi.Strip(m.View())
	if !strings.Contains(v, "[brainstorm]") {
		t.Fatalf("the mode pill must survive a narrow composer:\n%s", v)
	}
}

// With no strip and no pill the composer reserves no extra row (the blank
// composer's geometry is unchanged).
func TestStatRowAbsentByDefault(t *testing.T) {
	m := New()
	if m.StatsRows() != 0 {
		t.Fatalf("StatsRows = %d, want 0 for a blank composer", m.StatsRows())
	}
	if strings.Contains(ansi.Strip(m.View()), "ctx ") {
		t.Error("a blank composer must render no stat row")
	}
}

// A stats-only strip (no mode) still right-aligns.
func TestStatRowWithoutAModePill(t *testing.T) {
	m := New()
	m.Width = 80
	m.Stats = "$0.0123"
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "$0.0123") {
		t.Fatalf("the stats must render:\n%s", v)
	}
	if m.StatsRows() != 1 {
		t.Fatalf("StatsRows = %d, want 1", m.StatsRows())
	}
}
