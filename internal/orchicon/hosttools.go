package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/worktree"
	"github.com/bmatcuk/doublestar/v4"
)

// hosttools.go: the native session engine's HOST tool suite — the real
// implementation of the ToolRegistry contract (ADR-0007) that the loop's
// `s.tools == nil` branch surfaced as "tool registry not configured".
// Until this file, sessions only ever carried MCP tools (bridge) and the
// loop's built-in memory/todo tools; every core capability (read, write,
// edit, bash, …) returned the registry-absent error, so native executions
// could survey nothing and write nothing ("the plan is delivered inline").
//
// Design:
//   - REUSE the opencode sidecar's battle-tested composite-tool engine
//     (internal/worktree: BatchRead/BatchGrep/BatchWrite) rather than a
//     parallel reimplementation — one containment boundary (Base), one
//     cap/truncation semantics, one skip-list for .git/node_modules/…
//   - Tool names + arg shapes match the sidecar's composite tools AND the
//     loop's existing native-name gates (isFileWritingTool, the todowrite
//     stash) so parity is by construction.
//   - Containment: every path resolves inside the execution's working dir
//     (manifest.WorktreePath when provisioned, else ProjectDir) + the
//     sanctioned scratch dir; ProjectRoot stays READ-only (writes can
//     never land in the main checkout — worktree hygiene).

// HostTools is the ToolRegistry handed to every native session: the core
// file/shell suite scoped to the execution's working directory. The bridge
// composes MCP tools on top (multi-registry).
type HostTools struct {
	base worktree.Base
}

// NewHostTools builds the host tool suite scoped to the execution's
// working directory (worktree when provisioned, else project dir). The
// project root is passed READ-only when a worktree is provisioned so
// reads can reach run-state files outside the worktree (mirroring the
// opencode sidecar's boundary); writes never reach it.
func NewHostTools(workingDir, projectRoot string) *HostTools {
	return &HostTools{base: worktree.Base{
		Worktree:    workingDir,
		ProjectRoot: projectRoot,
		ScratchDir:  worktree.DefaultScratchDir,
	}}
}

