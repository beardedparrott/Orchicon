package md

// md_userbreak_test.go — A NEWLINE THE OPERATOR TYPED IS A LINE BREAK.
//
// The operator, on their own messages in both clients:
//
//	"I would like the user sent message to be formatted properly on the screen. Right now it's all just a
//	 bunch of text bunched up. It should respect the format that it was typed in, including newlines, bullets,
//	 numbered lists, etc."
//
// BULLETS AND NUMBERED LISTS ALREADY RENDERED — md.list has drawn them since the renderer was written. The gap
// was the NEWLINE: CommonMark (and therefore react-markdown, so the clients agree by construction) makes a
// single newline INSIDE a paragraph a SPACE, so a message typed across several lines arrived as one flowing
// block and was then re-wrapped to the pane's width.
//
// The two rules these pin are deliberately ASYMMETRIC — the operator's newlines are kept, the model's are
// collapsed — because the two texts are wrapped by different things. The model's prose is machine-wrapped and
// joining its lines is what lets it reflow to any width; the operator's is hand-wrapped and the line they broke
// is the line they meant.
//
// The ANSI stripping reuses this package's own strip/plain helpers (md_test.go) rather than a second copy, so
// there is one definition of "the text the operator reads".

import (
	"strings"
	"testing"
)

// read returns one rendered line as PLAIN trimmed text — the assertion is about what the operator reads, not
// about whichever attribute codes a surface happened to add.
func read(line string) string { return strings.TrimSpace(strip(line)) }

func TestATypedNewlineStaysALineInUserText(t *testing.T) {
	lines, _ := RenderUserOnSpans("first line\nsecond line", 40, Surface{})
	if len(lines) != 2 {
		t.Fatalf("a typed newline produced %d line(s), want 2 — the operator's line break was collapsed: %q",
			len(lines), lines)
	}
	if got := read(lines[0]); got != "first line" {
		t.Errorf("line 1 = %q, want \"first line\"", got)
	}
	if got := read(lines[1]); got != "second line" {
		t.Errorf("line 2 = %q, want \"second line\"", got)
	}
}

// EVERY line of a longer, hand-wrapped message survives — the case that produced "a bunch of text bunched up".
func TestALongHandWrappedMessageKeepsAllItsLines(t *testing.T) {
	src := "Bug report:\n\n- the rail shows 1-0/0\n- the counter never moves\n\nPlease fix."
	lines, _ := RenderUserOnSpans(src, 60, Surface{})

	for _, want := range []string{"Bug report:", "- the rail shows 1-0/0", "- the counter never moves",
		"Please fix."} {
		found := false
		for _, l := range lines {
			if strings.Contains(read(l), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is missing from the render — a line the operator typed was lost: %q", want, lines)
		}
	}
}

// AND THE MODEL'S PROSE STILL REFLOWS, which is the half that must NOT change: its text is machine-wrapped, and
// a newline kept there would freeze today's wrapping into every future pane width.
func TestAModelNewlineIsStillCollapsedToASpace(t *testing.T) {
	lines, _ := RenderOnSpans("first line\nsecond line", 40, Surface{})
	if len(lines) != 1 {
		t.Fatalf("the model's soft break produced %d lines, want 1: its prose must reflow to the pane — %q",
			len(lines), lines)
	}
	if got := read(lines[0]); got != "first line second line" {
		t.Errorf("the model's line = %q, want the two lines joined by a space", got)
	}
}

// THE OPTION IS SCOPED TO ITS RENDER AND CANNOT LEAK. md is a leaf several callers share and renders are
// serialized by renderMu, so a flag set for one render and not cleared would silently re-wrap the NEXT
// surface's text — the assistant transcript, a work-item description, an artifact preview — depending on call
// order.
func TestTheUserBreakOptionDoesNotLeakIntoTheNextRender(t *testing.T) {
	if user, _ := RenderUserOnSpans("a\nb", 40, Surface{}); len(user) != 2 {
		t.Fatalf("fixture: the user render produced %d lines, want 2", len(user))
	}
	lines, _ := RenderOnSpans("a\nb", 40, Surface{})
	if len(lines) != 1 {
		t.Errorf("a render AFTER a user render produced %d lines, want 1 — the user-break flag leaked out of "+
			"its render and would re-wrap every later surface: %q", len(lines), lines)
	}
}

// LISTS WERE NEVER THE PROBLEM, and the change must not disturb them. "- one / - two / - three" is already
// three list items — the parser handles it — so both modes must render three lines. This pins that the newline
// fix was scoped to PROSE and did not double-break structure that already worked.
func TestListsRenderIdenticallyInBothModes(t *testing.T) {
	src := "- one\n- two\n- three"
	user, _ := RenderUserOnSpans(src, 40, Surface{})
	model, _ := RenderOnSpans(src, 40, Surface{})
	if len(user) != 3 {
		t.Errorf("the user view rendered %d list lines, want 3: %q", len(user), user)
	}
	if len(model) != 3 {
		t.Errorf("the model view rendered %d list lines, want 3: %q", len(model), model)
	}
}

// A FENCED BLOCK KEEPS ITS NEWLINES IN BOTH MODES — code is not prose, and breaking it differently by author
// would corrupt anything the operator pasted.
func TestCodeBlocksAreUnaffectedByTheUserBreakOption(t *testing.T) {
	src := "```\nline a\nline b\n```"
	user, _ := RenderUserOnSpans(src, 40, Surface{})
	model, _ := RenderOnSpans(src, 40, Surface{})
	for name, lines := range map[string][]string{"user": user, "model": model} {
		joined := strings.Join(plain(lines), "\n")
		if !strings.Contains(joined, "line a") || !strings.Contains(joined, "line b") {
			t.Errorf("%s: the code block lost a line: %q", name, lines)
		}
	}
}
