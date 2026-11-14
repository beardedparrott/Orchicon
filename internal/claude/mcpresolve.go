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
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// resolveMCP is THE resolution step: exactly one scope, the RAW result WITH its provenance.
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

// renderMCP renders a resolution into claude's config list: the built-in Orchicon sidecar FIRST,
// then the resolved servers (rendered by MCPServersFromSpecs), with ${SECRET_NAME} refs expanded.
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

// resolveMCPServers returns every MCP server an ASK conversation gets: the built-in Orchicon
// sidecar, then the tenant-resolved PROJECT set.
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

// logMCPResolution records what the session actually got, because an MCP surface that is absent is
// otherwise invisible: the model simply reports it cannot call a tool.
func logMCPResolution(where string, servers []MCPServer, provenance string) {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	slog.Default().Info("claude: MCP servers registered", "transport", where, "servers", strings.Join(names, ","), "provenance", provenance)
}

// --- the conversation's MCP set, and why it is fingerprinted ----------------

// askMCPBuiltin is the ONE construction of the built-in Orchicon sidecar entry for this session, so the
// argv render and the staleness comparison cannot disagree about what the child was launched with.
func (s *askSession) askMCPBuiltin(convID, projectID string) (MCPServer, error) {
	// The sidecar is a CHILD PROCESS, so the conversation's scope has to travel in its ENVIRONMENT.
	return OrchiconMCPServer(HookBinaryPath(), s.tenantID, OrchiconMCPConversationEnv(convID, projectID)), nil
}

// recordMCPFingerprint stores the resolved set the child is being launched with.
func (s *askSession) recordMCPFingerprint(fp string) {
	s.mu.Lock()
	s.mcpFingerprint = fp
	s.mu.Unlock()
}

// sessionMCPStale reports whether the conversation's CURRENT MCP resolution (or the mode's may-act,
// which decides whether the operator's servers are offered) differs from what the live child.
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
	// RENDER, not just resolve: the fingerprint is over the RENDERED servers, because that is where the
	// credential is. A resolution whose secrets cannot be expanded is not a reason to tear down a working
	// child — the previous set stays until the config is fixed.
	//
	// THE BUILTIN MUST BE THE SAME ONE argv() RENDERS WITH, or this compares two different sets and every
	// turn respawns the child (measured: a session whose set was unchanged rebuilt on its second turn).
	// Both halves are therefore read off one function: askMCPRenderInputs.
	builtin, err := s.askMCPBuiltin(convID, projectID)
	if err != nil {
		return false
	}
	mayAct := askmode.MayAct(mode)
	servers, err := s.b.renderMCP(context.Background(), tenantID, res, builtin)
	if err != nil {
		return false
	}
	if !mayAct {
		servers = withholdOpaqueMCP(servers)
	}
	return mcpFingerprint(mayAct, servers) != fp
}

// mcpFingerprint is the ONE rendering of "what the child was launched with": the may-act flag (which
// decides whether the operator's opaque servers were offered) and a hash of the RENDERED servers — the
// argv half that carries the credential.
//
// IT HASHES THE SERVERS THAT GO INTO ARGV, NOT THE RAW RESOLUTION. The raw resolution still holds
// ${SECRET_NAME} REFERENCES (they are expanded during renderMCP), so fingerprinting it could not see a
// rotated secret at all: the value changed underneath a string that never did. The child's --mcp-config
// is fixed at fork/exec, so a rotation can only be applied by RESPAWNING — and this is what decides
// whether to.
//
// HASHED, NOT STORED: this value is compared, logged in the respawn line, and held on the session for
// its life. A resolved credential must not travel in it (the same rule ProvenanceString states).
func mcpFingerprint(mayAct bool, servers []MCPServer) string {
	h := sha256.New()
	for _, s := range servers {
		// A stable order per server: the maps are Go maps, so their iteration order is not.
		_, _ = fmt.Fprintf(h, "name=%q\ncmd=%q\n", s.Name, s.Command)
		for _, a := range s.Args {
			_, _ = fmt.Fprintf(h, "arg=%q\n", a)
		}
		_, _ = fmt.Fprintf(h, "url=%q\n", s.URL)
		for _, m := range []map[string]string{s.Env, s.Headers} {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				_, _ = fmt.Fprintf(h, "%s=%q\x00", k, m[k])
			}
		}
	}
	return fmt.Sprintf("mayact=%v;%x", mayAct, h.Sum(nil))
}

// withholdOpaqueMCP drops every server from an Ask render EXCEPT the platform's own Orchicon
// sidecar.
func withholdOpaqueMCP(servers []MCPServer) []MCPServer {
	out := make([]MCPServer, 0, 1)
	for _, s := range servers {
		if strings.EqualFold(strings.TrimSpace(s.Name), orchiconMCPServerName) {
			out = append(out, s)
		}
	}
	return out
}
