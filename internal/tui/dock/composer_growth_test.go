package dock

// composer_growth_test.go — HOW TALL THE COMPOSER MAY GROW.
//
// The operator: "as the text grows and/or newlines are inserted, the box height should
// grow automatically." Growth already worked, bounded by two constants. MinInputRows is
// the resting height and was left alone (three rows, two of them slack, so a single
// newline changes nothing on screen — a deliberate choice, not the reported problem).
// The other bound was the complaint: "Remove the cap."
//
// It cannot be removed outright — the composer shares the screen with the transcript,
// and a thousand-line buffer cannot have a thousand rows of composer. So the fixed eight
// became a viewport-derived ceiling that grows with the terminal. These tests pin the
// arithmetic, because it is the sort of bound that is easy to get subtly wrong and
// invisible when it is.

import (
	"strings"
	"testing"
)

// THE CEILING RISES WITH THE VIEWPORT, and is no longer the constant.
func TestGrowthCeilingFollowsTheViewport(t *testing.T) {
	tall := New()
	tall.SetViewportRows(60)
	short := New()
	short.SetViewportRows(18)

	if tall.maxInputRows() <= MaxInputRows {
		t.Errorf("a 60-row viewport allows only %d input rows, capped at the fixed %d — the whole point of "+
			"removing the cap is that a bigger terminal affords a bigger composer",
			tall.maxInputRows(), MaxInputRows)
	}
	if short.maxInputRows() >= tall.maxInputRows() {
		t.Errorf("an 18-row viewport allows %d input rows and a 60-row one %d — the ceiling must scale with the "+
			"space available", short.maxInputRows(), tall.maxInputRows())
	}
}

// AND IT NEVER EXCEEDS THE VIEWPORT, once the viewport is big enough to hold the box.
//
// THE DEGENERATE CASE IS CHOSEN, NOT OVERLOOKED: below the rows the composer needs for its
// chrome plus its resting height, the FLOOR wins over the ceiling and the box takes
// MinInputRows anyway. A zero-height composer would be unusable, and on a terminal that
// small everything else is broken regardless — so the box stays visible and the ceiling
// simply has no room to bind.
func TestTheCeilingNeverExceedsTheViewport(t *testing.T) {
	for _, vp := range []int{1, 6, 10, 14, 18, 24, 40, 120} {
		m := New()
		m.SetViewportRows(vp)
		got := m.maxInputRows()
		if got < MinInputRows {
			t.Errorf("viewport %d: ceiling %d is below MinInputRows (%d) — the box must always show its "+
				"resting height", vp, got, MinInputRows)
		}
		// The rows the box spends on everything except its input: the shell's chrome (4),
		// the box's border (2), and its hint/notice/chip rows.
		fixed := 4 + 2 + m.HintRows() + m.NoticeRows()
		if m.Chip != "" {
			fixed++
		}
		if vp >= fixed+MinInputRows && got > vp-fixed {
			t.Errorf("viewport %d: ceiling %d would leave %d rows for the box's chrome (%d) and nothing for the "+
				"screen", vp, got, vp-got, fixed)
		}
		if vp < fixed+MinInputRows && got != MinInputRows {
			t.Errorf("viewport %d is too small for the box's chrome (%d) plus its resting height; the floor should "+
				"win and give %d rows, got %d", vp, fixed, MinInputRows, got)
		}
	}
}

// A DOCK BUILT WITHOUT A VIEWPORT BEHAVES AS IT ALWAYS DID, which is what keeps the
// unit tests and any embedder that constructs a dock directly on the old contract.
func TestAnUntoldViewportFallsBackToTheConstant(t *testing.T) {
	m := New()
	if m.viewportRows != 0 {
		t.Fatalf("fixture: a fresh dock has viewportRows = %d, want 0", m.viewportRows)
	}
	m.SetValue(strings.Repeat("line\n", 50))
	if got := m.InputRows(); got != MaxInputRows {
		t.Errorf("with no viewport the input grew to %d, want the fallback %d", got, MaxInputRows)
	}
}

// THE BUFFER ACTUALLY DRIVES IT, ABOVE THE OLD CAP. This is the operator's request stated
// as a test: past the eleven lines that used to be the ceiling, the box keeps growing
// until it runs out of viewport.
func TestALongBufferGrowsPastTheOldCap(t *testing.T) {
	m := New()
	m.SetViewportRows(40)
	m.Width = 80 // a width, so wrapping has something to wrap against

	m.SetValue("one line")
	atRest := m.InputRows()

	m.SetValue(strings.Repeat("line\n", 30))
	grown := m.InputRows()
	if grown <= MaxInputRows {
		t.Fatalf("30 lines grew the box to %d input rows, still at the old fixed cap %d", grown, MaxInputRows)
	}
	if grown <= atRest {
		t.Fatalf("the box did not grow: %d rows for 30 lines, %d at rest", grown, atRest)
	}
	if grown != m.maxInputRows() {
		t.Errorf("a buffer longer than the viewport should pin the box AT the ceiling (%d), got %d",
			m.maxInputRows(), grown)
	}
}

// EVERY ROW THE CEILING SPENDS IS A ROW THE SCREEN KEEPS.
//
// The ceiling exists so growth comes out of the space going spare rather than out of the
// transcript. Stated as the inequality: viewport - shell chrome - box chrome - the rows
// the box needs for its hint/notice/chip - input rows >= minContentRows. If this ever
// fails, a long prompt is silently pushing content off the top of the screen.
func TestTheCeilingLeavesTheScreenItsMinimum(t *testing.T) {
	for _, vp := range []int{20, 24, 40, 60} {
		for _, extra := range []struct {
			hint, notice int
			chip         bool
		}{
			{0, 0, false},
			{1, 0, false},
			{1, 1, true}, // the worst realistic case: hint + error strip + chip
			{2, 1, true}, // a hint that wrapped to two rows
		} {
			m := New()
			m.SetViewportRows(vp)
			m.Context = strings.Repeat("ctrl+g text · ", extra.hint*6)
			if extra.notice > 0 {
				m.Notice = "sending …"
			}
			if extra.chip {
				m.Chip = "Executions: exec-1"
			}
			m.SetValue(strings.Repeat("line\n", 40))

			input := m.InputRows()
			// Reconstruct the content region the shell computes: viewport minus its own
			// chrome (4) minus the dock's full row budget.
			content := vp - 4 - m.Lines()
			if content < minContentRows {
				t.Errorf("viewport %d (hint=%d rows, notice=%v, chip=%v): composer took %d input rows and left "+
					"the content region %d rows, below the minimum %d — a long prompt would push the "+
					"transcript off the top",
					vp, m.HintRows(), extra.notice > 0, extra.chip, input, content, minContentRows)
			}
		}
	}
}

// A RESIZE RE-FITS IMMEDIATELY, without waiting for the next keystroke: growing the
// window must let the box expand to the new ceiling, and shrinking it must pull the box
// back in rather than leaving it overflowing.
func TestAResizeRefitsTheBoxAtOnce(t *testing.T) {
	m := New()
	m.Width = 80
	m.SetValue(strings.Repeat("line\n", 40))

	m.SetViewportRows(60)
	tall := m.InputRows()
	m.SetViewportRows(22)
	short := m.InputRows()

	if short >= tall {
		t.Fatalf("shrinking the viewport 60 -> 22 did not pull the box in: %d rows then %d", tall, short)
	}
	if m.ta.Height() != short {
		t.Errorf("the textarea is %d rows tall but InputRows reports %d — a resize must re-fit it at once, not "+
			"on the next keystroke", m.ta.Height(), short)
	}
}
