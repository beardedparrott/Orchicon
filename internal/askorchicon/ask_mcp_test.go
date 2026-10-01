package askorchicon

// ask_mcp_test.go — the NATIVE Ask MCP surface, end to end through the REAL tool
// choke point (AskToolDefs / ExecuteAskTool), with a fake scope resolver and a fake
// client. No server is spawned: the production connect seam (askMCPConnect) is
// substituted, the same test-seam pattern ask_file_root uses.
//
// WHAT IT PROVES (the work item's AC 2 native leg, AC 3, and AC 5's native half):
//
//   - a conversation in a project resolves the CONVERSATION scope (project ∪
//     conversation) and its discovered `mcp__<server>__<tool>` names are OFFERED in
//     a mode that may act;
//   - a call to one ROUTES to the client and returns its result;
//   - in Brainstorm / Quick Work the tool is neither offered NOR executed, and the
//     refusal names the mode and says the model cannot switch itself — from the SAME
//     table;
//   - the client is closed at conversation teardown, never per turn.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askMCPFakeResolver records the ScopeRef it was asked for and answers with a
// fixed resolution.
type askMCPFakeResolver struct {
	res mcpclient.Resolution
	got mcpclient.ScopeRef
	err error
}

func (f *askMCPFakeResolver) ResolveScope(_ context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	f.got = ref
	if f.err != nil {
		return mcpclient.Resolution{}, f.err
	}
	return f.res, nil
}

// askMCPFakeClient is a stand-in for *mcpclient.Manager: it reports fixed defs,
// records an executed call, and records its Close.
type askMCPFakeClient struct {
	defs    []mcpclient.ToolDef
	execOut string
	execErr error
	calls   []string
	closed  bool
}

func (c *askMCPFakeClient) Defs() []mcpclient.ToolDef { return c.defs }

func (c *askMCPFakeClient) Execute(_ context.Context, name, argsJSON string) (string, error) {
	c.calls = append(c.calls, name+"|"+argsJSON)
	if c.execErr != nil {
		return "", c.execErr
	}
	return c.execOut, nil
}

func (c *askMCPFakeClient) Close() error { c.closed = true; return nil }

// stubAskMCPConnect installs fn as the client-construction seam for one test.
func stubAskMCPConnect(fn func(ctx context.Context, specs []mcpclient.ServerSpec) (askMCPClient, error)) func() {
	old := askMCPConnect
	askMCPConnect = func(ctx context.Context, _ *slog.Logger, specs []mcpclient.ServerSpec) (askMCPClient, error) {
		return fn(ctx, specs)
	}
	return func() { askMCPConnect = old }
}

// opaqueMCPResolution is the fixture: one project-owned and one conversation-owned
// server, each advertising one tool.
func opaqueMCPResolution() mcpclient.Resolution {
	return mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "github", Type: mcpclient.TypeStdio, Command: []string{"/bin/github"}},
				From: mcpclient.ScopeProject, FromID: "project:p1", EntryID: "github"},
			{Spec: mcpclient.ServerSpec{ID: "notes", Type: mcpclient.TypeHTTP, URL: "https://mcp.notes"},
				From: mcpclient.ScopeConversation, FromID: "conversation:c1", EntryID: "notes"},
		},
		SelectedIDs: []string{"github", "notes"},
	}
}

// mcpDefsFor builds the two opaque defs a real manager would discover for the
// fixture above.
func mcpDefsFor() []mcpclient.ToolDef {
	return []mcpclient.ToolDef{
		{Name: mcpclient.ToolName("github", "create_issue"), Description: "Open an issue"},
		{Name: mcpclient.ToolName("notes", "append"), Description: "Append a note"},
	}
}

// mcpCtx builds the turn context the provider reads: tenant + conversation scope,
// under mode.
func mcpCtx(mode, convID, projectID string) context.Context {
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	ctx = withAskMode(ctx, mode)
	ctx = withAskConversation(ctx, convID)
	ctx = withAskConversationProject(ctx, projectID)
	return ctx
}

