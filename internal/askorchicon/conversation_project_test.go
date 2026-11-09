package askorchicon

// conversation_project_test.go — A CHAT KNOWS WHICH PROJECT FOLDER IT BELONGS TO, IN ALL THREE
// MODES.

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
		p := buildSystemPrompt(mode, cfg, reg, nil, true, nil, "", convProject, "")
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

// AND AN UNASSIGNED CHAT IS TOLD THAT, rather than being left to infer it from a missing line.
func TestAnUnassignedChatIsToldItHasNoProject(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	for _, mode := range everyMode {
		p := buildSystemPrompt(mode, cfg, reg, nil, true, nil, "", "", "")
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
	s := &Service{}
	r := db.ConversationRow{ID: "c1", TenantID: "t1", Title: "x", Mode: modeBrainstorm, ProjectID: "p-42"}
	got := s.conversationRowToProto(r, 0, "", turnStatusInfo{})
	if got.ProjectId != "p-42" {
		t.Errorf("Conversation.project_id = %q, want \"p-42\" — the client groups by this field", got.ProjectId)
	}
	// AND UNASSIGNED STAYS EMPTY rather than becoming a sentinel the clients would have to know about.
	empty := s.conversationRowToProto(db.ConversationRow{ID: "c2"}, 0, "", turnStatusInfo{})
	if empty.ProjectId != "" {
		t.Errorf("an unassigned conversation reported project_id %q, want empty", empty.ProjectId)
	}
}

// FULLSEND CROSSES THE PROTO BOUNDARY TOO, and the failure mode if it did not is worse than an
// empty grouping: a client that cannot read the flag renders a conversation as asking permission.
func TestConversationRowReportsFullsendOverTheAPI(t *testing.T) {
	s := &Service{fullsend: newFullsendStore()}
	r := db.ConversationRow{ID: "c1", TenantID: "t1"}
	if got := s.conversationRowToProto(r, 0, "", turnStatusInfo{}); got.Fullsend {
		t.Error("a fresh conversation reported fullsend ON — fullsend must never be the default")
	}
	s.fullsend.Set("c1", true)
	if got := s.conversationRowToProto(r, 0, "", turnStatusInfo{}); !got.Fullsend {
		t.Error("fullsend is on but the conversation reported it off — the indicator would read OFF while the gate is open")
	}
	// PER CONVERSATION, not per plane: arming one chat must not arm the next.
	if other := s.conversationRowToProto(db.ConversationRow{ID: "c2"}, 0, "", turnStatusInfo{}); other.Fullsend {
		t.Error("fullsend leaked to a different conversation")
	}
	// A SERVICE WITH NO STORE REPORTS OFF rather than panicking — the fail-closed
	// direction, so a store that was never initialised cannot silently approve everything.
	if bare := (&Service{}).conversationRowToProto(r, 0, "", turnStatusInfo{}); bare.Fullsend {
		t.Error("a Service with no fullsend store reported the mode as ON")
	}
}
