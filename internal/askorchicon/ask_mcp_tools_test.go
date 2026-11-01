package askorchicon

// ask_mcp_tools_test.go — THE TWO ANSWERS THAT CONTRADICTED EACH OTHER.
//
// REPORTED, and this is the root cause of a wasted session: for a conversation whose MCP servers all come from its
// PROJECT, `list_mcp_servers` with the conversation id answered "none". The tool passed only the conversation to
// ListForScope, so the query's project arm matched nothing — while the TURN resolved project ∪ conversation
// (mcpsettings.Resolver.resolveConversation) and registered two servers, which the platform log shows.
//
// The model believed the tool, concluded its mcp__aseprite__* tools were unavailable, and drove the server over
// bash for the rest of the session. So these assertions are about AGREEMENT as much as about output: a tool that
// contradicts the runtime is worse than no tool, because it is confidently wrong.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// projectOnlyConversation is THE REPORTED SHAPE: a conversation with no definitions of its own, whose PROJECT
// supplies an MCP server. The fixture is the platform's own end-to-end seed, so the server is a real
// streamable-HTTP one that answers tools/list.
func projectOnlyConversation(t *testing.T, pool *db.Pool) (convID, projectID string, serverID string) {
	t.Helper()
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})
	return seed.ConversationID, seed.ProjectID, seed.HTTPServerID
}

// TestListMCPServersWithAConversationIncludesItsProject is the fix for the contradiction.
func TestListMCPServersWithAConversationIncludesItsProject(t *testing.T) {
	pool := chatDBTestPool(t)
	convID, projectID, serverID := projectOnlyConversation(t, pool)
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	args, _ := json.Marshal(map[string]string{"conversation_id": convID})
	out, err := toolListMCPServers(ctx, pool, args)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, serverID) {
		t.Fatalf("the conversation-scoped list does not include the PROJECT's servers:\n%s\n\n"+
			"The turn resolves project ∪ conversation, so a list that reports only the conversation's own rows "+
			"contradicts what actually runs — and a model that believes it concludes its tools are unavailable.",
			text)
	}
	// Provenance, so an operator (or a model) can tell an inherited server from a conversation-owned one.
	if !strings.Contains(text, `"owned_by":"project"`) {
		t.Errorf("the list does not say the server is PROJECT-owned, so an inherited server is indistinguishable "+
			"from the conversation's own:\n%s", text)
	}
	_ = projectID
}

// TestListMCPServersDistinguishesOwnership — both scopes in one answer, each labelled.
func TestListMCPServersDistinguishesOwnership(t *testing.T) {
	pool := chatDBTestPool(t)
	convID, projectID, serverID := projectOnlyConversation(t, pool)
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	// Add a conversation-owned definition too.
	svc := mcpSvc(pool, nil)
	if _, err := svc.Create(ctx, "tnt_dev", mcpsettings.CreateInput{
		Name: "conv-only", Transport: "stdio", Command: "echo", ConversationID: convID, Enabled: true,
	}); err != nil {
		t.Fatalf("create conversation-owned server: %v", err)
	}

	args, _ := json.Marshal(map[string]string{"conversation_id": convID})
	out, err := toolListMCPServers(ctx, pool, args)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	text := string(out)
	for _, want := range []string{`"owned_by":"project"`, `"owned_by":"conversation"`, "conv-only"} {
		if !strings.Contains(text, want) {
			t.Errorf("the list is missing %q — the union must name where each entry came from:\n%s", want, text)
		}
	}
	// The PROJECT's server is still in the answer alongside the conversation's own.
	if !strings.Contains(text, serverID) {
		t.Errorf("the conversation-owned list dropped the project-supplied server %q:\n%s", serverID, text)
	}
	_ = projectID
}

