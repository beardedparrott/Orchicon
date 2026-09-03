// Package worktree implements the composite, context-efficient file tools
// Orchicon exposes to workers. They replace opencode's granular `read`,
// `grep` and `write`/`edit` tools with larger-chunk equivalents so a worker
// does the same work in far fewer turns — and, crucially, feeds far fewer
// tokens of bloat back into the conversation.
//
// Each tool is deliberately "capture the whole need in ONE call":
//
//	batch_read   — read many named files (or a directory) in a single call,
//	               with hard output caps + explicit truncation markers so the
//	               worker is never tempted to re-read for the part it already
//	               has. Empty/repeated paths are deduped.
//	batch_grep   — grep several patterns across a chosen subtree in a single
//	               call, returning only matches (file:line:content), capped.
//	batch_write  — apply several create/overwrite/edit/append operations in a
//	               single call, validated up-front (dry-run) and rolled back
//	               best-effort on any failure, so the worker stops issuing a
//	               long chain of one-at-a-time edits.
//
// Every path is resolved against the worktree base and is path-traversal-safe:
// absolute paths and any `..` that escapes the base are rejected outright.
// A path naming a directory is expanded RECURSIVELY into the files it
// contains (bounded by the file/dir caps), pruning VCS metadata and
// vendored/build output — so `batch_grep internal` really searches the
// subtree, and the whole-tree default (paths omitted → ".") stays fast.
package worktree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Default caps keep a single tool call well-bounded so it cannot blow up the
// context. They are high enough to capture a real need in one pass, low
// enough to never feed a document back wholesale.
const (
	defaultMaxBytes   = 128_000 // total batch_read output cap
	defaultPerFileMax = 32_000  // per-file read cap
	defaultMaxFiles   = 64      // directory expansion file cap
	defaultMaxMatches = 250     // batch_grep match cap
	defaultMaxDirs    = 64      // directory expansion dir cap
	// batch_grep's walk caps are deliberately larger than batch_read's: a
	// search returns only matches (bounded by maxBytesOut + maxMatches), so
	// reading more files is cheap — a read-sized 64-file cap would silently
	// miss most of a real tree (internal/ alone has 322 files).
	defaultMaxGrepFiles = 512
	defaultMaxGrepDirs  = 512
)

// ReadArgs are the batch_read tool inputs.
type ReadArgs struct {
	// Paths are project/worktree-relative paths. A directory expands to the
	// files it contains (bounded by the file cap).
	Paths []string `json:"paths"`
	// MaxBytes caps the total returned content (default defaultMaxBytes).
	MaxBytes int `json:"max_bytes,omitempty"`
	// PerFile caps each file's returned content (default defaultPerFileMax).
	PerFile int `json:"per_file_max,omitempty"`
	// LineNumbers prefixes each line with its 1-based line number.
	LineNumbers bool `json:"line_numbers,omitempty"`
}

// GrepArgs are the batch_grep tool inputs.
type GrepArgs struct {
	Patterns []string `json:"patterns"`
	Paths    []string `json:"paths,omitempty"` // omitted → ["."]
	// ContextLines adds N lines of context before/after a match.
	ContextLines int `json:"context_lines,omitempty"`
	// MaxMatches caps the total match lines returned.
	MaxMatches int `json:"max_matches,omitempty"`
}

// Write is a single operation within batch_write.
type Write struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"` // create | overwrite | edit | append
	Content string `json:"content,omitempty"`
	Old     string `json:"old,omitempty"` // edit: substring to replace
	New     string `json:"new,omitempty"` // edit: replacement
}

// WriteArgs are the batch_write tool inputs.
type WriteArgs struct {
	Writes []Write `json:"writes"`
}

// --- path safety ----------------------------------------------------------

