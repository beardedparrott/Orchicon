package claude

// mcpresolve.go — the ONE place this adapter decides which MCP servers an
// execution or conversation gets.
//
// # The agnostic shape, and why it is written this way
//
// Resolving MCP servers is NOT adapter work, and duplicating it per adapter is
// how two adapters end up disagreeing about which servers apply. The platform
// already owns the whole resolution:
//
//	mcpclient.ScopeResolver             — the seam: ResolveScope(ScopeRef)
//	mcpsettings.NewResolver(pool)       — the storage-backed implementation
//	mcpclient.Resolution/ScopedServer   — the union + per-server provenance
//	mcpclient.ServerSpec                — the neutral transport shape
//	mcpsettings.ResolveSecretRefs       — ${SECRET_NAME} → plaintext
//
// The NATIVE bridge consumes exactly that set (orchicon.SetScopeResolver +
// SetMCPSecretResolver, wired in server.go). This file makes claude consume the
// same set, so the two cannot drift about WHICH servers apply.
//
// The scope resolved here is the WORKER scope for a worker execution — the
// union of the project's owned definitions and the executing version's inline
// specs (ScopeRef.OwnPermissions, from ExecutionManifest.Permissions). An Ask
// conversation resolves the CONVERSATION scope — project-owned ∪ conversation-owned
// — so it receives both the project's servers and its own (child 6's Ask surface).
//
// WHAT IS ADAPTER-SPECIFIC IS ONE FUNCTION: rendering the neutral []ServerSpec
// into claude's config format (MCPServersFromSpecs in mcpconfig.go). A codex
// adapter needs its own renderer and nothing else — no second resolution stack,
// no second selection rule, no second secret path. That is the standardisation
// the operator asked for, and it is deliberately a SEAM rather than a framework:
// three small types, all already in the tree.
//
// # The built-in Orchicon server is added ON TOP, never instead
//
// The resolved set is the OPERATOR'S servers. The Orchicon sidecar is the
// platform's own and is appended unconditionally: a tenant that has configured
// no MCP servers must still get `orchicon_*` tools, which is the state this
// adapter was in until the operator's Opus 5 reported it.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// resolveMCP is THE resolution step: exactly one scope, the RAW result WITH
// its provenance. Nothing here renders, expands secrets, or adds the built-in —
// a resolution is an observation, and the log line and the parity test both
// read THIS value (never a post-expansion copy, which would carry plaintext
// credentials).
func (b *Bridge) resolveMCP(ctx context.Context, tenantID string, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	if b.mcpResolver == nil {
		return mcpclient.Resolution{}, nil
	}
	// The resolver reads the tenant from the context, so the session's tenant
	// must be ON it — the same convention the native bridge follows.
	res, err := b.mcpResolver.ResolveScope(tenant.WithID(ctx, tenantID), ref)
	if err != nil {
		return mcpclient.Resolution{}, fmt.Errorf("claude: resolve MCP servers: %w", err)
	}
	return res, nil
}

// renderMCP renders a resolution into claude's config list: the built-in
// Orchicon sidecar FIRST, then the resolved servers (rendered by
// MCPServersFromSpecs), with ${SECRET_NAME} refs expanded. The built-in is added
// ON TOP, never instead: a tenant with no configured servers must still get the
// orchicon_* surface.
//
// A missing selection is the native bridge's documented failure and is kept
// here — a worker whose MCP selection silently did not apply looks healthy and
// cannot do its job.
func (b *Bridge) renderMCP(ctx context.Context, tenantID string, res mcpclient.Resolution, builtin MCPServer) ([]MCPServer, error) {
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("claude: MCP server(s) selected but not configured (project / version permissions): %v — fix the project's or this version's MCP definitions", res.Missing)
	}
	if len(res.Servers) == 0 {
		return []MCPServer{builtin}, nil
	}
	specs := make([]mcpclient.ServerSpec, 0, len(res.Servers))
	for _, ss := range res.Servers {
		specs = append(specs, ss.Spec)
	}
	if err := b.resolveMCPSpecSecrets(ctx, tenantID, specs); err != nil {
		return nil, err
	}
	out := make([]MCPServer, 0, len(specs)+1)
	out = append(out, builtin)
	out = append(out, MCPServersFromSpecs(specs)...)
	return out, nil
}

// resolveMCPServers returns every MCP server an ASK conversation gets:
// the built-in Orchicon sidecar, then the tenant-resolved PROJECT set.
//
// It is the project-scope wrapper kept for Ask's caller (ask.go); a worker
// execution goes through resolveMCP + renderMCP with the WORKER scope so the
// version's inline specs are in the union. Callers that must LOG provenance
// call resolveMCP/renderMCP directly and read mcpclient.ProvenanceString off
// the RAW resolution.
//
// `builtin` is the platform's own Orchicon entry, built by the CALLER because
// only the caller knows its transport: a worker in a runtime container needs the
// daemon's bind-mounted binary and the in-container sandbox Postgres, while an
// Ask session runs this process's own executable. Resolution is shared; the
// built-in's shape is not.
func (b *Bridge) resolveMCPServers(ctx context.Context, tenantID, workerID, projectID string, builtin MCPServer) ([]MCPServer, error) {
	res, err := b.resolveMCP(ctx, tenantID, mcpclient.ScopeRef{
		Kind:      mcpclient.ScopeProject,
		ProjectID: projectID,
	})
	if err != nil {
		return nil, err
	}
	return b.renderMCP(ctx, tenantID, res, builtin)
}