// TestAskMCPToolsReportsTheMountedSurface is the structural fix: the question a model actually asks.
func TestAskMCPToolsReportsTheMountedSurface(t *testing.T) {
	pool := chatDBTestPool(t)
	convID, projectID, serverID := projectOnlyConversation(t, pool)
	svc := New(pool, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	ctx = withAskMode(ctx, modeIteration)
	ctx = withAskConversation(ctx, convID)
	ctx = withAskConversationProject(ctx, projectID)

	out, err := toolAskMCPTools(ctx, svc)
	if err != nil {
		t.Fatalf("ask_mcp_tools: %v", err)
	}
	var view askMCPToolsView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// THE SERVERS ARE REPORTED, with provenance — the answer list_mcp_servers got wrong.
	if len(view.Servers) == 0 {
		t.Fatalf("ask_mcp_tools reported no servers for a conversation whose project supplies them:\n%s", out)
	}
	var sawProjectProvenance, sawTheServer bool
	for _, s := range view.Servers {
		if strings.HasPrefix(s.Provenance, "project:") {
			sawProjectProvenance = true
		}
		if s.ID == serverID {
			sawTheServer = true
		}
	}
	// THE SEEDED SERVER IS THE ONE REPORTED. Without this the test would pass on any project-supplied server,
	// including one the fixture did not create.
	if !sawTheServer {
		t.Errorf("the project's own server %q is not in the mounted list:\n%s", serverID, out)
	}
	if !sawProjectProvenance {
		t.Errorf("no server is reported as project-supplied:\n%s", out)
	}

	// AND THE TOOLS — the names a model can actually call. This is the field the whole fix exists for.
	if len(view.Tools) == 0 {
		t.Fatalf("the servers are mounted but no tools were listed:\n%s", out)
	}
	for _, name := range view.Tools {
		if !strings.HasPrefix(name, "mcp__") {
			t.Errorf("tool %q does not carry the mcp__ prefix, so a model cannot tell it is an MCP tool", name)
		}
	}
	// An Iteration session may act, so nothing should be withheld here.
	if len(view.Hosted) > 0 {
		t.Errorf("tools were withheld in Iteration mode: %v", view.Hosted)
	}
}

// TestAskMCPToolsSaysWhyNothingIsOffered — the empty answer must not be a dead end.
func TestAskMCPToolsSaysWhyNothingIsOffered(t *testing.T) {
	pool := chatDBTestPool(t)
	convID := createConversation(t, pool, "m")
	svc := New(pool, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	ctx = withAskMode(ctx, modeIteration)
	ctx = withAskConversation(ctx, convID)

	out, err := toolAskMCPTools(ctx, svc)
	if err != nil {
		t.Fatalf("ask_mcp_tools: %v", err)
	}
	var view askMCPToolsView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Tools) != 0 {
		t.Fatalf("a conversation with no servers reported tools: %v", view.Tools)
	}
	if view.Note == "" {
		t.Fatal("an empty answer carried no note — that is the dead end that sent the reported session to bash")
	}
	// And it names the action, including the one thing not to do.
	for _, want := range []string{"list_mcp_servers", "do not attempt to drive an MCP server through a shell"} {
		if !strings.Contains(strings.ToLower(view.Note), strings.ToLower(want)) {
			t.Errorf("the note does not mention %q: %s", want, view.Note)
		}
	}
}

// TestTheToolIsOfferedInEveryMode — it must never be the thing a model cannot find.
//
// The probe is read-only, and a mode that cannot act is exactly where a model reasons hardest about capability.
// A self-check that vanished in Brainstorm would leave the mode most likely to plan around a phantom gap without
// the means to falsify it.
func TestTheToolIsOfferedInEveryMode(t *testing.T) {
	pool := chatDBTestPool(t)
	_ = pool
	reg := NewToolRegistry(nil, nil, nil)
	svc := New(nil, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	a := svc.NativeAskTools()

	for _, mode := range []string{modeIteration, modeBrainstorm, modeQuickWork} {
		ctx := withAskMode(context.Background(), mode)
		found := false
		for _, d := range a.AskToolDefs(ctx) {
			if d.Name == askMCPToolsToolName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s mode is not offered %s, so it cannot check its own MCP surface", mode, askMCPToolsToolName)
		}
	}
	// AND the check is not merely advertised: a mode may not run a listed tool it was not offered.
	if ok, _ := modeAllowsTool(modeBrainstorm, askMCPToolsToolName); !ok {
		t.Errorf("%s is filtered out of Brainstorm by the mode boundary — it is read-only introspection and must "+
			"survive every mode", askMCPToolsToolName)
	}
	_ = reg
	_ = db.MessageRow{}
}