// Base is the scoping boundary for the composite file tools. Each tool
// resolves every worker-supplied path against Worktree (the read+write
// root — the run worktree / in-place project dir), plus ProjectRoot for
// READ-only access and ScratchDir for read+write access.
type Base struct {
	// Worktree is the read+write root: the run worktree (provisioned) or the
	// in-place project dir. File writes and deletes resolve only inside it.
	Worktree string
	// ProjectRoot is the READ-only extra root: the project root that holds
	// the run-state `.orchicon/<run>/` files and `architecture-notes/`, which
	// sit OUTSIDE the run worktree (a sibling of it). Reads may additionally
	// reach here; writes never do (so batch_write stays out of the main
	// checkout). Empty in tests and in-place runs (worktree == project root).
	ProjectRoot string
	// ScratchDir is the read+write root for the sanctioned ephemeral scratch
	// area (guard.ScratchDir / opencode.ScratchDir, /tmp/orchicon). Replaces
	// the previously-broad os.TempDir() allow.
	ScratchDir string
}

// DefaultScratchDir is the sanctioned scratch area the composite tools may
// read and write: ONE documented boundary, aligned with guard.ScratchDir and
// opencode.ScratchDir (replaces the broad os.TempDir() allow).
const DefaultScratchDir = "/tmp/orchicon"

// BaseFor builds a Base for a plain worktree directory with the sanctioned
// scratch dir and NO project root. Used by direct-call tests and by callers
// that only need worktree+scratch scoping (e.g. the todo snapshot).
func BaseFor(worktreeDir string) Base {
	return Base{Worktree: worktreeDir, ScratchDir: DefaultScratchDir}
}

// safeResolve resolves a worker-supplied path against the scoping Base and
// rejects anything that escapes it. It is the single enforcement point for
// every composite tool.
//
// writable selects the read vs write roots:
//   - reads (writable==false): roots = {Worktree, ScratchDir, ProjectRoot}.
//     A `..` traversal is permitted ONLY when the cleaned, joined path still
//     lands under one of those roots — which lets a worktree worker read
//     <root>/.orchicon/<run>/facts_learned (via ../../.orchicon/<run>/... or
//     an absolute project-root path) without allowing arbitrary escape.
//   - writes (writable==true): roots = {Worktree, ScratchDir}. A `..` that
//     escapes to the project root is still rejected (batch_write must never
//     land in the main checkout); an absolute /tmp/orchicon/... path is
//     allowed via ScratchDir.
func safeResolve(b Base, rel string, writable bool) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path")
	}
	if b.Worktree == "" {
		return "", fmt.Errorf("empty worktree base")
	}
	roots := []string{filepath.Clean(b.Worktree)}
	if b.ScratchDir != "" {
		roots = append(roots, filepath.Clean(b.ScratchDir))
	}
	if !writable && b.ProjectRoot != "" {
		roots = append(roots, filepath.Clean(b.ProjectRoot))
	}
	under := func(p string) bool {
		for _, r := range roots {
			if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	if filepath.IsAbs(rel) {
		clean := filepath.Clean(rel)
		if under(clean) {
			return clean, nil
		}
		return "", fmt.Errorf("absolute path is outside the allowed roots (worktree/scratch/project root)")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		// `..` is allowed for a READ only when the joined path still resolves
		// under a read root (e.g. reaching the project-root run-state). For a
		// WRITE it is always rejected, so batch_write cannot escape the worktree.
		if writable {
			return "", fmt.Errorf("path escapes the worktree (..)")
		}
		p := filepath.Join(b.Worktree, clean)
		if under(p) {
			return p, nil
		}
		return "", fmt.Errorf("path escapes the workspace (..)")
	}
	p := filepath.Join(b.Worktree, clean)
	if !under(p) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return p, nil
}

// pruneDirName reports whether a directory should be pruned from a
// recursive walk: VCS metadata and vendored/build output that a worker
// never wants to read or search (a whole-tree grep over node_modules would
// otherwise drown in noise).
func pruneDirName(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".bzr",
		"node_modules", "vendor", "dist", "build", ".orchicon-worktrees":
		return true
	}
	return false
}

