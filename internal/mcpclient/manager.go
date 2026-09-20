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

// ToolDef mirrors the native tool-registry definition shape (the orchicon
// adapter converts to orchicon.ToolDef). ParamsJSON is the raw JSON
// schema, passed through verbatim so the model sees the server's real
// signature.
type ToolDef struct {
	Name        string
	Description string
	ParamsJSON  string
}

// Manager owns the MCP client connections for one session/execution
// (ADR-0008): connections are established lazily at session start (never
// at control-plane boot), tool discovery runs once at Start so signatures
// are present in the model's first request, tool calls are routed per
// server with per-call timeouts and cancellation, and Close tears down
// every connection (killing stdio children) at session end.
//
// Manager is safe for concurrent use; the orchicon loop may call Execute
// from a worker pool.
type Manager struct {
	log *slog.Logger

	mu        sync.Mutex
	conns     map[string]*serverConn // by server id
	connOrder []string               // deterministic defs order
	defs      []ToolDef              // cached discovery result (all servers)
	started   bool
	closed    bool
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

// Start lazily connects to every resolved server and discovers its tools.
// It is called at session start (per-session lazy startup) and blocks
// until discovery completes or a server fails. On failure the partial
// connections are torn down and the error is returned (actionable).
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
			m.teardownLocked()
			return nil, err
		}
		m.conns[spec.ID] = conn
		m.connOrder = append(m.connOrder, spec.ID)
		defs = append(defs, conn.defs...)
	}
	m.defs = defs
	return defs, nil
}

// connectOne dials one server (connect + tools/list) under a timeout.
func (m *Manager) connectOne(ctx context.Context, spec ServerSpec) (*serverConn, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout(spec))
	defer cancel()

	var transport mcp.Transport
	switch spec.TransportType() {
	case TypeHTTP:
		t, err := newHTTPTransport(spec)
		if err != nil {
			return nil, err
		}
		transport = t
	default:
		t, err := newStdioTransport(spec)
		if err != nil {
			return nil, err
		}
		transport = t
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "orchicon",
		Version: "0.1.0",
	}, nil)
	session, err := client.Connect(cctx, transport, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp server %q: connect timed out after %s (%s transport): %w", spec.ID, connectTimeout(spec), spec.TransportType(), err)
		}
		return nil, fmt.Errorf("mcp server %q: connect failed (%s transport): %w", spec.ID, spec.TransportType(), err)
	}

	res, err := session.ListTools(cctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp server %q: tool discovery timed out after %s: %w", spec.ID, connectTimeout(spec), err)
		}
		return nil, fmt.Errorf("mcp server %q: tool discovery failed: %w", spec.ID, err)
	}

	conn := &serverConn{spec: spec, client: client, session: session, toolName: make(map[string]string, len(res.Tools))}
	for _, t := range res.Tools {
		if t == nil || t.Name == "" {
			continue
		}
		local := ToolName(spec.ID, t.Name)
		conn.toolName[local] = t.Name
		def := ToolDef{Name: local, Description: t.Description}
		if t.InputSchema != nil {
			schema, err := json.Marshal(t.InputSchema)
			if err == nil {
				def.ParamsJSON = string(schema)
			}
		}
		conn.defs = append(conn.defs, def)
	}
	return conn, nil
}

// Defs returns the discovered tool definitions (all servers), merged into
// the native tool registry as mcp__<server>__<tool>. Safe to call before
// Start (returns nil) and after Close (returns the last-known defs).
func (m *Manager) Defs() []ToolDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolDef, len(m.defs))
	copy(out, m.defs)
	return out
}

// Execute routes a tool call to the owning server connection with a
// per-call timeout and honors context cancellation. Errors are actionable:
// unknown tool, server down, tool error (IsError), or timeout.
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

// Close tears down every connection: remote (HTTP) connections close
// cleanly, stdio children are terminated (the go-sdk CommandTransport
// Close: close stdin → SIGTERM → SIGKILL). Idempotent and safe to call
// multiple times (session end + defer). After Close, Start returns an
// error.
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

// RenderResult renders a CallToolResult into a plain-text string the
// model can read. It prefers StructuredContent (the modern structured
// form); otherwise it concatenates text content items. Used both for
// successful results and IsError payloads.
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
