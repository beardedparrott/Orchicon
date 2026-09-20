package tui

// modes_tui_test.go — THE THREE MODES IN THE TUI.
//
// The operator: "I would like to expand on the different modes that Ask Orchicon has and build in support for
// switching that in the TUI (The GUI already has a drop down for this)."
//
// The wire, the storage and the slash command already existed for one mode; what these pin is that the TUI
// now OFFERS all three, that the names an operator would actually type resolve, and that the two surfaces
// which display a mode (the composer's pill and the `/mode` confirmation) agree on what it is called.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// EVERY MODE AN OPERATOR WOULD TYPE RESOLVES, including the spellings the UI itself
// shows. "quick work" is the label, "quick_work" is the enum — rejecting either would
// be a spelling test, which is exactly what a mode command exists to avoid.
func TestParseModeAcceptsEverySpelling(t *testing.T) {
	for _, c := range []struct {
		typed string
		want  apiv1.ConversationMode
	}{
		{"brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM},
		{"Brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM},
		{"  BRAINSTORM  ", apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM},
		{"iteration", apiv1.ConversationMode_CONVERSATION_MODE_ITERATION},
		{"ITERATION", apiv1.ConversationMode_CONVERSATION_MODE_ITERATION},
		{"quick_work", apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK},
		{"quick work", apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK},
		{"quick-work", apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK},
		{"quickwork", apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK},
		{"Quick Work", apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK},
	} {
		got, ok := chat.ParseMode(c.typed)
		if !ok {
			t.Errorf("ParseMode(%q) rejected a spelling the UI itself uses", c.typed)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %v, want %v", c.typed, got, c.want)
		}
	}
	for _, bad := range []string{"warp", "", "   ", "orchicon", "quick"} {
		if _, ok := chat.ParseMode(bad); ok {
			t.Errorf("ParseMode(%q) was accepted — an unknown mode must be refused, not coerced", bad)
		}
	}
}

// THE LIST THE UI SHOWS IS THE LIST THE PARSER ACCEPTS. One source, so the help text
// cannot advertise a mode the command would reject.
func TestModeNamesAllParse(t *testing.T) {
	names := chat.ModeNames()
	if len(names) != 3 {
		t.Fatalf("ModeNames() = %v, want the three modes", names)
	}
	for _, n := range names {
		if _, ok := chat.ParseMode(n); !ok {
			t.Errorf("ModeNames() advertises %q, which ParseMode rejects — the help text and the parser "+
				"have drifted", n)
		}
	}
	// And nothing the parser accepts is missing from the list, or a mode would be
	// reachable but undiscoverable.
	for _, m := range []string{"brainstorm", "iteration", "quick_work"} {
		found := false
		for _, n := range names {
			if n == m {
				found = true
			}
		}
		if !found {
			t.Errorf("mode %q parses but is not listed, so it cannot be discovered", m)
		}
	}
}

// THE PILL NAMES EVERY MODE. `ConversationMode.String()` lowercased yields
// "conversation_mode_quick_work", which is a wire value — showing it in the composer
// would be showing the operator an implementation detail.
func TestCurrentModeLabelNamesEveryMode(t *testing.T) {
	for _, c := range []struct {
		mode apiv1.ConversationMode
		want string
	}{
		{apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM, "brainstorm"},
		{apiv1.ConversationMode_CONVERSATION_MODE_ITERATION, "iteration"},
		{apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK, "quick work"},
		{apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED, "brainstorm"},
	} {
		m := newTestApp()
		m.chat.SetPendingMode(c.mode)
		if got := m.currentModeLabel(); got != c.want {
			t.Errorf("currentModeLabel(%v) = %q, want %q", c.mode, got, c.want)
		}
		if strings.Contains(m.currentModeLabel(), "conversation_mode_") {
			t.Errorf("the pill leaks the wire spelling: %q", m.currentModeLabel())
		}
	}
}

// THE CONFIRMATION NAMES THE MODE FROM THE ENUM, not from what was typed. Echoing the
// argument back would confirm a misspelling as success.
func TestModeSwitchLabelDoesNotEchoTheArgument(t *testing.T) {
	m := newTestApp()
	for _, c := range []struct {
		mode apiv1.ConversationMode
		want string
	}{
		{apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM, "brainstorm"},
		{apiv1.ConversationMode_CONVERSATION_MODE_ITERATION, "iteration"},
		{apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK, "quick work"},
	} {
		if got := m.modeSwitchLabel(c.mode); got != c.want {
			t.Errorf("modeSwitchLabel(%v) = %q, want %q", c.mode, got, c.want)
		}
	}
}

// A BARE /mode REPORTS AND LISTS. An operator who does not already know the mode names
// cannot guess them, so the report is what makes the command self-teaching.
func TestBareModeReportsAndListsTheModes(t *testing.T) {
	m := askRelaunched(t, "c1")

	if handled, _ := m.dispatchSlash("/mode"); !handled {
		t.Fatal("/mode with no argument must be handled")
	}
	notice := m.dock.Notice
	if !strings.Contains(notice, "mode: ") {
		t.Errorf("a bare /mode does not report the current mode: %q", notice)
	}
	for _, n := range chat.ModeNames() {
		if !strings.Contains(notice, n) {
			t.Errorf("a bare /mode does not list %q, so the mode cannot be discovered: %q", n, notice)
		}
	}
}

// SWITCHING TO EACH MODE REACHES THE SERVER. Driven through the real slash dispatch, so
// this fails if a mode parses but its write never goes out.
func TestSwitchingToEveryModeWritesIt(t *testing.T) {
	for _, c := range []struct {
		typed string
		want  apiv1.ConversationMode
	}{
		{"brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM},
		{"iteration", apiv1.ConversationMode_CONVERSATION_MODE_ITERATION},
		{"quick work", apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK},
	} {
		m, stub := newAskApp(t)
		m.chatConvID = "c1"

		handled, cmd := m.dispatchSlash("/mode " + c.typed)
		if !handled {
			t.Errorf("/mode %s was not handled", c.typed)
			continue
		}
		if cmd == nil {
			t.Errorf("/mode %s dispatched no write while a conversation is open", c.typed)
			continue
		}
		mm, ok := cmd().(chat.ConversationMutatedMsg)
		if !ok || mm.Err != "" {
			t.Errorf("/mode %s result = %#v", c.typed, mm)
			continue
		}
		if stub.modeSet != c.want {
			t.Errorf("/mode %s sent %v, want %v", c.typed, stub.modeSet, c.want)
		}
		// The confirmation names the mode, so the operator can see the switch landed.
		if !strings.Contains(m.dock.Notice, "mode →") {
			t.Errorf("/mode %s did not confirm the switch: %q", c.typed, m.dock.Notice)
		}
	}
}

// A NEW CONVERSATION IS CREATED WITH THE MODE THE OPERATOR PICKED, not with the
// default. Without this the switch would apply only to conversations that already exist,
// which is the opposite of what a mode chosen before the first message should do.
func TestPendingModeReachesANewConversation(t *testing.T) {
	m, stub := newAskApp(t)
	// No conversation open: the mode applies to the next one.
	if handled, _ := m.dispatchSlash("/mode iteration"); !handled {
		t.Fatal("/mode iteration must be handled with no conversation open")
	}
	if got := m.chat.PendingMode(); got != apiv1.ConversationMode_CONVERSATION_MODE_ITERATION {
		t.Fatalf("the pending mode is %v, want ITERATION — the pick did not stick", got)
	}
	if !strings.Contains(m.dock.Notice, "next new conversation") {
		t.Errorf("the notice does not say the mode applies to the next conversation: %q", m.dock.Notice)
	}

	// Creating the conversation carries it. The fourth argument is the PROJECT: empty here, because this
	// test is about the mode — the project-aware create is covered by conversation_project_rail_test.go.
	cmd := m.chat.CreateConversation("", m.chat.PendingMode(), "hello", "")
	if cmd == nil {
		t.Fatal("CreateConversation produced no command")
	}
	cmd()
	if stub.createdMode != apiv1.ConversationMode_CONVERSATION_MODE_ITERATION {
		t.Errorf("the conversation was created with mode %v, want ITERATION", stub.createdMode)
	}
	_ = tea.Batch
}

// THE PILL READS THE OPEN CONVERSATION'S PERSISTED MODE.
//
// This is the path that had NEVER worked, which is why it had no test. currentModeLabel looked the
// conversation up in `m.chat.Conversations()` — a slice nothing in the codebase ever writes — so the lookup
// always missed and the pill fell through to the pending mode. It showed the last mode this PROCESS had set,
// never the conversation's own. The operator, seeing it disagree with the server:
//
//	"we need to fix the whole stale pill. If someone sets the mode in the GUI or TUI, it should not matter. It
//	 should change for both."
//
// The stale pending mode here is the point of the test: with a conversation open, the CONVERSATION wins.
func TestThePillReadsTheOpenConversationsMode(t *testing.T) {
	m := scopePlane(t)
	m.chatConvID = "c-1"
	for i := range m.conversations {
		if m.conversations[i].ID == "c-1" {
			m.conversations[i].Mode = apiv1.ConversationMode_CONVERSATION_MODE_ITERATION
		}
	}
	// A stale pending mode, exactly the one the pill used to show instead.
	m.chat.SetPendingMode(apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK)

	if got := m.currentModeLabel(); got != "iteration" {
		t.Errorf("the pill reports %q with c-1 open and in iteration — it is reading the pending mode rather than "+
			"the conversation, which is the staleness the operator reported", got)
	}
}

// AND CHANGING AN OPEN CONVERSATION'S MODE DOES NOT LEAK INTO THE NEXT NEW ONE.
//
// The operator: "When someone creates a new conversation, it should always default back to brainstorm unless
// they do /mode again." The pending mode used to be set unconditionally by /mode, so switching the conversation
// you were in also changed what the NEXT one would be created with.
func TestSwitchingAnOpenConversationDoesNotChangeTheDefaultForTheNext(t *testing.T) {
	m := scopePlane(t)
	m.chatConvID = "c-1"
	m.chat.SetPendingMode(apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM)

	if err := runSlashTUI(t, m, "/mode iteration"); err != "" {
		t.Fatalf("/mode iteration returned an error notice: %s", err)
	}
	if got := m.chat.PendingMode(); got != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Errorf("the pending mode became %v after switching an OPEN conversation — the mode leaked forward to the "+
			"next new conversation", got)
	}
}

// AND A NEW CHAT RESETS THE PENDING MODE, so a mode picked for a conversation cannot survive it.
//
// The controller's pending mode is the pre-conversation choice (the launch page's selector), and it is
// legitimate — but it belongs to the conversation it created, not to every conversation afterwards.
func TestANewChatStartsFromTheDefaultMode(t *testing.T) {
	m := scopePlane(t)
	m.chat.SetPendingMode(apiv1.ConversationMode_CONVERSATION_MODE_ITERATION)

	m.newChat()

	if got := m.chat.PendingMode(); got != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Errorf("a new chat inherited %v as its mode, want brainstorm — a new conversation must start at the "+
			"default unless the operator sets it again", got)
	}
}