// expandPaths resolves each entry; a directory is expanded RECURSIVELY into
// the regular files it contains (bounded by the file/dir caps, pruning
// noise dirs via pruneDirName) so `batch_grep "internal"` searches the
// whole subtree and `batch_read "docs"` grabs the docs in one call. An
// unsafe path (absolute or traversal) is FATAL — the whole call is
// rejected so a crafted path can never touch a file outside the worktree.
// Missing paths are skipped with a note. truncated reports that the walk
// stopped at a cap with entries left unvisited — callers must surface it so
// a partial read/search is never mistaken for an exhaustive one.
func expandPaths(b Base, entries []string, maxFiles, maxDirs int) (files []string, skipped []string, truncated bool, err error) {
	seen := map[string]bool{}
	dirs := 0
	var walk func(dir string)
	walk = func(dir string) {
		if len(files) >= maxFiles {
			truncated = true
			return
		}
		des, rerr := os.ReadDir(dir)
		if rerr != nil {
			skipped = append(skipped, dir+" ("+rerr.Error()+")")
			return
		}
		for _, de := range des {
			if len(files) >= maxFiles {
				truncated = true
				break
			}
			name := de.Name()
			fp := filepath.Join(dir, name)
			if de.IsDir() {
				if pruneDirName(name) {
					continue
				}
				if dirs >= maxDirs {
					skipped = append(skipped, fp+" (dir cap reached)")
					truncated = true
					continue
				}
				dirs++
				walk(fp)
				continue
			}
			// Symlinks and specials are skipped during the walk: a symlink
			// to a directory could loop forever, and specials are not
			// readable content. (Explicit named paths below still follow
			// os.Stat, so a directly-named symlink target is read.)
			if !de.Type().IsRegular() {
				continue
			}
			if seen[fp] {
				continue
			}
			seen[fp] = true
			files = append(files, fp)
		}
	}
	for i, e := range entries {
		p, rerr := safeResolve(b, e, false)
		if rerr != nil {
			return nil, nil, false, rerr
		}
		info, serr := os.Stat(p)
		if serr != nil {
			skipped = append(skipped, e+" (not found)")
			continue
		}
		if info.IsDir() {
			if dirs >= maxDirs {
				skipped = append(skipped, e+" (dir cap reached)")
				truncated = true
				continue
			}
			dirs++
			walk(p)
			continue
		}
		if !info.Mode().IsRegular() {
			skipped = append(skipped, e+" (not a regular file)")
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		files = append(files, p)
		// Mark truncation only when the cap leaves more entries unvisited —
		// an exact-cap read (e.g. 64 named files) is NOT truncated.
		if len(files) >= maxFiles && i < len(entries)-1 {
			truncated = true
			break
		}
	}
	sort.Strings(files)
	return files, skipped, truncated, nil
}

// --- batch_read -----------------------------------------------------------

// BatchRead implements batch_read. It returns a bounded text block with a
// per-file header, explicit truncation markers, and a dedupe/skip summary.
func BatchRead(b Base, args ReadArgs) (string, error) {
	maxBytes := args.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	perFile := args.PerFile
	if perFile <= 0 {
		perFile = defaultPerFileMax
	}
	files, skipped, walkTruncated, err := expandPaths(b, args.Paths, defaultMaxFiles, defaultMaxDirs)
	if err != nil {
		return "", err
	}
	if len(args.Paths) > 0 && len(files) == 0 && len(skipped) == 0 {
		return "batch_read: no matching paths were readable.", nil
	}

	// Parallel read phase (D3): independent file reads run CONCURRENTLY with
	// bounded parallelism (default 8, overridable via
	// ORCHICON_TOOL_PARALLELISM) and the results are emitted in DETERMINISTIC
	// path order (the order `expandPaths` returned), never completion order —
	// the model sees one batch_read result block whose file order matches its
	// request order, so its reasoning stays stable. The per-file read itself
	// is the expensive part (disk + decode); the rendering below is serial.
	var (
		readWg sync.WaitGroup
		readMu sync.Mutex
	)
	type fileRead struct {
		fp   string
		rel  string
		data []byte
		err  error
	}
	reads := make([]fileRead, 0, len(files))
	par := readParallelism()
	sem := make(chan struct{}, par)
	for _, fp := range files {
		reads = append(reads, fileRead{fp: fp, rel: relPath(b.Worktree, fp)})
	}
	for i := range reads {
		readWg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer readWg.Done()
			defer func() { <-sem }()
			data, err := os.ReadFile(reads[i].fp)
			readMu.Lock()
			reads[i].data = data
			reads[i].err = err
			readMu.Unlock()
		}(i)
	}
	readWg.Wait()

	var buf bytes.Buffer
	total := 0
	truncated := 0
	for _, fr := range reads {
		if fr.err != nil {
			skipped = append(skipped, fr.fp+" ("+fr.err.Error()+")")
			continue
		}
		data := fr.data
		rel := fr.rel
		line := "==> " + rel + " (" + humanBytes(len(data)) + ") <==\n"
		// Reserve space for the header + a trailing marker even when hitting
		// the total cap, so the model sees which file was cut off.
		if total+len(line) > maxBytes && total > 0 {
			truncated++
			continue
		}
		buf.WriteString(line)
		total += len(line)

		content := data
		var shown []byte
		if len(content) > perFile {
			shown = content[:perFile]
			truncated++
		} else {
			shown = content
		}
		if args.LineNumbers {
			for i, l := range bytes.Split(shown, []byte("\n")) {
				buf.WriteString(fmt.Sprintf("%6d: %s\n", i+1, l))
			}
		} else {
			if len(shown) > 0 {
				buf.Write(shown)
				if shown[len(shown)-1] != '\n' {
					buf.WriteByte('\n')
				}
			}
		}
		total += len(shown)
		if len(content) > perFile {
			buf.WriteString(fmt.Sprintf("... [truncated %s in %s]\n", humanBytes(len(content)-perFile), rel))
			total += 40
		}
		if total >= maxBytes {
			truncated++
			break
		}
	}

	summary := fmt.Sprintf("batch_read: %d file(s)", len(files))
	if walkTruncated {
		summary += fmt.Sprintf(", walk capped at %d files", len(files))
	}
	if truncated > 0 {
		summary += fmt.Sprintf(", %d truncated", truncated)
	}
	if len(skipped) > 0 {
		summary += fmt.Sprintf(", %d skipped", len(skipped))
	}
	summary += "\n"
	return summary + buf.String(), nil
}

