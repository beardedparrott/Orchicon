package chat

import (
	"strings"
	"testing"
)

func TestRenderItemsShapes(t *testing.T) {
	items := []ChatItem{
		{Kind: KindUser, Text: "hello there friend", Key: "u1"},
		{Kind: KindText, Text: "hi! doing the thing", Key: "t1"},
		{Kind: KindReasoning, Text: "pondering", Key: "r1"},
		{Kind: KindTool, Tool: &ParsedTool{ID: "1", ToolName: "bash", Input: "ls", Output: "files"}, Key: "t2"},
		{Kind: KindArtifact, Name: "main.go", Type: "file", Content: "package main", Key: "a1"},
		{Kind: KindError, Text: "boom", Key: "e1"},
		{Kind: KindSession, SessionID: "ses_1", ServeURL: "http://x", Key: "s1"},
	}
	out := RenderItems(items, 80)
	// Chat bubbles are shaded and aligned (user right, model left) rather than
	// label-prefixed, so assert the BODY text plus the structural shapes.
	for _, want := range []string{"hello there friend", "hi! doing the thing", "thinking", "⚙ bash", "⬒ main.go", "error", "boom", "session ses_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

// The operator asked for the GUI's chat shape: the user's message on the
// RIGHT, the model's on the LEFT, each in a shaded bubble.
func TestUserBubbleRightAlignedModelLeftAligned(t *testing.T) {
	out := RenderItems([]ChatItem{
		{Kind: KindUser, Text: "mine", Key: "u1"},
		{Kind: KindText, Text: "theirs", Key: "t1"},
	}, 60)
	var user, model string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "mine") {
			user = l
		}
		if strings.Contains(l, "theirs") {
			model = l
		}
	}
	if user == "" || model == "" {
		t.Fatalf("both bubbles must render:\n%s", out)
	}
	// Measure the TEXT's position, not the line's first glyph: the operator's
	// band now carries a left-edge speaker label, so the first non-space column
	// is the label rather than the message.
	userAt, modelAt := strings.Index(user, "mine"), strings.Index(model, "theirs")
	if userAt <= modelAt {
		t.Fatalf("the operator's text must sit further right than the model's (user %d, model %d):\n%s", userAt, modelAt, out)
	}
	if userAt == 0 {
		t.Fatalf("user bubble is not right-aligned:\n%q", user)
	}
	// The speaker label rides the operator's band and not the model's.
	if !strings.Contains(user, userBandLabel) {
		t.Fatalf("the operator's band must be labelled %q:\n%q", userBandLabel, user)
	}
	if strings.Contains(model, userBandLabel) {
		t.Fatalf("the model's band must not carry the operator's label:\n%q", model)
	}
}

func TestRenderItemsWrapClampsWidth(t *testing.T) {
	long := strings.Repeat("word ", 60)
	out := RenderItems([]ChatItem{{Kind: KindText, Text: long}}, 40)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len([]rune(line)) > 45 { // label prefix + slack for ANSI-free check
			t.Fatalf("line too wide (%d): %q", len([]rune(line)), line)
		}
	}
}

func TestRenderEmptyItems(t *testing.T) {
	if got := RenderItems(nil, 80); got != "" {
		t.Fatalf("got %q", got)
	}
}

// The operator's ask: each message is a FULL-WIDTH background band (user
// lighter, model darker) that runs from the left edge of the pane to its right
// edge — not a tint on the glyphs. Every rendered line of a message must
// therefore fill the whole pane.
func TestChatMessagesRenderFullWidthBands(t *testing.T) {
	const pane = 60
	out := RenderItems([]ChatItem{
		{Kind: KindUser, Text: "mine", Key: "u1"},
		{Kind: KindText, Text: "theirs", Key: "t1"},
	}, pane)

	var userLine, modelLine string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.Contains(l, "mine"):
			userLine = l
		case strings.Contains(l, "theirs"):
			modelLine = l
		}
	}
	if userLine == "" || modelLine == "" {
		t.Fatalf("both bands must render:\n%q", out)
	}
	// FULL width: the band spans the whole pane, not just the text.
	for name, l := range map[string]string{"user": userLine, "model": modelLine} {
		if n := len([]rune(l)); n != pane {
			t.Fatalf("%s band is %d cells wide, want the full pane (%d): %q", name, n, pane, l)
		}
	}
	// The operator's TEXT sits at the right of its band, the model's at the left.
	// Measured on the text itself: the operator's band carries a left-edge speaker
	// label, so the line's first non-space column is the label, not the message.
	ui := strings.Index(userLine, "mine")
	mi := strings.Index(modelLine, "theirs")
	if ui <= mi {
		t.Fatalf("the operator's text must sit further right (user %d, model %d)\n%q", ui, mi, out)
	}

	// And the bands must be SEPARATED: blank rows between them, so the two
	// speakers read as distinct blocks rather than one continuous fill
	// (the operator's "there should be a visible gap between user messages and
	// model messages").
	var between int
	seenUser := false
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.Contains(l, "mine") {
			seenUser = true
			continue
		}
		if !seenUser {
			continue
		}
		if strings.TrimSpace(l) == "" {
			between++
		}
	}
	if between < chatBandGap {
		t.Fatalf("only %d blank rows between the bands, want at least %d\n%q", between, chatBandGap, out)
	}
}
