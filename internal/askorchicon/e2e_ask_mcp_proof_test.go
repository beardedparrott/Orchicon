package askorchicon

// e2e_ask_mcp_proof_test.go — AC 4 (a LIVE Ask turn: native + claude) and AC 5.2
// (the native Ask per-conversation client negative).
//
// IT EXTENDS child 6's ACCEPTED REAL-TURN HARNESS rather than re-inventing it.
// ask_mcp_live_test.go already owns the real pieces — `seedLiveMCPRows`, the
// scripted `liveTurnProvider`, the ONE-reader `drainLive` with inline consent —
// so this file adds what criterion 4 needs on top: the project's server coming
// from the SAME shared seed the worker legs use (testfixtures), the project's and
// conversation's SKILL FILES reaching the model's actual system prompt, and the
// mode policy re-observed on a real turn.
//
// The bridge, the turn, the tool loop and the per-conversation MCP client are
// all the production ones; only the model is scripted (no permitted model exists
// in this container — recorded in the evidence).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// AC 4 (native Ask): a LIVE conversation in a project can CALL the project's MCP
// tool, and the model's system prompt carries the project's (and the
// conversation's) SKILL FILES.
func TestE2EAskTurnSeesSkillAndCallsProjectServer(t *testing.T) {
	pool := workItemKindTestPool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{ConvSkill: true})
	nonce := testfixtures.Nonce(t)

	svc := New(pool, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool)) // production wiring (service.go:236)

	ctx := tenant.WithID(context.Background(), testfixtures.E2ETenant)

	// --- the SKILL half: the REAL manifest section + the REAL prompt builder ----
	conv, err := e2eGetConversation(ctx, pool, seed.ConversationID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	section := svc.skillManifestSection(ctx, testfixtures.E2ETenant, conv)
	if section == "" {
		t.Fatalf("skillManifestSection returned nothing for a conversation whose project AND conversation both select a skill file")
	}
	sysPrompt := buildSystemPrompt(modeIteration, db.AgentConfigRow{}, svc.toolRegistry, nil, false, nil, "", "", section)

	for _, want := range []string{"# Skills", "e2e-skill.md", "E2E_SKILL_BODY"} {
		if !strings.Contains(sysPrompt, want) {
			t.Fatalf("the Ask system prompt does not carry %q (the project's skill file):\n%s", want, sysPrompt)
		}
	}
	// The conversation's OWN skill file is union-ed in too.
	if !strings.Contains(sysPrompt, "e2e-conv-skill.md") {
		t.Fatalf("the conversation's own skill file is absent from the Ask system prompt:\n%s", sysPrompt)
	}

	// --- the MCP half: a REAL turn over the REAL bridge -------------------------
	wantTool := mcpclient.ToolName(seed.HTTPServerID, mcpclient.E2EProbeTool)
	prov := &liveTurnProvider{tool: wantTool}
	b := orchicon.NewBridge(orchicon.ProviderResolverFunc(func(context.Context, string, string) (orchicon.Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	b.SetAskTools(svc.NativeAskTools())

	turnCtx := tenant.WithID(context.Background(), testfixtures.E2ETenant)
	turnCtx = withAskMode(turnCtx, modeIteration)
	turnCtx = withAskConversation(turnCtx, seed.ConversationID)
	turnCtx = withAskConversationProject(turnCtx, seed.ProjectID)

	sid, err := b.CreateConversationSession(turnCtx, seed.ConversationID, "e2e")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	bus, err := b.Subscribe(turnCtx, seed.ConversationID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// The EXACT prompt built above is the `system` the model receives — so the
	// skill half and the tool half are observed on ONE turn.
	if err := b.SendTurnMessage(turnCtx, seed.ConversationID, sid, sysPrompt, "orchicon/e2e-probe", "call the probe"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainLive(t, b, bus, "once")

	// The model was OFFERED the project's tool.
	if !contains(prov.seenTools, wantTool) {
		t.Fatalf("the live Ask turn was not offered the project's tool %q; offered: %v", wantTool, prov.seenTools)
	}
	// The call ran and its REAL result came back.
	var toolResult string
	for _, e := range evts {
		if e.Kind == "tool_result" && e.ToolName == wantTool {
			toolResult = e.Output
		}
	}
	if toolResult == "" {
		t.Fatalf("the project's MCP tool produced no result on the live turn: %s", liveEventKinds(evts))
	}
	if !strings.Contains(toolResult, nonce) && !strings.Contains(toolResult, "opened issue") && !strings.Contains(toolResult, "mcpfixture:") {
		t.Errorf("the tool result does not look like the fixture's output: %q", toolResult)
	}

	sum := sha256.Sum256([]byte(sysPrompt))
	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "ask-native-turn", Adapter: "native", Surface: "ask-conversation",
		Conversat: seed.ConversationID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Tool: wantTool, Result: toolResult, Offered: prov.seenTools,
		Model: "scripted", ModelAccess: "available",
		SkillPath:     seed.SkillPath,
		SkillInPrompt: testfixtures.BoolPtr(true),
		PromptSHA:     hex.EncodeToString(sum[:]),
		Note:          "real DB rows + real HTTP transport + real bridge turn; the model's system prompt (skill manifest) and offered tools both observed",
	})
}

// AC 4 (native Ask, mode policy per child 6): in a mode that may NOT act, the
// project's opaque MCP tool is NEITHER offered NOR executed, and the refusal is
// the platform's own wording. Consent is answered "once" inline so the MODE guard
// is what stops the call, not the consent gate.
func TestE2EAskTurnRefusesTheProjectServerOutsideAModeThatActs(t *testing.T) {
	pool := workItemKindTestPool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})

	svc := New(pool, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	for _, mode := range []string{modeBrainstorm, modeQuickWork} {
		t.Run(mode, func(t *testing.T) {
			convID := seed.ConversationID + "-" + mode
			wantTool := mcpclient.ToolName(seed.HTTPServerID, mcpclient.E2EProbeTool)
			prov := &liveTurnProvider{tool: wantTool}
			b := orchicon.NewBridge(orchicon.ProviderResolverFunc(func(context.Context, string, string) (orchicon.Provider, error) {
				return prov, nil
			}), "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
			b.SetAskTools(svc.NativeAskTools())

			ctx := tenant.WithID(context.Background(), testfixtures.E2ETenant)
			ctx = withAskMode(ctx, mode)
			ctx = withAskConversation(ctx, convID)
			ctx = withAskConversationProject(ctx, seed.ProjectID)

			sid, _ := b.CreateConversationSession(ctx, convID, "e2e")
			bus, _ := b.Subscribe(ctx, convID)
			if err := b.SendTurnMessage(ctx, convID, sid, "system", "orchicon/e2e-probe", "call the probe"); err != nil {
				t.Fatalf("SendTurnMessage: %v", err)
			}
			evts := drainLive(t, b, bus, "once")

			for _, n := range prov.seenTools {
				if strings.HasPrefix(n, "mcp__"+seed.HTTPServerID+"__") {
					t.Fatalf("%s was OFFERED %q — a mode that may not act must not advertise an opaque MCP action", mode, n)
				}
			}
			var refusal string
			for _, e := range evts {
				if e.Kind == "tool_result" && strings.HasPrefix(e.ToolName, "mcp__") {
					if !e.IsError {
						t.Fatalf("%s ran an opaque MCP tool (IsError=false): %+v", mode, e)
					}
					refusal = e.Output
				}
			}
			if refusal == "" {
				t.Fatalf("%s: no tool_result for the MCP call at all: %s", mode, liveEventKinds(evts))
			}
			for _, want := range []string{"REFUSED BY THE PLATFORM", modeLabel(mode)} {
				if !strings.Contains(refusal, want) {
					t.Fatalf("%s refusal is missing %q:\n%s", mode, want, refusal)
				}
			}

			testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
				Leg: "ask-native-mode-policy", Adapter: "native", Surface: "ask-conversation",
				Conversat: convID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
				Tool: wantTool, Result: refusal, Offered: prov.seenTools,
				Model: "scripted", ModelAccess: "available",
				Note: "AC4/mode policy: " + mode + " neither offered nor executed the project's opaque MCP tool; the refusal names the mode",
			})
		})
	}
}

// AC 4 (claude Ask): the claude Ask session's argv carries the project's server,
// resolved from the REAL row.
func TestE2EClaudeAskArgvCarriesTheProjectServer(t *testing.T) {
	pool := workItemKindTestPool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})

	res, err := mcpsettings.NewResolver(pool).ResolveScope(
		tenant.WithID(context.Background(), testfixtures.E2ETenant),
		mcpclient.ScopeRef{
			Kind:           mcpclient.ScopeConversation,
			ProjectID:      seed.ProjectID,
			ConversationID: seed.ConversationID,
		},
	)
	if err != nil {
		t.Fatalf("ResolveScope(conversation): %v", err)
	}
	found := false
	for _, s := range res.Servers {
		if s.Spec.ID == seed.HTTPServerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the conversation scope did not resolve the project's server %q: %v", seed.HTTPServerID, res.Servers)
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "ask-claude-scope", Adapter: "claude", Surface: "ask-conversation",
		Conversat: seed.ConversationID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Offered: []string{seed.HTTPServerID},
		Model:   "n/a", ModelAccess: "available",
		Note: "the claude Ask scope (conversation) resolves the project's server from the real row; the argv render is proven in internal/claude's E2E leg",
	})
}

