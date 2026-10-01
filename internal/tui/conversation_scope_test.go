package tui

// conversation_scope_test.go — the CONVERSATION scope's MCP definitions and skill files
// (child 7, AC 8b + AC 9 + AC 10).
//
// The MCP half goes through screens/mcpforms with Owner{ConversationID}; the skills half
// through the controller's SetConversationSkillFiles. Both are reached the SAME way the
// conversation's other per-conversation controls are — the slash surface — which is the
// TUI's mirror of the GUI's header disclosure beside SessionGrants.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// /skills with no argument REPORTS the conversation's list (the SERVER's list, from the
// rail row — the same reload the mode pill follows).
func TestSkillsReportsTheOpenConversationFiles(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1", SkillFiles: []string{"/a/SKILL.md", "/b"}}}

	cmd := mustSlash(t, m, "/skills")
	if cmd == nil {
		t.Fatal("/skills with no argument must report")
	}
	msg, ok := cmd().(convScopeMsg)
	if !ok {
		t.Fatalf("report produced %T, want convScopeMsg", cmd())
	}
	if msg.err != "" {
		t.Fatalf("report errored: %s", msg.err)
	}
	if !strings.Contains(msg.detail, "/a/SKILL.md") || !strings.Contains(msg.detail, "/b") {
		t.Fatalf("report = %q, want both paths", msg.detail)
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
	for _, line := range []string{"/skills /a", "/mcp", "/mcp define"} {
		m.dock.SetError("")
		if cmd := mustSlash(t, m, line); cmd != nil {
			t.Fatalf("%q produced a cmd with no conversation open — it must refuse", line)
		}
		if !strings.Contains(strings.ToLower(m.dock.Err), "no conversation") {
			t.Fatalf("%q did not explain the refusal: %q", line, m.dock.Err)
		}
	}
}

// /mcp IS THE CONVERSATION SCOPE, and it is REGISTERED: the name that the removed control
// source used to generate now belongs to the conversation's own definitions (AC 9's net
// inversion).
func TestMcpSlashBelongsToTheConversationScope(t *testing.T) {
	m, _ := newAskApp(t)
	c := m.slash.resolve("/mcp")
	if c == nil {
		t.Fatal("/mcp is not in the registry")
	}
	if !strings.Contains(c.Desc, "conversation") {
		t.Fatalf("/mcp's description does not name the conversation scope: %q", c.Desc)
	}
	if !strings.Contains(c.Usage, "define") {
		t.Fatalf("/mcp usage does not offer the DEFINE path: %q", c.Usage)
	}
}

// /mcp define OPENS THE TYPED DEFINITION FORM — this is AC 9: the TUI DEFINES an entry
// (command/args/env or url/headers), it does not select from a tenant list. The form is
// owner-stamped for the conversation, so the write cannot land as a tenant-level row.
func TestMcpDefineOpensAnOwnedDefinitionForm(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}

	if cmd := mustSlash(t, m, "/mcp define"); cmd != nil {
		t.Fatalf("/mcp define must open a modal, not schedule a cmd (%T)", cmd())
	}
	f := m.convScopeForm
	if f == nil {
		t.Fatal("/mcp define opened no form — the TUI cannot define a conversation MCP entry")
	}
	for _, name := range []string{"name", "transport", "command", "args", "env", "url", "headers"} {
		if !formHasField(f, name) {
			t.Errorf("the definition form offers no %q field — the define path is incomplete", name)
		}
	}
}

// /mcp <bad subcommand> is an explicit refusal, never a silent no-op.
func TestMcpUnknownSubcommandRefuses(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}
	m.dock.SetError("")
	if cmd := mustSlash(t, m, "/mcp wat"); cmd != nil {
		t.Fatalf("an unknown subcommand produced a cmd (%T)", cmd())
	}
	if !strings.Contains(m.dock.Err, "unknown /mcp subcommand") {
		t.Fatalf("unknown subcommand was not explained: %q", m.dock.Err)
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