// AC 2 (native leg) + AC 3: the conversation's MCP tools are offered in Iteration
// and a call routes to the client.
func TestNativeAskOffersAndExecutesTheConversationsMCPTools(t *testing.T) {
	fake := &askMCPFakeClient{defs: mcpDefsFor(), execOut: "issue #42 opened"}
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		return fake, nil
	})
	defer restore()

	resolver := &askMCPFakeResolver{res: opaqueMCPResolution()}
	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(resolver)
	p := svc.NativeAskTools()

	ctx := mcpCtx(modeIteration, "c1", "p1")
	defs := p.AskToolDefs(ctx)
	var offered []string
	for _, d := range defs {
		offered = append(offered, d.Name)
	}
	for _, want := range []string{"mcp__github__create_issue", "mcp__notes__append"} {
		if !contains(offered, want) {
			t.Errorf("iteration was not offered %q — the conversation's MCP surface is missing: %v", want, offered)
		}
	}
	// AC 2 native leg: the scope asked for is the CONVERSATION, with BOTH ids — the
	// union of the project's and the conversation's servers.
	if resolver.got.Kind != mcpclient.ScopeConversation {
		t.Errorf("resolved scope = %q, want conversation", resolver.got.Kind)
	}
	if resolver.got.ProjectID != "p1" || resolver.got.ConversationID != "c1" {
		t.Errorf("resolved ref = %+v, want project p1 conversation c1 — the ids must not be empty", resolver.got)
	}

	// AC 3: a call routes to the client and returns its result.
	out, err := p.ExecuteAskTool(ctx, "mcp__github__create_issue", `{"title":"x"}`)
	if err != nil {
		t.Fatalf("ExecuteAskTool: %v", err)
	}
	if out != "issue #42 opened" {
		t.Errorf("result = %q, want the client's output", out)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "mcp__github__create_issue|{\"title\":\"x\"}" {
		t.Errorf("the call did not reach the client intact: %v", fake.calls)
	}
}

// AC 5 (native half): in a mode that may not act, the opaque MCP tool is NEITHER
// offered NOR executed, and the refusal names the mode and says the model cannot
// switch itself. Same table for offer and call.
func TestNativeAskRefusesOpaqueMCPOutsideAModeThatActs(t *testing.T) {
	fake := &askMCPFakeClient{defs: mcpDefsFor(), execOut: "should never be reached"}
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		return fake, nil
	})
	defer restore()

	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(&askMCPFakeResolver{res: opaqueMCPResolution()})
	p := svc.NativeAskTools()

	for _, mode := range []string{modeBrainstorm, modeQuickWork} {
		ctx := mcpCtx(mode, "c1", "p1")
		// NOT OFFERED: the defs filter drops it.
		for _, d := range p.AskToolDefs(ctx) {
			if strings.HasPrefix(d.Name, "mcp__github__") {
				t.Errorf("%s mode was OFFERED %q — the offered surface must not advertise an action the mode refuses", mode, d.Name)
			}
		}
		// NOT EXECUTED: the call guard refuses it, with a usable message.
		_, err := p.ExecuteAskTool(ctx, "mcp__github__create_issue", `{}`)
		if err == nil {
			t.Fatalf("%s executed an opaque MCP tool — a planning-mode conversation can act, the exact hole this closes", mode)
		}
		for _, want := range []string{modeLabel(mode), "REFUSED BY THE PLATFORM", "switch your own mode", "ASK THE USER"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s refusal is missing %q: %s", mode, want, err.Error())
			}
		}
		if len(fake.calls) != 0 {
			t.Errorf("%s reached the MCP client despite the refusal: %v", mode, fake.calls)
		}
	}
}

// A conversation that resolves NO servers offers no MCP tools and is never an error
// (parity with the worker path: "no servers, no MCP tools").
func TestNativeAskWithNoResolvedServersIsClean(t *testing.T) {
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		t.Fatal("the connect seam was called for a conversation with no servers")
		return nil, nil
	})
	defer restore()

	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(&askMCPFakeResolver{res: mcpclient.Resolution{}})
	p := svc.NativeAskTools()
	ctx := mcpCtx(modeIteration, "c1", "")
	for _, d := range p.AskToolDefs(ctx) {
		if strings.HasPrefix(d.Name, "mcp__") {
			t.Errorf("an MCP tool was offered for an empty resolution: %q", d.Name)
		}
	}
}

