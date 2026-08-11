package askorchicon

// Task 4 mode-model tests: the per-mode persona prompts, the mode
// validation/mapping helpers, and (DB-backed) the SetConversationMode RPC +
// per-turn mode application over the session transport.
//
// DB-backed tests skip unless ORCHICON_TEST_DSN points at a disposable
// database (the pattern used across the repo):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'TestMode|TestSetConversationMode|TestBuildSystemPrompt' -v

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

// --- Per-mode prompt assembly (pure unit tests, no DB) --------------------

// testToolRegistry returns a registry with one read-only + one mutating tool
// so prompt tests can assert the tools listing is emitted.
func testToolRegistry() *ToolRegistry {
	r := NewToolRegistry(nil, nil)
	r.Add(ToolDefinition{
		Name:        "list_projects",
		Description: "List the tenant's projects",
	})
	r.Add(ToolDefinition{
		Name:        "create_work_item",
		Description: "Create a work item",
		Mutating:    true,
	})
	return r
}

func testAgentConfig() db.AgentConfigRow {
	return db.AgentConfigRow{
		Role:          "You are a governed Orchicon expert.",
		Skills:        "Operate the platform.",
		Behavior:      "Refuse requests outside Orchicon.",
		SystemPrompt:  "Tenant additional instructions.",
	}
}

// TestBuildSystemPromptBrainstorm verifies the default persona: opens with the
// greeting, general design/coding/brainstorming is in scope (no governed
// refusal rules), the full tool list is present, and the DB Additional
// Instructions are applied while role/skills/behavior (Orchicon-mode-only)
// are NOT.
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
	} {
		if !strings.Contains(p, want) {
			t.Errorf("brainstorm prompt missing %q", want)
		}
	}
	for _, banned := range []string{
		"refuse general coding help",
		"Refuse any request that falls outside Orchicon",
		"## Role",
		"## Skills",
		"## Behavior",
		"governed Orchicon expert.",
		"## Mode Awareness",
	} {
		if strings.Contains(p, banned) {
			t.Errorf("brainstorm prompt must not contain %q (governed rules leak into the open mode)", banned)
		}
	}
}

// TestBuildSystemPromptOrchicon verifies the governed persona: the refusal
// rules, the DB role/skills/behavior customization, the tools list, and the
// Additional Instructions are all present.
func TestBuildSystemPromptOrchicon(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	p := BuildSystemPrompt(modeOrchicon, cfg, reg)

	for _, want := range []string{
		"You are Orchicon, an AI assistant for the Orchicon platform.",
		"Refuse any request that falls outside Orchicon",
		"general knowledge, coding help unrelated to Orchicon",
		"## Mode Awareness",
		"switch to **Brainstorm mode**",
		"## Project Awareness",
		"## Role",
		"## Skills",
		"## Behavior",
		"## Available Tools",
		"## Additional Instructions",
		"governed Orchicon expert.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("orchicon prompt missing %q", want)
		}
	}
	if strings.Contains(p, "What can I help you create today?") {
		t.Error("orchicon prompt must not contain the brainstorm greeting")
	}
}

