package kit2

// list_share_test.go — THE MASTER-DETAIL SPLIT IS ADJUSTABLE, AND STILL ONE LAYOUT.
//
// The operator: "I am talking about every pane in the TUI where there is a tree and detail view. I would like
// to be able to expand the tree view to see the full work item names, execution names, etc."
//
// The split was a hard 50/50 through SplitWidths, so a long name was truncated in the tree however wide the
// terminal was — half the screen went to the detail pane even when it was empty.
//
// THE RISK THIS FILE EXISTS FOR: the split is read by the LAYOUT (SinglePane) and by the CLICK HIT-TEST
// (mouseRegion). If those two disagree, a click lands in a pane the operator is not looking at — the exact
// class of bug mouseRegion's own comment records ("clicking one thing selected another").

import (
	"testing"
)

func TestTheDefaultSplitIsUnchanged(t *testing.T) {
	// NO PREFERENCE MUST REPRODUCE THE OLD BEHAVIOUR EXACTLY, so adding the preference cannot move a single
	// column for an operator who never touches the chord.
	for _, w := range []int{40, 60, 80, 100, 121, 200} {
		b := &Base{width: w}
		lw, dw := b.twoPaneWidths(w)
		want := SplitWidths(w, 2, 1)
		if lw != want[0] || dw != want[1] {
			t.Errorf("w=%d: default split = (%d,%d), want the even split (%d,%d)", w, lw, dw, want[0], want[1])
		}
	}
}

func TestWideningGivesTheListMoreAndTheDetailLess(t *testing.T) {
	// The operator's actual need: more columns for the tree.
	const w = 120
	narrow := &Base{width: w}
	narrow.SetListSharePct(minListSharePct)
	nw, nd := narrow.twoPaneWidths(w)

	wide := &Base{width: w}
	wide.SetListSharePct(maxListSharePct)
	ww, wd := wide.twoPaneWidths(w)

	if ww <= nw {
		t.Errorf("the widest share gives the list %d columns and the narrowest %d — widening did nothing", ww, nw)
	}
	if wd >= nd {
		t.Errorf("the detail pane must give columns up: %d at max share vs %d at min", wd, nd)
	}
	// The total is the pane's budget either way — the split moves the boundary, it does not change the frame.
	if nw+nd != ww+wd {
		t.Errorf("the two shares spend different totals: %d vs %d", nw+nd, ww+wd)
	}
}

func TestTheLayoutAndTheHitTestAgree(t *testing.T) {
	// THE ASSERTION THAT MATTERS. A click must land in the pane the operator is LOOKING at, so the columns
	// mouseRegion classifies as "the list" must be exactly the columns SinglePane gave the list pane.
	for _, share := range []int{0, minListSharePct, 50, 70, maxListSharePct} {
		for _, w := range []int{60, 80, 100, 121} {
			b := &Base{width: w, active: 0}
			b.sources = []*source{{}} // one source: the two-pane master/detail case
			b.SetListSharePct(share)
			lw, _ := b.twoPaneWidths(w)

			// Columns strictly inside the list belong to the focused source pane…
			for x := 0; x < lw; x++ {
				if got, isDetail := b.mouseRegion(x); isDetail || got != b.active {
					t.Errorf("share=%d w=%d: column %d is inside the list pane (list ends at %d) but the "+
						"hit-test says source=%d detail=%v", share, w, x, lw-1, got, isDetail)
				}
			}
			// …the boundary column is the divider (neither pane)…
			if got, isDetail := b.mouseRegion(lw); isDetail || got != -1 {
				t.Errorf("share=%d w=%d: the divider column %d resolved to source=%d detail=%v", share, w, lw, got, isDetail)
			}
			// …and everything past it is the detail pane.
			for x := lw + 1; x < w; x++ {
				if got, isDetail := b.mouseRegion(x); !isDetail || got != -1 {
					t.Errorf("share=%d w=%d: column %d is in the detail pane but the hit-test says source=%d "+
						"detail=%v", share, w, x, got, isDetail)
				}
			}
		}
	}
}

func TestTheShareIsClampedAndResettable(t *testing.T) {
	b := &Base{width: 100}
	// 0 means "no preference" and must NOT be clamped to the minimum — that would leave every screen on a
	// split nobody chose (the App's zero value is pushed out to the screens on every message).
	b.SetListSharePct(0)
	if got := b.ListSharePct(); got != defaultListSharePct {
		t.Errorf("SetListSharePct(0) left the share at %d, want the default %d", got, defaultListSharePct)
	}
	b.SetListSharePct(5)
	if got := b.ListSharePct(); got != minListSharePct {
		t.Errorf("a below-minimum share = %d, want the clamp at %d", got, minListSharePct)
	}
	b.SetListSharePct(95)
	if got := b.ListSharePct(); got != maxListSharePct {
		t.Errorf("an above-maximum share = %d, want the clamp at %d", got, maxListSharePct)
	}
}

func TestNudgingReportsWhetherItMoved(t *testing.T) {
	// The shell consumes the chord either way, so this is not about the key — it is about the App being able
	// to store what the screen APPLIED rather than recomputing the clamp itself in a second place.
	b := &Base{width: 100}
	before := b.ListSharePct()
	if !b.NudgeListShare(+1) {
		t.Error("a nudge from the middle reported no movement")
	}
	if b.ListSharePct() <= before {
		t.Errorf("nudging right did not widen: %d -> %d", before, b.ListSharePct())
	}
	if !b.NudgeListShare(-1) {
		t.Error("a nudge back reported no movement")
	}
	if b.ListSharePct() != before {
		t.Errorf("nudging right then left landed on %d, want the original %d", b.ListSharePct(), before)
	}
	// At a bound it reports false, which is what lets a caller tell "nothing happened" from "it changed".
	b.SetListSharePct(maxListSharePct)
	if b.NudgeListShare(+1) {
		t.Error("a nudge at the maximum reported movement")
	}
	b.SetListSharePct(minListSharePct)
	if b.NudgeListShare(-1) {
		t.Error("a nudge at the minimum reported movement")
	}
}
