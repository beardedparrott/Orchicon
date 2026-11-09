package askorchicon

// ask_mcp.go — the NATIVE Ask path's MCP client: one live client per CONVERSATION.

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

// askMCPClient is the small surface the Ask turn needs from a conversation's MCP client: the
// discovered defs, a routed call, and a close.
type askMCPClient interface {
	Defs() []mcpclient.ToolDef
	Execute(ctx context.Context, name, argsJSON string) (string, error)
	Close() error
}

// askMCPConnect is the client-construction seam: it builds a manager and connects every spec.
var askMCPConnect = func(ctx context.Context, log *slog.Logger, specs []mcpclient.ServerSpec) (askMCPClient, []mcpclient.ServerFailure, error) {
	mgr := mcpclient.NewManager(log)
	if _, err := mgr.Start(ctx, specs); err != nil {
		return nil, mgr.Failures(), err
	}
	return mgr, mgr.Failures(), nil
}

// askMCPEntry is one conversation's live MCP client.
type askMCPEntry struct {
	mgr askMCPClient
	// servers is the RAW resolution, kept for the provenance log line and for DescribeFailedServer's
	// scope annotation.
	servers    []mcpclient.ScopedServer
	provenance string
	// projectID is the project the entry was resolved for.
	projectID string
	// fingerprint is what this entry WAS RESOLVED FOR: the project id and the resolved server ids.
	fingerprint string
}

// askMCPFingerprint is the ONE rendering of "which servers this conversation resolves, in which
// project": the project id and the resolved server ids.
func askMCPFingerprint(projectID string, servers []mcpclient.ScopedServer) string {
	names := make([]string, 0, len(servers))
	for _, ss := range servers {
		names = append(names, ss.Spec.ID)
	}
	return projectID + ";" + strings.Join(names, ",")
}

// resolveAskMCPRaw performs the conversation's SCOPE resolution and returns the raw result WITHOUT
// connecting anything.
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
func (s *Service) resolveAndStartAskMCP(ctx context.Context) (*askMCPEntry, error) {
	scope, res, err := s.resolveAskMCPRaw(ctx)
	if err != nil {
		return nil, err
	}
	return s.startAskMCPEntry(ctx, scope, res)
}

// startAskMCPEntry turns a RAW resolution into a live entry: it logs what was resolved (with
// provenance, BEFORE secret expansion mutates the specs), expands ${SECRET_NAME} references.
func (s *Service) startAskMCPEntry(ctx context.Context, scope askmode.ConversationScope, res mcpclient.Resolution) (*askMCPEntry, error) {
	fp := askMCPFingerprint(scope.ProjectID, res.Servers)
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("ask mcp: MCP server(s) selected but not configured (project / conversation): %v — fix the project's or this conversation's MCP definitions", res.Missing)
	}
	if len(res.Servers) == 0 {
		// A REAL ANSWER, cached (see the entry's fingerprint field): this conversation resolves no servers.
		if s.log != nil {
			s.log.Info("ask: MCP servers resolved to none",
				"conversation", scope.ConversationID,
				"project", scope.ProjectID,
				"disabled", strings.Join(res.Disabled, ","))
		}
		return &askMCPEntry{projectID: scope.ProjectID, fingerprint: fp}, nil
	}
	// LOG WHAT WAS RESOLVED, WITH PROVENANCE, BEFORE the secret expansion below mutates the specs'
	// Env/Headers in place.
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
	mgr, failures, err := askMCPConnect(context.WithoutCancel(ctx), s.log, specs)
	// REPORT THE PARTIAL FAILURE EVEN WHEN THE START SUCCEEDED.
	if s.log != nil {
		for _, f := range failures {
			s.log.Warn("ask: an MCP server could not be started — ITS tools are absent this conversation, the rest of the surface is unaffected",
				"conversation", scope.ConversationID,
				"server", f.ServerID,
				"transport", f.Transport,
				"scope", mcpclient.DescribeFailedServer(f.Err, res.Servers))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ask mcp: connect: %w", mcpclient.DescribeFailedServer(err, res.Servers))
	}
	return &askMCPEntry{mgr: mgr, servers: res.Servers, provenance: prov, projectID: scope.ProjectID, fingerprint: fp}, nil
}

// resolveAskMCPSecrets expands ${SECRET_NAME} references in each spec's env/headers in place, using
// the same mcpsettings resolver every other path uses.
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
	// A CACHED ENTRY WITH A NIL CLIENT IS A REAL ANSWER ("this conversation resolves no MCP servers"),
	// not a miss: without caching it.
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

// refreshAskMCP reconciles the conversation's live MCP client with what the scope resolves NOW.
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
		// LOGGED, because the consequence is a STALE CLIENT and an unfalsifiable failure: the model reports
		// that it cannot call a tool, and nothing in the log says the refresh was what failed.
		if s.log != nil {
			s.log.Warn("ask: MCP refresh could not resolve the conversation's scope — keeping the previous client",
				"conversation", convID, "error", err)
		}
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
		// SAME REASON as the resolve failure above: the old client stays, so the tool surface silently stops
		// matching the configuration and the only symptom is a model saying it cannot call something.
		if s.log != nil {
			s.log.Warn("ask: MCP refresh could not start the conversation's servers — keeping the previous client",
				"conversation", convID, "servers", len(res.Servers), "error", err)
		}
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

// askMCPDefs returns the conversation's discovered MCP tool definitions, mapped onto the
// substrate's wire shape.
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
		// A NIL ENTRY IS A REAL ANSWER AND USED TO BE SILENT.
		if s.log != nil {
			s.log.Info("ask: no MCP servers for this turn",
				"conversation", askConversationFromContext(ctx), "project", askConversationProjectFromContext(ctx))
		}
		return nil
	}
	defs := entry.mgr.Defs()
	// HOW MANY TOOLS WERE ACTUALLY DISCOVERED.
	//
	// The resolution line above says what was CONFIGURED; this says what the client's tools/list returned. Without
	// it, three very different failures are indistinguishable in the log:
	//
	//	started but tools/list returned 0     (the server is up and advertises nothing)
	//	defs built but dropped downstream     (offered to nobody — the turn's tool array is where this shows)
	//	offered fine                          (the model has them and still did not use them)
	//
	// REPORTED from a session that spent itself driving a server over bash: the log showed the servers
	// registered and nothing about tools, so there was no way to tell which of those three it had been.
	if s.log != nil {
		s.log.Info("ask: MCP tool defs offered",
			"conversation", askConversationFromContext(ctx),
			"servers", len(entry.servers),
			"tools", len(defs))
	}
	return defs
}

// isAskMCPTool reports whether name is one of THIS conversation's advertised MCP tools.
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
