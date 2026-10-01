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
// resolution is ONE union, addressed by SCOPE (project / conversation /
// worker / run) — see ScopeResolver; there is no precedence chain and no
// tenant-default tier any more.
package mcpclient

import (
	"context"
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

// ServerSpec is one MCP server entry (one element of a resolved scope;
// storage is owned by internal/mcpsettings, this package only consumes
// it).
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
	// Env are extra environment variables for stdio servers (e.g.
	// GITHUB_PERSONAL_ACCESS_TOKEN). Values are already resolved from the
	// tenant secrets store by the caller — never placeholders.
	Env map[string]string `json:"env,omitempty"`
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

// ScopeKind addresses a resolution scope. RUN is run-shaped on purpose: its
// result is applied ONCE per container (child 4), so it must never be
// derived from an executing worker.
type ScopeKind string

const (
	ScopeProject      ScopeKind = "project"
	ScopeConversation ScopeKind = "conversation"
	ScopeWorker       ScopeKind = "worker"
	ScopeRun          ScopeKind = "run"
)

// ScopeRef addresses exactly one scope. The zero value resolves to nothing.
type ScopeRef struct {
	Kind           ScopeKind
	ProjectID      string
	ConversationID string
	WorkerID       string // ScopeWorker only; the version is the latest published
	RunID          string // ScopeRun only

	// Version is the PINNED worker version this ref resolves, used for
	// provenance when Kind is ScopeWorker (the "inline:<workerID>@<n>"
	// shape). Zero means "unpinned" — the resolver reads the latest
	// published version and reports "inline:<workerID>".
	Version int
	// OwnPermissions, when non-empty, is the CALLER-HELD own-set for this
	// scope: the executing worker version's permissions jsonb
	// (worker_versions.permissions → scheduler.ExecutionManifest.Permissions).
	// When set the resolver unions THIS and does NOT read the latest
	// published version — a pinned dispatch must resolve the version it
	// actually pinned. When empty the storage path holds.
	OwnPermissions []byte
}

// ScopedServer is one resolved server plus its PROVENANCE (which scope
// supplied it) — the caller logs and reports it.
type ScopedServer struct {
	Spec   ServerSpec
	From   ScopeKind
	FromID string // "project:<id>" | "conversation:<id>" | "worker:<id>@<n>" | "inline:<workerID>@<n>"
	// EntryID is the mcp_servers row id, "" for an inline (worker-owned)
	// definition — an inline definition has no row.
	EntryID string
}

// Resolution is the outcome of resolving ONE scope: the union of the
// project-owned definitions and the scope's own definitions, deduped and
// order-stable (project rows by name, then own definitions in declaration
// order), plus the skill-file union for the run scope.
type Resolution struct {
	Servers     []ScopedServer
	SelectedIDs []string
	Missing     []string // selected ids with no matching row/definition
	Disabled    []string // resolved definitions whose enabled flag is false
	Skills      []InlineSkillFile
}

// InlineSkillFile is one inline skill file carried by the scope (a worker
// version's permissions.skill_files). Mirrors db.InlineSkillFile so this
// package needs no db import.
type InlineSkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ScopeResolver is the ONE resolution contract. Exactly one implementation
// exists (mcpsettings.Resolver).
type ScopeResolver interface {
	ResolveScope(ctx context.Context, ref ScopeRef) (Resolution, error)
}

// NoopScopeResolver resolves every scope to nothing (tests, unwired planes).
type NoopScopeResolver struct{}

// ResolveScope implements ScopeResolver: always the empty resolution.
func (NoopScopeResolver) ResolveScope(context.Context, ScopeRef) (Resolution, error) {
	return Resolution{}, nil
}

// ProvenanceString renders the resolved set's provenance as order-stable
// "<server-id>=<from-id>" pairs, comma separated.
//
// IT READS ONLY Spec.ID / From / FromID ON PURPOSE. The specs' Env and Headers
// are mutated in place by the caller's secret expansion
// (${SECRET_NAME} → plaintext) BEFORE the transport connects, so a formatter
// that touched them would put resolved credentials in the log. Provenance is
// the observation this package exists to make falsifiable; the payload never
// is.
func ProvenanceString(servers []ScopedServer) string {
	if len(servers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(servers))
	for _, s := range servers {
		from := s.FromID
		if from == "" {
			from = string(s.From)
		}
		parts = append(parts, s.Spec.ID+"="+from)
	}
	return strings.Join(parts, ",")
}

// DescribeFailedServer annotates a connect/tool-discovery error with the SCOPE
// of the server it names, so a server that cannot run is ACTIONABLE: the
// operator sees both which server failed and where its definition came from.
//
// Manager.Start stops at the FIRST failing server over the SAME ordered spec
// list, and connectOne quotes the id as `mcp server %q: ...`, so matching the
// quoted id against the ordered ScopedServer list is deterministic. When no
// server matches (a manager-level error such as "manager closed"), the error is
// returned unchanged.
func DescribeFailedServer(err error, servers []ScopedServer) error {
	if err == nil || len(servers) == 0 {
		return err
	}
	msg := err.Error()
	for _, s := range servers {
		id := s.Spec.ID
		if id == "" {
			continue
		}
		if !strings.Contains(msg, fmt.Sprintf("%q", id)) {
			continue
		}
		scope := s.FromID
		if scope == "" {
			scope = string(s.From)
		}
		return fmt.Errorf("MCP server %q cannot run (scope %s): %w", id, scope, err)
	}
	return err
}
