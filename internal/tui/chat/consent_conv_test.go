package chat

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestPermissionAskFromProtoCarriesTheConversation pins the adapter half of the ask-bleed fix: the
// wire's conversation_id must survive onto the TUI's model.
func TestPermissionAskFromProtoCarriesTheConversation(t *testing.T) {
	perm := PermissionAskFromProto(&apiv1.PermissionAsk{
		AskId: "a1", ConversationId: "c-perm", Tool: "write",
		Targets: []string{"/x/main.go"},
	})
	if perm.ConvID != "c-perm" {
		t.Errorf("permission ask ConvID = %q, want c-perm", perm.ConvID)
	}
	if perm.Kind != AskTool {
		t.Errorf("precondition: want a tool ask, got kind %q", perm.Kind)
	}

	q := PermissionAskFromProto(&apiv1.PermissionAsk{
		AskId: "a2", ConversationId: "c-q", Question: "Which branch?",
		Options: []string{"develop", "main"}, AllowOther: true,
	})
	if q.ConvID != "c-q" {
		t.Errorf("question ask ConvID = %q, want c-q", q.ConvID)
	}
	if q.Kind != AskQuestion {
		t.Errorf("precondition: want a question ask, got kind %q", q.Kind)
	}
}
