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
package worktree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Default caps keep a single tool call well-bounded so it cannot blow up the
// context. They are high enough to capture a real need in one pass, low
// enough to never feed a document back wholesale.
const (
	defaultMaxBytes   = 128_000 // total batch_read output cap
	defaultPerFileMax = 32_000  // per-file read cap
	defaultMaxFiles   = 64      // directory expansion file cap
	defaultMaxMatches = 250     // batch_grep match cap
	defaultMaxDirs    = 64      // directory expansion entry cap
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

// safeResolve resolves a worker-supplied path against the worktree base and
// rejects anything that escapes it. It is the single enforcement point for
// every composite tool.
func safeResolve(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed (path traversal)")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the worktree (..)")
	}
	p := filepath.Join(base, clean)
	bp := filepath.Clean(base)
	if p != bp && !strings.HasPrefix(p, bp+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the worktree")
	}
	return p, nil
}

// expandPaths resolves each entry; a directory is expanded to its immediate
// files (non-recursive, bounded) so `batch_read "docs"` grabs the docs in one
// call without a full-tree walk. An unsafe path (absolute or traversal) is
// FATAL — the whole call is rejected so a crafted path can never touch a file
// outside the worktree. Missing paths are skipped with a note.
func expandPaths(base string, entries []string, maxFiles, maxDirs int) (files []string, skipped []string, err error) {
	seen := map[string]bool{}
	dirs := 0
	for _, e := range entries {
		p, err := safeResolve(base, e)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(p)
		if err != nil {
			skipped = append(skipped, e+" (not found)")
			continue
		}
		if info.IsDir() {
			dirs++
			if dirs > maxDirs {
				skipped = append(skipped, e+" (dir cap reached)")
				continue
			}
			des, err := os.ReadDir(p)
			if err != nil {
				skipped = append(skipped, e+" ("+err.Error()+")")
				continue
			}
			for _, de := range des {
				if de.IsDir() {
					continue
				}
				fp := filepath.Join(p, de.Name())
				if seen[fp] {
					continue
				}
				seen[fp] = true
				files = append(files, fp)
			}
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
		if len(files) >= maxFiles {
			break
		}
	}
	sort.Strings(files)
	return files, skipped, nil
}

// --- batch_read -----------------------------------------------------------

// BatchRead implements batch_read. It returns a bounded text block with a
// per-file header, explicit truncation markers, and a dedupe/skip summary.
func BatchRead(base string, args ReadArgs) (string, error) {
	maxBytes := args.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	perFile := args.PerFile
	if perFile <= 0 {
		perFile = defaultPerFileMax
	}
	files, skipped, err := expandPaths(base, args.Paths, defaultMaxFiles, defaultMaxDirs)
	if err != nil {
		return "", err
	}
	if len(args.Paths) > 0 && len(files) == 0 && len(skipped) == 0 {
		return "batch_read: no matching paths were readable.", nil
	}

	var b bytes.Buffer
	total := 0
	truncated := 0
	for _, fp := range files {
		data, err := os.ReadFile(fp)
		if err != nil {
			skipped = append(skipped, fp+" ("+err.Error()+")")
			continue
		}
		rel := relPath(base, fp)
		line := "==> " + rel + " (" + humanBytes(len(data)) + ") <==\n"
		// Reserve space for the header + a trailing marker even when hitting
		// the total cap, so the model sees which file was cut off.
		if total+len(line) > maxBytes && total > 0 {
			truncated++
			continue
		}
		b.WriteString(line)
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
				b.WriteString(fmt.Sprintf("%6d: %s\n", i+1, l))
			}
		} else {
			if len(shown) > 0 {
				b.Write(shown)
				if shown[len(shown)-1] != '\n' {
					b.WriteByte('\n')
				}
			}
		}
		total += len(shown)
		if len(content) > perFile {
			b.WriteString(fmt.Sprintf("... [truncated %s in %s]\n", humanBytes(len(content)-perFile), rel))
			total += 40
		}
		if total >= maxBytes {
			truncated++
			break
		}
	}

	summary := fmt.Sprintf("batch_read: %d file(s)", len(files))
	if truncated > 0 {
		summary += fmt.Sprintf(", %d truncated", truncated)
	}
	if len(skipped) > 0 {
		summary += fmt.Sprintf(", %d skipped", len(skipped))
	}
	summary += "\n"
	return summary + b.String(), nil
}

// --- batch_grep -----------------------------------------------------------

// BatchGrep implements batch_grep. It greps each pattern across the selected
// subtree (default ".") and returns only the matches, capped.
func BatchGrep(base string, args GrepArgs) (string, error) {
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

	files, skipped, err := expandPaths(base, paths, defaultMaxFiles, defaultMaxDirs)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	matched := 0
	bytesOut := 0
	for _, fp := range files {
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		rel := relPath(base, fp)
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
				b.WriteString(lineOut)
				bytesOut += len(lineOut)
			}
			b.WriteString("\n")
			bytesOut++
		}
		if b.Len() > maxBytesOut() {
			break
		}
	}
	out := b.String()
	summary := fmt.Sprintf("batch_grep: %d match line(s) across %d file(s)", matched, len(files))
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
func BatchWrite(base string, args WriteArgs) (string, error) {
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
	for _, w := range args.Writes {
		abs, err := safeResolve(base, w.Path)
		if err != nil {
			return "", fmt.Errorf("batch_write aborted (path traversal): %s", w.Path)
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
				invalid = append(invalid, w.Path+" exists (use overwrite/edit)")
				continue
			}
			next = w.Content
		case "overwrite":
			next = w.Content
		case "append":
			next = curContent + w.Content
		case "edit":
			if !curExist {
				invalid = append(invalid, w.Path+" does not exist (use create)")
				continue
			}
			if w.Old == "" {
				invalid = append(invalid, w.Path+" edit requires old")
				continue
			}
			if !strings.Contains(curContent, w.Old) {
				invalid = append(invalid, w.Path+" does not contain the old substring")
				continue
			}
			next = strings.ReplaceAll(curContent, w.Old, w.New)
		default:
			invalid = append(invalid, w.Path+" unknown mode "+w.Mode)
			continue
		}

		stateContent[abs] = next
		stateExists[abs] = true
		ops = append(ops, writeOp{w: w, abs: abs, next: next, orig: origContent[abs], exist: origExists[abs]})
	}
	if len(invalid) > 0 {
		return "", fmt.Errorf("batch_write aborted (nothing written): %s", strings.Join(invalid, "; "))
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
