package askorchicon

// ask_mcp.go — the NATIVE Ask path's MCP client: one live client per CONVERSATION.
//
// WHY THIS FILE EXISTS. Native Ask started no MCP client at all. The worker path
// (internal/orchicon/mcptools.go, mcpResolveAndStart) starts one per EXECUTION and closes it at
// session end; the Ask turn surface (native_tools.go's AskToolDefs / ExecuteAskTool) offered only
// the product registry plus the file/shell suite. So a conversation could own MCP servers and a
// project could own them, and neither could ever reach a native Ask turn.
//
// THE SCOPE IS THE CONVERSATION, resolved by the ONE resolver (internal/mcpsettings): project-owned
// rows ∪ conversation-owned rows, with provenance per row. There is no tenant tier and no second
// resolution — re-reading the conversation row here would be the drift this closes.
//
// LIFECYCLE — THE PART THAT IS NOT THE WORKER PATH. An Ask turn is long-lived by design (the
// sessionless bridge keeps history per conversation), so the worker's per-execution close does not
// apply: closing per turn would re-spawn stdio children on every message. The client is therefore
// created ONCE per conversation and closed at exactly two seams:
//
//   - conversation teardown: Service.DeleteConversation calls closeAskMCP(convID);
//   - plane shutdown: the server calls Service.CloseAskMCP().
//
// AND THE SET IS RECONCILED PER TURN, not just per conversation: the client outlives a turn, but the
// servers it should serve do NOT — the operator can give a conversation a project or add a server to
// it between two messages. So AskToolDefs calls refreshAskMCP once per turn, which re-resolves and
// swaps the client only when the resolved set's fingerprint changed (see refreshAskMCP). That is the
// native half of the same rule claude's Ask child implements with sessionMCPStale.
//
// The belt-and-braces that reclaim a leaked child after a SIGKILL already exist and are relied on
// rather than duplicated: MCP stdio children carry ORCHICON_MCP_STDIO + PDEATHSIG, and
// mcpclient.SweepStaleChildren runs at boot and every 30s (internal/server/server.go).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askMCPClient is the small surface the Ask turn needs from a conversation's MCP
// client: the discovered defs, a routed call, and a close. An INTERFACE rather than
// the concrete *mcpclient.Manager so a test can drive the defs/route/close path
// without spawning a server — the production value is a *mcpclient.Manager.
type askMCPClient interface {
	Defs() []mcpclient.ToolDef
	Execute(ctx context.Context, name, argsJSON string) (string, error)
	Close() error
}

// askMCPConnect is the client-construction seam: it builds a manager and connects
// every spec. A var so a test can substitute a fake (the same test-seam pattern as
// askFileRootResolve); production uses mcpclient.NewManager + Start.
var askMCPConnect = func(ctx context.Context, log *slog.Logger, specs []mcpclient.ServerSpec) (askMCPClient, error) {
	mgr := mcpclient.NewManager(log)
	if _, err := mgr.Start(ctx, specs); err != nil {
		return nil, err
	}
	return mgr, nil
}

// askMCPEntry is one conversation's live MCP client.
type askMCPEntry struct {
	mgr askMCPClient
	// servers is the RAW resolution, kept for the provenance log line and for
	// DescribeFailedServer's scope annotation. It is NOT post-secret-expansion
	// (ProvenanceString reads only ids and scope labels anyway — see the manager's
	// comment for why the payload must never be formatted).
	servers    []mcpclient.ScopedServer
	provenance string
	// projectID is the project the entry was resolved for. The conversation's
	// project can move mid-life (SetConversationProject), and a client resolved
	// against the old project is exactly the stale-scope defect this closes — so a
	// turn whose project differs from this invalidates the entry.
	projectID string
	// fingerprint is what this entry WAS RESOLVED FOR: the project id and the
	// resolved server ids, built by askMCPFingerprint — the SAME function the
	// per-turn comparator uses, so the two cannot drift. A later turn whose
	// resolution fingerprints differently replaces the entry (see refreshAskMCP).
	fingerprint string
}

// askMCPFingerprint is the ONE rendering of "which servers this conversation resolves, in which
// project": the project id and the resolved server ids. BOTH the producer (the entry, at resolve
// time) and the comparator (refreshAskMCP, on the next turn) call it, so the two cannot drift — the
// same discipline claude's mcpFingerprint follows for its `--mcp-config` child.
func askMCPFingerprint(projectID string, servers []mcpclient.ScopedServer) string {
	names := make([]string, 0, len(servers))
	for _, ss := range servers {
		names = append(names, ss.Spec.ID)
	}
	return projectID + ";" + strings.Join(names, ",")
}

