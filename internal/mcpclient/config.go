// Package mcpclient implements the native adapter's MCP (Model Context
// Protocol) client: per-session connections to external MCP servers over
// stdio (CommandTransport) and streamable HTTP (StreamableClientTransport)
// built on the official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// ADR-0008: tools are discovered at session start and merged into the
// native tool registry as mcp__<server>__<tool>; connections are lazily
// established per session (never at control-plane boot); stdio children
// cannot outlive a dead control plane (PDEATHSIG + boot-time sweep);
// MCP tool calls honor per-call timeouts and cancellation; config
// resolution is worker selection → project selection → none over the
// tenant-configured server list.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Type discriminates an MCP server's transport kind.
type Type string

const (
	// TypeStdio runs the server as a subprocess of the control plane and
	// speaks newline-delimited JSON over stdin/stdout.
	TypeStdio Type = "stdio"
	// TypeHTTP connects to a remote server via the streamable HTTP
	// transport (MCP spec, SEP-2575 sessionless variant included).
	TypeHTTP Type = "http"
)

// ServerSpec is a tenant-configured MCP server entry (one element of the
// selectable universe consumed by ConfigSource; storage is owned by the
// adapter-settings task, this package only consumes it).
type ServerSpec struct {
	// ID is the stable server identifier (also the prefix in the
	// mcp__<server>__<tool> namespace).
	ID string `json:"id"`
	// Type selects the transport: "stdio" or "http". Empty means "http"
	// when URL is set, otherwise "stdio".
	Type Type `json:"type,omitempty"`
	// Command is the stdio server argv (first element is the executable).
	Command []string `json:"command,omitempty"`
	// URL is the streamable HTTP endpoint for remote servers.
	URL string `json:"url,omitempty"`
	// Headers are extra HTTP request headers (e.g. Authorization) for
	// header/bearer auth on remote servers. OAuth is out of scope for v1.
	Headers map[string]string `json:"headers,omitempty"`
	// Timeout bounds each tool call against this server (and the connect
	// handshake). Zero → defaultToolCallTimeout.
	Timeout time.Duration `json:"-"`
	// OnError selects the failure mode when the server is selected but
	// unreachable/missing: "fail" (default — the session start fails
	// actionably) or "degrade" (the session starts without this server's
	// tools).
	OnError string `json:"onError,omitempty"`
}

// FailOnError reports whether a selected-but-unreachable server should
// fail the session start actionably (vs degrading).
func (s ServerSpec) FailOnError() bool {
	return s.OnError != "degrade"
}

// TransportType resolves the effective transport kind of a spec
// (empty Type inferred from fields).
func (s ServerSpec) TransportType() Type {
	switch s.Type {
	case TypeStdio, TypeHTTP:
		return s.Type
	}
	if s.URL != "" {
		return TypeHTTP
	}
	return TypeStdio
}

// Defaults.
const (
	// defaultToolCallTimeout bounds one MCP tool call when the spec has no
	// explicit Timeout.
	defaultToolCallTimeout = 120 * time.Second
	// defaultConnectTimeout bounds the lazy per-session connect/discovery.
	defaultConnectTimeout = 15 * time.Second
)

// ConfigSource is the contract the session manager uses to resolve which
// MCP servers an execution may use (ADR-0008). Resolution order:
// worker selection → project selection → none. Implementations are
// tenant-scoped; the storage backend is owned by the adapter-settings
// task — until it lands, NoopConfigSource degrades safely.
type ConfigSource interface {
	// ServerList returns the tenant-configured selectable MCP servers.
	// This is the universe selections are drawn from.
	ServerList(ctx context.Context) ([]ServerSpec, error)
	// WorkerSelection returns the ids of MCP servers selected for a
	// worker (its permissions.mcp_servers). Nil/empty = no worker
	// selection.
	WorkerSelection(ctx context.Context, workerID string) ([]string, error)
	// ProjectSelection returns the ids of MCP servers selected for a
	// project. Nil/empty = no project selection.
	ProjectSelection(ctx context.Context, projectID string) ([]string, error)
}

// NoopConfigSource is the default ConfigSource shipped until the
// adapter-settings task lands tenant server-list storage: no servers are
// configured and no selections exist, so MCP tools are simply absent
// (the feature degrades safely and never errors).
type NoopConfigSource struct{}

func (NoopConfigSource) ServerList(context.Context) ([]ServerSpec, error) { return nil, nil }
func (NoopConfigSource) WorkerSelection(context.Context, string) ([]string, error) {
	return nil, nil
}
func (NoopConfigSource) ProjectSelection(context.Context, string) ([]string, error) {
	return nil, nil
}

