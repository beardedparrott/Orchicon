package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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
	// failures records, per server, why it is NOT part of defs. ONE UNSTARTABLE SERVER MUST NOT COST
	// THE REST OF THE SURFACE (see Start), so a failure is a recorded fact rather than an abort.
	failures []ServerFailure
}

// ServerFailure is one server that could not be started, and why. It is the shape a caller reports to
// the operator — the point of Start's per-spec tolerance is that this is a DIAGNOSTIC, not a
// catastrophe.
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

// Start lazily connects to every resolved server and discovers its tools. It is called at session
// start (per-session lazy startup) and blocks until discovery completes.
//
// PER-SPEC FAILURE TOLERANCE, and it is the whole point of this function's shape.
//
// This used to abort the union on the FIRST server that would not start: it tore down every connection
// it had built and returned that one error. Reported consequence — a conversation resolving TWO servers
// (123 aseprite tools + 166 unity tools) received NEITHER, because one of them timed out. The model then
// correctly observed that it had no MCP tools, correctly concluded the tools were unavailable, and spent
// the session driving the server over a hand-written stdio bridge. 289 working tools were lost to 1
// broken one, and nothing said so.
//
// So a failing server now costs only ITS OWN tools. Its failure is recorded in Failures() for the caller
// to log and for the model to be told about, and the error is returned ONLY when the surface is genuinely
// empty — no specs at all, or every spec failed — because that is the case a caller cannot continue past
// silently.
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
	// THE SURFACE IS EMPTY AND THAT IS NOT A PARTIAL FAILURE. Either nothing was configured (callers
	// handle that before Start) or every configured server failed — in both cases a caller that keeps
	// going would be running without any MCP tools it was promised, so say so.
	if len(specs) > 0 && len(m.connOrder) == 0 {
		return nil, fmt.Errorf("no MCP server could be started (%d failed): %w", len(m.failures), m.failures[0].Err)
	}
	return defs, nil
}

// Failures returns the servers that could not be started, in spec order. It is the report a caller logs
// and, where it can, tells the model about: "these tools are missing and here is why" is actionable in a
// way that an empty tool array is not.
func (m *Manager) Failures() []ServerFailure {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerFailure, len(m.failures))
	copy(out, m.failures)
	return out
}

// connectOne dials one server (connect + tools/list) under a timeout.
func (m *Manager) connectOne(ctx context.Context, spec ServerSpec) (*serverConn, error) {
	timeout := connectTimeout(spec)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var transport mcp.Transport
	// spawn is non-nil only for a stdio child: it carries the command and its captured stderr, which is
	// what turns "context deadline exceeded" into a diagnosis (stdio_spawn.go).
	var spawn *stdioSpawn
	switch spec.TransportType() {
	case TypeHTTP:
		t, err := newHTTPTransport(spec)
		if err != nil {
			return nil, err
		}
		transport = t
	default:
		sp, err := newStdioTransport(spec)
		if err != nil {
			return nil, err
		}
		spawn = sp
		transport = sp.transport
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "orchicon",
		Version: "0.1.0",
	}, nil)
	started := time.Now()
	session, err := client.Connect(cctx, transport, nil)
	if err != nil {
		// KILL THE CHILD WE JUST SPAWNED, because the SDK will not. mcp.connect returns the handshake
		// error without closing the transport, so the only handle on a live child was being dropped here
		// on every failed start — which is how a server was found still running 18 hours later. Killing it
		// is both the leak fix and what makes the diagnosis below truthful (the child is gone either way).
		if spawn != nil {
			killStopsTheChild(spawn.cmd)
		}
		elapsed := time.Since(started).Round(time.Millisecond)
		detail := ""
		if spawn != nil {
			detail = truncateForError(spawn.diagnosis())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp server %q: connect timed out after %s (%s transport, waited %s)%s: %w",
				spec.ID, timeout, spec.TransportType(), elapsed, detail, err)
		}
		return nil, fmt.Errorf("mcp server %q: connect failed after %s (%s transport)%s: %w",
			spec.ID, elapsed, spec.TransportType(), detail, err)
	}

	res, err := session.ListTools(cctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()
		detail := ""
		if spawn != nil {
			detail = truncateForError(spawn.diagnosis())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp server %q: tool discovery timed out after %s%s: %w", spec.ID, timeout, detail, err)
		}
		return nil, fmt.Errorf("mcp server %q: tool discovery failed%s: %w", spec.ID, detail, err)
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
