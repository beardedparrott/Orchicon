// Single-op wrappers + the `list` and `todoread` tools for the composite
// worktree tool suite (D1/D2 of the core-tool-suite work item).
//
// The batch tools (batch_read / batch_grep / batch_write) remain the
// documented PREFERRED interface for independent operations — one call
// carrying several operations is one turn instead of N. The single-op
// variants below are THIN WRAPPERS over the same batch engine (a
// one-element batch call) so a worker that genuinely needs one read/grep/
// write/edit does not pay batch-arg overhead. Every path still goes
// through safeResolve, so the wrappers inherit the same project-dir +
// /tmp containment as the batch tools.
//
// `list` is a directory-listing tool (the `ls`-equivalent the work item
// lists alongside glob): traversal-safe, bounded, and cheap — it enumerates
// names, never content, so it stays a low-token path-enumeration call.
//
// `todoread` reads back the worker's LATEST todo list. In the DB-less
// worktree sidecar there is no Postgres transcript, so the worker session
// writes its current todo snapshot to a sidecar file under .orchicon/ and
// todoread returns it verbatim — the same shape the GetExecutionTodos RPC
// surfaces for DB-backed sessions (native surface by construction).
package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// defaultListEntries bounds a single `list` call (directory expansion cap
// for entries returned per directory).
const defaultListEntries = 500

// ListArgs are the `list` tool inputs.
type ListArgs struct {
	// Paths are project/worktree-relative directories (or files) to list.
	// Omitted → the worktree root.
	Paths []string `json:"paths,omitempty"`
	// MaxEntries bounds the entries returned per directory.
	MaxEntries int `json:"max_entries,omitempty"`
}

// List implements `list`: a directory listing equivalent of glob. It
// returns, per requested path, the names of the entries inside it
// (directories suffixed with "/") plus a one-line stat hint, bounded and
// traversal-safe. A path naming a regular file is reported as a single
// entry with its size.
func List(base string, args ListArgs) (string, error) {
	paths := args.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	maxEntries := args.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultListEntries
	}
	var b strings.Builder
	var skipped []string
	for _, e := range paths {
		p, err := safeResolve(base, e)
		if err != nil {
			return "", fmt.Errorf("list: %w", err)
		}
		info, err := os.Stat(p)
		if err != nil {
			skipped = append(skipped, e+" (not found)")
			continue
		}
		if !info.IsDir() {
			rel := relPath(base, p)
			if strings.HasPrefix(rel, "..") {
				rel = p
			}
			b.WriteString(fmt.Sprintf("%s (%s)\n", rel, humanBytes(int(info.Size()))))
			continue
		}
		des, rerr := os.ReadDir(p)
		if rerr != nil {
			skipped = append(skipped, e+" ("+rerr.Error()+")")
			continue
		}
		rel := relPath(base, p)
		if strings.HasPrefix(rel, "..") {
			rel = p
		}
		header := "./" + rel
		if rel == "." {
			header = "."
		}
		b.WriteString(header + "/\n")
		names := make([]string, 0, len(des))
		for _, de := range des {
			name := de.Name()
			if de.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		sort.Strings(names)
		trunc := 0
		for _, n := range names {
			if len(names) > maxEntries && trunc >= maxEntries {
				trunc++
				continue
			}
			b.WriteString("  " + n + "\n")
		}
		if trunc > 0 {
			b.WriteString(fmt.Sprintf("  ... [%d more entries — re-list with max_entries raised or a narrower path]\n", trunc))
		}
	}
	if len(skipped) > 0 {
		b.WriteString(fmt.Sprintf("list: %d path(s) skipped: %s\n", len(skipped), strings.Join(skipped, "; ")))
	}
	if b.Len() == 0 && len(skipped) == 0 {
		return "list: nothing to list.", nil
	}
	return b.String(), nil
}

// TodoItem is one entry of the latest todo list, matching the shape the
// GetExecutionTodos RPC surfaces (internal/execution/todos.go).
type TodoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// todoSnapshotFile is the sidecar file the DB-less worktree sidecar uses
// to persist the worker's latest todo list (mirrors the durable
// transcript's latest todowrite payload for DB-backed sessions). The
// .orchicon/ directory is gitignored, so the snapshot never lands in the
// repo; it is pruned with the worktree.
const todoSnapshotFile = ".orchicon/todos.json"