// --- batch_grep -----------------------------------------------------------

// BatchGrep implements batch_grep. It greps each pattern across the selected
// subtree (default ".") and returns only the matches, capped.
func BatchGrep(b Base, args GrepArgs) (string, error) {
	if len(args.Patterns) == 0 {
		return "", fmt.Errorf("batch_grep: at least one pattern is required")
	}
	paths := args.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	ctxN := args.ContextLines
	if ctxN < 0 {
		ctxN = 0
	}
	maxMatches := args.MaxMatches
	if maxMatches <= 0 {
		maxMatches = defaultMaxMatches
	}

	files, skipped, truncated, err := expandPaths(b, paths, defaultMaxGrepFiles, defaultMaxGrepDirs)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		if len(skipped) > 0 {
			return fmt.Sprintf("batch_grep: no files to search (all %d path(s) skipped: %s)", len(skipped), strings.Join(skipped, "; ")), nil
		}
		return fmt.Sprintf("batch_grep: no files to search (no matching files for paths: %s)", strings.Join(paths, ", ")), nil
	}

	var buf bytes.Buffer
	matched := 0
	bytesOut := 0
	for _, fp := range files {
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		rel := relPath(b.Worktree, fp)
		lines := bytes.Split(data, []byte("\n"))
		// Which lines match any pattern.
		hit := make([]bool, len(lines))
		for i, ln := range lines {
			if matchesAny(ln, args.Patterns) {
				hit[i] = true
				matched++
			}
		}
		if matched > maxMatches {
			break
		}
		for i := range lines {
			if !hit[i] {
				continue
			}
			lo := i - ctxN
			if lo < 0 {
				lo = 0
			}
			hi := i + ctxN + 1
			if hi > len(lines) {
				hi = len(lines)
			}
			for j := lo; j < hi; j++ {
				var prefix string
				if j == i {
					prefix = ">"
				} else {
					prefix = " "
				}
				lineOut := fmt.Sprintf("%s%s:%d:%s\n", prefix, rel, j+1, pathSafeLine(lines[j]))
				buf.WriteString(lineOut)
				bytesOut += len(lineOut)
			}
			buf.WriteString("\n")
			bytesOut++
		}
		if buf.Len() > maxBytesOut() {
			break
		}
	}
	out := buf.String()
	summary := fmt.Sprintf("batch_grep: %d match line(s) across %d file(s)", matched, len(files))
	if truncated {
		summary += fmt.Sprintf(", walk capped at %d files", len(files))
	}
	if len(skipped) > 0 {
		summary += fmt.Sprintf(", %d skipped", len(skipped))
	}
	if matched > maxMatches {
		summary += fmt.Sprintf(" (capped at %d)", maxMatches)
	}
	combined := summary + "\n" + out
	if len(combined) > maxBytesOut() {
		combined = combined[:maxBytesOut()] + "\n... [truncated]\n"
	}
	return combined, nil
}

