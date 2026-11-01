package tui

// msgmenu.go — THE PER-MESSAGE EDIT MENU: undo a message, or revert to it.
//
// THE OPERATOR'S REQUEST, in their words: "right click for the TUI to bring up a popup menu to choose your
// option", offering the two verbs "click on a user message and undo it (remove it from the screen and context),
// and also revert to that message removing anything that has come after it".
//
// WHAT EACH ONE REMOVES, because this is the only destructive gesture in the transcript:
//
//	Undo this message   ONLY that message. Its reply stays, so forgetting the question need not lose the answer.
//	Revert to here      that message AND everything after it, leaving the conversation ending at the turn before.
//
// BOTH ALSO RESET THE CONVERSATION'S MODEL SESSION, which the server does. That is the half an operator cannot
// see: turns are continued on a stored session id, so without the reset the message would leave the transcript
// while staying in the model's context — the screen saying one thing and the model knowing another.
//
// TWO CONFIRMATIONS, DELIBERATELY. Opening the menu from a right-click is one gesture and pressing Enter on an
// entry is another, so an accidental right-click followed by an accidental Enter would otherwise delete history
// with no chance to notice. The entry ARMS the action (the footer says what will go and that it cannot be
// undone) and a second Enter performs it. Escape backs out of either step.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// msgMenuEntry is one row of the menu.
type msgMenuEntry struct {
	Label string
	// Detail states the SCOPE, which is the thing that differs between the two entries. It is on the row
	// rather than in a help screen because the choice IS the scope.
	Detail string
	Mode   apiv1.MessageDeleteMode
}

// msgMenuEntries is the menu, in order. Undo is first: it is the narrower and the one the operator named first.
var msgMenuEntries = []msgMenuEntry{
	{
		Label:  "Undo this message",
		Detail: "removes only this message; its reply stays",
		Mode:   chat.MessageDeleteOnly,
	},
	{
		Label:  "Revert to here",
		Detail: "removes this message and everything after it",
		Mode:   chat.MessageDeleteAndFollowing,
	},
}

// msgMenu is the open menu: which message, where it came from, and how far the operator has got.
type msgMenu struct {
	conversationID string
	messageID      string
	// preview is the message's own text, shown as the menu's header so the operator can see WHICH message a
	// right-click landed on. A menu that removed "a message" without naming it would be asking for trust.
	preview string
	sel     int
	// armed is the entry the operator has chosen and must confirm. nil = still choosing.
	armed *int
}

// openMsgMenu opens the menu for one message.
func (m *App) openMsgMenu(conversationID, messageID, preview string) {
	if conversationID == "" || messageID == "" {
		return
	}
	m.msgMenu = &msgMenu{
		conversationID: conversationID,
		messageID:      messageID,
		preview:        preview,
	}
}

// openMsgMenuArmed opens the menu with one entry ALREADY ARMED.
//
// IT IS HOW THE SLASH COMMANDS REACH THIS FEATURE, and it is deliberately not a second destructive path: /undo
// and /revert open the same menu with the matching entry chosen, so the operator still confirms once and there is
// exactly one place a deletion can be dispatched from. A command that executed on Enter would be a second code
// path to the same irreversible act — two things to keep safe instead of one.
func (m *App) openMsgMenuArmed(conversationID, messageID, preview string, mode apiv1.MessageDeleteMode) {
	m.openMsgMenu(conversationID, messageID, preview)
	if m.msgMenu == nil {
		return
	}
	for i, e := range msgMenuEntries {
		if e.Mode == mode {
			m.msgMenu.sel = i
			sel := i
			m.msgMenu.armed = &sel
			return
		}
	}
}

// closeMsgMenu dismisses the menu without acting.
func (m *App) closeMsgMenu() { m.msgMenu = nil }

// handleMsgMenuKey drives the menu and reports whether it consumed the key.
//
// IT SWALLOWS EVERY KEY WHILE OPEN. The composer must not receive a character typed at a menu — the same rule
// the help overlay and the tab dropdown follow.
func (m *App) handleMsgMenuKey(k tea.KeyMsg) tea.Cmd {
	menu := m.msgMenu
	if menu == nil {
		return nil
	}
	switch k.String() {
	case "esc":
		// Escape backs out ONE step: an armed entry returns to the list, and only a second Escape closes. That
		// is what makes the first Escape after choosing a no-op rather than a dismissal the operator has to
		// re-open the menu to undo.
		if menu.armed != nil {
			menu.armed = nil
			return nil
		}
		m.closeMsgMenu()
		return nil
	case "up", "k":
		if menu.armed == nil {
			menu.sel = (menu.sel + len(msgMenuEntries) - 1) % len(msgMenuEntries)
		}
		return nil
	case "down", "j":
		if menu.armed == nil {
			menu.sel = (menu.sel + 1) % len(msgMenuEntries)
		}
		return nil
	case "enter":
		if menu.armed == nil {
			sel := menu.sel
			menu.armed = &sel
			return nil
		}
		entry := msgMenuEntries[*menu.armed]
		m.closeMsgMenu()
		if m.chat == nil {
			return nil
		}
		return m.chat.DeleteMessage(menu.conversationID, menu.messageID, entry.Mode)
	}
	return nil
}

// msgMenuBlocksKeys reports whether the menu owns the keyboard right now.
func (m *App) msgMenuBlocksKeys() bool { return m.msgMenu != nil }

// msgMenuView renders the menu as an overlay block, centred by the same compositor every other overlay uses.
func (m *App) msgMenuView() string {
	menu := m.msgMenu
	if menu == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Edit message\n")
	// THE MESSAGE ITSELF, truncated. Naming it is the difference between editing a message and deleting an
	// anonymous row — and a right-click is easy to land one row off.
	if prev := strings.TrimSpace(menu.preview); prev != "" {
		b.WriteString(theme.HintText.Render(clipTo(prev, 64)) + "\n")
	}
	b.WriteString("\n")
	for i, e := range msgMenuEntries {
		marker := "  "
		if i == menu.sel && menu.armed == nil {
			marker = "› "
		}
		line := marker + e.Label
		if i == menu.sel && menu.armed == nil {
			b.WriteString(line + "\n")
			b.WriteString(theme.HintText.Render("   "+e.Detail) + "\n")
			continue
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	if menu.armed != nil {
		b.WriteString(theme.ErrorText.Render("Enter again to remove — this cannot be undone") + "\n")
		b.WriteString(theme.HintText.Render("Esc goes back") + "\n")
	} else {
		b.WriteString(theme.HintText.Render("↑/↓ choose · Enter select · Esc close") + "\n")
	}
	return theme.HelpOverlay.Render(b.String())
}

// clipTo truncates s to n runes on ONE line, so a multi-line message does not distort the menu's geometry.
func clipTo(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
