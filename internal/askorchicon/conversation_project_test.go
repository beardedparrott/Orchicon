package askorchicon

// conversation_project_test.go — A CHAT KNOWS WHICH PROJECT FOLDER IT BELONGS TO, IN ALL THREE MODES.
//
// The operator: "Also we should add context to all three modes to know which chat belongs to which project
// folder. We need better organization here."
//
// Two things had to be true for that, and each is asserted separately:
//
//  1. The conversation row carries the association, and the API reports it (conversationRowToProto).
//  2. The turn's system prompt STATES it — in Brainstorm, Iteration and Quick Work alike, because the block is
//     built from the conversation rather than inside any one persona.
//
// The prompt half is the one that could regress silently: a persona rewrite would not touch the block, but a
// refactor of buildSystemPrompt's parameters could drop it, and nothing else would notice.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// THE CONVERSATION'S PROJECT IS NAMED IN EVERY MODE — the operator's "all three modes", iterated over the
// roster so a mode added later is covered without anyone remembering to add it here.
func TestEveryModeIsToldWhichProjectTheChatIsIn(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	convProject := "This chat belongs to the project **Orchicon** (ID 01ABC, status active), whose directory is " +
		"`/home/me/projects/Orchicon`.\n"
	for _, mode := range everyMode {
		p := buildSystemPrompt(mode, cfg, reg, nil, true, nil, "", convProject)
		if !strings.Contains(p, "## This conversation's project") {
			t.Errorf("%s: the prompt has no conversation-project section, so the agent cannot know which chat "+
				"belongs to which project folder", mode)
		}
		if !strings.Contains(p, "/home/me/projects/Orchicon") {
			t.Errorf("%s: the project's DIRECTORY is missing from the prompt — the folder is the whole point", mode)
		}
		if !strings.Contains(p, "Orchicon") {
			t.Errorf("%s: the project is not named", mode)
		}
	}
}

// AND AN UNASSIGNED CHAT IS TOLD THAT, rather than being left to infer it from a missing line. The agent has to
// be able to say "this chat has no project" instead of quietly operating on the tenant's default directory as
// though it were the conversation's own.
func TestAnUnassignedChatIsToldItHasNoProject(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	for _, mode := range everyMode {
		p := buildSystemPrompt(mode, cfg, reg, nil, true, nil, "", "")
		if !strings.Contains(p, "## This conversation's project") {
			t.Errorf("%s: the section header is omitted when unassigned, so the omission is silent", mode)
		}
		if !strings.Contains(p, "not assigned to a project") {
			t.Errorf("%s: an unassigned chat is not told so", mode)
		}
		if !strings.Contains(p, "ask_file_root") {
			t.Errorf("%s: the unassigned text does not tell the agent how to discover the directory it IS "+
				"scoped to, which is the one concrete thing it can still check", mode)
		}
	}
}

// THE ASSOCIATION SURVIVES THE PROTO BOUNDARY. Without this the UI would group by a field that is always
// empty, which is the failure mode that looks like "the feature was never wired".
func TestConversationRowReportsItsProjectOverTheAPI(t *testing.T) {
	r := db.ConversationRow{ID: "c1", TenantID: "t1", Title: "x", Mode: modeBrainstorm, ProjectID: "p-42"}
	got := conversationRowToProto(r, 0, "", turnStatusInfo{})
	if got.ProjectId != "p-42" {
		t.Errorf("Conversation.project_id = %q, want \"p-42\" — the client groups by this field", got.ProjectId)
	}
	// AND UNASSIGNED STAYS EMPTY rather than becoming a sentinel the clients would have to know about.
	empty := conversationRowToProto(db.ConversationRow{ID: "c2"}, 0, "", turnStatusInfo{})
	if empty.ProjectId != "" {
		t.Errorf("an unassigned conversation reported project_id %q, want empty", empty.ProjectId)
	}
}
