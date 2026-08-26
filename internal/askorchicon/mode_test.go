package askorchicon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func TestBuildSystemPromptBrainstorm(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	p := BuildSystemPrompt(modeBrainstorm, cfg, reg)
	for _, want := range []string{
		"What can I help you create today?",
		"## Purpose",
		"## Available Tools",
		"`orchicon_list_projects`",
		"`orchicon_create_work_item`",
		"## Additional Instructions",
		"Tenant additional instructions.",
		"## About Orchicon",
		"Be planner first, implementer second",
		"Workflow & runtime prompt",
		"workflow they want to bind",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("brainstorm prompt missing %q", want)
		}
	}
}

func TestConversationModeFromProto(t *testing.T) {
	cases := []struct {
		name string
		m    apiv1.ConversationMode
		want string
	}{
		{"unspecified defaults to brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED, modeBrainstorm},
		{"brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM, modeBrainstorm},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := conversationModeFromProto(c.m)
			if err != nil {
				t.Fatalf("conversationModeFromProto(%v) error: %v", c.m, err)
			}
			if got != c.want {
				t.Errorf("conversationModeFromProto(%v) = %q, want %q", c.m, got, c.want)
			}
		})
	}
	_, err := conversationModeFromProto(apiv1.ConversationMode(99))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("unknown mode error = %v, want CodeInvalidArgument", err)
	}
}

func TestConversationModeToProto(t *testing.T) {
	if got := conversationModeToProto(modeBrainstorm); got != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Errorf("brainstorm -> %v, want BRAINSTORM", got)
	}
	if got := conversationModeToProto("nope"); got != apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED {
		t.Errorf("unknown -> %v, want UNSPECIFIED", got)
	}
}

func TestSetConversationMode(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")
	resp, err := s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: convID, Mode: apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM,
	}))
	if err != nil {
		t.Fatalf("set mode: %v", err)
	}
	if resp.Msg.Conversation.Mode != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Errorf("returned mode = %v, want BRAINSTORM", resp.Msg.Conversation.Mode)
	}
}

func TestSetConversationModeValidation(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")
	_, err := s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: convID, Mode: apiv1.ConversationMode(99),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("unknown mode error = %v, want InvalidArgument", err)
	}
	_, err = s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{Id: "", Mode: apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("empty id error = %v, want InvalidArgument", err)
	}
	_, err = s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{Id: "does-not-exist", Mode: apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("missing conversation error = %v, want NotFound", err)
	}
}

func TestCreateConversationWithMode(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	resp, err := s.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{Mode: apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Msg.Conversation.Mode != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Errorf("created mode = %v, want BRAINSTORM", resp.Msg.Conversation.Mode)
	}
}

func TestModePersistsAcrossTurns(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")
	ack1, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	waitForSend(t, client, 1)
	sys1 := client.sendCalls[0].system
	if !strings.Contains(sys1, "What can I help you create today?") {
		t.Errorf("first turn system is not brainstorm")
	}
	if strings.Contains(strings.ToLower(sys1), "refuse general coding help") {
		t.Error("leaked governed rules")
	}
	ses1 := client.sendCalls[0].sessionID
	client.sub.feed(busText(ses1, "brainstorm reply"))
	client.sub.feed(busIdle(ses1))
	waitForMessage(t, pool, convID, ack1)
}

func testToolRegistry() *ToolRegistry {
	r := NewToolRegistry(nil, nil)
	r.Add(ToolDefinition{Name: "list_projects", Description: "List projects"})
	r.Add(ToolDefinition{Name: "create_work_item", Description: "Create work item", Mutating: true})
	return r
}
func testAgentConfig() db.AgentConfigRow {
	return db.AgentConfigRow{Role: "x", Skills: "y", Behavior: "z", SystemPrompt: "Tenant additional instructions."}
}
