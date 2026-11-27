package antigravity

// ask_mcp_scope_test.go — the antigravity Ask half of scope item 5: the platform's `orchicon_ask_user` card
// only works if the Orchicon MCP server is actually IN the conversation's `mcpServers`, and the operator's own
// servers are scoped to the CONVERSATION (not the worker) with the conversation's real ids.
//
// THIS IS THE CLAUDE ANALOGUE of internal/claude/ask_mcp_scope_test.go, deliberately asserting the same three
// things — the scope ref carries the project AND conversation ids, the resolved servers reach the session, and
// the platform's own server is present even when resolution FAILS — because the two adapters must not diverge
// on the one surface that makes the ask card reachable at all.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// TestAskResolvesTheConversationScopeWithRealIDs: an Ask conversation resolves scope CONVERSATION with the
// conversation's project id and conversation id — empty ids are the defect this closes, because a resolver
// given nothing cannot return the conversation's servers.
func TestAskResolvesTheConversationScopeWithRealIDs(t *testing.T) {
	h := newAskHarness(t, script{})
	resolver := &fakeScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "proj-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/proj"}},
				From: mcpclient.ScopeProject, FromID: "project:prj_1", EntryID: "proj-srv"},
			{Spec: mcpclient.ServerSpec{ID: "conv-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/conv"}},
				From: mcpclient.ScopeConversation, FromID: "conversation:conv_agy_1", EntryID: "conv-srv"},
		},
		SelectedIDs: []string{"proj-srv", "conv-srv"},
	}}
	h.b.SetScopeResolver(resolver)

	// The conversation is created with a project so the ref has a project id to carry.
	ctx := askmode.WithConversationScope(h.turnCtx(askmode.Iteration),
		askmode.ConversationScope{ConversationID: "conv_agy_1", ProjectID: "prj_1"})
	if _, err := h.b.CreateConversationSession(ctx, "conv_agy_1", "ask-orchicon:conv_agy_1"); err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}

	if len(resolver.refs) == 0 {
		t.Fatal("the scope resolver was never consulted — no MCP server reached the conversation")
	}
	last := resolver.refs[len(resolver.refs)-1]
	if last.Kind != mcpclient.ScopeConversation {
		t.Errorf("resolved scope = %q, want %q", last.Kind, mcpclient.ScopeConversation)
	}
	if last.ConversationID != "conv_agy_1" {
		t.Errorf("resolved conversation id = %q, want %q — an empty id cannot return the conversation's own "+
			"servers, and the ask card lives on one", last.ConversationID, "conv_agy_1")
	}

	// THE WIRE: the resolved servers are in `session/new`'s mcpServers, and `orchicon` is one of them.
	servers := mcpServersFromSessionNew(t, h.proc(0))
	for _, want := range []string{"proj-srv", "conv-srv", "orchicon"} {
		if !hasServer(servers, want) {
			t.Errorf("session/new mcpServers = %q, want it to contain %q", servers, want)
		}
	}
}

// TestAskRegistersTheOrchiconServerWhenResolutionFails: a resolver failure must NOT leave the conversation
// with no platform tools at all. `orchicon_ask_user` lives on the Orchicon MCP server, so losing it silently
// would remove the ask card — the one interaction the conversation exists to have — and the model would
// simply report that it cannot ask.
func TestAskRegistersTheOrchiconServerWhenResolutionFails(t *testing.T) {
	h := newAskHarness(t, script{})
	h.b.SetScopeResolver(&fakeScopeResolver{err: context.DeadlineExceeded})

	if _, err := h.b.CreateConversationSession(h.turnCtx(askmode.Iteration), "conv_agy_1", "ask-orchicon:conv_agy_1"); err != nil {
		t.Fatalf("CreateConversationSession with a failing resolver: %v", err)
	}

	servers := mcpServersFromSessionNew(t, h.proc(0))
	if !hasServer(servers, "orchicon") {
		t.Fatalf("session/new mcpServers = %q, want the built-in orchicon server — a failed resolution must "+
			"not remove the only channel an ask card can arrive on", servers)
	}
}

// TestAskModeThatMayNotActWithholdsOpaqueMCP: Brainstorm and Quick Work may not ACT, so a third-party MCP
// server is not OFFERED to the session at all — but the platform's own server still is, because the ask card
// is not an action. This is the offered half of askmode.MayAct, and it is what stops a Brainstorm
// conversation being handed `mcp_github_create_issue` while the permission handler would refuse it anyway.
func TestAskModeThatMayNotActWithholdsOpaqueMCP(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		wantOpaque bool
	}{
		{askmode.Brainstorm, false},
		{askmode.QuickWork, false},
		{askmode.Iteration, true},
	} {
		h := newAskHarness(t, script{})
		h.b.SetScopeResolver(&fakeScopeResolver{res: mcpclient.Resolution{
			Servers: []mcpclient.ScopedServer{
				{Spec: mcpclient.ServerSpec{ID: "github", Type: mcpclient.TypeStdio, Command: []string{"/bin/gh"}},
					From: mcpclient.ScopeProject, FromID: "project:prj_1", EntryID: "github"},
			},
			SelectedIDs: []string{"github"},
		}})
		h.create(tc.mode)

		servers := mcpServersFromSessionNew(t, h.proc(0))
		if !hasServer(servers, "orchicon") {
			t.Errorf("%s: session/new mcpServers = %q, want the platform's own server (the ask card must "+
				"survive in every mode)", tc.mode, servers)
		}
		if got := hasServer(servers, "github"); got != tc.wantOpaque {
			t.Errorf("%s: opaque server offered = %v, want %v (servers = %q)", tc.mode, got, tc.wantOpaque, servers)
		}
	}
}

// hasServer reports whether a server NAME is in the list (case-insensitive, exact element match — a substring
// test would make "orchicon" match "mcp_orchicon_orchicon_ask_user" and hide a missing server).
func hasServer(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// mcpServersFromSessionNew reads the server NAMES out of the child's `session/new` params.
func mcpServersFromSessionNew(t *testing.T, fp *fakeACP) []string {
	t.Helper()
	params := fp.sessionNewParams()
	if len(params) == 0 {
		t.Fatal("the child received no session/new, so no MCP server could have been registered")
	}
	var body struct {
		MCPServers []struct {
			Name string `json:"name"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(params[0], &body); err != nil {
		t.Fatalf("decode session/new params: %v", err)
	}
	names := make([]string, 0, len(body.MCPServers))
	for _, s := range body.MCPServers {
		names = append(names, strings.TrimSpace(s.Name))
	}
	return names
}
