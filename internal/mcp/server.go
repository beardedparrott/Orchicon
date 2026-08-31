// Package mcp implements a Model Context Protocol (MCP) server that exposes
// Orchicon's tool registry as JSON-RPC methods over stdio. opencode and other
// MCP clients (Claude Desktop, Cursor, etc.) can connect to discover and call
// Orchicon tools — create projects, manage work items, list workers, etc.
//
// Protocol: JSON-RPC 2.0, newline-delimited, stdio transport.
//
//	Client sends requests to stdin, server writes responses to stdout.
//
// Tenancy: the server operates on a single tenant per process, taken from
// the ORCHICON_MCP_TENANT_ID env var. The control plane injects that var via
// the opencode config's MCP server `environment` map, so an `orchicon mcp`
// sidecar spawned for a worker execution or an Ask Orchicon chat is scoped to
// the right tenant. When unset (e.g. a human wires `orchicon mcp` into Claude
// Desktop manually) it falls back to the dev tenant with a warning.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// JSON-RPC message types.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeMethod   = -32601
	codeInternal = -32603
)

// MCP method names.
const (
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodPing        = "ping"
	methodToolsList   = "tools/list"
	methodToolsCall   = "tools/call"
)

// Protocol version fallback when the client sends none. We echo the
// client's requested version in practice (see handleInitialize) because the
// tools/list + tools/call surface is identical across MCP protocol versions
// and some SDK clients reject a downgrade to an older version they no longer
// advertise.
const fallbackProtocolVersion = "2025-11-25"

// MCP tool definition matching the MCP spec schema.
type mcpTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]propertySchema `json:"properties"`
	Required   []string                  `json:"required,omitempty"`
}

type propertySchema struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Default     any    `json:"default,omitempty"`
}

// ToolRegistry is the interface the MCP server needs to list and execute tools.
type ToolRegistry interface {
	List() []ToolDef
	Execute(ctx context.Context, pool *db.Pool, name string, args json.RawMessage) (json.RawMessage, error)
}

// ToolDef is a simplified tool definition for the MCP server.
type ToolDef struct {
	Name        string
	Description string
	Mutating    bool
	Properties  map[string]propertySchema // input field definitions
	Required    []string                  // required field names
}

// Ensure the askorchicon.ToolRegistry satisfies our interface via adapter.

// Server is a stdio-based MCP JSON-RPC server.
type Server struct {
	log      *slog.Logger
	pool     *db.Pool
	tools    ToolRegistry
	tenantID string
	// runContext is the owning workflow run's run_context, so a work item
	// created during a recurring fire's run is stamped with the fire's
	// provenance (feature 4.1, AC2). Nil for a plain MCP.
	runContext []byte
}

// New creates an MCP server. The tenant is resolved from the
// ORCHICON_MCP_TENANT_ID env var (set by the control plane when it spawns
// opencode with the Orchicon MCP registered); falls back to the dev tenant
// with a warning so a manually-wired `orchicon mcp` still works.
func New(log *slog.Logger, pool *db.Pool, tools ToolRegistry) *Server {
	tenantID := os.Getenv("ORCHICON_MCP_TENANT_ID")
	if tenantID == "" {
		tenantID = "tnt_dev"
		if log != nil {
			log.Warn("ORCHICON_MCP_TENANT_ID unset — MCP server scoped to the dev tenant", "tenant_id", tenantID)
		}
	}
	return &Server{log: log, pool: pool, tools: tools, tenantID: tenantID, runContext: loadRunContext(log, pool, tenantID)}
}

// loadRunContext resolves the run_context of ORCHICON_MCP_WORKFLOW_RUN_ID (the
// workflow run whose runtime container hosts this worker) against the MCP's
// pool, so a work item created during a recurring fire's run is stamped with
// the fire's provenance block (feature 4.1, AC2). Best-effort: an unset id, a
// missing run, or a DB error yields nil, so a plain create is unaffected.
func loadRunContext(log *slog.Logger, pool *db.Pool, tenantID string) []byte {
	runID := os.Getenv("ORCHICON_MCP_WORKFLOW_RUN_ID")
	if runID == "" || pool == nil || tenantID == "" {
		return nil
	}
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		if log != nil {
			log.Warn("mcp: begin tenant tx for run_context", "error", err)
		}
		return nil
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if log != nil {
			log.Warn("mcp: load run_context", "run", runID, "error", err)
		}
		return nil
	}
	return run.RunContext
}

