package tui

// conversation_scope_test.go — the CONVERSATION scope's WRITES, and the slash surface that reaches them.
//
// The MCP half goes through screens/mcpforms with Owner{ConversationID}; the skills half through the
// controller's SetConversationSkillFiles. Both are reached the SAME way the conversation's other
// per-conversation controls are — the slash surface — which is the TUI's mirror of the GUI's header
// disclosure beside SessionGrants.
//
// THE MODAL ITSELF IS TESTED IN scope_modal_test.go. What is asserted here is the split between the two
// kinds of slash command the scope has: the names that OPEN the modal (/scope, /mcp, and bare /skills)
// and the DIRECT-WRITE form (/skills <paths>), which needs no modal because a typed path list is a
// complete statement.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// BARE /skills OPENS THE SCOPE MODAL — the operator's "Same with /skills. It should pop up the same
// modal." The report it used to print is now something the operator can SEE, with the paths and the
// keys that act on them.
func TestSkillsWithoutArgumentsOpensTheScope(t *testing.T) {
	m, _, _ := newScopeApp(t)
	if cmd := mustSlash(t, m, "/skills"); cmd == nil {
		t.Fatal("/skills with no argument must fetch the scope, not print a line and vanish")
	}
	if m.scope == nil {
		t.Fatal("/skills with no argument opened no modal")
	}
	if got := m.scopeConversation(); got != "c1" {
		t.Fatalf("the modal is about %q, want the open conversation", got)
	}
}

// /skills <paths> WRITES the list — the conversation's half of the skills feature, and AC
// 10's "the capability is identical, only the control differs": a typed path list with the
// SAME server-side validation.
func TestSkillsWritesTheConversationList(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}

	cmd := mustSlash(t, m, "/skills /a/SKILL.md, /b")
	if cmd == nil {
		t.Fatal("/skills <paths> must produce a write")
	}
	if msg, ok := cmd().(chat.ConversationMutatedMsg); !ok || msg.Err != "" {
		t.Fatalf("skills write produced %#v", cmd())
	}
	if stub.skillFilesFor != "c1" {
		t.Fatalf("the write targeted %q, want c1", stub.skillFilesFor)
	}
	if len(stub.skillFilesSet) != 2 || stub.skillFilesSet[0] != "/a/SKILL.md" || stub.skillFilesSet[1] != "/b" {
		t.Fatalf("the write carried %v, want the two typed paths (comma-separated accepted)", stub.skillFilesSet)
	}
}

// /skills clear EMPTIES the list — an empty list is a legitimate write, not a no-op (the
// reasoning the old clear-is-legitimate test used).
func TestSkillsClearIsALegitimateWrite(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1", SkillFiles: []string{"/a"}}}

	cmd := mustSlash(t, m, "/skills clear")
	if cmd == nil {
		t.Fatal("/skills clear must produce a write")
	}
	if msg, ok := cmd().(chat.ConversationMutatedMsg); !ok || msg.Err != "" {
		t.Fatalf("clear produced %#v", cmd())
	}
	if stub.skillFilesFor != "c1" || len(stub.skillFilesSet) != 0 {
		t.Fatalf("clear sent (%q, %v), want (c1, empty)", stub.skillFilesFor, stub.skillFilesSet)
	}
}

// The conversation commands GUARD on an open conversation, the same idiom /fullsend uses:
// a conversation-scoped write with no conversation is refused, never silently applied to
// something else.
func TestConversationScopeCommandsNeedAnOpenConversation(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = ""
	for _, line := range []string{"/skills /a", "/mcp", "/scope", "/skills"} {
		m.dock.SetError("")
		if cmd := mustSlash(t, m, line); cmd != nil {
			t.Fatalf("%q produced a cmd with no conversation open — it must refuse", line)
		}
		if !strings.Contains(strings.ToLower(m.dock.Err), "no conversation") {
			t.Fatalf("%q did not explain the refusal: %q", line, m.dock.Err)
		}
		if m.scope != nil {
			t.Fatalf("%q opened the scope modal with no conversation open", line)
		}
	}
}

// THE NAMES ARE REGISTERED AND THEY ALL MEAN THE SAME SURFACE: /scope is the honest name, /mcp and
// /skills are the words an operator types. A name that resolves to nothing is a name that lies.
func TestTheScopeNamesAreAllRegistered(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}
	for _, name := range []string{"/scope", "/mcp", "/skills"} {
		c := m.slash.resolve(name)
		if c == nil {
			t.Fatalf("%s is not in the registry", name)
		}
	}
	// The MCP name still names MCP (the operator types it to find the servers), and /scope names the
	// whole scope — the two descriptions must not collapse into one another.
	if c := m.slash.resolve("/mcp"); !strings.Contains(strings.ToLower(c.Desc), "mcp") {
		t.Errorf("/mcp's description does not name MCP: %q", c.Desc)
	}
	if c := m.slash.resolve("/scope"); !strings.Contains(strings.ToLower(c.Desc), "scope") {
		t.Errorf("/scope's description does not name the scope: %q", c.Desc)
	}
}

// THE SUBCOMMAND GRAMMAR IS GONE, AND THE REFUSAL SAYS WHERE IT WENT. The old contract was
// "/mcp [define | edit | delete | secret | install]" — five verbs an operator had to know, in a usage
// string the palette had to clip. An argument now gets a refusal that names the modal's keys, because a
// command that silently ignores what it was given is worse than one that refuses.
func TestMcpTakesNoSubcommandsAndSaysWhereTheVerbsWent(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}
	m.dock.SetError("")
	if cmd := mustSlash(t, m, "/mcp define"); cmd != nil {
		t.Fatalf("/mcp with an argument produced a cmd (%T) — the grammar is gone", cmd())
	}
	if m.scope != nil {
		t.Error("/mcp define opened the modal anyway, leaving the operator to wonder why " +
			"\"define\" did nothing")
	}
	err := m.dock.Err
	if !strings.Contains(err, "not a subcommand") {
		t.Fatalf("the refusal does not say the grammar is gone: %q", err)
	}
	// The refusal must be USABLE: it names each verb the operator used to TYPE, so the capability is
	// found rather than lost. (The words, not the key glyphs — a message that listed keys without
	// saying what they do would leave the operator guessing.)
	for _, verb := range []string{"add", "catalog", "edit", "install", "credential", "delete", "skill files"} {
		if !strings.Contains(err, verb) {
			t.Errorf("the refusal never mentions %q, so an operator who typed the old verb cannot find "+
				"where it went: %q", verb, err)
		}
	}
}

// mustSlash resolves and runs a slash line, returning the cmd it produced.
func mustSlash(t *testing.T, m *App, line string) tea.Cmd {
	t.Helper()
	name, args, _, ok := ParseSlash(line)
	if !ok {
		t.Fatalf("%q did not parse as a slash command", line)
	}
	c := m.slash.resolve(name)
	if c == nil {
		t.Fatalf("%s is not in the slash registry", name)
	}
	return c.Run(m, args)
}

// formHasField reports whether a kit2 form offers a named field.
func formHasField(f *kit2.Form, name string) bool {
	for i := range f.Specs {
		if f.Specs[i].Name == name {
			return true
		}
	}
	return false
}