// legacyBuildSystemPrompt is the pre-Task-4 single BuildSystemPrompt, captured
// verbatim from git history. TestBuildSystemPromptOrchiconByteIdentical pins
// the governed mode to it so the Orchicon persona can never drift.
func legacyBuildSystemPrompt(cfg db.AgentConfigRow, toolRegistry *ToolRegistry) string {
	var b strings.Builder

	b.WriteString(`You are Orchicon, an AI assistant for the Orchicon platform.

## Purpose
Your purpose is to help users accomplish tasks inside Orchicon and to answer questions about Orchicon and their projects. You can create, read, update, and delete any first-class Orchicon entity, diagnose failures, and create project directory scaffolding.

## Identity
- You are Orchicon. You are not Claude, ChatGPT, or any other AI assistant.
- You are an integral part of the Orchicon control plane.
- You speak in first person as Orchicon.

## Absolute Rules
1. ALWAYS ask clarifying questions before performing any action that creates, updates, or deletes data. Never assume the user's intent.
2. Refuse any request that falls outside Orchicon, the user's Orchicon projects, or legitimate development assistance related to those projects.
3. Do not engage in general knowledge, coding help unrelated to Orchicon, or personal conversation.
4. When the user asks you to create a new project, ask "Do you have a project directory in mind or would you like me to create one?"
5. When the user's request is ambiguous, ask for clarification before proceeding.
6. Explain your plan before executing multi-step operations.
7. Be concise in your responses unless the user asks for detail.
8. Use the Orchicon tools listed below for all operations. Do NOT use general-purpose tools like terminal, file_edit, grep, glob, or web_fetch — use the Orchicon tools instead.
9. When asked to create a project directory, use the create_project_directory tool — do not try to create directories via shell commands.
`)

	if cfg.Role != "" {
		b.WriteString("\n## Role\n")
		b.WriteString(cfg.Role)
		b.WriteString("\n")
	}
	if cfg.Skills != "" {
		b.WriteString("\n## Skills\n")
		b.WriteString(cfg.Skills)
		b.WriteString("\n")
	}
	if cfg.Behavior != "" {
		b.WriteString("\n## Behavior\n")
		b.WriteString(cfg.Behavior)
		b.WriteString("\n")
	}
	if cfg.AgentsMD != "" {
		b.WriteString("\n## AGENTS.md\n")
		b.WriteString(cfg.AgentsMD)
		b.WriteString("\n")
	}

	b.WriteString(`
## Mode Awareness
You are currently in Orchicon mode — the governed platform expert. You can only
help with tasks inside Orchicon: managing projects, workers, work items, workflows,
policies, and understanding platform state.

If the user asks for something outside Orchicon's scope (general coding help,
design brainstorming, architecture discussion, or any non-platform task), tell
them:

"I'm in Orchicon mode, which is focused on platform operations. For general help
with coding, design, or brainstorming, switch to **Brainstorm mode** using the
dropdown in the chat input."

Do NOT attempt to fulfill out-of-scope requests. Do NOT pretend you can help
with general tasks. Redirect to Brainstorm mode.
`)

	b.WriteString(`
## Project Awareness
You have access to the user's Orchicon projects and can see their current status
(project directory, context files, enabled state). Use the orchicon_* tools to
inspect project details when relevant.
`)

	b.WriteString(`
## About Orchicon
Orchicon is an AI orchestration platform. It separates orchestration from execution: Orchicon orchestrates, runtimes execute.

- **Control plane**: a single Go binary running k8s-style reconcilers that converge the world state on the desired state. The API is Protobuf + Connect (gRPC + REST + streaming); data lives in PostgreSQL with row-level security.
- **First-class entities**: Projects (each with a project_dir and context_files), Workers (draft → published → deprecated → retired; published versions are immutable), Work Items (Epic → Feature → Task → Subtask, max 4 levels, forming a DAG), Workflows (step DAGs with gates) and their Runs, Worker Executions, Policies (Rego/OPA), Approvals, Webhooks, Recoveries, and tenant Settings.
- **Execution**: the TaskReconciler creates WorkerExecutions for ready work items and dispatches them to a runtime adapter (opencode) — in-process or inside a per-workflow runtime container. A worker's model_ref is pinned by a human; there is no automatic model failover.
- **Recovery**: execution failures are recoverable by default (opt-out). The recovery flow captures → summarizes → preserves → reviews → plans → resumes, with bounded auto-relax and L1→L2→L3 escalation.
- **Telemetry**: OpenTelemetry → Grafana stack (Tempo traces, Loki logs, VictoriaMetrics metrics).
- **Deployment**: the whole stack runs in one container (Postgres, NATS, Grafana plane, control plane) via the orchicon container subcommand; orchicon install brings it up with one command.
- **Projects**: a project's project_dir is where workers operate; context_files are injected into prompts (a context path may be a file or a directory — directories are listed and read in full by the worker). Work items can also carry their own context_files, rendered into the worker's prompt exactly like the project's. Workers must operate within their assigned project directory. To see or read a project's files, use the list_project_dir and read_project_file tools — your session has no filesystem access of its own.
`)

	b.WriteString("\n## Available Tools\n")
	b.WriteString("Orchicon's tools are available to you as MCP tools named `orchicon_<tool>` — call them directly through your tool mechanism and the system executes them against Orchicon, returning real results. Mutating tools run only after user confirmation.\n\n")
	for _, td := range toolRegistry.List() {
		mutability := "read-only"
		if td.Mutating {
			mutability = "mutates data — requires user confirmation"
		}
		b.WriteString("- `orchicon_" + td.Name + "`: " + td.Description + " (" + mutability + ")\n")
	}
	b.WriteString("\nAlways use these tools to perform actions. Do not simulate actions — call the appropriate tool.")

	if cfg.SystemPrompt != "" {
		b.WriteString("\n\n## Additional Instructions\n")
		b.WriteString(cfg.SystemPrompt)
		b.WriteString("\n")
	}

	return b.String()
}

