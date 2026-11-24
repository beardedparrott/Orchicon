package orchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/worktree"
	"github.com/bmatcuk/doublestar/v4"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// hosttools.go: the native session engine's HOST tool suite.

// HostTools is the ToolRegistry handed to every native session: the core file/shell suite scoped to
// the execution's working directory.
type HostTools struct {
	base worktree.Base
	// containerExec routes bash into the run container; nil = in-process.
	containerExec func(ctx context.Context, command string, env []string, cwd string) (stdout, stderr string, exitCode int, err error)
	// homeDir is the ORIGINAL working directory (the conversation's project_dir), captured on first
	// rebind so use_project_dir can restore it. Empty until set_workdir is first called.
	homeDir string
	// envForBash, when set, REPLACES the inherited environment for in-process bash (cmd.Env).
	envForBash func() []string
	// pathPolicy, when set, is consulted with every path-like argument of every call BEFORE the call is
	// dispatched, and its error aborts the call verbatim.
	pathPolicy func(paths []string) error
}

// SetPathPolicy installs the persistent-permission-policy hook (see the
// field doc). A nil fn removes it.
func (h *HostTools) SetPathPolicy(fn func(paths []string) error) {
	h.pathPolicy = fn
}

// NewHostTools builds the host tool suite scoped to the execution's working directory (worktree
// when provisioned, else project dir).
func NewHostTools(workingDir, projectRoot string) *HostTools {
	return &HostTools{base: worktree.Base{
		Worktree:    workingDir,
		ProjectRoot: projectRoot,
		ScratchDir:  worktree.DefaultScratchDir,
	}}
}

// NewHostToolsUnrestricted builds the host tool suite for the INTERACTIVE (Ask) path: the same
// suite, the same relative-path anchor and bash cwd (workingDir).
func NewHostToolsUnrestricted(workingDir string) *HostTools {
	return &HostTools{base: worktree.Base{
		Worktree:     workingDir,
		ScratchDir:   worktree.DefaultScratchDir,
		AllowAnyPath: true,
	}}
}

// NewContainerHostTools is NewHostTools with bash routed into the run's container via execFn
// (command + env + cwd -> stdout/stderr/exit).
func NewContainerHostTools(workingDir, projectRoot string, execFn func(ctx context.Context, command string, env []string, cwd string) (string, string, int, error)) *HostTools {
	h := NewHostTools(workingDir, projectRoot)
	h.containerExec = execFn
	return h
}

// planeStateRootEnvPOISON is what a worker's bash is told about the plane's own state root.
//
// WHY A WORKER MUST NOT SEE IT. The plane's state root holds the live opencode stores, the instance
// data and the backups. Nothing a worker does needs to read or write it, and a worker that CAN reach it
// will: a test suite resolves a store path from the environment and writes to whatever it finds. That is
// not hypothetical — an unguarded `go test ./internal/runtime/` in a worker's worktree overwrote four
// live opencode stores with a 29-byte stub reading "this is not a sqlite database", which killed every
// serve start and every recovery pass for six hours.
//
// A SOURCE FIX IS NOT ENOUGH, and that is the point of doing it here. The source fix (#1008) protects
// worktrees cut from a CURRENT develop. Eight worktrees predating it still carry the unguarded test and
// keep re-poisoning the store every time a worker runs their suite. Scrubbing the variable in the
// worker's ENVIRONMENT protects every worktree at once, whatever its source version.
//
// The variable is REDIRECTED rather than removed: a test that needs a store still gets one, in the
// worker's own scratch, so this hides nothing and breaks no fixture.
func sandboxedBashEnviron(base []string, scratchDir string) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "XDG_DATA_HOME", "ORCHICON_DATA_DIR", "ORCHICON_SERVE_STATE_DIR", "ORCHICON_SERVE_LOG_FILE":
			// Dropped; re-pointed below at the worker's scratch.
			continue
		}
		out = append(out, kv)
	}
	if strings.TrimSpace(scratchDir) != "" {
		mine := filepath.Join(scratchDir, "xdg")
		_ = os.MkdirAll(mine, 0o700)
		out = append(out,
			"XDG_DATA_HOME="+mine,
			"ORCHICON_DATA_DIR="+filepath.Join(scratchDir, "data"),
		)
	}
	return out
}