// hostToolDefs is the core suite advertised to the model every turn. Arg
// shapes match the opencode sidecar's composite tools (one grammar to
// learn) — write/edit use `filePath` (sidecar parity), NOT `path`.
var hostToolDefs = []ToolDef{
	{
		Name:        "batch_read",
		Description: "Read several files (or every file in a directory subtree) in a single call. Use this instead of one-at-a-time `read`s. Returns content with a per-file header, hard output caps, and explicit truncation markers so you never need to re-read a file for content you already have. Paths are project-relative; a directory is expanded recursively (bounded, skips .git/node_modules/dist/build/vendor).",
		ParamsJSON:  `{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"},"description":"Project-relative file or directory paths to read in one call (directories expand recursively)."},"max_bytes":{"type":"integer","description":"Total output cap (default 128000)."},"per_file_max":{"type":"integer","description":"Per-file cap (default 32000)."},"line_numbers":{"type":"boolean","description":"Prefix each line with its 1-based line number."}},"required":["paths"]}`,
	},
	{
		Name:        "batch_grep",
		Description: "Search several literal patterns across a subtree in a single call. Use this instead of one pattern per `grep` call. Returns only file:line matches (plus optional context), capped at a bounded number of matches. Paths are project-relative; a directory searches its WHOLE subtree recursively (skips .git/node_modules/dist/build/vendor); omit paths to search the whole working dir. If the walk hits its file cap the summary says so.",
		ParamsJSON:  `{"type":"object","properties":{"patterns":{"type":"array","items":{"type":"string"},"description":"Literal substrings to match (non-regex)."},"paths":{"type":"array","items":{"type":"string"},"description":"Project-relative paths/subtree to search (default [\".\"])."},"context_lines":{"type":"integer","description":"Lines of context before/after a match."},"max_matches":{"type":"integer","description":"Bounded match-line cap (default 250)."}},"required":["patterns"]}`,
	},
	{
		Name:        "batch_write",
		Description: "Apply several file writes in a single call. Use this instead of a long chain of one-at-a-time `write`/`edit` calls. Each write is {path, mode, content?, old?, new?}; mode is create|overwrite|edit|append. edit replaces every occurrence of `old` with `new`. The whole batch is validated up front and is all-or-nothing (nothing is written if any op is invalid).",
		ParamsJSON:  `{"type":"object","properties":{"writes":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"mode":{"type":"string","enum":["create","overwrite","edit","append"]},"content":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","mode"]}}},"required":["writes"]}`,
	},
	{
		Name:        "read",
		Description: "Read a single file. Thin wrapper over batch_read for a one-file need: pass `path`. Prefer batch_read when reading several files — one call carrying several reads is one turn instead of N.",
		ParamsJSON:  `{"type":"object","properties":{"path":{"type":"string","description":"Project-relative file path to read."},"max_bytes":{"type":"integer","description":"Output cap (default 128000)."},"line_numbers":{"type":"boolean","description":"Prefix each line with its 1-based line number."}},"required":["path"]}`,
	},
	{
		Name:        "grep",
		Description: "Search a literal pattern across a subtree (or a single path). Thin wrapper over batch_grep for a one-pattern need: pass `pattern` and optionally `path`. Prefer batch_grep when searching several patterns.",
		ParamsJSON:  `{"type":"object","properties":{"pattern":{"type":"string","description":"Literal substring to match (non-regex)."},"path":{"type":"string","description":"Project-relative path/subtree to search (default \".\" — the whole working dir)."},"context_lines":{"type":"integer","description":"Lines of context before/after a match."},"max_matches":{"type":"integer","description":"Bounded match-line cap (default 250)."}},"required":["pattern"]}`,
	},
	{
		Name:        "write",
		Description: "Write or overwrite a single file. Thin wrapper over batch_write for a one-file need: pass `filePath` and `content`. Prefer batch_write when applying several writes in one call.",
		ParamsJSON:  `{"type":"object","properties":{"filePath":{"type":"string","description":"Project-relative file path to write."},"content":{"type":"string","description":"Full file content."}},"required":["filePath","content"]}`,
	},
	{
		Name:        "edit",
		Description: "Apply an exact string replacement in a single file. Thin wrapper over batch_write for a one-edit need: pass `filePath`, `oldString`, `newString`. Prefer batch_write when applying several edits in one call.",
		ParamsJSON:  `{"type":"object","properties":{"filePath":{"type":"string","description":"Project-relative file path to edit."},"oldString":{"type":"string","description":"Substring to replace (every occurrence is replaced)."},"newString":{"type":"string","description":"Replacement text."}},"required":["filePath","oldString","newString"]}`,
	},
	{
		Name:        "list",
		Description: "List a directory's entries (the `ls` equivalent of glob — cheap path enumeration, never content). Pass one or more project-relative paths; directories expand to their entry names (subdirectories suffixed with /), files report their size. Bounded per directory. Prefer glob for pattern-based finding; use list to see what is actually in a directory.",
		ParamsJSON:  `{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"},"description":"Project-relative directories (or files) to list (default [\".\"])."},"max_entries":{"type":"integer","description":"Entries returned per directory (default 500)."}}}`,
	},
	{
		Name:        "glob",
		Description: "Find files by pattern (e.g. **/*.go). Patterns are doublestar globs rooted at the working dir; matches are project-relative. Bounded.",
		ParamsJSON:  `{"type":"object","properties":{"pattern":{"type":"string","description":"Doublestar glob pattern (e.g. **/*.go)."},"path":{"type":"string","description":"Root the glob at this project-relative subdir (default \".\")."},"max_matches":{"type":"integer","description":"Bounded match cap (default 500)."}},"required":["pattern"]}`,
	},
	{
		Name:        "bash",
		Description: "Run a shell command in the execution's working directory. Use for builds, tests, git, and anything the file tools cannot express. Output is capped; long-running commands are bounded by a timeout. Prefer the file tools for pure reads/writes — they are faster and capped deterministically.",
		ParamsJSON:  `{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run (bash -c)."},"timeout_seconds":{"type":"integer","description":"Hard timeout in seconds (default 120, max 600). The command is killed on expiry."}},"required":["command"]}`,
	},
	{
		Name:        "todoread",
		Description: "Read back the worker's LATEST todo list (the same list the execution UI renders from your todowrite calls). One cheap call to re-sync your plan mid-run — never re-derive it from memory. Returns each item with its status and priority.",
		ParamsJSON:  `{"type":"object","properties":{}}`,
	},
}