// A selected-but-unconfigured server (res.Missing) fails LOUD at call time rather
// than silently offering nothing.
func TestNativeAskFailsLoudOnAMissingSelection(t *testing.T) {
	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(&askMCPFakeResolver{res: mcpclient.Resolution{
		SelectedIDs: []string{"ghost"},
		Missing:     []string{"ghost"},
	}})
	p := svc.NativeAskTools()
	ctx := mcpCtx(modeIteration, "c1", "p1")
	if _, err := p.ExecuteAskTool(ctx, "mcp__ghost__do", `{}`); err == nil {
		t.Fatal("a selected-but-unconfigured server was accepted")
	} else if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the failure does not name the missing id: %v", err)
	}
}

// The client is closed at conversation teardown and NOT per turn. Two turns share
// ONE client (a per-turn close would re-spawn every stdio server per message).
func TestNativeAskMCPClientIsCachedPerConversationAndClosedOnTeardown(t *testing.T) {
	fake := &askMCPFakeClient{defs: mcpDefsFor(), execOut: "ok"}
	connects := 0
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		connects++
		return fake, nil
	})
	defer restore()

	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(&askMCPFakeResolver{res: opaqueMCPResolution()})
	p := svc.NativeAskTools()
	ctx := mcpCtx(modeIteration, "c1", "p1")

	a := p.AskToolDefs(ctx)
	b := p.AskToolDefs(ctx)
	if connects != 1 {
		t.Errorf("the client was constructed %d times for one conversation, want 1 (long-lived, cached)", connects)
	}
	if len(a) != len(b) {
		t.Errorf("the defs list changed between turns: %d then %d", len(a), len(b))
	}
	if fake.closed {
		t.Error("the client was closed mid-conversation")
	}
	svc.closeAskMCP("c1")
	if !fake.closed {
		t.Error("teardown did not close the conversation's MCP client")
	}
	// Idempotent.
	svc.closeAskMCP("c1")
	// CloseAskMCP (the plane-shutdown seam) also closes a live one.
	fake.closed = false
	_ = p.AskToolDefs(ctx)
	svc.CloseAskMCP()
	if !fake.closed {
		t.Error("plane shutdown did not close the conversation's MCP client")
	}
}

// A resolution error yields NO MCP defs and never fails the turn (the list-time
// contract: fail loud at CALL time, not at list time).
func TestNativeAskMCPResolutionErrorDegradesToNoMCPDefs(t *testing.T) {
	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(&askMCPFakeResolver{err: errors.New("storage down")})
	p := svc.NativeAskTools()
	ctx := mcpCtx(modeIteration, "c1", "p1")
	for _, d := range p.AskToolDefs(ctx) {
		if strings.HasPrefix(d.Name, "mcp__") {
			t.Errorf("an MCP tool was offered despite a resolution failure: %q", d.Name)
		}
	}
}

// A conversation whose PROJECT changed mid-life is re-resolved, not served by a
// stale client (the stale-scope defect).
func TestNativeAskRestartsTheClientWhenTheProjectChanges(t *testing.T) {
	fake := &askMCPFakeClient{defs: mcpDefsFor(), execOut: "ok"}
	resolver := &askMCPFakeResolver{res: opaqueMCPResolution()}
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		return fake, nil
	})
	defer restore()

	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(resolver)
	p := svc.NativeAskTools()

	_ = p.AskToolDefs(mcpCtx(modeIteration, "c1", "p1"))
	if resolver.got.ProjectID != "p1" {
		t.Fatalf("first resolve asked for project %q, want p1", resolver.got.ProjectID)
	}
	// The conversation moves to p2: the cached entry is invalidated and re-resolved
	// against the new project.
	_ = p.AskToolDefs(mcpCtx(modeIteration, "c1", "p2"))
	if resolver.got.ProjectID != "p2" {
		t.Errorf("after the move the resolver asked for %q, want p2 — a stale client would keep serving p1", resolver.got.ProjectID)
	}
	if !fake.closed {
		t.Error("the stale project's client was not closed before the restart")
	}
}

