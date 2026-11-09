// Single-op wrappers + the `list` and `todoread` tools for the composite worktree tool suite (D1/D2
// of the core-tool-suite work item).
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

// List implements `list`: a directory listing equivalent of glob.
func List(b Base, args ListArgs) (string, error) {
	paths := args.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	maxEntries := args.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultListEntries
	}
	var sb strings.Builder
	var skipped []string
	for _, e := range paths {
		p, err := safeResolve(b, e, false)
		if err != nil {
			return "", fmt.Errorf("list: %w", err)
		}
		info, err := os.Stat(p)
		if err != nil {
			skipped = append(skipped, e+" (not found)")
			continue
		}
		if !info.IsDir() {
			rel := relPath(b.Worktree, p)
			if strings.HasPrefix(rel, "..") {
				rel = p
			}
			sb.WriteString(fmt.Sprintf("%s (%s)\n", rel, humanBytes(int(info.Size()))))
			continue
		}
		des, rerr := os.ReadDir(p)
		if rerr != nil {
			skipped = append(skipped, e+" ("+rerr.Error()+")")
			continue
		}
		rel := relPath(b.Worktree, p)
		if strings.HasPrefix(rel, "..") {
			rel = p
		}
		header := "./" + rel
		if rel == "." {
			header = "."
		}
		sb.WriteString(header + "/\n")
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
			sb.WriteString("  " + n + "\n")
		}
		if trunc > 0 {
			sb.WriteString(fmt.Sprintf("  ... [%d more entries — re-list with max_entries raised or a narrower path]\n", trunc))
		}
	}
	if len(skipped) > 0 {
		sb.WriteString(fmt.Sprintf("list: %d path(s) skipped: %s\n", len(skipped), strings.Join(skipped, "; ")))
	}
	if sb.Len() == 0 && len(skipped) == 0 {
		return "list: nothing to list.", nil
	}
	return sb.String(), nil
}

// TodoItem is one entry of the latest todo list, matching the shape the
// GetExecutionTodos RPC surfaces (internal/execution/todos.go).
type TodoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// todoSnapshotFile is the sidecar file the DB-less worktree sidecar uses to persist the worker's
// latest todo list (mirrors the durable transcript's latest todowrite payload for DB-backed.
const todoSnapshotFile = ".orchicon/todos.json"

// THE PLANE MUST READ IT, AND THE PLANE IS NOT THE TOOL.
const TodoSnapshotName = "todos.json"

// RunStateDir returns the run-state directory for a run: <projectDir>/.orchicon/<runID>.
func RunStateDir(projectDir, runID string) string {
	if projectDir == "" || runID == "" {
		return ""
	}
	if !filepath.IsAbs(projectDir) {
		return ""
	}
	return filepath.Join(projectDir, ".orchicon", runID)
}

// SaveRunTodoSnapshot writes the worker's latest todo list into the RUN-STATE directory, which is
// the authoritative copy the todo panel reads.
func SaveRunTodoSnapshot(runStateDir string, items []TodoItem) error {
	if runStateDir == "" {
		return fmt.Errorf("no run-state directory: the tool was not told its run")
	}
	if len(items) == 0 {
		return nil
	}
	data, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("marshal todos: %w", err)
	}
	if err := os.MkdirAll(runStateDir, 0o755); err != nil {
		return fmt.Errorf("create run-state dir: %w", err)
	}
	abs := filepath.Join(runStateDir, TodoSnapshotName)
	tmp := abs + ".tmp." + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write todos: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		return fmt.Errorf("commit todos: %w", err)
	}
	return nil
}

// ReadRunTodoSnapshot reads the run-state todo snapshot.
func ReadRunTodoSnapshot(runStateDir string) ([]TodoItem, bool) {
	if runStateDir == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(runStateDir, TodoSnapshotName))
	if err != nil {
		return nil, false
	}
	var items []TodoItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, false
	}
	return items, true
}

