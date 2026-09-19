package tui

// code_block_click_copy_test.go — CLICKING A CODE BLOCK COPIES THE CODE, not the cells it was drawn in.
//
// The operator: "if the command is stretching on more than one line with say a / then it copies the enter
// width to the pane. You can't copy just the block itself with no added. We should format it properly and not
// highlight the entire block. I also think the click treatment like we did with the user message would be an
// added bonus."
//
// The "added" is REAL and cannot be trimmed away: the transcript band indents every line by one cell, so a
// drag-select yields " sudo pacman -U --needed \" — and a leading space cannot be stripped, because it is
// indistinguishable from a genuine code indent (Python and Makefiles would be silently corrupted by trying).
//
// So the block becomes clickable and copies the FENCE'S SOURCE, which never went through the renderer and has
// nothing to strip. These tests assert the exact bytes, because "close enough" is what a paste cannot forgive.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// blockReply is the operator's own repro: a multi-line shell command with continuations, which is where the
// wrapping and the width complaints came from.
const blockReply = "Run this:\n\n```bash\n" +
	"sudo pacman -U --needed \\\n" +
	"  --assume-installed 'http-parser=2.9.4' \\\n" +
	"  ~/Downloads/r2modman-3.2.19.pacman\n" +
	"```\n\nThen reboot."

// blockPlane builds a conversation whose reply contains that block.
func blockPlane(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: blockReply, Key: "a1", At: 1})
	m.onChatWake()
	m.convRailOpen = true
	m.refreshLayout()
	_ = m.View()
	return m
}

// blockRow is the frame row of the first line of the command, found by what is DRAWN — a fixture row that
// moved must fail loudly rather than turn the assertion below into a tautology.
func blockRow(t *testing.T, m *App) int {
	t.Helper()
	for i, row := range strings.Split(m.viewFrame(), "\n") {
		if strings.Contains(row, "sudo pacman") {
			return i
		}
	}
	t.Fatal("fixture: the command is not on screen, so nothing could be clicked")
	return -1
}

// THE BLOCK IS ON SCREEN AT ALL — the precondition, and the one that would otherwise let every test below pass
// by finding nothing.
func TestTheCodeBlockRendersInTheTranscript(t *testing.T) {
	m := blockPlane(t)
	frame := m.viewFrame()
	for _, want := range []string{"sudo pacman -U --needed", "assume-installed", "r2modman-3.2.19.pacman"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the reply's code block is not on screen (%q missing): the click tests below would pass "+
				"vacuously", want)
		}
	}
}

// AND CLICKING IT COPIES THE FENCE, EXACTLY. Every line is asserted verbatim, so a leading band indent, a
// trailing fill, a wrap artefact or a missing continuation all fail here.
func TestClickingACodeBlockCopiesTheExactSource(t *testing.T) {
	m := blockPlane(t)
	row := blockRow(t, m)
	got, ok := m.transcriptCodeBlockAtFrameRow(row)
	if !ok {
		t.Fatal("a click on the block resolved to nothing")
	}
	want := "sudo pacman -U --needed \\\n" +
		"  --assume-installed 'http-parser=2.9.4' \\\n" +
		"  ~/Downloads/r2modman-3.2.19.pacman"
	if got != want {
		t.Errorf("the copied block is not the fence's source.\n got: %q\nwant: %q", got, want)
	}
	// THE THREE SPECIFIC CORRUPTIONS, named separately so a failure says which one came back.
	if strings.HasPrefix(got, " ") {
		t.Error("the copy carries the band's leading indent — which is exactly what \"no added\" rules out")
	}
	if strings.ContainsAny(got, "│┃║") {
		t.Error("the copy carries a pane border character")
	}
	if strings.HasSuffix(got, " ") {
		t.Error("the copy carries the fill's trailing padding")
	}
}

// THE LABEL ROW IS PART OF THE BLOCK, so clicking `bash` copies the code rather than nothing. It is the block's
// own header, and a one-row dead spot above a live one reads as a broken affordance.
func TestClickingTheLanguageLabelCopiesTheBlockToo(t *testing.T) {
	m := blockPlane(t)
	labelRow := -1
	for i, row := range strings.Split(m.viewFrame(), "\n") {
		if strings.Contains(row, "bash") {
			labelRow = i
			break
		}
	}
	if labelRow < 0 {
		t.Fatal("fixture: the language label is not on screen")
	}
	got, ok := m.transcriptCodeBlockAtFrameRow(labelRow)
	if !ok {
		t.Fatal("clicking the block's own label resolved to nothing")
	}
	if !strings.HasPrefix(got, "sudo pacman") {
		t.Errorf("the label row did not resolve to the block's source: %q", got)
	}
}

// AND THE CLICK IS WIRED TO THE COPY — not merely resolvable. A press on the block has to produce the toast,
// which is the only observable the operator gets.
func TestAClickOnTheBlockConfirmsWithTheToast(t *testing.T) {
	m := blockPlane(t)
	row := blockRow(t, m)
	nm, cmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: row,
	})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if cmd == nil {
		t.Fatal("a click on the code block produced no command, so nothing was copied")
	}
	if app.clip.toast == "" {
		t.Error("no confirmation was raised, so the operator gets no sign the click did anything")
	}
}

// A CLICK ON PROSE IS STILL PROSE. Adding the block rule must not make an ordinary paragraph resolve to a
// block — the two rules are adjacent, and this is the boundary between them.
func TestAClickOnProseIsNotACodeBlock(t *testing.T) {
	m := blockPlane(t)
	proseRow := -1
	for i, row := range strings.Split(m.viewFrame(), "\n") {
		if strings.Contains(row, "Then reboot.") {
			proseRow = i
			break
		}
	}
	if proseRow < 0 {
		t.Fatal("fixture: the trailing prose is not on screen")
	}
	if got, ok := m.transcriptCodeBlockAtFrameRow(proseRow); ok {
		t.Errorf("a click on ordinary prose resolved to a code block: %q", got)
	}
}