// Defs implements ToolRegistry: the host suite (a fresh slice — callers
// may append).
func (h *HostTools) Defs() []ToolDef {
	out := make([]ToolDef, len(hostToolDefs))
	copy(out, hostToolDefs)
	return out
}

// Execute implements ToolRegistry: route one call to the worktree engine
// (composite + thin wrappers) or the shell (bash). Every path argument
// resolves inside the execution's working dir + the sanctioned scratch
// dir; escapes are refused by the engine's containment boundary.
func (h *HostTools) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	switch name {
	case "batch_read":
		var a worktree.ReadArgs
		if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("batch_read: %w", err)
		}
		return worktree.BatchRead(h.base, a)
	case "batch_grep":
		var a worktree.GrepArgs
		if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("batch_grep: %w", err)
		}
		return worktree.BatchGrep(h.base, a)
	case "batch_write":
		var a worktree.WriteArgs
		if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("batch_write: %w", err)
		}
		return worktree.BatchWrite(h.base, a)
	case "read":
		return h.execRead(argsJSON)
	case "grep":
		return h.execGrep(argsJSON)
	case "write":
		return h.execWrite(argsJSON)
	case "edit":
		return h.execEdit(argsJSON)
	case "list":
		return h.execList(argsJSON)
	case "glob":
		return h.execGlob(argsJSON)
	case "bash":
		return h.execBash(ctx, argsJSON)
	case "todoread":
		// The loop serves todoread natively (session todo snapshot) when it
		// recognizes the name; reaching the registry here means it did not
		// — defensive parity with the sidecar's registry: an empty list,
		// never an error.
		return `{"todos":[]}`, nil
	default:
		return "", fmt.Errorf("hosttools: unknown tool %q", name)
	}
}

// --- thin wrappers over the composite engine -------------------------------

