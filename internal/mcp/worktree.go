package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// worktreeRegistry exposes the composite, context-efficient file tools
// (batch_read / batch_grep / batch_write) over MCP so a worker calls them
// exactly like any other opencode tool. The registry is selected instead of
// the askorchicon (DB) registry when the server is spawned with
// ORCHICON_MCP_WORKTREE_DIR set; it operates purely on the filesystem within
// that base directory and needs no Postgres connection.
//
// Tool names are NOT prefixed with `orchicon_` — they are intended to be the
// drop-in replacements for opencode's built-in `read`/`grep`/`write`/`edit`
// tools, so the model reaches for them naturally.
type worktreeRegistry struct {
	baseDir string
}

// NewWorktreeRegistry returns a ToolRegistry bound to a worktree base
// directory. All path arguments are resolved against it and can never escape.
func NewWorktreeRegistry(baseDir string) ToolRegistry {
	return &worktreeRegistry{baseDir: baseDir}
}

func (r *worktreeRegistry) List() []ToolDef {
	return []ToolDef{
		{
			Name:        "batch_read",
			Description: "Read several files (or every immediate file in a directory) in a single call. Use this instead of one-at-a-time `read`s. Returns content with a per-file header, hard output caps, and explicit truncation markers so you never need to re-read a file for content you already have. Paths are project-relative.",
			Properties: map[string]propertySchema{
				"paths":        {Type: "array", Description: "Project-relative file or directory paths to read in one call."},
				"max_bytes":    {Type: "integer", Description: "Total output cap (default 128000)."},
				"per_file_max": {Type: "integer", Description: "Per-file cap (default 32000)."},
				"line_numbers": {Type: "boolean", Description: "Prefix each line with its 1-based line number."},
			},
			Required: []string{"paths"},
		},
		{
			Name:        "batch_grep",
			Description: "Search several literal patterns across a subtree in a single call. Use this instead of one pattern per `grep` call. Returns only file:line matches (plus optional context), capped at a bounded number of matches. Paths are project-relative; omit `paths` to search the whole worktree.",
			Properties: map[string]propertySchema{
				"patterns":      {Type: "array", Description: "Literal substrings to match (non-regex)."},
				"paths":         {Type: "array", Description: "Project-relative paths/subtree to search (default [\".\"])."},
				"context_lines": {Type: "integer", Description: "Lines of context before/after a match."},
				"max_matches":   {Type: "integer", Description: "Bounded match-line cap (default 250)."},
			},
			Required: []string{"patterns"},
		},
		{
			Name:        "batch_write",
			Description: "Apply several file writes in a single call. Use this instead of a long chain of one-at-a-time `write`/`edit` calls. Each write is {path, mode, content?, old?, new?}; mode is create|overwrite|edit|append. edit replaces every occurrence of `old` with `new`. The whole batch is validated up front and is all-or-nothing (nothing is written if any op is invalid).",
			Properties: map[string]propertySchema{
				"writes": {Type: "array", Description: "[{path, mode, content?, old?, new?}] — path is project-relative, mode is create|overwrite|edit|append."},
			},
			Required: []string{"writes"},
			Mutating: true,
		},
	}
}

// Executes a batch tool. The db.Pool is intentionally unused: these are
// filesystem tools scoped to the worktree base, never the platform DB.
func (r *worktreeRegistry) Execute(_ context.Context, _ *db.Pool, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "batch_read":
		var a worktree.ReadArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("batch_read: %w", err)
		}
		out, err := worktree.BatchRead(r.baseDir, a)
		if err != nil {
			return nil, fmt.Errorf("batch_read: %w", err)
		}
		return json.RawMessage(out), nil
	case "batch_grep":
		var a worktree.GrepArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("batch_grep: %w", err)
		}
		out, err := worktree.BatchGrep(r.baseDir, a)
		if err != nil {
			return nil, fmt.Errorf("batch_grep: %w", err)
		}
		return json.RawMessage(out), nil
	case "batch_write":
		var a worktree.WriteArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("batch_write: %w", err)
		}
		out, err := worktree.BatchWrite(r.baseDir, a)
		if err != nil {
			return nil, fmt.Errorf("batch_write: %w", err)
		}
		return json.RawMessage(out), nil
	}
	return nil, fmt.Errorf("unknown worktree tool: %s", name)
}