// RenderTodoList renders a todo list as the human-readable text `todoread`
// returns. Shared so the worktree-scoped and run-state reads cannot drift.
func RenderTodoList(items []TodoItem) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("todoread: %d todo item(s):\n", len(items)))
	for i, t := range items {
		status := t.Status
		if status == "" {
			status = "pending"
		}
		pri := t.Priority
		if pri == "" {
			pri = "-"
		}
		sb.WriteString(fmt.Sprintf("  %d. [%s] (priority: %s) %s\n", i+1, status, pri, t.Content))
	}
	return sb.String()
}

// SaveTodoSnapshot persists the worker's latest todo list to the sidecar file so `todoread` can
// read it back without a Postgres transcript.
func SaveTodoSnapshot(b Base, items []TodoItem) {
	if len(items) == 0 {
		return
	}
	abs, err := safeResolve(b, todoSnapshotFile, true)
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

// TodoRead implements `todoread`: it reads back the worker's latest todo list snapshot (the same
// shape GetExecutionTodos returns for DB-backed sessions).
func TodoRead(b Base, _ TodoReadArgs) (string, error) {
	abs, err := safeResolve(b, todoSnapshotFile, false)
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
	var sb strings.Builder
	sb.WriteString(RenderTodoList(items))
	sb.WriteString("\nraw:\n" + string(data))
	return sb.String(), nil
}

// SingleReadArgs are the single-op `read` wrapper inputs.
type SingleReadArgs struct {
	Path        string `json:"path"`
	MaxBytes    int    `json:"max_bytes,omitempty"`
	LineNumbers bool   `json:"line_numbers,omitempty"`
}

// Read implements the single-op `read` wrapper: a one-element batch_read
// for a single path.
func Read(b Base, args SingleReadArgs) (string, error) {
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("read: path is required")
	}
	return BatchRead(b, ReadArgs{Paths: []string{args.Path}, MaxBytes: args.MaxBytes, LineNumbers: args.LineNumbers})
}

// SingleGrepArgs are the single-op `grep` wrapper inputs.
type SingleGrepArgs struct {
	Pattern      string `json:"pattern"`
	Path         string `json:"path,omitempty"`
	ContextLines int    `json:"context_lines,omitempty"`
	MaxMatches   int    `json:"max_matches,omitempty"`
}

// Grep implements the single-op `grep` wrapper: a one-pattern batch_grep.
func Grep(b Base, args SingleGrepArgs) (string, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}
	paths := []string{}
	if args.Path != "" {
		paths = []string{args.Path}
	}
	return BatchGrep(b, GrepArgs{Patterns: []string{args.Pattern}, Paths: paths, ContextLines: args.ContextLines, MaxMatches: args.MaxMatches})
}

// SingleWriteArgs are the single-op `write` wrapper inputs (opencode-builtin
// shape: {filePath, content}).
type SingleWriteArgs struct {
	FilePath string `json:"filePath"`
	Content  string `json:"content"`
}

// SingleWrite implements the single-op `write` wrapper: a one-op batch_write.
func SingleWrite(b Base, args SingleWriteArgs) (string, error) {
	if strings.TrimSpace(args.FilePath) == "" {
		return "", fmt.Errorf("write: filePath is required")
	}
	return BatchWrite(b, WriteArgs{Writes: []Write{{Path: args.FilePath, Mode: "overwrite", Content: args.Content}}})
}

// SingleEditArgs are the single-op `edit` wrapper inputs (opencode-builtin
// shape: {filePath, oldString, newString}).
type SingleEditArgs struct {
	FilePath  string `json:"filePath"`
	OldString string `json:"oldString"`
	NewString string `json:"newString"`
}

// SingleEdit implements the single-op `edit` wrapper: a one-op batch_write.
func SingleEdit(b Base, args SingleEditArgs) (string, error) {
	if strings.TrimSpace(args.FilePath) == "" {
		return "", fmt.Errorf("edit: filePath is required")
	}
	if args.OldString == "" {
		return "", fmt.Errorf("edit: oldString is required")
	}
	return BatchWrite(b, WriteArgs{Writes: []Write{{Path: args.FilePath, Mode: "edit", Old: args.OldString, New: args.NewString}}})
}
