package ask

// modal_frame_test.go — the /grants and /permissions surfaces are STANDARD MODALS: a bordered box, centered,
// over a darkened backdrop, inside a frame of exactly w×h.
//
// The operator, looking at /grants: "it puts it in the middle of screen and it's hard to see because it writes
// it overtop text. Maybe a standard darkened modal in the center would be better like other screens in
// Orchicon?" The box was already centered and its rows already opaque; what was missing was (a) the border that
// makes it read as a modal at all, and (b) any recessing of the frame behind it, so the transcript stayed at
// full brightness on either side and competed with the list.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// modalTrueColor forces a colour-capable profile for one test: without it lipgloss degrades to plain text and
// the recessing is unobservable (the same reason internal/tui/theme's opaque suite does this).
func modalTrueColor(t *testing.T) {
	t.Helper()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
}

func fixtureGrants(t *testing.T) (*Model, *stubHost) {
	t.Helper()
	m, h := newTestModel(t)
	h.grantAvail = true
	h.grantLoaded = true
	now := time.Now().Unix()
	h.grants = []chat.SessionGrant{
		{Directory: "/home/ops/project", GrantedAt: now - 60},
		{Directory: "/home/ops/other", GrantedAt: now - 3600},
	}
	return m, h
}

func frameRows(t *testing.T, m *Model) []string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")
	if len(rows) != m.h {
		t.Fatalf("frame has %d rows, want exactly %d — an overlay must never change the row count", len(rows), m.h)
	}
	for i, r := range rows {
		if w := len([]rune(ansi.Strip(r))); w != m.w {
			t.Fatalf("row %d is %d cells, want exactly %d — an overlay must never change the frame width", i, w, m.w)
		}
	}
	return rows
}

// TestGrantsOverlayIsABorderedCenteredModal pins the shape the operator asked for: the list is inside a border
// (so it reads as a modal, like every other Orchicon modal), and that border is centered in the frame.
func TestGrantsOverlayIsABorderedCenteredModal(t *testing.T) {
	m, _ := fixtureGrants(t)
	m.openGrants()
	rows := frameRows(t, m)

	// Locate the box by its first content row, then find the border it must sit inside.
	title := -1
	for i, r := range rows {
		if strings.Contains(ansi.Strip(r), "Session grants") {
			title = i
			break
		}
	}
	if title < 0 {
		t.Fatalf("the grants modal is not on screen:\n%s", ansi.Strip(strings.Join(rows, "\n")))
	}
	// THE BORDER: the row directly above the list opens a box, and a matching row closes it.
	top := title - 1
	if top < 0 || !strings.HasPrefix(strings.TrimSpace(ansi.Strip(rows[top])), "┌") {
		t.Fatalf("the list must be inside a modal border; row %d is %q", top, ansi.Strip(rows[top]))
	}
	bottom := -1
	for i := title; i < len(rows); i++ {
		if strings.HasPrefix(strings.TrimSpace(ansi.Strip(rows[i])), "└") {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		t.Fatal("the modal's border is never closed")
	}
	// The list's own rows are INSIDE the border, not spilling past it.
	if first := ansi.Strip(rows[title]); !strings.Contains(first, "Session grants") {
		t.Fatalf("row %d should be the list's first row: %q", title, first)
	}

	// CENTERED: against the frame it is drawn into.
	boxH := bottom - top + 1
	wantTop := (len(rows) - boxH) / 2
	if d := top - wantTop; d < -1 || d > 1 {
		t.Fatalf("the modal is not centered: its border starts at row %d, want %d (a %d-row box in %d rows)",
			top, wantTop, boxH, len(rows))
	}
	// And the operator's own content is still legible inside it.
	if !strings.Contains(ansi.Strip(strings.Join(rows, "\n")), "/home/ops/project") {
		t.Fatal("the modal must show the grants it lists")
	}
}

// TestOverlayDimsTheBackdropWithoutChangingTheFrame pins the recessing: closing the overlay restores the frame's
// own styling, and the difference is styling ONLY — the content behind the modal is still there (dimmed), and
// the frame stays exactly w×h either way.
func TestOverlayDimsTheBackdropWithoutChangingTheFrame(t *testing.T) {
	modalTrueColor(t)
	m, _ := fixtureGrants(t)

	// The frame as the operator sees it with no modal up.
	bright := frameRows(t, m)

	m.openGrants()
	dimmed := frameRows(t, m)

	// A row ABOVE the modal is backdrop: it must be recessed, i.e. the same content in different styling.
	// (Row 0 is the pane's own top border — always present, and never inside the modal.)
	if strip(dimmed[0]) != strip(bright[0]) {
		t.Fatalf("the backdrop's CONTENT changed:\n  with modal: %q\n  without:    %q", strip(dimmed[0]), strip(bright[0]))
	}
	if dimmed[0] == bright[0] {
		t.Fatal("the backdrop was not recessed — the modal still competes with the frame behind it")
	}
}

func strip(s string) string { return ansi.Strip(s) }