// matchesAny reports whether a line contains any of the (literal) patterns.
func matchesAny(line []byte, patterns []string) bool {
	for _, p := range patterns {
		if p != "" && bytes.Contains(line, []byte(p)) {
			return true
		}
	}
	return false
}

// pathSafeLine replaces newlines so a single output line can never break the
// file:line framing.
func pathSafeLine(l []byte) []byte {
	l = bytes.ReplaceAll(l, []byte("\r"), []byte(" "))
	l = bytes.ReplaceAll(l, []byte("\n"), []byte(" "))
	return l
}

// maxBytesOut is the whole-result cap for batch_grep.
func maxBytesOut() int { return defaultMaxBytes }

// readParallelism is the bounded-parallelism cap for the batch_read parallel
// read phase (D3): independent file reads run concurrently up to this many
// goroutines. Configurable via ORCHICON_TOOL_PARALLELISM (0/negative → the
// default 8; 1 → fully serial). Deterministic result ORDER is preserved
// regardless of the cap — completion order never leaks into the output.
func readParallelism() int {
	const def = 8
	if v := os.Getenv("ORCHICON_TOOL_PARALLELISM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 1 {
				return def
			}
			return n
		}
	}
	return def
}

// --- batch_write ----------------------------------------------------------

// writeOp is a validated, applied operation plus the original disk state it
// must revert to on a mid-sequence failure.
type writeOp struct {
	w     Write
	abs   string
	next  string
	orig  string // on-disk content before this batch ("" = absent)
	exist bool   // whether the path existed on disk before this batch
}