// Run reads JSON-RPC requests from stdin and writes responses to stdout until
// stdin closes. It blocks; call in a goroutine.
func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.writeError(nil, codeParse, "Parse error", nil)
			continue
		}
		s.handle(ctx, req)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcp stdin scan: %w", err)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, req jsonRPCRequest) {
	switch req.Method {
	case methodInitialize:
		s.handleInitialize(req)
	case methodInitialized:
		// Notification — no response expected.
	case methodPing:
		if req.ID != nil {
			s.writeResult(req.ID, map[string]any{})
		}
	case methodToolsList:
		s.handleToolsList(req)
	case methodToolsCall:
		s.handleToolsCall(ctx, req)
	default:
		if req.ID != nil {
			s.writeError(req.ID, codeMethod, fmt.Sprintf("Method not found: %s", req.Method), nil)
		}
	}
}

// handleInitialize negotiates the MCP protocol version. We echo the
// client's requested protocol version back verbatim: our tools/list and
// tools/call handling is identical across MCP protocol versions, and some
// SDK clients (opencode 1.18 sends 2025-11-25) reject a handshake that
// downgrades to an older version they no longer advertise. Empty requests
// fall back to the newest known version.
func (s *Server) handleInitialize(req jsonRPCRequest) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &params)

	version := params.ProtocolVersion
	if version == "" {
		version = fallbackProtocolVersion
	}

	result := map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "orchicon",
			"version": "0.1.0",
		},
	}
	s.writeResult(req.ID, result)
}

func (s *Server) handleToolsList(req jsonRPCRequest) {
	defs := s.tools.List()
	tools := make([]mcpTool, 0, len(defs))
	for _, td := range defs {
		t := mcpTool{
			Name:        td.Name,
			Description: td.Description + fmt.Sprintf(" (%s)", mutabilityLabel(td.Mutating)),
			// Always emit a non-nil properties map: the MCP SDK's
			// tools/list schema validation rejects `properties: null`.
			InputSchema: inputSchema{Type: "object", Properties: map[string]propertySchema{}},
		}
		if len(td.Properties) > 0 {
			t.InputSchema.Properties = td.Properties
			t.InputSchema.Required = td.Required
		}
		tools = append(tools, t)
	}
	s.writeResult(req.ID, map[string]any{"tools": tools})
}

// handleToolsCall executes a tool. Per the MCP spec, a tool that runs but
// fails returns a result with isError=true (not a JSON-RPC error); JSON-RPC
// errors are reserved for protocol-level problems (bad params, unknown tool).
func (s *Server) handleToolsCall(ctx context.Context, req jsonRPCRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, codeInvalid, "Invalid tool call parameters", nil)
		return
	}
	if params.Name == "" {
		s.writeError(req.ID, codeInvalid, "tool name is required", nil)
		return
	}
	if params.Arguments == nil {
		params.Arguments = json.RawMessage("{}")
	}

	// MCP runs over stdio with no HTTP middleware to inject the tenant,
	// so the tenant comes from the server's ORCHICON_MCP_TENANT_ID env
	// (set by the control plane when it spawns opencode). Every tool
	// function reads the tenant from context via tenant.FromContext() and
	// scopes its DB operations accordingly.
	ctx = tenant.WithID(ctx, s.tenantID)
	// Inject the owning workflow run's run_context so a work item created
	// during a recurring fire's run is stamped with automation provenance
	// (feature 4.1, AC2). No-op (nil) for a plain MCP.
	ctx = workitem.WithAutomationRunContext(ctx, s.runContext)

	result, err := s.tools.Execute(ctx, s.pool, params.Name, params.Arguments)
	if err != nil {
		s.writeResult(req.ID, map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": err.Error(),
				},
			},
			"isError": true,
		})
		return
	}

	// Return tool result as MCP content items. An empty/absent result is
	// still a successful call — surface it as a readable message so the
	// model never sees an empty content array.
	text := string(result)
	if text == "" || text == "null" {
		text = "ok"
	}
	content := []map[string]any{
		{
			"type": "text",
			"text": text,
		},
	}
	s.writeResult(req.ID, map[string]any{"content": content})
}

func (s *Server) writeResult(id any, result any) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintln(os.Stdout, string(data))
}

func (s *Server) writeError(id any, code int, message string, data any) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	enc, _ := json.Marshal(resp)
	fmt.Fprintln(os.Stdout, string(enc))
}

func mutabilityLabel(mutating bool) string {
	if mutating {
		return "mutates data — requires user confirmation"
	}
	return "read-only"
}

// Compile-time check: ensure connect is used (keeps the import alive).
var _ = connect.CodeInternal