// bashEnviron is the environment in-process bash actually gets: the caller's factory (when wired),
// passed through the plane-state scrub.
func (h *HostTools) bashEnviron() []string {
	base := os.Environ()
	if h.envForBash != nil {
		base = h.envForBash()
	}
	return sandboxedBashEnviron(base, h.base.ScratchDir)
}

// SetBashEnviron wires the environment factory for IN-PROCESS bash (cmd.Env).
func (h *HostTools) SetBashEnviron(fn func() []string) {
	h.envForBash = fn
}

// hostToolDefs is the core suite advertised to the model every turn.
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
		Name: "set_workdir",
		Description: "Rebind this session's relative-path anchor and bash working directory — typically to " +
			"a git worktree under .orchicon-worktrees/, so relative reads/writes and bare bash commands " +
			"operate inside it instead of the main checkout. ADDITIVE, never narrowing: the project " +
			"directory stays readable/writable and pre-approved, and use_project_dir restores the " +
			"original anchor. No arguments reports the current anchor and available roots.",
		ParamsJSON: `{"type":"object","properties":{"path":{"type":"string","description":"Absolute path, or a path relative to the project directory. It must lie inside the project directory or a configured worktree root."},"use_project_dir":{"type":"boolean","description":"Restore the project directory as the anchor (takes precedence over path)."}}}`,
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
	{
		Name:        "todowrite",
		Description: "Maintain the worker's live todo list (full replacement array on every call). Each item is {content, status, priority}; status is pending|in_progress|completed|cancelled. The execution UI renders this list live and the session folds it into the todo digest — emit it at every turn boundary.",
		ParamsJSON:  `{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string"},"priority":{"type":"string"}},"required":["content","status"]},"description":"Full replacement todo list."}},"required":["todos"]}`,
	},
}

// Defs implements ToolRegistry: the host suite (a fresh slice — callers
// may append).
func (h *HostTools) Defs() []ToolDef {
	out := make([]ToolDef, len(hostToolDefs))
	copy(out, hostToolDefs)
	return out
}

// Execute implements ToolRegistry: route one call to the worktree engine (composite + thin
// wrappers) or the shell (bash).
func (h *HostTools) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	// THE POLICY CHOKE POINT, and it runs before EVERY branch.
	if err := h.checkPolicy(name, argsJSON); err != nil {
		return "", err
	}
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
	case "set_workdir":
		return h.execSetWorkdir(argsJSON)
	case "bash":
		return h.execBash(ctx, argsJSON)
	case "todoread":
		// The loop serves todoread natively (session todo snapshot) when it recognizes the name; reaching
		// the registry here means it did not — defensive parity with the sidecar's registry: an empty list.
		return `{"todos":[]}`, nil
	case "todowrite":
		// Session todo state is folded into the digest by the loop's stashMutableToolCall before execution;
		// reaching the registry here means the loop did not intercept.
		return `{"ok":true}`, nil
	default:
		return "", fmt.Errorf("hosttools: unknown tool %q", name)
	}
}

// policyTargetCandidates extracts the path-like targets a call names, each in BOTH spellings the
// operator might have written a policy entry for: as the model wrote it (`~/.ssh/id_rsa`.
func (h *HostTools) policyTargetCandidates(name, argsJSON string) []string {
	var root any
	if err := json.Unmarshal([]byte(argsJSON), &root); err != nil {
		// Not JSON: fall back to the shell reading, which is what a
		// malformed/mis-typed call looks like.
		root = nil
	}
	raw := make([]string, 0, 8)
	collectPolicyPaths(root, &raw)
	if name == "bash" {
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err == nil {
			raw = append(raw, shellPolicyTokens(a.Command)...)
		}
	}
	out := make([]string, 0, len(raw)*2)
	seen := make(map[string]bool, len(raw)*2)
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range raw {
		add(t)
		// A path written RELATIVE lands inside the suite's working dir; pass the resolved form too so a
		// policy entry written either way matches.
		if !filepath.IsAbs(t) && !strings.HasPrefix(t, "~") && h.base.Worktree != "" {
			add(filepath.Join(h.base.Worktree, t))
		}
	}
	return out
}