// resolveAskMCPRaw performs the conversation's SCOPE resolution and returns the raw
// result WITHOUT connecting anything. It is the ONE place the Ask path asks the shared
// resolver, shared by the first-use start and the per-turn refresh (which must be able
// to compare fingerprints BEFORE deciding to spawn).
//
// A nil resolver (tests / DB-less planes) yields an empty resolution and no error:
// "no MCP tools" is not a failure.
func (s *Service) resolveAskMCPRaw(ctx context.Context) (askmode.ConversationScope, mcpclient.Resolution, error) {
	scope := askmode.ConversationScopeFromContext(ctx)
	if s.mcpResolver == nil {
		return scope, mcpclient.Resolution{}, nil
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return scope, mcpclient.Resolution{}, fmt.Errorf("ask mcp: no tenant in context")
	}
	ref := mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeConversation,
		ProjectID:      scope.ProjectID,
		ConversationID: scope.ConversationID,
	}
	res, err := s.mcpResolver.ResolveScope(tenant.WithID(ctx, tenantID), ref)
	if err != nil {
		return scope, mcpclient.Resolution{}, fmt.Errorf("ask mcp: resolve the conversation's MCP servers: %w", err)
	}
	return scope, res, nil
}

// resolveAndStartAskMCP resolves the conversation's MCP servers by SCOPE and starts the client.
//
// It returns an entry with NO client (mgr == nil) when the conversation resolves NO servers: no MCP
// tools, never an error — mirroring the worker path. The entry is still NON-NIL so refreshAskMCP can
// tell "resolved, and the answer is none" from "never resolved", and re-check it on a later turn.
// A resolution that names a server that does not exist, or a server that cannot run, FAILS LOUD and
// names both the server and where its definition came from: a silently degraded MCP surface is the
// failure mode this whole epic exists to make unfalsifiable.
func (s *Service) resolveAndStartAskMCP(ctx context.Context) (*askMCPEntry, error) {
	scope, res, err := s.resolveAskMCPRaw(ctx)
	if err != nil {
		return nil, err
	}
	return s.startAskMCPEntry(ctx, scope, res)
}

// startAskMCPEntry turns a RAW resolution into a live entry: it logs what was resolved (with
// provenance, BEFORE secret expansion mutates the specs), expands ${SECRET_NAME} references, and
// connects. A selected-but-unconfigured server fails LOUD here rather than silently offering
// nothing.
func (s *Service) startAskMCPEntry(ctx context.Context, scope askmode.ConversationScope, res mcpclient.Resolution) (*askMCPEntry, error) {
	fp := askMCPFingerprint(scope.ProjectID, res.Servers)
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("ask mcp: MCP server(s) selected but not configured (project / conversation): %v — fix the project's or this conversation's MCP definitions", res.Missing)
	}
	if len(res.Servers) == 0 {
		// A REAL ANSWER, cached (see the entry's fingerprint field): this conversation
		// resolves no servers. A later turn re-checks the fingerprint and only then
		// spawns.
		return &askMCPEntry{projectID: scope.ProjectID, fingerprint: fp}, nil
	}
	// LOG WHAT WAS RESOLVED, WITH PROVENANCE, BEFORE the secret expansion below
	// mutates the specs' Env/Headers in place. The names + scope ids are the whole
	// point: an absent MCP surface is otherwise invisible (the model just reports it
	// cannot call a tool), which is exactly why this line did not exist and why an
	// MCP problem was unfalsifiable.
	names := make([]string, 0, len(res.Servers))
	for _, ss := range res.Servers {
		names = append(names, ss.Spec.ID)
	}
	prov := mcpclient.ProvenanceString(res.Servers)
	if s.log != nil {
		s.log.Info("ask: MCP servers registered",
			"conversation", scope.ConversationID,
			"project", scope.ProjectID,
			"servers", strings.Join(names, ","),
			"provenance", prov)
	}

	specs := make([]mcpclient.ServerSpec, 0, len(res.Servers))
	for _, ss := range res.Servers {
		specs = append(specs, ss.Spec)
	}
	if err := s.resolveAskMCPSecrets(ctx, tenant.FromContext(ctx), specs); err != nil {
		// A spec whose ${SECRET_NAME} cannot be resolved cannot run either, so the
		// failure is scope-annotated exactly like a connect failure.
		return nil, mcpclient.DescribeFailedServer(err, res.Servers)
	}
	mgr, err := askMCPConnect(context.WithoutCancel(ctx), s.log, specs)
	if err != nil {
		return nil, fmt.Errorf("ask mcp: connect: %w", mcpclient.DescribeFailedServer(err, res.Servers))
	}
	return &askMCPEntry{mgr: mgr, servers: res.Servers, provenance: prov, projectID: scope.ProjectID, fingerprint: fp}, nil
}