// TestBuildSystemPromptOrchiconByteIdentical pins the governed persona to the
// pre-Task-4 output: the mode split must never change what Orchicon mode
// sends to the model.
func TestBuildSystemPromptOrchiconByteIdentical(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	got := BuildSystemPrompt(modeOrchicon, cfg, reg)
	want := legacyBuildSystemPrompt(cfg, reg)
	if got != want {
		t.Errorf("orchicon prompt drifted from the pre-Task-4 output:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// --- Mode validation/mapping helpers ---------------------------------------

func TestConversationModeFromProto(t *testing.T) {
	cases := []struct {
		name string
		m    apiv1.ConversationMode
		want string
	}{
		{"unspecified defaults to brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED, modeBrainstorm},
		{"brainstorm", apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM, modeBrainstorm},
		{"orchicon", apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON, modeOrchicon},
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
	// Unknown enum ints on the wire must be rejected, never silently coerced.
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
	if got := conversationModeToProto(modeOrchicon); got != apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON {
		t.Errorf("orchicon -> %v, want ORCHICON", got)
	}
	if got := conversationModeToProto("nope"); got != apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED {
		t.Errorf("unknown -> %v, want UNSPECIFIED", got)
	}
}

// --- SetConversationMode RPC (DB-backed) -----------------------------------

func TestSetConversationMode(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	convID := createConversation(t, pool, "")

	// New conversations default to brainstorm (the migration default).
	resp, err := s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id:   convID,
		Mode: apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON,
	}))
	if err != nil {
		t.Fatalf("set mode: %v", err)
	}
	if resp.Msg.Conversation.Mode != apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON {
		t.Errorf("returned mode = %v, want ORCHICON", resp.Msg.Conversation.Mode)
	}

	// The DB row is authoritative and GetConversation reflects the change.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	row, err := db.GetConversation(ctx, ttx.Tx, "tnt_dev", convID)
	ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if row.Mode != modeOrchicon {
		t.Errorf("db mode = %q, want %q", row.Mode, modeOrchicon)
	}

	// Toggle back to brainstorm.
	resp, err = s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: convID, Mode: apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM,
	}))
	if err != nil {
		t.Fatalf("set mode back: %v", err)
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

	// Unknown enum value -> InvalidArgument.
	_, err := s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: convID, Mode: apiv1.ConversationMode(99),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("unknown mode error = %v, want InvalidArgument", err)
	}

	// Empty id -> InvalidArgument.
	_, err = s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: "", Mode: apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON,
	}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("empty id error = %v, want InvalidArgument", err)
	}

	// Missing conversation -> NotFound.
	_, err = s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: "does-not-exist", Mode: apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON,
	}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("missing conversation error = %v, want NotFound", err)
	}
}

// TestCreateConversationWithMode verifies create-with-mode: an explicit mode
// is persisted, UNSPECIFIED falls back to brainstorm, and an unknown value is
// rejected.
func TestCreateConversationWithMode(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	resp, err := s.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
		Mode: apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON,
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Msg.Conversation.Mode != apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON {
		t.Errorf("created mode = %v, want ORCHICON", resp.Msg.Conversation.Mode)
	}

	resp, err = s.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{}))
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	if resp.Msg.Conversation.Mode != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Errorf("default mode = %v, want BRAINSTORM", resp.Msg.Conversation.Mode)
	}

	_, err = s.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
		Mode: apiv1.ConversationMode(42),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("create unknown mode error = %v, want InvalidArgument", err)
	}
}

// TestModePersistsAcrossTurns verifies the acceptance criteria end to end:
// the mode is applied per message as the per-turn system prompt, and a
// mid-conversation toggle changes the NEXT message's prompt on the SAME
// opencode session — no session change, no serve restart.
func TestModePersistsAcrossTurns(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	convID := createConversation(t, pool, "")

	// Turn 1: default brainstorm persona on a fresh session.
	ack1, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	waitForSend(t, client, 1)
	sys1 := client.sendCalls[0].system
	if !strings.Contains(sys1, "What can I help you create today?") {
		t.Errorf("first turn system is not the brainstorm persona (len %d)", len(sys1))
	}
	if strings.Contains(strings.ToLower(sys1), "refuse general coding help") {
		t.Error("first turn system leaked governed rules into brainstorm")
	}
	ses1 := client.sendCalls[0].sessionID
	client.sub.feed(busText(ses1, "brainstorm reply"))
	client.sub.feed(busIdle(ses1))
	waitForMessage(t, pool, convID, ack1)

	// Toggle to Orchicon mid-conversation.
	_, err = s.SetConversationMode(ctx, connect.NewRequest(&apiv1.SetConversationModeRequest{
		Id: convID, Mode: apiv1.ConversationMode_CONVERSATION_MODE_ORCHICON,
	}))
	if err != nil {
		t.Fatalf("set mode: %v", err)
	}

	// Turn 2: SAME session (reuse — no CreateSession), governed persona.
	ack2, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "follow up", nil)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	waitForSend(t, client, 2)
	if got := client.sendCalls[1]; got.sessionID != ses1 {
		t.Errorf("second turn session = %q, want %q (same opencode session across the mode switch)", got.sessionID, ses1)
	}
	sys2 := client.sendCalls[1].system
	if !strings.Contains(sys2, "Refuse any request that falls outside Orchicon") {
		t.Errorf("second turn system is not the governed Orchicon persona (len %d)", len(sys2))
	}
	client.sub.feed(busText(ses1, "orchicon reply"))
	client.sub.feed(busIdle(ses1))
	waitForMessage(t, pool, convID, ack2)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 1 {
		t.Errorf("created sessions = %d, want 1 (the mode switch must not create a session)", len(client.created))
	}
}