// collectPolicyPaths walks a decoded JSON value and appends every string under a path-naming key
// (path/paths/filePath/filePaths), descending into objects and arrays so `writes: [{path.
func collectPolicyPaths(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch k {
			case "path", "paths", "filePath", "filePaths":
				switch s := val.(type) {
				case string:
					*out = append(*out, s)
				case []any:
					for _, e := range s {
						if str, ok := e.(string); ok {
							*out = append(*out, str)
						}
					}
				}
			default:
				collectPolicyPaths(val, out)
			}
		}
	case []any:
		for _, e := range t {
			collectPolicyPaths(e, out)
		}
	}
}

// shellPolicyTokens splits a bash command into whitespace-separated tokens and keeps the ones that
// could name a file: any token containing a path separator.
func shellPolicyTokens(command string) []string {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	fields := strings.Fields(command)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		tok := strings.Trim(f, `'"`)
		if i := strings.Index(tok, "="); i > 0 && !strings.Contains(tok[:i], "/") {
			tok = tok[i+1:] // VAR=path → path
			tok = strings.Trim(tok, `'"`)
		}
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "/") || strings.HasPrefix(tok, "~") {
			out = append(out, tok)
		}
	}
	return out
}

// checkPolicy runs the installed policy hook over the call's targets.
func (h *HostTools) checkPolicy(name, argsJSON string) error {
	if h.pathPolicy == nil {
		return nil
	}
	targets := h.policyTargetCandidates(name, argsJSON)
	if len(targets) == 0 {
		return nil
	}
	err := h.pathPolicy(targets)
	if err == nil {
		return nil
	}
	var lf interface{ PolicyLoadFailure() bool }
	if errors.As(err, &lf) && lf.PolicyLoadFailure() && readOnlyHostTool(name) {
		return nil
	}
	return err
}

