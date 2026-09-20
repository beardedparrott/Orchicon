package tui

// transcript_affordance_test.go — THE COPY MARKER, AND THE DUPLICATION IT SITS ON TOP OF.
//
// The operator, three reports in one message:
//
//	"if I interject on an ongoing message in conversations in the TUI, it duplicates my message"
//	"We should put a little icon on user messages and code blocks in conversations in the TUI indicating to
//	 people that they can be copied."
//
// The first turned out to be a merge bug MEASURED with a probe rather than reasoned about: the mid-turn poll
// merges the server's 250ms durable mirror of the running reply, and the live stream chunks were keyed in a
// different namespace (`st-*` vs `m-*`), so the identity rule could never match them and the reply rendered
// TWICE on every mid-turn poll. The interjection made it obvious because the superseded turn's close made the
// shell fall into the REPLACE path mid-turn.
//
// The second is the affordance: a gesture nobody can see is a gesture nobody has.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// --- the duplication -----------------------------------------------------------------------------------

// A MID-TURN MERGE MUST NOT RENDER THE REPLY TWICE. This is the measured bug, at the level it lives: the
// durable mirror already carries the text a live chunk also carries.
func TestMidTurnMergeDoesNotDuplicateTheLiveHalfOfTheReply(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	// The operator sends; turn 1 streams a chunk.
	s.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "A", Key: "draft-1", Live: true})
	s.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "reply1", Key: "st-1", Live: true})

	// The mid-turn poll brings the server's mirror of the SAME reply.
	s.mergeHistory("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: "A", Key: "m-1"},
		{Kind: chat.KindText, Text: "reply1", Key: "m-2"},
	})

	texts := 0
	for _, it := range s.snapshot("c1") {
		if it.Kind == chat.KindText && it.Text == "reply1" {
			texts++
		}
	}
	if texts != 1 {
		t.Fatalf("the reply rendered %d times after a mid-turn merge, want 1: %+v", texts, s.snapshot("c1"))
	}
	// AND THE USER MESSAGE IS STILL SINGLE — the older dedupe rule must keep working alongside the new one.
	users := 0
	for _, it := range s.snapshot("c1") {
		if it.Kind == chat.KindUser {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("the user message rendered %d times, want 1: %+v", users, s.snapshot("c1"))
	}
}

// AND TEXT THE MIRROR HAS NOT CAUGHT UP WITH IS KEPT. The mirror is written per part and can lag by up to a
// poll interval, so dropping every live chunk once a mirror exists would blank the newest text on screen.
func TestMidTurnMergeKeepsTextTheMirrorHasNotCaughtUpWith(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	s.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "hello ", Key: "st-1", Live: true})
	s.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "world", Key: "st-2", Live: true})

	// The mirror has only the first half.
	s.mergeHistory("c1", []chat.ChatItem{{Kind: chat.KindText, Text: "hello ", Key: "m-2"}})

	items := s.snapshot("c1")
	var out string
	for _, it := range items {
		if it.Kind == chat.KindText {
			out += it.Text
		}
	}
	if !strings.Contains(out, "world") {
		t.Errorf("the text the mirror had not written yet was dropped, so the reply would visibly rewind on "+
			"every poll: %q (items: %+v)", out, items)
	}
}

// --- the affordance ------------------------------------------------------------------------------------

// THE OPERATOR'S OWN MESSAGE SAYS IT CAN BE COPIED. The marker rides the band label the message already has.
func TestTheUserMessageCarriesTheCopyAffordance(t *testing.T) {
	body, _ := chat.RenderItemsSpansWithCopy([]chat.ChatItem{
		{Kind: chat.KindUser, Text: "hello there", Key: "u1"},
	}, 60, chat.CopyGlyph, nil)

	if !strings.Contains(body, chat.CopyGlyph) {
		t.Fatalf("the operator's message carries no copy marker: %q", body)
	}
	if !strings.Contains(body, "You") {
		t.Errorf("the marker displaced the label rather than joining it: %q", body)
	}
}

// AND A CODE BLOCK SAYS IT TOO, on its label row — which is also the row that is clickable, so the marker sits
// on the thing it describes.
func TestACodeBlockCarriesTheCopyAffordance(t *testing.T) {
	body, spans := chat.RenderItemsSpansWithCopy([]chat.ChatItem{
		{Kind: chat.KindText, Text: "run this:\n\n```bash\nls -la\n```", Key: "t1"},
	}, 60, chat.CopyGlyph, nil)

	if !strings.Contains(body, chat.CopyGlyph) {
		t.Fatalf("the code block carries no copy marker: %q", body)
	}
	if !strings.Contains(body, "bash") {
		t.Errorf("the marker displaced the language label: %q", body)
	}
	if len(spans) == 0 || len(spans[0].Code) == 0 {
		t.Fatal("fixture: the block did not register a code span, so the marker cannot be on its label row")
	}
}

// A BLOCK WITHOUT A LANGUAGE STILL SAYS IT. The marker is what tells the operator the block is clickable, so
// it must not depend on the fence having an info string.
func TestAUnlabelledCodeBlockStillCarriesTheAffordance(t *testing.T) {
	body, _ := chat.RenderItemsSpansWithCopy([]chat.ChatItem{
		{Kind: chat.KindText, Text: "run this:\n\n```\nls -la\n```", Key: "t1"},
	}, 60, chat.CopyGlyph, nil)

	if !strings.Contains(body, chat.CopyGlyph) {
		t.Errorf("an unlabelled fence shows no copy marker, so the operator cannot tell it is clickable: %q", body)
	}
}

// THE AFFORDANCE IS ABSENT WHERE THE GESTURE IS: the plain render draws no marker at all, which is what the
// slide-out strip uses — a click there does nothing, so promising a copy would be a lie.
func TestThePlainTranscriptRenderDrawsNoAffordance(t *testing.T) {
	items := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "hello", Key: "u1"},
		{Kind: chat.KindText, Text: "```bash\nls -la\n```", Key: "t1"},
	}
	withCopy, _ := chat.RenderItemsSpansWithCopy(items, 60, chat.CopyGlyph, nil)
	plain, _ := chat.RenderItemsSpans(items, 60)

	if !strings.Contains(withCopy, chat.CopyGlyph) {
		t.Fatal("fixture: the copy render produced no marker, so this test would pass vacuously")
	}
	if strings.Contains(plain, chat.CopyGlyph) {
		t.Errorf("the plain render drew a copy marker, promising a gesture that surface does not have: %q", plain)
	}
}

// AND THE MARKER IS NOT PART OF THE COPY. A code block's clipboard payload is the fence's SOURCE, so the glyph
// on the label row must never reach it — the operator's earlier report was exactly this class of pollution.
func TestTheCopyMarkerIsNotInTheCopiedSource(t *testing.T) {
	m := blockPlane(t)
	row := blockRow(t, m)
	got, ok := m.transcriptCodeBlockAtFrameRow(row)
	if !ok {
		t.Fatal("fixture: clicking the block resolved to nothing")
	}
	if strings.Contains(got, chat.CopyGlyph) {
		t.Errorf("the copy marker leaked into the copied code: %q", got)
	}
}