// workerPermissionSelection is the shape of the per-worker permissions
// jsonb already carried on executions (ExecutionManifest.Permissions):
// {"mcp_servers": [{"id": "...", "command": "..."}]} (frontend
// MCPPicker/MCPConfig shape; entries may also be bare id strings).
type workerPermissionSelection struct {
	MCPServers []mcpSelectionEntry `json:"mcp_servers"`
}

type mcpSelectionEntry struct {
	ID      string `json:"id"`
	Command string `json:"command,omitempty"`
}

// ManifestConfigSource is a ConfigSource backed by one execution's
// manifest data: the worker's permissions jsonb (which carries its MCP
// selection) plus a server list from a static set. The server list is
// the tenant-configured universe; until storage lands the caller passes
// the static set (e.g. from config). Project selection is empty (no
// project-selection storage exists yet — the adapter-settings task owns
// it; the resolution order still holds: worker → project → none).
type ManifestConfigSource struct {
	// TenantServers is the selectable universe for this execution.
	TenantServers []ServerSpec
	// PermissionsJSON is the worker's permissions jsonb
	// (worker_versions.permissions → ExecutionManifest.Permissions).
	PermissionsJSON []byte
	// ProjectMCPServers optionally supplies a project-level selection
	// (ids); nil = no project selection.
	ProjectMCPServers []string
}

// ServerList implements ConfigSource.
func (m ManifestConfigSource) ServerList(context.Context) ([]ServerSpec, error) {
	return m.TenantServers, nil
}

// WorkerSelection implements ConfigSource: it parses the worker
// permissions' mcp_servers list. A malformed permissions jsonb degrades
// to no selection (never fails a session over worker config shape).
func (m ManifestConfigSource) WorkerSelection(_ context.Context, _ string) ([]string, error) {
	if len(m.PermissionsJSON) == 0 {
		return nil, nil
	}
	var p workerPermissionSelection
	if err := json.Unmarshal(m.PermissionsJSON, &p); err != nil {
		return nil, nil // malformed → no selection (defensive; never blocks a session)
	}
	ids := make([]string, 0, len(p.MCPServers))
	for _, e := range p.MCPServers {
		if id := strings.TrimSpace(e.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ProjectSelection implements ConfigSource.
func (m ManifestConfigSource) ProjectSelection(context.Context, string) ([]string, error) {
	return m.ProjectMCPServers, nil
}

// Resolved is the outcome of config resolution for one execution: the
// concrete, ordered server set to connect, plus provenance for errors.
type Resolved struct {
	// Servers are the specs to connect, in selection order (worker
	// selection first when present, else project, else none).
	Servers []ServerSpec
	// SelectedIDs are the raw ids the resolution drew (worker or
	// project), for actionable error messages.
	SelectedIDs []string
	// Missing are selected ids that are NOT present in the tenant server
	// list (selected-but-unconfigured). Resolution fails the session
	// actionably for these unless the matched server degrades.
	Missing []string
}

// Resolve implements ADR-0008 resolution: worker selection → project
// selection → none, over the tenant-configured server list. A selected
// id with no matching configured server is reported as Missing (the
// caller decides fail vs degrade per the offending spec / policy). An
// empty ServerList result or empty selections yields an empty Resolved
// (no MCP tools — never an error).
func Resolve(ctx context.Context, src ConfigSource, workerID, projectID string) (Resolved, error) {
	var r Resolved
	if src == nil {
		return r, nil
	}
	servers, err := src.ServerList(ctx)
	if err != nil {
		return r, fmt.Errorf("mcp config: server list: %w", err)
	}
	byID := make(map[string]ServerSpec, len(servers))
	for _, s := range servers {
		byID[s.ID] = s
	}

	sel, err := src.WorkerSelection(ctx, workerID)
	if err != nil {
		return r, fmt.Errorf("mcp config: worker selection: %w", err)
	}
	if len(sel) == 0 {
		sel, err = src.ProjectSelection(ctx, projectID)
		if err != nil {
			return r, fmt.Errorf("mcp config: project selection: %w", err)
		}
	}
	r.SelectedIDs = sel
	if len(sel) == 0 {
		return r, nil // no selection → no MCP tools (resolved, not an error)
	}
	for _, id := range sel {
		spec, ok := byID[id]
		if !ok {
			r.Missing = append(r.Missing, id)
			continue
		}
		r.Servers = append(r.Servers, spec)
	}
	return r, nil
}
