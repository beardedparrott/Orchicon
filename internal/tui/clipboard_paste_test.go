package tui

// clipboard_paste_test.go — ctrl+v SERVES BOTH THE IMAGE AND THE TEXT.
//
// The operator: "The new ctrl+v to paste screenshots broke the ability to simply ctrl+v paste normal text. It
// should detect it and allow both."
//
// The attachment work bound ctrl+v to the image reader alone, so a clipboard holding a sentence produced only
// "the clipboard holds no image" — and there was no OTHER way to paste text, because this chord is taken.
//
// THE FALLBACK IS THE HALF THAT MATTERS, so it is the half these tests drive. The readers shell out to a
// clipboard helper and cannot run in a test, so the decision takes them as parameters (clipboardPasteCmd) and
// the assertions are on which outcome each combination of reads produces.

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pasteWith runs the decision with stubbed readers and returns the message it produced.
func pasteWith(
	t *testing.T,
	img func([]attachment) (attachment, error),
	txt func() (string, error),
) tea.Msg {
	t.Helper()
	cmd := clipboardPasteCmd(img, txt)
	if cmd == nil {
		t.Fatal("clipboardPasteCmd produced no command")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("the clipboard command produced no message — ctrl+v would do nothing at all")
	}
	return msg
}

// An IMAGE on the clipboard ATTACHES, and the text reader is not consulted.
func TestClipboardPastePrefersAnImage(t *testing.T) {
	att := attachment{Name: "clip.png", MimeType: "image/png", Data: []byte("png")}
	consultedText := false
	msg := pasteWith(t,
		func([]attachment) (attachment, error) { return att, nil },
		func() (string, error) { consultedText = true; return "some text", nil },
	)

	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("an image on the clipboard produced %T, want attachResultMsg", msg)
	}
	if res.err != nil {
		t.Fatalf("an image on the clipboard reported an error: %v", res.err)
	}
	if res.att.Name != "clip.png" {
		t.Errorf("attached %q, want the clipboard image", res.att.Name)
	}
	if consultedText {
		t.Error("the text reader ran even though the clipboard held an image — the two reads must not both " +
			"happen, because the image is the case with no alternative")
	}
}

// TEXT on the clipboard IS PASTED — the operator's report, in full.
func TestClipboardPasteFallsBackToText(t *testing.T) {
	msg := pasteWith(t,
		func([]attachment) (attachment, error) { return attachment{}, errNoClipboardImage },
		func() (string, error) { return "a copied sentence", nil },
	)

	text, ok := msg.(clipboardTextMsg)
	if !ok {
		t.Fatalf("text on the clipboard produced %T, want clipboardTextMsg — this is the operator's "+
			"\"broke the ability to simply ctrl+v paste normal text\"", msg)
	}
	if text.text != "a copied sentence" {
		t.Errorf("pasted %q, want the clipboard's text", text.text)
	}
}

// A REAL IMAGE FAILURE IS REPORTED, NOT RETRIED AS TEXT.
//
// This is the case a naive fallback gets wrong: an image that is too large, or in an unsupported format, has
// been FOUND — so telling the operator "the clipboard holds no text" describes a clipboard that plainly holds
// the picture they were trying to send, and sends them looking for the wrong problem.
func TestClipboardPasteDoesNotRetryARealImageFailure(t *testing.T) {
	realFailure := errors.New("clip.png is too large (max 10MB)")
	consultedText := false
	msg := pasteWith(t,
		func([]attachment) (attachment, error) { return attachment{}, realFailure },
		func() (string, error) { consultedText = true; return "unrelated text", nil },
	)

	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("a real image failure produced %T, want attachResultMsg", msg)
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "too large") {
		t.Errorf("the refusal was not reported: %v", res.err)
	}
	if consultedText {
		t.Error("a REAL image failure was retried as text, so the operator would be told \"no text\" about a " +
			"clipboard holding the image they were sending")
	}
}

// AN EMPTY CLIPBOARD SAYS SO, once, in the image's words — not "no text".
func TestClipboardPasteWithNothingSaysThereIsNoImage(t *testing.T) {
	msg := pasteWith(t,
		func([]attachment) (attachment, error) { return attachment{}, errNoClipboardImage },
		func() (string, error) { return "", errors.New("the clipboard holds no text") },
	)
	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("an empty clipboard produced %T, want attachResultMsg", msg)
	}
	if res.err == nil {
		t.Fatal("an empty clipboard reported success")
	}
	if !errors.Is(res.err, errNoClipboardImage) {
		t.Errorf("an empty clipboard reported %q, want the image-less message (ctrl+v's headline job is the "+
			"screenshot, so an empty clipboard is not best explained as \"no text\")", res.err)
	}
}

// WHITESPACE IS NOT TEXT. A clipboard holding a newline would otherwise insert nothing while claiming to have
// pasted something.
func TestClipboardPasteTreatsWhitespaceAsEmpty(t *testing.T) {
	msg := pasteWith(t,
		func([]attachment) (attachment, error) { return attachment{}, errNoClipboardImage },
		func() (string, error) { return "  \n\t ", nil },
	)
	if _, ok := msg.(clipboardTextMsg); ok {
		t.Fatal("whitespace on the clipboard was pasted as text")
	}
	res, ok := msg.(attachResultMsg)
	if !ok || res.err == nil {
		t.Fatalf("whitespace produced %T (err=%v), want a reported emptiness", msg, res.err)
	}
}