// resolveAskMCPSecrets expands ${SECRET_NAME} references in each spec's env/headers in place, using
// the same mcpsettings resolver every other path uses. A nil KEK with no references is a no-op; with
// a reference it fails naming the secret — parity with the worker and claude paths.
func (s *Service) resolveAskMCPSecrets(ctx context.Context, tenantID string, specs []mcpclient.ServerSpec) error {
	if s.pool == nil {
		return nil
	}
	for i := range specs {
		env, headers, err := mcpsettings.ResolveSecretRefs(ctx, s.pool, s.kek, tenantID, specs[i].Env, specs[i].Headers)
		if err != nil {
			return fmt.Errorf("MCP server %q: %w", specs[i].ID, err)
		}
		specs[i].Env = env
		specs[i].Headers = headers
	}
	return nil
}

// askMCPFor returns the conversation's live MCP client, starting it on first use and RESTARTING it
// when the conversation's project has changed since it was resolved.
//
// The project is read from the turn's context on EVERY call rather than cached: a conversation can be
// re-assigned to another project mid-life, and a client resolved against the previous project would
// keep serving the wrong project's servers — the stale-scope defect this closes. The cheap
// project check here is a SAFETY NET under refreshAskMCP; the per-turn fingerprint compare is what
// catches a change to the conversation's OWN servers with the project unchanged, so a conversation
// that started with no project (cached "none") is re-resolved the moment it gains one.
func (s *Service) askMCPFor(ctx context.Context) (*askMCPEntry, error) {
	convID := askConversationFromContext(ctx)
	projectID := askConversationProjectFromContext(ctx)
	if convID == "" {
		// No conversation on the context: nothing to cache against. Resolve fresh
		// (a direct test call), never storing it under an empty key.
		return s.resolveAndStartAskMCP(ctx)
	}
	s.askMCPMu.Lock()
	entry, ok := s.askMCP[convID]
	s.askMCPMu.Unlock()
	// A CACHED ENTRY WITH A NIL CLIENT IS A REAL ANSWER ("this conversation resolves no MCP
	// servers"), not a miss: without caching it, every tool call of every MCP-less conversation
	// would re-run the resolver. The project check keeps the fast path honest; the per-turn
	// fingerprint compare in refreshAskMCP is what catches a changed SET at an unchanged project.
	if ok && (entry == nil || entry.projectID == projectID) {
		return entry, nil
	}
	if ok && entry != nil {
		// The conversation moved projects: retire the old client so its stdio
		// children do not accumulate, then resolve afresh.
		if s.log != nil {
			s.log.Info("ask: the conversation's project changed — restarting its MCP client",
				"conversation", convID, "was", entry.projectID, "now", projectID)
		}
		s.closeAskMCP(convID)
	}
	fresh, err := s.resolveAndStartAskMCP(ctx)
	if err != nil {
		return nil, err
	}
	s.askMCPMu.Lock()
	if s.askMCP == nil {
		s.askMCP = make(map[string]*askMCPEntry)
	}
	s.askMCP[convID] = fresh
	s.askMCPMu.Unlock()
	return fresh, nil
}