// resolveMCPSpecSecrets expands ${SECRET_NAME} references in place, mirroring
// the native bridge's resolver semantics exactly (nil resolver = pass through).
func (b *Bridge) resolveMCPSpecSecrets(ctx context.Context, tenantID string, specs []mcpclient.ServerSpec) error {
	if b.mcpSecretResolver == nil {
		return nil
	}
	for i := range specs {
		env, headers, err := b.mcpSecretResolver(ctx, tenantID, specs[i].Env, specs[i].Headers)
		if err != nil {
			return fmt.Errorf("claude: MCP server %q: %w", specs[i].ID, err)
		}
		specs[i].Env = env
		specs[i].Headers = headers
	}
	return nil
}

// logMCPResolution records what the session actually got, because an MCP surface
// that is absent is otherwise invisible: the model simply reports it cannot call
// a tool, and nothing in the log says why.
//
// `provenance` names WHICH SCOPE supplied each server ("gh=inline:w1@3"), because
// "the server is missing" and "the server resolved from the WRONG scope" are
// different problems with different fixes. It is built from the RAW resolution
// (before ${SECRET} expansion), so it can never carry a credential.
func logMCPResolution(where string, servers []MCPServer, provenance string) {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	slog.Default().Info("claude: MCP servers registered", "transport", where, "servers", strings.Join(names, ","), "provenance", provenance)
}

// --- the conversation's MCP set, and why it is fingerprinted ----------------

// mcpServerNames renders a RAW resolution's server ids as a stable join: the
// fingerprint of the set, independent of provenance or transport.
func mcpServerNames(servers []mcpclient.ScopedServer) string {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Spec.ID)
	}
	return strings.Join(names, ",")
}

// recordMCPFingerprint stores the resolved set the child is being launched with,
// tagged with whether the mode could act (which is what decided whether the
// operator's opaque servers were offered). The value is built by mcpFingerprint,
// the SAME function the comparator uses, so the two cannot drift.
func (s *askSession) recordMCPFingerprint(fp string) {
	s.mu.Lock()
	s.mcpFingerprint = fp
	s.mu.Unlock()
}

// sessionMCPStale reports whether the conversation's CURRENT MCP resolution (or the
// mode's may-act, which decides whether the operator's servers are offered) differs
// from what the live child was launched with.
//
// WHY THIS EXISTS RATHER THAN BEING IMPLIED. claude's Ask child is long-lived per
// conversation and takes its servers as `--mcp-config` at SPAWN, so a live child
// cannot see a server added afterwards, nor a mode switch that flips MayAct. Without
// this check, AC2/AC4 ("a conversation receives its project's and its own servers")
// would hold only for a set that was complete before the first message. A false result
// on any resolution failure is deliberate: argv() reports that failure at spawn, so
// re-resolving here must not turn a reporting concern into a respawn loop.
func (s *askSession) sessionMCPStale() bool {
	s.mu.Lock()
	alive, fp, mode := s.alive, s.mcpFingerprint, s.mode
	tenantID, projectID, convID := s.tenantID, s.projectID, s.convID
	s.mu.Unlock()
	if !alive {
		return false
	}
	res, err := s.b.resolveMCP(context.Background(), tenantID, mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeConversation,
		ProjectID:      projectID,
		ConversationID: convID,
	})
	if err != nil {
		return false
	}
	return mcpFingerprint(askmode.MayAct(mode), res.Servers) != fp
}

// mcpFingerprint is the ONE rendering of "what the child was launched with": the
// may-act flag (which decides whether the operator's opaque servers were offered) and
// the resolved server ids. Both producer and comparator call it, so they cannot drift.
func mcpFingerprint(mayAct bool, servers []mcpclient.ScopedServer) string {
	return fmt.Sprintf("mayact=%v;%s", mayAct, mcpServerNames(servers))
}

// withholdOpaqueMCP drops every server from an Ask render EXCEPT the platform's own
// Orchicon sidecar. It is the OFFERED half of the opaque-MCP mode rule for this
// adapter: the operator's servers are opaque, so a mode that may not act is not
// offered them at all.
//
// The platform's sidecar is kept unconditionally: its tools ARE classified by the
// table (`mcp__orchicon__create_work_item` etc.), so the hook's mode gate governs
// them precisely, and withholding them in Brainstorm/Quick Work would remove the
// in-Orchicon work those modes exist to do.
func withholdOpaqueMCP(servers []MCPServer) []MCPServer {
	out := make([]MCPServer, 0, 1)
	for _, s := range servers {
		if strings.EqualFold(strings.TrimSpace(s.Name), orchiconMCPServerName) {
			out = append(out, s)
		}
	}
	return out
}