// AC 2 (native leg), THE CASE THE PROJECT CHECK ALONE MISSES: a conversation that
// started with NO project resolves nothing (a cached "none"), then GAINS a project.
// Without the per-turn reconciliation the cached empty answer would persist for the
// life of the conversation and the new project's servers would never be offered.
func TestNativeAskResolvesWhenAConversationGainsAProject(t *testing.T) {
	fake := &askMCPFakeClient{defs: mcpDefsFor(), execOut: "ok"}
	// The resolver answers by the project asked for: none ⇒ nothing.
	resolver := &askMCPProjectResolver{
		byProject: map[string]mcpclient.Resolution{"": {}, "p1": opaqueMCPResolution()},
	}
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		return fake, nil
	})
	defer restore()

	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(resolver)
	p := svc.NativeAskTools()

	// Turn 1: unassigned — no MCP tools, no client, and no error.
	for _, d := range p.AskToolDefs(mcpCtx(modeIteration, "c1", "")) {
		if strings.HasPrefix(d.Name, "mcp__") {
			t.Fatalf("a project-less conversation was offered an MCP tool: %q", d.Name)
		}
	}
	// Turn 2: the operator assigns p1 — its servers must now be offered.
	var offered []string
	for _, d := range p.AskToolDefs(mcpCtx(modeIteration, "c1", "p1")) {
		offered = append(offered, d.Name)
	}
	for _, want := range []string{"mcp__github__create_issue", "mcp__notes__append"} {
		if !contains(offered, want) {
			t.Errorf("after gaining a project the conversation was not offered %q — the cached empty answer was not reconciled: %v", want, offered)
		}
	}
	// And the call routes to the live client.
	if _, err := p.ExecuteAskTool(mcpCtx(modeIteration, "c1", "p1"), "mcp__github__create_issue", `{}`); err != nil {
		t.Fatalf("ExecuteAskTool after gaining a project: %v", err)
	}
}

// AC 2 (native leg), THE OTHER CASE: the conversation's OWN server is added between
// two turns while its project is unchanged. The project check cannot see this, so the
// client would keep serving the set resolved before the write. The fingerprint compare
// catches it and the new tool becomes offerable.
func TestNativeAskPicksUpAServerAddedMidConversation(t *testing.T) {
	fake := &askMCPFakeClient{defs: mcpDefsFor(), execOut: "ok"}
	resolver := &askMCPProjectResolver{byProject: map[string]mcpclient.Resolution{
		"p1": opaqueMCPResolution(),
	}}
	// A second resolution with ONE MORE conversation-owned server, swapped in below.
	grown := mcpclient.Resolution{
		Servers: append(append([]mcpclient.ScopedServer{}, opaqueMCPResolution().Servers...),
			mcpclient.ScopedServer{Spec: mcpclient.ServerSpec{ID: "late", Type: mcpclient.TypeStdio, Command: []string{"/bin/late"}},
				From: mcpclient.ScopeConversation, FromID: "conversation:c1", EntryID: "late"}),
		SelectedIDs: []string{"github", "notes", "late"},
	}
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		return fake, nil
	})
	defer restore()

	svc := &Service{toolRegistry: NewToolRegistry(nil, nil, nil)}
	svc.SetScopeResolver(resolver)
	p := svc.NativeAskTools()

	// Turn 1: the original set. One connect.
	_ = p.AskToolDefs(mcpCtx(modeIteration, "c1", "p1"))
	// The operator adds a server to the conversation (same project).
	resolver.byProject["p1"] = grown
	// Turn 2: the client must be replaced so the new server's tools are offerable.
	// The fake reports the grown set so the assertion reads the ROUTED defs.
	fake.defs = append(mcpDefsFor(), mcpclient.ToolDef{Name: mcpclient.ToolName("late", "do"), Description: "late"})
	var offered []string
	for _, d := range p.AskToolDefs(mcpCtx(modeIteration, "c1", "p1")) {
		offered = append(offered, d.Name)
	}
	if !contains(offered, "mcp__late__do") {
		t.Errorf("a server added mid-conversation was not picked up — the client was not reconciled: %v", offered)
	}
	if !fake.closed {
		t.Error("the stale set's client was not closed when it was replaced")
	}
}

// askMCPProjectResolver answers per-project, so a test can change what a project
// resolves BETWEEN turns (a server added, a conversation assigned a project).
// askMCPFakeResolver cannot: its answer is fixed.
// The write below is not concurrently raced in these tests (single goroutine).
type askMCPProjectResolver struct {
	byProject map[string]mcpclient.Resolution
	got       []mcpclient.ScopeRef
}

func (r *askMCPProjectResolver) ResolveScope(_ context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	r.got = append(r.got, ref)
	return r.byProject[ref.ProjectID], nil
}