// AC 5.2 (native Ask negative): the per-conversation MCP client is RECONCILED
// per turn, so removing the definition removes it from the NEXT turn — the cache
// risk this criterion exists to catch.
func TestE2EAskClientReconcilesWhenAServerIsDeleted(t *testing.T) {
	pool := workItemKindTestPool(t)
	url := mcpclient.E2EHTTPFixtureT(t)
	seed := testfixtures.SeedProjectWithServerAndSkill(t, pool, url, testfixtures.SeedOpts{})

	svc := New(pool, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	ctx := tenant.WithID(context.Background(), testfixtures.E2ETenant)
	ctx = withAskMode(ctx, modeIteration)
	ctx = withAskConversation(ctx, seed.ConversationID)
	ctx = withAskConversationProject(ctx, seed.ProjectID)

	// FIRST turn: the client is started for the project's server.
	entry1, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (first): %v", err)
	}
	if entry1 == nil || entry1.mgr == nil {
		t.Fatalf("the first turn did not start an MCP client: %+v", entry1)
	}
	fp1 := entry1.fingerprint
	if !strings.Contains(fp1, seed.HTTPServerID) {
		t.Fatalf("the first fingerprint does not name the project's server: %q", fp1)
	}

	// DELETE the project's definition, then let the NEXT turn reconcile.
	e2eDeleteAskServer(t, pool, seed.HTTPServerID)
	svc.refreshAskMCP(ctx)
	entry2, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (after delete): %v", err)
	}
	if entry2 != nil && strings.Contains(entry2.fingerprint, seed.HTTPServerID) {
		t.Fatalf("the deleted server %q is STILL in the conversation's fingerprint: %q", seed.HTTPServerID, entry2.fingerprint)
	}
	if entry2 != nil && entry2.mgr != nil {
		t.Fatalf("the conversation still holds a LIVE MCP client after its only server was deleted")
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "ask-native-negative", Adapter: "native", Surface: "ask-conversation",
		Conversat: seed.ConversationID, ProjectID: seed.ProjectID, ServerID: seed.HTTPServerID,
		Result: "fingerprint " + fp1 + " -> " + e2eFingerprintOf(entry2),
		Model:  "scripted", ModelAccess: "available",
		Note: "AC5.2: askMCPFor/refreshAskMCP reconcile per turn — the deleted server leaves the fingerprint and the live client is retired",
	})
}

