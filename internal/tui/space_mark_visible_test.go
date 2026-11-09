package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// TestSpaceMarksConversationsThroughTheRealAskScreen is the regression pin for the operator's
// report.
func TestSpaceMarksConversationsThroughTheRealAskScreen(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, ask.New(nil, m.reg))
	m.dispatch(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.convLoaded = true
	m.convErr = ""
	m.chatConvID = "conv-01"
	for i := 1; i <= 5; i++ {
		m.conversations = append(m.conversations, chat.Conversation{
			ID:    fmt.Sprintf("conv-%02d", i),
			Title: fmt.Sprintf("conversation %02d", i), MessageN: 2,
		})
	}
	m.convSel, m.convScroll = 0, 0
	m.dock.SetValue("")
	m.refreshLayout()

	// Every focus level the rail's keys are documented to work from: the composer (its keys are
	// driven from there) and the content (where the operator reads the transcript).
	for _, focus := range []focusMode{focusComposer, focusContent} {
		t.Run(fmt.Sprintf("focus=%v", focus), func(t *testing.T) {
			mm := m
			mm.setFocus(focus)
			mm.convSel = 0
			mm.convMarked = nil
			openBefore := mm.chatConvID

			before := mm.rightRailView()
			nm, _ := mm.Update(tea.KeyMsg{Type: tea.KeySpace})
			got := nm.(*App)

			// 1. STATE: marked, and stepped one row.
			if ids := got.convMarkedIDs(); len(ids) != 1 || ids[0] != "conv-01" {
				t.Fatalf("space did not MARK the highlighted conversation: marked=%v (want [conv-01])", ids)
			}
			if got.convSel != 1 {
				t.Errorf("space must step one row after marking (kit2 does), got convSel=%d", got.convSel)
			}

			// 2. NOT A SELECTION: the open conversation is unchanged.
			if got.chatConvID != openBefore {
				t.Errorf("space CHANGED the open conversation (%q → %q) — that is the select behavior the "+
					"operator reported; marking must leave the open chat alone", openBefore, got.chatConvID)
			}

			// 3. VISIBLE: the mark glyph is painted and the hint names the count.
			if strings.Contains(before, "✓") {
				t.Fatal("fixture: the rail already showed a mark before space")
			}
			after := got.rightRailView()
			if !strings.Contains(after, "✓") {
				t.Errorf("space marked state but painted NO glyph — a bare cursor move reads as 'selecting'. "+
					"marked=%v\nrail:\n%s", got.convMarkedIDs(), after)
			}
			if hint := got.railHintLine(); !strings.Contains(hint, "marked") {
				t.Errorf("the rail hint does not report the mark after space: %q", hint)
			}
		})
	}
}