// readOnlyHostTool reports whether a host-suite tool only reads.
func readOnlyHostTool(name string) bool {
	for _, n := range ConsentReadOnlyTools {
		if n == name {
			return true
		}
	}
	return false
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
	// Always-container: route through the run's container when wired.
	if h.containerExec != nil {
		stdout, stderr, code, err := h.containerExec(cctx, a.Command, []string{"ORCHICON_TEST_DSN=" + runtime.SandboxPostgresDSN}, h.base.Worktree)
		if err != nil {
			return "", fmt.Errorf("bash (container): %w", err)
		}
		msg := stdout
		if stderr != "" {
			if msg != "" {
				msg += "\n"
			}
			msg += stderr
		}
		if code != 0 && msg == "" {
			msg = fmt.Sprintf("exit code %d", code)
		}
		return msg, nil
	}
	cmd := procwin.CommandContext(cctx, "bash", "-c", a.Command)
	cmd.Dir = h.base.Worktree
	// ALWAYS SET, and always through the scrub: an unwired factory must not fall back to the plane's
	// own environment either (a worker inheriting ORCHICON_DATA_DIR is the bug this closes).
	cmd.Env = h.bashEnviron()
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

// resolveDir resolves a path for the directory-listing tools (list/glob) through the SAME resolver
// the composite tools use (worktree.ResolvePath).
func (h *HostTools) resolveDir(p string) (string, error) {
	return worktree.ResolvePath(h.base, p, false)
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

// doublestarGlob matches pattern under root (doublestar semantics, rooted AT root so `**/*.go`
// stays inside it), returning absolute paths, bounded by max.
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

// execSetWorkdir rebinds the session's relative-path anchor and bash cwd.
//
// WHY IT EXISTS. The Ask suite's base.Worktree is the conversation's project_dir, which is the right
// default but the wrong anchor for a session working in a git worktree: a relative `read`/`write` and
// every bare `bash` command silently operated on the main checkout while the work was supposed to be
// isolated. That failure is silent — edits land in the user's working tree, the tool reports success,
// and only a `git status` in the other tree reveals it.
//
// THE SAFETY STORY, which is why this cannot over-grant. The candidate must lie INSIDE the project
// directory (or one of the base's already-configured roots). A worktree at
// <project>/.orchicon-worktrees/<id> is therefore inside the PRE-APPROVED project directory, so
// rebinding to it grants no new consent — it is the same tree the operator already approved. Because
// containment is required, this tool can never be used to walk the session's scope to another
// project, and the consent layer's pre-approval set is untouched by it.
//
// ADDITIVE, NEVER NARROWING. The project directory remains readable and writable, and it remains the
// pre-approved root (the consent layer derives that from the conversation's scope, not from this
// base). Only the DEFAULT for relative paths and the bash cwd moves — which is exactly the ambiguity
// that caused the bug.
func (h *HostTools) execSetWorkdir(argsJSON string) (string, error) {
	var a struct {
		Path          string `json:"path"`
		UseProjectDir bool   `json:"use_project_dir"`
	}
	if err := worktree.MarshalArgs([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("set_workdir: %w", err)
	}
	// The original anchor, captured once so it survives repeated rebinds.
	if h.homeDir == "" {
		h.homeDir = h.base.Worktree
	}
	projectDir := h.homeDir
	available := h.workdirRoots()

	if strings.TrimSpace(a.Path) == "" && !a.UseProjectDir {
		return workdirReport(projectDir, h.base.Worktree, available), nil
	}
	if a.UseProjectDir {
		h.base.Worktree = projectDir
		return workdirReport(projectDir, h.base.Worktree, available), nil
	}

	candidate := strings.TrimSpace(a.Path)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(h.base.Worktree, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("set_workdir: resolve %q: %w", a.Path, err)
	}
	// Resolve links before the containment check so a symlink cannot name a directory
	// outside the allowed roots while appearing to be inside one.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("set_workdir: %q does not exist (%v)", abs, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("set_workdir: stat %q: %w", real, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("set_workdir: %q is not a directory", real)
	}
	if !h.withinWorkdirRoots(real) {
		return "", fmt.Errorf("set_workdir: %q is outside the allowed roots (%s) — a session may rebind only "+
			"inside its project directory or a configured worktree root", real, strings.Join(available, ", "))
	}
	h.base.Worktree = real
	return workdirReport(projectDir, h.base.Worktree, h.workdirRoots()), nil
}

// workdirRoots lists the directories a rebind may target: the project directory plus whatever extra
// roots the Ask path configured (its own project dir, and one level of .orchicon-worktrees entries).
func (h *HostTools) workdirRoots() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(h.homeDir)
	for _, r := range h.base.ExtraRoots {
		add(r)
	}
	// Worktrees are always UNDER the project dir in this platform's convention
	// (.orchicon-worktrees/<id>), so they are discovered rather than enumerated by the caller.
	if h.homeDir != "" {
		base := filepath.Join(h.homeDir, ".orchicon-worktrees")
		if entries, err := os.ReadDir(base); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					add(filepath.Join(base, e.Name()))
				}
			}
		}
	}
	return out
}

// withinWorkdirRoots reports whether target lies inside any allowed rebind root.
func (h *HostTools) withinWorkdirRoots(target string) bool {
	for _, root := range h.workdirRoots() {
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// workdirReport is the tool's "say what the state now is" payload — the operator and the model both
// need to see the anchor, not infer it.
func workdirReport(projectDir, current string, available []string) string {
	b, err := json.MarshalIndent(map[string]any{
		"project_dir": projectDir,
		"anchor":      current,
		"available":   available,
		"note": "Relative reads/writes and bare bash commands now resolve against " + current +
			". Absolute paths are unaffected, the project directory stays readable and writable, " +
			"and set_workdir with use_project_dir=true restores the project directory.",
	}, "", "  ")
	if err != nil {
		return "anchor: " + current
	}
	return string(b)
}

// SetExtraRoots configures additional read+write default roots (the interactive path's per-session
// worktree). Consulted only under AllowAnyPath, so a confined worker base is unaffected.
func (h *HostTools) SetExtraRoots(roots []string) { h.base.ExtraRoots = roots }

// SetWorkdir rebinds the relative-path anchor and bash cwd, and records the ORIGINAL anchor as
// homeDir so use_project_dir can restore it. Unlike the set_workdir TOOL it performs no containment
// check — it is the host's own restore path, called from the code that already resolved the root.
func (h *HostTools) SetWorkdir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	if h.homeDir == "" {
		h.homeDir = h.base.Worktree
	}
	h.base.Worktree = dir
}

// Anchor reports the current relative-path anchor (the bash cwd and the base for relative paths).
func (h *HostTools) Anchor() string { return h.base.Worktree }
