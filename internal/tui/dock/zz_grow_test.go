package dock

import (
	"strings"
	"testing"
)

// The operator-visible property: the box must be tall enough to show every wrapped row the operator can
// type into. If InputRows() < the widget's real wrapped height, the caret sits on a row that is not drawn.
//
// MININPUTROWS MATTERS HERE: the box has a resting height (3 rows), so a short message is never blind even
// when the estimate is wrong. The bug appears once the text is long enough that the real wrap exceeds that
// floor — which is precisely "after you type a sentence about halfway".
func TestBoxGrowsAsTextWraps(t *testing.T) {
	m := New()
	m.Width = 80
	m.SetViewportRows(40)
	m.resizeTa()
	tw := m.taWidth()
	t.Logf("taWidth=%d MinInputRows=%d", tw, MinInputRows)

	var sb strings.Builder
	words := strings.Fields("the quick brown fox jumps over the lazy dog and keeps running through the " +
		"field toward the barn where a second fox waits and then a third arrives to join the chase")
	blind := 0
	for _, w := range words {
		sb.WriteString(w)
		sb.WriteString(" ")
		m.ta.SetValue(sb.String())
		m.resizeTa()
		visible := m.InputRows()
		realRows := m.ta.LineInfo().Height
		if realRows > visible {
			blind++
			if blind <= 3 {
				t.Errorf("len=%3d: widget wraps to %d rows but the box shows %d — the operator types blind",
					len(sb.String()), realRows, visible)
			}
		}
	}
	t.Logf("blind states across the message: %d", blind)
}