func (h *HostTools) execRead(argsJSON string) (string, error) {
	var a struct {
		Path        string `json:"path"`
		MaxBytes    int    `json:"max_bytes"`
		PerFile     int    `json:"per_file_max"`
		LineNumbers bool   `json:"line_numbers"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("read: path is required")
	}
	return worktree.BatchRead(h.base, worktree.ReadArgs{
		Paths:       []string{a.Path},
		MaxBytes:    a.MaxBytes,
		PerFile:     a.PerFile,
		LineNumbers: a.LineNumbers,
	})
}

func (h *HostTools) execGrep(argsJSON string) (string, error) {
	var a struct {
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		ContextLines int    `json:"context_lines"`
		MaxMatches   int    `json:"max_matches"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	return worktree.BatchGrep(h.base, worktree.GrepArgs{
		Patterns:     []string{a.Pattern},
		Paths:        pathsOrDot(a.Path),
		ContextLines: a.ContextLines,
		MaxMatches:   a.MaxMatches,
	})
}

func (h *HostTools) execWrite(argsJSON string) (string, error) {
	var a struct {
		FilePath string `json:"filePath"`
		Content  string `json:"content"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if a.FilePath == "" {
		return "", fmt.Errorf("write: filePath is required")
	}
	return worktree.BatchWrite(h.base, worktree.WriteArgs{
		Writes: []worktree.Write{{Path: a.FilePath, Mode: "overwrite", Content: a.Content}},
	})
}

func (h *HostTools) execEdit(argsJSON string) (string, error) {
	var a struct {
		FilePath  string `json:"filePath"`
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if a.FilePath == "" || a.OldString == "" {
		return "", fmt.Errorf("edit: filePath and oldString are required")
	}
	return worktree.BatchWrite(h.base, worktree.WriteArgs{
		Writes: []worktree.Write{{Path: a.FilePath, Mode: "edit", Old: a.OldString, New: a.NewString}},
	})
}

func (h *HostTools) execList(argsJSON string) (string, error) {
	var a struct {
		Paths      []string `json:"paths"`
		MaxEntries int      `json:"max_entries"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	if len(a.Paths) == 0 {
		a.Paths = []string{"."}
	}
	per := a.MaxEntries
	if per <= 0 {
		per = 500
	}
	type listEntry struct {
		Name string `json:"name"`
		Dir  bool   `json:"dir,omitempty"`
		Size int64  `json:"size,omitempty"`
	}
	out := map[string]any{}
	for _, p := range a.Paths {
		dir, err := h.resolveDir(p)
		if err != nil {
			return "", fmt.Errorf("list: %w", err)
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return "", fmt.Errorf("list %s: %w", p, err)
		}
		list := make([]listEntry, 0, len(ents))
		truncated := false
		for _, e := range ents {
			if len(list) >= per {
				truncated = true
				break
			}
			le := listEntry{Name: e.Name(), Dir: e.IsDir()}
			if !e.IsDir() {
				if info, ierr := e.Info(); ierr == nil {
					le.Size = info.Size()
				}
			}
			list = append(list, le)
		}
		key := p
		if key == "." {
			key = "./"
		}
		out[key] = map[string]any{"entries": list, "truncated": truncated}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	return string(b), nil
}

func (h *HostTools) execGlob(argsJSON string) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxMatches int    `json:"max_matches"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	max := a.MaxMatches
	if max <= 0 {
		max = 500
	}
	root := "."
	if a.Path != "" {
		root = a.Path
	}
	absRoot, err := h.resolveDir(root)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	matches, err := doublestarGlob(absRoot, a.Pattern, max)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	rel := make([]string, 0, len(matches))
	for _, m := range matches {
		r, rerr := filepath.Rel(h.base.Worktree, m)
		if rerr != nil {
			r = m
		}
		rel = append(rel, r)
	}
	b, _ := json.Marshal(map[string]any{"matches": rel, "truncated": len(matches) >= max})
	return string(b), nil
}

// bashTimeout bounds one bash tool call.
const (
	bashTimeoutDefault = 120 * time.Second
	bashTimeoutMax     = 600 * time.Second
)

func (h *HostTools) execBash(ctx context.Context, argsJSON string) (string, error) {
	var a struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("bash: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("bash: command is required")
	}
	to := bashTimeoutDefault
	if a.TimeoutSeconds > 0 {
		to = time.Duration(a.TimeoutSeconds) * time.Second
	}
	if to > bashTimeoutMax {
		to = bashTimeoutMax
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-c", a.Command)
	cmd.Dir = h.base.Worktree
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := cctx.Err(); ctxErr == context.DeadlineExceeded {
			return "", fmt.Errorf("bash: timed out after %s", to)
		}
		// Non-zero exit is a RESULT, not an error: the model needs stdout +
		// stderr to course-correct. Only infrastructure failures error.
		msg := string(out)
		if stderr.Len() > 0 {
			if msg != "" {
				msg += "\n"
			}
			msg += stderr.String()
		}
		if msg == "" {
			msg = err.Error()
		}
		return msg, nil
	}
	if stderr.Len() > 0 {
		return string(out) + "\n" + stderr.String(), nil
	}
	return string(out), nil
}

// resolveDir resolves a project-relative directory to an absolute path
// inside the containment boundary (worktree root, project root READ-only,
// or scratch). Returns an error on escape.
func (h *HostTools) resolveDir(p string) (string, error) {
	clean := filepath.Clean(p)
	if filepath.IsAbs(clean) {
		for _, root := range []string{h.base.Worktree, h.base.ProjectRoot, h.base.ScratchDir} {
			if root != "" && (clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))) {
				return clean, nil
			}
		}
		return "", fmt.Errorf("path %q escapes the working dir", p)
	}
	// Relative paths resolve against the worktree root only.
	return filepath.Join(h.base.Worktree, clean), nil
}

// pathsOrDot normalizes a single optional path into the slice form the
// batch engine expects (omitted → ["."]).
func pathsOrDot(p string) []string {
	if p == "" {
		return nil // engine defaults to ["."]
	}
	return []string{p}
}

// jsonStringArray marshals a []string as a JSON array (error-swallowed:
// strings never fail).
func jsonStringArray(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}

// doublestarGlob matches pattern under root (doublestar semantics,
// rooted AT root so `**/*.go` stays inside it), returning absolute paths,
// bounded by max.
func doublestarGlob(root, pattern string, max int) ([]string, error) {
	fsys := os.DirFS(root)
	matches, err := doublestar.Glob(fsys, pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	if len(matches) > max {
		matches = matches[:max]
	}
	abs := make([]string, 0, len(matches))
	for _, m := range matches {
		abs = append(abs, filepath.Join(root, m))
	}
	return abs, nil
}
