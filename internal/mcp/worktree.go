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
// exactly like any other opencode tool, plus the single-op wrappers
// (read/write/edit/grep), the `list` directory-listing tool, and the
// `todoread` todo read-back. The registry is selected instead of the
// askorchicon (DB) registry when the server is spawned with
// ORCHICON_MCP_WORKTREE_DIR set; it operates purely on the filesystem within
// that base directory and needs no Postgres connection.
//
// Tool names are NOT prefixed with `orchicon_` — they are intended to be the
// drop-in replacements for opencode's built-in `read`/`grep`/`write`/`edit`
// tools, so the model reaches for them naturally. The batch tools remain the
// documented PREFERRED interface for independent operations (one call
// carrying several operations = one turn instead of N); the single-op
// variants are thin wrappers over the same batch engine.
type worktreeRegistry struct {
	baseDir     string
	projectRoot string
}

// NewWorktreeRegistry returns a ToolRegistry bound to a worktree base
// directory. All path arguments are resolved against it and can never escape.
func NewWorktreeRegistry(baseDir, projectRoot string) ToolRegistry {
	return &worktreeRegistry{baseDir: baseDir, projectRoot: projectRoot}
}

func (r *worktreeRegistry) List() []ToolDef {
	return []ToolDef{
		{
			Name:        "batch_read",
			Description: "Read several files (or every file in a directory subtree) in a single call. Use this instead of one-at-a-time `read`s. Returns content with a per-file header, hard output caps, and explicit truncation markers so you never need to re-read a file for content you already have. Paths are project-relative; a directory is expanded recursively (bounded, skips .git/node_modules/dist/build/vendor).",
			Properties: map[string]propertySchema{
				"paths":        {Type: "array", Description: "Project-relative file or directory paths to read in one call (directories expand recursively)."},
				"max_bytes":    {Type: "integer", Description: "Total output cap (default 128000)."},
				"per_file_max": {Type: "integer", Description: "Per-file cap (default 32000)."},
				"line_numbers": {Type: "boolean", Description: "Prefix each line with its 1-based line number."},
			},
			Required: []string{"paths"},
		},
		{
			Name:        "batch_grep",
			Description: "Search several literal patterns across a subtree in a single call. Use this instead of one pattern per `grep` call. Returns only file:line matches (plus optional context), capped at a bounded number of matches. Paths are project-relative; a path naming a directory searches its WHOLE subtree recursively (skips .git/node_modules/dist/build/vendor); omit `paths` to search the whole worktree. If the walk hits its file cap the summary says so.",
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
		{
			Name:        "read",
			Description: "Read a single file. Thin wrapper over batch_read for a one-file need: pass `path`. Prefer batch_read when reading several files — one call carrying several reads is one turn instead of N.",
			Properties: map[string]propertySchema{
				"path":         {Type: "string", Description: "Project-relative file path to read."},
				"max_bytes":    {Type: "integer", Description: "Output cap (default 128000)."},
				"line_numbers": {Type: "boolean", Description: "Prefix each line with its 1-based line number."},
			},
			Required: []string{"path"},
		},
		{
			Name:        "grep",
			Description: "Search a literal pattern across a subtree (or a single path). Thin wrapper over batch_grep for a one-pattern need: pass `pattern` and optionally `path`. Prefer batch_grep when searching several patterns.",
			Properties: map[string]propertySchema{
				"pattern":       {Type: "string", Description: "Literal substring to match (non-regex)."},
				"path":          {Type: "string", Description: "Project-relative path/subtree to search (default \".\" — the whole worktree)."},
				"context_lines": {Type: "integer", Description: "Lines of context before/after a match."},
				"max_matches":   {Type: "integer", Description: "Bounded match-line cap (default 250)."},
			},
			Required: []string{"pattern"},
		},
		{
			Name:        "write",
			Description: "Write or overwrite a single file. Thin wrapper over batch_write for a one-file need: pass `filePath` and `content`. Prefer batch_write when applying several writes in one call.",
			Properties: map[string]propertySchema{
				"filePath": {Type: "string", Description: "Project-relative file path to write."},
				"content":  {Type: "string", Description: "Full file content."},
			},
			Required: []string{"filePath", "content"},
			Mutating: true,
		},
		{
			Name:        "edit",
			Description: "Apply an exact string replacement in a single file. Thin wrapper over batch_write for a one-edit need: pass `filePath`, `oldString`, `newString`. Prefer batch_write when applying several edits in one call.",
			Properties: map[string]propertySchema{
				"filePath":  {Type: "string", Description: "Project-relative file path to edit."},
				"oldString": {Type: "string", Description: "Substring to replace (every occurrence is replaced)."},
				"newString": {Type: "string", Description: "Replacement text."},
			},
			Required: []string{"filePath", "oldString", "newString"},
			Mutating: true,
		},
		{
			Name:        "list",
			Description: "List a directory's entries (the `ls` equivalent of glob — cheap path enumeration, never content). Pass one or more project-relative paths; directories expand to their entry names (subdirectories suffixed with /), files report their size. Bounded per directory. Prefer glob for pattern-based finding; use list to see what is actually in a directory.",
			Properties: map[string]propertySchema{
				"paths":       {Type: "array", Description: "Project-relative directories (or files) to list (default [\".\"])."},
				"max_entries": {Type: "integer", Description: "Entries returned per directory (default 500)."},
			},
		},
		{
			Name:        "todoread",
			Description: "Read back the worker's LATEST todo list (the same list the execution UI renders from your todowrite calls). One cheap call to re-sync your plan mid-run — never re-derive it from memory. Returns each item with its status and priority.",
			Properties:  map[string]propertySchema{},
		},
	}
}

// Executes a batch tool. The db.Pool is intentionally unused: these are
// filesystem tools scoped to the worktree base, never the platform DB.
func (r *worktreeRegistry) Execute(_ context.Context, _ *db.Pool, name string, args json.RawMessage) (json.RawMessage, error) {
	// b is the scoping boundary for every worktree.* call: reads reach the
	// worktree + the sanctioned scratch + the READ-only project root (the
	// run-state .orchicon/<run>/ and architecture-notes); writes reach only
	// the worktree + scratch, so batch_write never lands in the main checkout.
	b := worktree.Base{Worktree: r.baseDir, ProjectRoot: r.projectRoot, ScratchDir: worktree.DefaultScratchDir}
	switch name {
	case "batch_read":
		var a worktree.ReadArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("batch_read: %w", err)
		}
		out, err := worktree.BatchRead(b, a)
		if err != nil {
			return nil, fmt.Errorf("batch_read: %w", err)
		}
		return json.RawMessage(out), nil
	case "batch_grep":
		var a worktree.GrepArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("batch_grep: %w", err)
		}
		out, err := worktree.BatchGrep(b, a)
		if err != nil {
			return nil, fmt.Errorf("batch_grep: %w", err)
		}
		return json.RawMessage(out), nil
	case "batch_write":
		var a worktree.WriteArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("batch_write: %w", err)
		}
		out, err := worktree.BatchWrite(b, a)
		if err != nil {
			return nil, fmt.Errorf("batch_write: %w", err)
		}
		return json.RawMessage(out), nil
	case "read":
		var a worktree.SingleReadArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		out, err := worktree.Read(b, a)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		return json.RawMessage(out), nil
	case "grep":
		var a worktree.SingleGrepArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("grep: %w", err)
		}
		out, err := worktree.Grep(b, a)
		if err != nil {
			return nil, fmt.Errorf("grep: %w", err)
		}
		return json.RawMessage(out), nil
	case "write":
		var a worktree.SingleWriteArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
		out, err := worktree.SingleWrite(b, a)
		if err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
		return json.RawMessage(out), nil
	case "edit":
		var a worktree.SingleEditArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("edit: %w", err)
		}
		out, err := worktree.SingleEdit(b, a)
		if err != nil {
			return nil, fmt.Errorf("edit: %w", err)
		}
		return json.RawMessage(out), nil
	case "list":
		var a worktree.ListArgs
		if err := worktree.MarshalArgs(args, &a); err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}
		out, err := worktree.List(b, a)
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}
		return json.RawMessage(out), nil
	case "todoread":
		out, err := worktree.TodoRead(b, worktree.TodoReadArgs{})
		if err != nil {
			return nil, fmt.Errorf("todoread: %w", err)
		}
		return json.RawMessage(out), nil
	}
	return nil, fmt.Errorf("unknown worktree tool: %s", name)
}
