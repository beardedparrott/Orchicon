package screenkit

// detail_laid_out_test.go — A BODY THE CALLER ALREADY LAID OUT IS NOT RE-RENDERED AS MARKDOWN.
//
// The operator, on the Execution pane: "Executions have somehow lost markup or is slightly broken. You can
// see in the screenshot that todo is scrunched and has no newlines to view it properly, and tools and chats
// are no longer being collapsed like they were previously."
//
// ONE CAUSE, AND IT WAS THE MARKDOWN PASS. The pane serves every list+detail screen from one body slot, and
// it decided whether to render markdown by asking `md.LooksLikeMarkdown(body)` — a CONTENT heuristic. That
// works for a body which is wholly markdown source and for one which has no markers at all, and it fails on
// the body in between, which is what a live execution is: a laid-out todo list and a transcript of
// collapsed blocks, with a single `**success**` in the worker's summary making the WHOLE body look like
// markdown.
//
// Markdown JOINS consecutive non-blank lines into one paragraph, so the layout came out as one scrunched
// line. MEASURED, before the fix, on the operator's own shape:
//
//	todo (6/6 done) [x] Read run .orchicon files + verify repo/branch state (!) [x] Delete leftover …
//
// The collapsible blocks went the same way — they were still BUILT (the block model was untouched), and then
// flattened by the renderer, which is why "tools and chats are no longer being collapsed".
//
// THE FIX IS A DECLARATION, NOT A BETTER HEURISTIC. No test on the body's CONTENT can separate "markdown
// source" from "already laid out", because the difference is not in the text — it is in who produced it. So
// the caller says so (SetContentLaidOut), and the pane returns that body verbatim.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// executionBody is the operator's shape: a laid-out todo list and a transcript, with ONE markdown marker
// far down in the worker's summary. That marker is what made the heuristic fire.
const executionBody = "todo (2/2 done)\n" +
	"  [x] Read run .orchicon files + verify repo/branch state  (!)\n" +
	"  [x] Create PR from run branch into develop  (!)\n" +
	"\n" +
	"output\n" +
	"ORCHICON WORKER SUMMARY: **success** — DevOps step complete\n" +
	"\n" +
	"▸ tool bash\n" +
	"▸ tool batch_read\n" +
	"▾ reasoning · 400 chars\n" +
	"  weighing the options\n"

// countRows returns how many rendered rows contain want. A laid-out body keeps one row per line; a
// markdown-rendered one merges them, so the count collapses.
func countRows(view, want string) int {
	n := 0
	for _, ln := range strings.Split(view, "\n") {
		if strings.Contains(ln, want) {
			n++
		}
	}
	return n
}

// THE DECLARED-LAID-OUT BODY KEEPS ITS NEWLINES — the operator's report, at the pane.
func TestLaidOutBodyKeepsItsNewlines(t *testing.T) {
	var d Detail
	d.Width, d.Height = 100, 40
	d.SetContentLaidOut("Execution x", nil, executionBody)
	got := ansi.Strip(d.View())

	// Each todo row is its own row.
	if n := countRows(got, "[x]"); n != 2 {
		t.Errorf("the two todo items rendered on %d rows, want 2 — markdown joined them into one "+
			"paragraph:\n%s", n, got)
	}
	// The summarised sections and the collapsed blocks each keep their own row.
	for _, want := range []string{"todo (2/2 done)", "output", "ORCHICON WORKER SUMMARY", "▸ tool bash", "▸ tool batch_read", "▾ reasoning"} {
		if n := countRows(got, want); n != 1 {
			t.Errorf("%q rendered on %d rows, want 1:\n%s", want, n, got)
		}
	}
	// And the markdown marker is NOT interpreted on a laid-out body: this text was authored by the worker,
	// already styled, and is not the pane's to rewrite.
	if strings.Contains(got, "\x1b[1msuccess") {
		t.Errorf("a laid-out body was still run through the markdown pass (bold SGR applied):\n%q", got)
	}
}

// THE FLAG IS LOAD-BEARING: the SAME body handed to the markdown path IS flattened, so this is not a
// no-op assertion that would pass either way.
func TestTheSameBodyIsFlattenedWhenNotDeclaredLaidOut(t *testing.T) {
	var rendered Detail
	rendered.Width, rendered.Height = 100, 40
	rendered.SetContent("Execution x", nil, executionBody)
	markdownView := ansi.Strip(rendered.View())

	var laidOut Detail
	laidOut.Width, laidOut.Height = 100, 40
	laidOut.SetContentLaidOut("Execution x", nil, executionBody)
	laidOutView := ansi.Strip(laidOut.View())

	if countRows(markdownView, "[x]") >= countRows(laidOutView, "[x]") {
		t.Fatalf("rendering the body as markdown did NOT merge its rows, so this test cannot tell the two "+
			"paths apart — the markdown heuristic must have stopped firing, and this test needs revisiting.\n"+
			"markdown rows with [x]: %d\nlaid-out rows with [x]: %d",
			countRows(markdownView, "[x]"), countRows(laidOutView, "[x]"))
	}
}

// AND MARKDOWN SOURCE IS STILL RENDERED — the fix must not disable the feature the markdown commit added.
// A work item's description is authored markdown: its blockquote is attribution and its headings are
// headings, and the operator asked for both.
func TestMarkdownSourceIsStillRendered(t *testing.T) {
	body := strings.Join([]string{
		"# Feature: Composer 2.0",
		"",
		"> \"the box at the bottom that you type for chats needs to be larger\"",
		"",
		"- no border today",
	}, "\n")

	var d Detail
	d.Width, d.Height = 80, 24
	d.SetContent("Work Item: x", nil, body)
	got := ansi.Strip(d.View())

	if strings.Contains(got, "> ") {
		t.Errorf("a raw blockquote marker reached the pane:\n%s", got)
	}
	if !strings.Contains(got, "│") {
		t.Errorf("the quote is not drawn as a block:\n%s", got)
	}
	if !strings.Contains(got, "• no border today") {
		t.Errorf("the list was not rendered:\n%s", got)
	}
}

// A DECLARATION DOES NOT STICK ACROSS CALLS. The pane is shared, so a screen that sets a laid-out body and
// then a markdown one (or the reverse) must get the treatment the LATEST call asked for — otherwise the
// first caller's choice would silently govern every later body.
//
// Asserted through View() rather than bodyContent(), because bodyContent depends on the viewport's width and
// the viewport is sized during a render: calling it first would test an un-initialised pane rather than the
// flag.
func TestTheDeclarationIsPerCall(t *testing.T) {
	var d Detail
	d.Width, d.Height = 80, 24

	// A laid-out body first: verbatim, so its newlines survive.
	d.SetContentLaidOut("Execution x", nil, executionBody)
	got := ansi.Strip(d.View())
	if n := countRows(got, "[x]"); n != 2 {
		t.Fatalf("fixture: the laid-out body did not keep its rows (%d rows with [x]):\n%s", n, got)
	}

	// Now a MARKDOWN body: the previous declaration must not carry over, or the raw markers would print.
	md := "# Heading\n\n> quoted\n"
	d.SetContent("Work Item: y", nil, md)
	got = ansi.Strip(d.View())
	if strings.Contains(got, "> ") {
		t.Errorf("a markdown body after a laid-out one was passed through verbatim — the flag stuck:\n%s", got)
	}
	if !strings.Contains(got, "│") {
		t.Errorf("the blockquote is not drawn as a block after a laid-out body:\n%s", got)
	}
}
