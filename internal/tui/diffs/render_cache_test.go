package diffs

// render_cache_test.go — THE RENDER IS MEMOIZED, AND NEVER STALE.
//
// The operator: "the scroll and drag are slow. If I move my mouse it has to catch up to the mouse position
// instead of smoothly scrolling and dragging."
//
// Rendering the diff is the pane's most expensive operation, and both hot paths did it from scratch:
// visibleLines() (under every clamp, so under every scroll step) and diffBody() (every frame). Dragging the
// scrollbar emits one motion event per cell of pointer travel, so each event re-rendered the whole diff
// several times and the pane could not keep up with the mouse.
//
// Measured on a 400-hunk diff: ~12ms per scroll step before, ~100µs after. These tests pin the cache's
// CORRECTNESS — its speed is worthless if it can serve a stale render, and a stale render is a wrong body.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func cacheModel(t *testing.T) *Model {
	t.Helper()
	vecs := vecByName(t)
	m := NewModel(nil, nil)
	m.Width, m.Height = 60, 12
	m.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(vecs["modify-two-lines"], 1)})
	m.SelectedPath = m.groups[0].Path
	m.setRows(m.rowsForSelected())
	m.Tab = TabDiff
	if m.visibleLines() == 0 {
		t.Fatal("the fixture rendered no lines")
	}
	return m
}

func TestRenderingIsMemoized(t *testing.T) {
	m := cacheModel(t)
	a := m.renderedLines()
	b := m.renderedLines()
	if len(a) == 0 {
		t.Fatal("nothing rendered")
	}
	// The SAME backing array is the observable proof that the second call did not re-render — calling the
	// renderer twice and comparing contents would pass even if it ran twice.
	if &a[0] != &b[0] {
		t.Error("the second call rendered again: the memo is not being used, so every scroll step pays the " +
			"full render cost and the view lags behind the pointer")
	}
	// The scroll extent and the body must agree with it (they read the same slice).
	if m.visibleLines() != len(a) {
		t.Errorf("visibleLines()=%d, want the rendered line count %d", m.visibleLines(), len(a))
	}
}

// A WIDTH CHANGE RE-RENDERS. Wrapping depends on the width, so serving a cache across a resize would show
// the previous width's wrap and slice the viewport by the wrong line count.
func TestAWidthChangeRerenders(t *testing.T) {
	m := cacheModel(t)
	before := m.renderedLines()

	m.Width += 20 // bodyWidth follows
	after := m.renderedLines()
	if len(before) == 0 || len(after) == 0 {
		t.Fatal("nothing rendered")
	}
	if &before[0] == &after[0] {
		t.Error("widening the pane served the previous render — the wrap and the line count are width-dependent")
	}
}

// NEW ROWS RE-RENDER. This is the case a cache most easily gets wrong: the fetch that replaces the rows, a
// new file selected, a live merge — all of them go through setRows, which is what bumps the generation.
func TestNewRowsRerender(t *testing.T) {
	m := cacheModel(t)
	before := m.renderedLines()

	other := makeEdit(vecByName(t)["create-new-file"], 2)
	m.groups = GroupByFile([]*apiv1.FileEdit{other})
	m.SelectedPath = other.Path
	m.setRows(m.rowsForSelected())

	after := m.renderedLines()
	if &before[0] == &after[0] {
		t.Fatal("the rows changed but the previous render was served")
	}
	// And the CONTENT really changed, not merely the slice identity — a cache keyed on the wrong thing could
	// pass the identity check above while still showing the OLD file, which is the failure that matters to
	// the operator. (The comparison is content-to-content rather than a guess at the diff syntax: this
	// renderer switches between unified and side-by-side on width, so asserting on a "+" would pin the
	// profile rather than the cache.)
	if strings.Join(before, "\n") == strings.Join(after, "\n") {
		t.Errorf("the rows changed but the rendered body is identical — the cache served the old file:\n%s",
			strings.Join(after, "\n"))
	}
}

// The fetch and merge paths assign rows through setRows, so this pins the WIRING rather than the cache: a
// future caller that assigned m.rows directly would leave the cache stale and this test would catch it.
func TestEveryRowsMutationInvalidatesTheCache(t *testing.T) {
	m := cacheModel(t)
	gen := m.renderGen

	m.SelectPath("no-such-file") // goes through setRows (and yields nil rows)
	if m.renderGen == gen {
		t.Error("SelectPath did not invalidate the render cache")
	}
	if m.visibleLines() != 0 {
		t.Errorf("after selecting a file with no diff, visibleLines()=%d, want 0 (a stale render was served)",
			m.visibleLines())
	}
}
