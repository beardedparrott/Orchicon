package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolDef mirrors the native tool-registry definition shape (the orchicon adapter converts to
// orchicon.ToolDef).
type ToolDef struct {
	Name        string
	Description string
	ParamsJSON  string
}

// Manager owns the MCP client connections for one session/execution: connections are
// established lazily at session start (never at control-plane boot).
type Manager struct {
	log *slog.Logger

	mu        sync.Mutex
	conns     map[string]*serverConn // by server id
	connOrder []string               // deterministic defs order
	defs      []ToolDef              // cached discovery result (all servers)
	started   bool
	closed    bool
	// failures records, per server, why it is NOT part of defs. ONE UNSTARTABLE SERVER MUST NOT COST
	// THE REST OF THE SURFACE (see Start), so a failure is a recorded fact rather than an abort.
	failures []ServerFailure
}

// ServerFailure is one server that could not be started, and why.
type ServerFailure struct {
	// ServerID is the resolved definition id (the same id the provenance log names).
	ServerID string
	// Transport is "stdio" or "streamable-http".
	Transport string
	// Err is the failure, already carrying the server id and the phase that failed
	// (spawn / connect / tool discovery).
	Err error
}

// Error renders the failure for a log line without repeating the id twice.
func (f ServerFailure) Error() string {
	if f.Err == nil {
		return f.ServerID + ": unknown failure"
	}
	return f.Err.Error()
}

// serverConn is one live connection: the go-sdk client + session + the
// spec it came from.
type serverConn struct {
	spec    ServerSpec
	client  *mcp.Client
	session *mcp.ClientSession
	defs    []ToolDef
	// toolName maps the local mcp__<server>__<tool> name back to the
	// server tool name.
	toolName map[string]string
}

// NewManager returns an idle Manager (no connections, nothing spawned).
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, conns: make(map[string]*serverConn)}
}

// Its failure is recorded in Failures() for the caller to log and for the model to be told about,
// and the error is returned ONLY when the surface is genuinely empty — no specs at all.
func (m *Manager) Start(ctx context.Context, specs []ServerSpec) ([]ToolDef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("mcp client: manager closed")
	}
	if m.started {
		return m.defs, nil
	}
	m.started = true

	var defs []ToolDef
	for _, spec := range specs {
		conn, err := m.connectOne(ctx, spec)
		if err != nil {
			// RECORD, DO NOT ABORT. The next server may be perfectly healthy, and its tools are
			// worth more than the tidy all-or-nothing failure this used to produce.
			f := ServerFailure{ServerID: spec.ID, Transport: string(spec.TransportType()), Err: err}
			m.failures = append(m.failures, f)
			m.log.Warn("mcp: a server could not be started — its tools are absent from this session, the rest of the surface is unaffected",
				"server", spec.ID, "transport", f.Transport, "error", err,
				"started", len(m.connOrder), "failed", len(m.failures), "remaining", len(specs)-len(m.connOrder)-len(m.failures))
			continue
		}
		m.conns[spec.ID] = conn
		m.connOrder = append(m.connOrder, spec.ID)
		defs = append(defs, conn.defs...)
		m.log.Info("mcp: server started", "server", spec.ID, "tools", len(conn.defs))
	}
	m.defs = defs
	// THE SURFACE IS EMPTY AND THAT IS NOT A PARTIAL FAILURE.
	if len(specs) > 0 && len(m.connOrder) == 0 {
		return nil, fmt.Errorf("no MCP server could be started (%d failed): %w", len(m.failures), m.failures[0].Err)
	}
	return defs, nil
}

// Failures returns the servers that could not be started, in spec order.
func (m *Manager) Failures() []ServerFailure {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerFailure, len(m.failures))
	copy(out, m.failures)
	return out
}

// connectOne dials one server (connect + tools/list) under a timeout.
func (m *Manager) connectOne(ctx context.Context, spec ServerSpec) (*serverConn, error) {
	return m.connectWithLegacyRetry(ctx, spec)
}

// Defs returns the discovered tool definitions (all servers), merged into the native tool registry
// as mcp__<server>__<tool>.
func (m *Manager) Defs() []ToolDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolDef, len(m.defs))
	copy(out, m.defs)
	return out
}

// Execute routes a tool call to the owning server connection with a per-call timeout and honors
// context cancellation.
func (m *Manager) Execute(ctx context.Context, name string, argsJSON string) (string, error) {
	conn := m.connFor(name)
	if conn == nil {
		return "", fmt.Errorf("mcp tool %q: unknown tool (not discovered at session start or server not selected)", name)
	}
	tool, ok := conn.toolName[name]
	if !ok {
		return "", fmt.Errorf("mcp tool %q: unknown tool on server %q", name, conn.spec.ID)
	}

	var args map[string]any
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("mcp tool %q: invalid arguments: %w", name, err)
		}
	}

	timeout := toolTimeout(conn.spec)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := conn.session.CallTool(callCtx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("mcp tool %q: timed out after %s on server %q", name, timeout, conn.spec.ID)
		}
		if errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("mcp tool %q: cancelled", name)
		}
		return "", fmt.Errorf("mcp tool %q: call failed on server %q: %w", name, conn.spec.ID, err)
	}
	text := RenderResult(res)
	if res.IsError {
		return "", fmt.Errorf("mcp tool %q: server error: %s", name, text)
	}
	return text, nil
}

// connFor returns the connection that owns a tool name (nil if unknown).
func (m *Manager) connFor(name string) *serverConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.connOrder {
		if strings.HasPrefix(name, ToolName(id, "")) {
			return m.conns[id]
		}
	}
	return nil
}

// Close tears down every connection: remote (HTTP) connections close cleanly, stdio children are
// terminated (the go-sdk CommandTransport Close: close stdin → SIGTERM → SIGKILL).
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.teardownLocked()
}

func (m *Manager) teardownLocked() error {
	var firstErr error
	for _, id := range m.connOrder {
		conn := m.conns[id]
		if conn == nil || conn.session == nil {
			continue
		}
		if err := conn.session.Close(); err != nil {
			m.log.Warn("mcp client: session close failed", "server", id, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	m.conns = make(map[string]*serverConn)
	m.connOrder = nil
	return firstErr
}

// ToolName builds the namespaced tool name mcp__<server>__<tool>.
func ToolName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// RenderResult renders a CallToolResult into a plain-text string the model can read.
func RenderResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			return string(b)
		}
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if c == nil {
			continue
		}
		if txt, ok := c.(*mcp.TextContent); ok && txt != nil {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(txt.Text)
			continue
		}
		if b, err := json.Marshal(c); err == nil {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(string(b))
		}
	}
	return sb.String()
}
