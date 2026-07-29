// Package mcp implements a Model Context Protocol (MCP) server that exposes
// Orchicon's tool registry as JSON-RPC methods over stdio. opencode and other
// MCP clients (Claude Desktop, Cursor, etc.) can connect to discover and call
// Orchicon tools — create projects, manage work items, list workers, etc.
//
// Protocol: JSON-RPC 2.0, newline-delimited, stdio transport.
//   Client sends requests to stdin, server writes responses to stdout.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"connectrpc.com/connect"
)

// JSON-RPC message types.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      any         `json:"id"`
	Result  any         `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	codeParse     = -32700
	codeInvalid   = -32600
	codeMethod    = -32601
	codeInternal  = -32603
)

// MCP method names.
const (
	methodInitialize       = "initialize"
	methodInitialized      = "notifications/initialized"
	methodToolsList        = "tools/list"
	methodToolsCall        = "tools/call"
)

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
}

// Ensure the askorchicon.ToolRegistry satisfies our interface via adapter.

// Server is a stdio-based MCP JSON-RPC server.
type Server struct {
	log    *slog.Logger
	pool   *db.Pool
	tools  ToolRegistry
}

// New creates an MCP server.
func New(log *slog.Logger, pool *db.Pool, tools ToolRegistry) *Server {
	return &Server{log: log, pool: pool, tools: tools}
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

func (s *Server) handleInitialize(req jsonRPCRequest) {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
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
		tools = append(tools, mcpTool{
			Name:        td.Name,
			Description: td.Description + fmt.Sprintf(" (%s)", mutabilityLabel(td.Mutating)),
			InputSchema: genericInputSchema(),
		})
	}
	s.writeResult(req.ID, map[string]any{"tools": tools})
}

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

	result, err := s.tools.Execute(ctx, s.pool, params.Name, params.Arguments)
	if err != nil {
		s.writeError(req.ID, codeInternal, fmt.Sprintf("Tool execution failed: %s", err), nil)
		return
	}

	// Return tool result as MCP content items.
	content := []map[string]any{
		{
			"type": "text",
			"text": string(result),
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

func genericInputSchema() inputSchema {
	return inputSchema{
		Type:       "object",
		Properties: map[string]propertySchema{},
	}
}

// Compile-time check: ensure connect is used (keeps the import alive).
var _ = connect.CodeInternal