// SaveTodoSnapshot persists the worker's latest todo list to the sidecar
// file so `todoread` can read it back without a Postgres transcript. The
// file is written atomically (temp + rename) and best-effort: a failed
// snapshot write is a log-level concern, never a tool failure — the
// todowrite call itself (an opencode built-in) already succeeded.
func SaveTodoSnapshot(base string, items []TodoItem) {
	if len(items) == 0 {
		return
	}
	abs, err := safeResolve(base, todoSnapshotFile)
	if err != nil {
		return
	}
	data, err := json.Marshal(items)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return
	}
	tmp := abs + ".tmp." + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, abs)
}

// TodoReadArgs are the `todoread` tool inputs (currently empty — the
// tool returns the latest snapshot).
type TodoReadArgs struct{}

// TodoRead implements `todoread`: it reads back the worker's latest todo
// list snapshot (the same shape GetExecutionTodos returns for DB-backed
// sessions). Returns a human-readable rendering plus the raw JSON array.
// When no snapshot exists yet (no todowrite has been issued in this
// session), it returns an explicit empty-list message instead of an error
// — a worker that just started has no todos, which is not a failure.
func TodoRead(base string, _ TodoReadArgs) (string, error) {
	abs, err := safeResolve(base, todoSnapshotFile)
	if err != nil {
		return "", fmt.Errorf("todoread: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "todoread: no todo list has been recorded this session (call todowrite first).", nil
		}
		return "", fmt.Errorf("todoread: %w", err)
	}
	var items []TodoItem
	if err := json.Unmarshal(data, &items); err != nil {
		return "", fmt.Errorf("todoread: malformed snapshot: %w", err)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("todoread: %d todo item(s):\n", len(items)))
	for i, t := range items {
		status := t.Status
		if status == "" {
			status = "pending"
		}
		pri := t.Priority
		if pri == "" {
			pri = "-"
		}
		b.WriteString(fmt.Sprintf("  %d. [%s] (priority: %s) %s\n", i+1, status, pri, t.Content))
	}
	b.WriteString("\nraw:\n" + string(data))
	return b.String(), nil
}

// SingleReadArgs are the single-op `read` wrapper inputs.
type SingleReadArgs struct {
	Path        string `json:"path"`
	MaxBytes    int    `json:"max_bytes,omitempty"`
	LineNumbers bool   `json:"line_numbers,omitempty"`
}

// Read implements the single-op `read` wrapper: a one-element batch_read
// for a single path.
func Read(base string, args SingleReadArgs) (string, error) {
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("read: path is required")
	}
	return BatchRead(base, ReadArgs{Paths: []string{args.Path}, MaxBytes: args.MaxBytes, LineNumbers: args.LineNumbers})
}

// SingleGrepArgs are the single-op `grep` wrapper inputs.
type SingleGrepArgs struct {
	Pattern      string `json:"pattern"`
	Path         string `json:"path,omitempty"`
	ContextLines int    `json:"context_lines,omitempty"`
	MaxMatches   int    `json:"max_matches,omitempty"`
}

// Grep implements the single-op `grep` wrapper: a one-pattern batch_grep.
func Grep(base string, args SingleGrepArgs) (string, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}
	paths := []string{}
	if args.Path != "" {
		paths = []string{args.Path}
	}
	return BatchGrep(base, GrepArgs{Patterns: []string{args.Pattern}, Paths: paths, ContextLines: args.ContextLines, MaxMatches: args.MaxMatches})
}

// SingleWriteArgs are the single-op `write` wrapper inputs (opencode-builtin
// shape: {filePath, content}).
type SingleWriteArgs struct {
	FilePath string `json:"filePath"`
	Content  string `json:"content"`
}

// SingleWrite implements the single-op `write` wrapper: a one-op batch_write.
func SingleWrite(base string, args SingleWriteArgs) (string, error) {
	if strings.TrimSpace(args.FilePath) == "" {
		return "", fmt.Errorf("write: filePath is required")
	}
	return BatchWrite(base, WriteArgs{Writes: []Write{{Path: args.FilePath, Mode: "overwrite", Content: args.Content}}})
}

// SingleEditArgs are the single-op `edit` wrapper inputs (opencode-builtin
// shape: {filePath, oldString, newString}).
type SingleEditArgs struct {
	FilePath  string `json:"filePath"`
	OldString string `json:"oldString"`
	NewString string `json:"newString"`
}

// SingleEdit implements the single-op `edit` wrapper: a one-op batch_write.
func SingleEdit(base string, args SingleEditArgs) (string, error) {
	if strings.TrimSpace(args.FilePath) == "" {
		return "", fmt.Errorf("edit: filePath is required")
	}
	if args.OldString == "" {
		return "", fmt.Errorf("edit: oldString is required")
	}
	return BatchWrite(base, WriteArgs{Writes: []Write{{Path: args.FilePath, Mode: "edit", Old: args.OldString, New: args.NewString}}})
}