// refreshAskMCP reconciles the conversation's live MCP client with what the scope resolves NOW. It is
// called ONCE PER TURN (from AskToolDefs), not per tool call: a resolution is a DB read, and
// re-running it on every file/shell call would tax calls that have nothing to do with MCP.
//
// WHY IT IS NEEDED. The client is long-lived per conversation, but the SET is not fixed: the operator
// can give a conversation a project (SetConversationProject), drop a server on it, or add one to the
// project. A cached entry would then keep serving the set resolved BEFORE that write — or, worse,
// keep answering "this conversation has no servers" for the life of the conversation. This is the
// native half of the rule claude's Ask child already applies (sessionMCPStale), with the SAME
// fingerprint shape (askMCPFingerprint).
//
// IT DOES NOT RESPAWN ON AN UNCHANGED SET: a matching fingerprint keeps the live client, so a
// conversation in steady state pays one resolution per turn and nothing else.
//
// A resolution failure is SWALLOWED on purpose: the defs path logs it and the call path reports it
// loudly, and a refresh that turned a reporting concern into a dropped turn would be worse than the
// staleness it repairs.
func (s *Service) refreshAskMCP(ctx context.Context) {
	if s.mcpResolver == nil {
		return
	}
	convID := askConversationFromContext(ctx)
	if convID == "" {
		return
	}
	scope, res, err := s.resolveAskMCPRaw(ctx)
	if err != nil {
		return
	}
	fp := askMCPFingerprint(scope.ProjectID, res.Servers)
	s.askMCPMu.Lock()
	cur, ok := s.askMCP[convID]
	s.askMCPMu.Unlock()
	if ok && cur != nil && cur.fingerprint == fp {
		return // unchanged: keep the live client, do not respawn stdio children
	}
	fresh, err := s.startAskMCPEntry(ctx, scope, res)
	if err != nil {
		return
	}
	if ok && cur != nil && cur.mgr != nil {
		if cerr := cur.mgr.Close(); cerr != nil && s.log != nil {
			s.log.Warn("ask: closing a stale conversation MCP client failed", "conversation", convID, "error", cerr)
		}
	}
	s.askMCPMu.Lock()
	if s.askMCP == nil {
		s.askMCP = make(map[string]*askMCPEntry)
	}
	s.askMCP[convID] = fresh
	s.askMCPMu.Unlock()
}

// askMCPDefs returns the conversation's discovered MCP tool definitions, mapped onto the substrate's
// wire shape. A resolve/connect failure is logged and yields NO defs: the tool LIST must never fail a
// turn (the call-time path reports the failure loudly instead — the same split the host-suite branch
// already documents in native_tools.go).
func (s *Service) askMCPDefs(ctx context.Context) []mcpclient.ToolDef {
	entry, err := s.askMCPFor(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("ask: the conversation's MCP servers could not be started — no MCP tools this turn",
				"conversation", askConversationFromContext(ctx), "error", err)
		}
		return nil
	}
	if entry == nil || entry.mgr == nil {
		return nil
	}
	return entry.mgr.Defs()
}

// isAskMCPTool reports whether name is one of THIS conversation's advertised MCP tools.
//
// Routed by the client's own discovered defs rather than by an `mcp__` prefix test alone, so a call
// for a tool this conversation was never offered falls through to the registry's loud "not
// registered" error rather than producing a confusing MCP error.
func (s *Service) isAskMCPTool(ctx context.Context, name string) bool {
	entry, err := s.askMCPFor(ctx)
	if err != nil || entry == nil || entry.mgr == nil {
		return false
	}
	for _, d := range entry.mgr.Defs() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// executeAskMCP routes one MCP tool call to the conversation's client, with the TURN's context so a
// call is cancellable with the turn (the connection itself is not — it outlives the turn).
func (s *Service) executeAskMCP(ctx context.Context, name, argsJSON string) (string, error) {
	entry, err := s.askMCPFor(ctx)
	if err != nil {
		return "", err
	}
	if entry == nil || entry.mgr == nil {
		return "", fmt.Errorf("ask mcp: this conversation has no MCP client (the server %q is not available here)", name)
	}
	return entry.mgr.Execute(ctx, name, argsJSON)
}

// closeAskMCP tears down one conversation's MCP client: stdio children are terminated, HTTP
// connections closed. Idempotent — a conversation with no entry is a successful no-op.
func (s *Service) closeAskMCP(convID string) {
	if convID == "" {
		return
	}
	s.askMCPMu.Lock()
	entry := s.askMCP[convID]
	delete(s.askMCP, convID)
	s.askMCPMu.Unlock()
	if entry != nil && entry.mgr != nil {
		if err := entry.mgr.Close(); err != nil && s.log != nil {
			s.log.Warn("ask: closing a conversation's MCP client failed", "conversation", convID, "error", err)
		}
	}
}

// CloseAskMCP tears down EVERY conversation's MCP client. It is the plane-shutdown seam the server
// calls beside claudeBridge.CloseAsk(), so a plane restart does not leak stdio children.
func (s *Service) CloseAskMCP() {
	s.askMCPMu.Lock()
	entries := s.askMCP
	s.askMCP = nil
	s.askMCPMu.Unlock()
	for convID, entry := range entries {
		if entry != nil && entry.mgr != nil {
			if err := entry.mgr.Close(); err != nil && s.log != nil {
				s.log.Warn("ask: closing a conversation's MCP client failed", "conversation", convID, "error", err)
			}
		}
	}
}
