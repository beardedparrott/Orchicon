package screenkit

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestDetailBodyRendersMarkdownForWorkItems is the work-item surface: a description and acceptance
// criteria are AUTHORED MARKDOWN, and the GUI renders them through markdown.tsx. The TUI showed the
// source, so a quoted operator requirement read as "> ..." — punctuation noise that also loses the
// attribution the marker encodes.
func TestDetailBodyRendersMarkdownForWorkItems(t *testing.T) {
	body := strings.Join([]string{
		"# Feature: Composer 2.0",
		"",
		"## Operator requirement (verbatim intent)",
		"",
		"> \"the box at the bottom that you type for chats needs to be larger\"",
		"",
		"- no border today",
		"- no padding today",
	}, "\n")

	var d Detail
	d.Width, d.Height = 80, 24
	d.SetContent("Work Item: x", nil, body)
	got := ansi.Strip(d.View())

	if strings.Contains(got, "> ") {
		t.Fatalf("a raw blockquote marker reached the detail pane:\n%s", got)
	}
	if !strings.Contains(got, "│") {
		t.Fatalf("the quote is not drawn as a block:\n%s", got)
	}
	if !strings.Contains(got, "the box at the bottom that you type for chats needs to be larger") {
		t.Fatalf("the quoted words must survive verbatim:\n%s", got)
	}
	if strings.Contains(got, "##") {
		t.Fatalf("a heading marker was printed literally:\n%s", got)
	}
	if !strings.Contains(got, "• no border today") {
		t.Fatalf("the list was not rendered:\n%s", got)
	}
}

// TestNonMarkdownBodyIsPassedThroughUntouched is the safety property of rendering in this ONE pane:
// it serves every list+detail screen, and several of them put a log dump, a trace, a JSON blob or a
// pre-composed key/value block in the body. Rendering those through markdown would CONSUME characters
// (emphasis and link markers vanish) — and a log line is evidence.
func TestNonMarkdownBodyIsPassedThroughUntouched(t *testing.T) {
	bodies := []string{
		`{"level":"info","msg":"started","args":["a","b"]}`,
		"2026-09-16T18:25:53Z INFO listening on :8080",
		"status: running\nworker: Quick Software Engineer\nsteps: 3",
		"a plain sentence with no markup",
	}
	for _, body := range bodies {
		var d Detail
		d.Width, d.Height = 80, 24
		d.SetContent("Trace", nil, body)
		got := ansi.Strip(d.View())
		// Per LINE, right-trimmed: the viewport pads every row to the pane width, so comparing the
		// whole block as one substring would fail on trailing padding rather than on any alteration.
		for _, want := range strings.Split(body, "\n") {
			if !strings.Contains(got, want) {
				t.Errorf("a non-markdown body was altered:\n line %q\nmissing from %q", want, got)
			}
		}
	}
}

// TestDetailBodyIsLaidOutToThePane: this pane never wrapped — it handed the raw string to a viewport,
// which does not reflow, so a long description was CUT at the pane's edge with nothing to scroll
// horizontally. Rendering lays every line to the width.
func TestDetailBodyIsLaidOutToThePane(t *testing.T) {
	body := "> " + strings.Repeat("quoted words ", 20) + "\n\n| a | b |\n|---|---|\n| " + strings.Repeat("x", 60) + " | y |"
	for _, w := range []int{40, 60, 100} {
		var d Detail
		d.Width, d.Height = w, 24
		d.SetContent("Work Item: long", nil, body)
		for _, l := range strings.Split(ansi.Strip(d.View()), "\n") {
			if got := len([]rune(l)); got > w {
				t.Fatalf("width %d: detail row is %d cells: %q", w, got, l)
			}
		}
	}
}

// TestDetailReRendersWhenTheWidthChanges: markdown is laid out at a CONCRETE width, so a resize must
// invalidate the rendered lines even though the SOURCE is unchanged — otherwise the pane keeps the
// previous width's line breaks, which are either too short or (worse) too wide for the new pane.
//
// The RENDERED CONTENT is measured, not the number of rows View() prints: the viewport clips to its
// height, so counting visible rows would report 17 either way and the assertion would be vacuous.
func TestDetailReRendersWhenTheWidthChanges(t *testing.T) {
	body := "> " + strings.Repeat("word ", 30)

	rows := func(w int) int {
		var d Detail
		d.Width, d.Height = w, 20
		d.SetContent("Work Item", nil, body)
		d.ensureVP()
		return len(strings.Split(d.bodyContent(), "\n"))
	}
	narrow, wide := rows(30), rows(100)
	if wide >= narrow {
		t.Fatalf("the same body at a wider pane must occupy FEWER lines (narrow=%d wide=%d) — the layout is not width-aware",
			narrow, wide)
	}

	// And RESIZING an existing pane must re-render in place, not keep the old break points.
	var d Detail
	d.Width, d.Height = 100, 20
	d.SetContent("Work Item", nil, body)
	d.View() // paints once, caching the layout at 100
	before := len(strings.Split(d.bodyContent(), "\n"))

	d.Width = 30
	d.View() // the resize must invalidate the cached layout
	after := len(strings.Split(d.bodyContent(), "\n"))
	if after <= before {
		t.Fatalf("shrinking the pane did not re-render the markdown body (before=%d after=%d)", before, after)
	}
}