// BatchWrite implements batch_write. It validates every operation first
// (a virtual-state dry-run, so chained writes to the same path in one batch
// are correctly assessed), then applies each in order. Any write failure
// triggers a best-effort rollback of every applied path to its original disk
// state, so the batch is all-or-nothing.
func BatchWrite(b Base, args WriteArgs) (string, error) {
	if len(args.Writes) == 0 {
		return "batch_write: no writes provided.", nil
	}

	// Pass 1: validate with a virtual file-state view so create→edit chains
	// within one batch are evaluated against their in-flight state, not just
	// the untouched disk.
	stateContent := map[string]string{} // path → current virtual content
	stateExists := map[string]bool{}    // path → current virtual existence
	origContent := map[string]string{}  // path → original on-disk content
	origExists := map[string]bool{}     // path → whether it existed on disk

	var ops []writeOp
	var invalid []string
	for i, w := range args.Writes {
		abs, err := safeResolve(b, w.Path, true)
		if err != nil {
			return "", fmt.Errorf("batch_write aborted (path traversal) op %d (%s): %v", i, w.Path, err)
		}
		// Determine the current virtual state for this path.
		curContent, curExist := stateContent[abs], stateExists[abs]
		if _, ok := stateContent[abs]; !ok {
			if data, rerr := os.ReadFile(abs); rerr == nil {
				curContent, curExist = string(data), true
			} else if os.IsNotExist(rerr) {
				curContent, curExist = "", false
			} else {
				invalid = append(invalid, w.Path+" ("+rerr.Error()+")")
				continue
			}
		}
		// Record original disk state once per path (for rollback).
		if _, done := origContent[abs]; !done {
			if data, rerr := os.ReadFile(abs); rerr == nil {
				origContent[abs], origExists[abs] = string(data), true
			} else if os.IsNotExist(rerr) {
				origContent[abs], origExists[abs] = "", false
			} else {
				origContent[abs], origExists[abs] = "", false
			}
		}

		var next string
		switch w.Mode {
		case "create":
			if curExist {
				invalid = append(invalid, fmt.Sprintf("op %d (%s): exists (use overwrite/edit)", i, w.Path))
				continue
			}
			next = w.Content
		case "overwrite":
			next = w.Content
		case "append":
			next = curContent + w.Content
		case "edit":
			if !curExist {
				invalid = append(invalid, fmt.Sprintf("op %d (%s): does not exist (use create)", i, w.Path))
				continue
			}
			if w.Old == "" {
				invalid = append(invalid, fmt.Sprintf("op %d (%s): edit requires old", i, w.Path))
				continue
			}
			if !strings.Contains(curContent, w.Old) {
				invalid = append(invalid, fmt.Sprintf("op %d (%s): does not contain the old substring (anchor mismatch)", i, w.Path))
				continue
			}
			next = strings.ReplaceAll(curContent, w.Old, w.New)
		default:
			if w.Mode == "" {
				invalid = append(invalid, fmt.Sprintf("op %d (%s): missing mode (expected create|overwrite|edit|append)", i, w.Path))
			} else {
				invalid = append(invalid, fmt.Sprintf("op %d (%s): unknown mode %s", i, w.Path, w.Mode))
			}
			continue
		}

		stateContent[abs] = next
		stateExists[abs] = true
		ops = append(ops, writeOp{w: w, abs: abs, next: next, orig: origContent[abs], exist: origExists[abs]})
	}
	if len(invalid) > 0 {
		return "", fmt.Errorf("batch_write aborted (nothing written) %d of %d ops invalid: %s", len(invalid), len(args.Writes), strings.Join(invalid, "; "))
	}

	// Pass 2: apply in order; on failure roll back every applied path.
	applied := make([]writeOp, 0, len(ops))
	for _, op := range ops {
		if err := os.MkdirAll(filepath.Dir(op.abs), 0o755); err != nil {
			rollback(applied)
			return "", fmt.Errorf("batch_write: mkdir %s: %w", op.w.Path, err)
		}
		if err := os.WriteFile(op.abs, []byte(op.next), 0o644); err != nil {
			rollback(applied)
			return "", fmt.Errorf("batch_write: write %s: %w", op.w.Path, err)
		}
		applied = append(applied, op)
	}

	return fmt.Sprintf("batch_write: applied %d write(s): %s", len(applied), strings.Join(paths(applied), ", ")), nil
}

func paths(ops []writeOp) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.w.Path)
	}
	return out
}

// rollback restores the original disk state for every applied path, in
// reverse, best-effort: a path that did not exist is removed, a path that did
// is rewritten with its prior content.
func rollback(applied []writeOp) {
	for i := len(applied) - 1; i >= 0; i-- {
		op := applied[i]
		if !op.exist {
			_ = os.Remove(op.abs)
		} else {
			_ = os.WriteFile(op.abs, []byte(op.orig), 0o644)
		}
	}
}

// --- helpers --------------------------------------------------------------

func relPath(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return r
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// MarshalArgs is a small helper used by the MCP layer to unmarshal raw JSON
// arguments into a typed args value.
func MarshalArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}