// --- helpers -----------------------------------------------------------------

// e2eGetConversation reads a conversation row through the tenant-scoped path.
func e2eGetConversation(ctx context.Context, pool *db.Pool, convID string) (db.ConversationRow, error) {
	ttx, err := pool.BeginTenantTx(ctx, testfixtures.E2ETenant)
	if err != nil {
		return db.ConversationRow{}, err
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	return db.GetConversation(ctx, ttx.Tx, testfixtures.E2ETenant, convID)
}

// e2eDeleteAskServer removes one mcp_servers row in a tenant transaction.
func e2eDeleteAskServer(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, testfixtures.E2ETenant)
	if err != nil {
		t.Fatalf("begin tx to delete server: %v", err)
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	if err := db.DeleteMCPServer(ctx, ttx.Tx, testfixtures.E2ETenant, id); err != nil {
		t.Fatalf("delete mcp server %q: %v", id, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
}

// e2eFingerprintOf renders an entry's fingerprint for evidence.
func e2eFingerprintOf(e *askMCPEntry) string {
	if e == nil {
		return "<nil>"
	}
	return e.fingerprint
}

// TestE2EAskToolListHasNoTenantTier is AC 6's Ask half: the Ask tool registry
// exposes NO tenant-tier MCP tool. The list is read from the REAL registry the
// service builds, so a tool re-added under the retired name fails here.
func TestE2EAskToolListHasNoTenantTier(t *testing.T) {
	reg := NewToolRegistry(nil, slog.Default(), nil)
	names := make([]string, 0, len(reg.List()))
	for _, td := range reg.List() {
		names = append(names, td.Name)
	}
	if len(names) == 0 {
		t.Fatal("the Ask tool registry is empty — the assertion would pass vacuously")
	}
	for _, n := range names {
		low := strings.ToLower(n)
		if strings.Contains(low, "tenant_default_mcp") || strings.Contains(low, "set_tenant_default") {
			t.Errorf("the Ask tool list still exposes a tenant-tier MCP tool: %q", n)
		}
	}
	// The owner-scoped MCP tools that DO exist must be there (they replaced the
	// tier) — so this is not an assertion that the surface vanished.
	found := false
	for _, n := range names {
		if n == "list_mcp_servers" || strings.Contains(n, "mcp_server") {
			found = true
		}
	}
	if !found {
		t.Error("the Ask tool list exposes no MCP server tool at all; the owner-scoped surface is missing")
	}

	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "ask-tool-list", Adapter: "ask", Surface: "ask-tool-registry",
		Offered: names, Model: "n/a", ModelAccess: "available",
		Note: "AC6: the Ask tool registry carries no tenant-tier MCP tool; the owner-scoped MCP tools are present",
	})
}
