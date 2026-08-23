// Package contextfiles is the shared implementation of Orchicon's
// "context paths" model. Both projects.context_files and
// work_items.context_files hold a JSON array of absolute paths that may
// be files OR directories; this package provides the one validator, the
// one path resolver, and the one prompt-section renderer so projects and
// work items behave identically (AGENTS.md: fix the whole class, don't
// copy-paste).
//
// The package is a leaf: it imports only the standard library so both
// the project service and the work-item service can call it without an
// import cycle.
package contextfiles

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Bounds mirror the project service's input caps (internal/project/validate.go).
const (
	// MaxContextFiles is the max number of entries in a context_files list.
	MaxContextFiles = 1000
	// MaxFilePathLen is the max length of a single context path.
	MaxFilePathLen = 4096
	// MaxInlineFileBytes caps a single file's contents inlined into the
	// worker prompt. Directory expansion multiplies content, so a cap is
	// mandatory to keep prompts bounded (the old unbounded os.ReadFile of
	// a single huge file could already blow a prompt — this is a strict
	// improvement). Beyond the cap the worker is told to read the file
	// from disk. The project-directory read tool uses the same cap as its
	// default read bound.
	MaxInlineFileBytes = 64 * 1024 // 64 KiB — a big single file stays on disk
	// MaxInlineContextBytes is the CUMULATIVE budget for ALL inlined
	// context (file contents + directory listings) rendered into one prompt
	// section. Each inlined byte is re-sent to the model on EVERY tool call
	// of the run, so the sum — not any single file — is what inflates
	// context. Beyond this budget later paths degrade to a "read from disk"
	// note instead of inlining, keeping the section bounded.
	MaxInlineContextBytes = 384 * 1024 // 384 KiB ≈ ~90k tokens max per context section
)

// noiseDirNames are VCS/build/runtime-cache directories skipped when
// expanding a directory into the prompt. Listing them adds nothing for
// the model and would otherwise balloon the prompt.
var noiseDirNames = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	"__pycache__":  true,
	".orchicon":    true,
	".cache":       true,
}

// IsNoiseDir reports whether name is a VCS/build/runtime-cache directory
// skipped when expanding a directory into a listing. This is the same set
// WalkDir skips, exposed for the project-directory list tool.
func IsNoiseDir(name string) bool {
	return noiseDirNames[name]
}

// Validate checks a list of context paths. Each path must be non-empty,
// not exceed the max length, must be absolute, and must not contain
// path-traversal components like "..". These are exactly the rules the
// project service used to enforce in validateContextFiles.
func Validate(paths []string) error {
	if len(paths) > MaxContextFiles {
		return fmt.Errorf("context_files exceeds max of %d entries", MaxContextFiles)
	}
	for i, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			return fmt.Errorf("context_files[%d] must not be empty", i)
		}
		if len(p) > MaxFilePathLen {
			return fmt.Errorf("context_files[%d] exceeds max length of %d characters", i, MaxFilePathLen)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("context_files[%d] must be an absolute path", i)
		}
		if strings.Contains(p, "..") {
			return fmt.Errorf("context_files[%d] must not contain path-traversal components", i)
		}
	}
	return nil
}

// ValidateWithin runs Validate and additionally requires every path to be
// the project directory itself or a descendant of it (lexical check). The
// project directory is the only directory guaranteed to be mounted into the
// container where workers run, so a context path outside it would be
// invisible to the worker — selecting it is pointless. projectDir == ""
// (no directory configured yet) skips the within-check; Validate still
// applies.
func ValidateWithin(paths []string, projectDir string) error {
	if err := Validate(paths); err != nil {
		return err
	}
	root := strings.TrimSpace(projectDir)
	if root == "" {
		return nil
	}
	root = filepath.Clean(root)
	for i, p := range paths {
		rel, err := filepath.Rel(root, filepath.Clean(strings.TrimSpace(p)))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("context_files[%d] must be inside the project directory (%s)", i, root)
		}
	}
	return nil
}

// ToJSON marshals a list of paths to the JSONB column value. A nil list
// becomes an empty JSON array (never null).
func ToJSON(paths []string) ([]byte, error) {
	if paths == nil {
		paths = []string{}
	}
	return json.Marshal(paths)
}

// FromJSON unmarshals the JSONB column value back to a path list.
// Returns nil for empty input. A malformed payload is returned as an
// error so callers can decide how to degrade (the project service treats
// it as an empty list).
func FromJSON(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("parse context_files: %w", err)
	}
	return paths, nil
}

// Resolve normalizes a context path to an absolute, cleaned path.
// Absolute paths pass through; relative paths are joined to projectDir
// (backward compatibility — the project service historically allowed
// relative entries). Returns "" when the path still isn't absolute after
// resolution so callers can skip it.
func Resolve(p, projectDir string) string {
	resolved := strings.TrimSpace(p)
	if !filepath.IsAbs(resolved) && projectDir != "" {
		resolved = filepath.Join(projectDir, resolved)
	}
	if !filepath.IsAbs(resolved) {
		return ""
	}
	return filepath.Clean(resolved)
}

// ResolveWithin resolves a caller-supplied path against a project root,
// rejecting path traversal (..), absolute paths outside the root, and
// symlink escapes. It returns the cleaned absolute path for OS calls.
//
// Defense in depth, in order:
//  1. The caller's path is trimmed, must be non-empty, and is bounded to
//     MaxFilePathLen.
//  2. The root itself is normalized through filepath.Abs + Clean + a full
//     EvalSymlinks pass (the project_dir may live under a symlinked home),
//     yielding the evaluated root used for the symlink re-check.
//  3. The target is built lexically: absolute paths are cleaned as-is,
//     relative paths are joined to the root. Both are then containment-
//     checked against the root — this rejects `..`, `..\`-style escapes,
//     and absolute paths that leave the root.
//  4. The target is fully evaluated (EvalSymlinks) and re-checked against
//     the evaluated root: a symlink inside the root that resolves outside
//     is rejected; one that stays inside is allowed (legit generated
//     links). A target that does not exist — or is a broken symlink —
//     keeps its clean lexical form so the downstream operation reports
//     the not-found itself.
//
// The returned path is the original cleaned target (not the evaluated
// one), so error messages and tool responses stay in operator-facing
// terms. Callers must still use Lstat for listing (never descend into
// symlinked directories) — see the list tool.
func ResolveWithin(root, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if len(p) > MaxFilePathLen {
		return "", fmt.Errorf("path exceeds max length of %d characters", MaxFilePathLen)
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("project directory must not be empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve project directory %q: %w", root, err)
	}
	absRoot = filepath.Clean(absRoot)
	evalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project directory %q: %w", absRoot, err)
	}
	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else {
		target = filepath.Clean(filepath.Join(absRoot, p))
	}
	if err := within(absRoot, target); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		if err := within(evalRoot, resolved); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve %q: %w", target, err)
	}
	return target, nil
}

// within reports whether target is root itself or a lexical descendant.
func within(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("path %q is not inside the project directory %q", target, root)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes the project directory %q", target, root)
	}
	return nil
}

// WalkDir lists the files under root recursively, bounded to maxEntries
// results and skipping VCS/build noise directories. Symlinked
// directories are not followed (fs.WalkDir semantics). The returned
// paths are absolute.
func WalkDir(root string, maxEntries int) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // race with deletion — skip
			}
			return err
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			if noiseDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= maxEntries {
			return filepath.SkipAll
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %q: %w", root, err)
	}
	return out, nil
}

// Render builds the prompt section for a list of context paths. rootNote
// is the section heading (e.g. "# Project context" or "# Work item
// context"); paths may contain files AND directories.
//
// Per path:
//   - Regular file: the contents are inlined (capped at maxInlineFileBytes,
//     with a truncation note beyond it).
//   - Directory: a "directory context" block lists every file inside
//     (WalkDir, capped at MaxContextFiles) and explicitly instructs the
//     worker to read them and NOT to open the directory as a file
//     ("not a file").
//   - Missing / unreadable / neither: a non-fatal "could not read" note.
//
// Returns the rendered section (which may be empty when every path was
// dropped). Rendering never hard-fails the prompt build — errors in a
// single path degrade to a note.
func Render(rootNote string, paths []string, projectDir string) string {
	var sb strings.Builder
	if len(paths) == 0 {
		return ""
	}
	if strings.TrimSpace(rootNote) != "" {
		fmt.Fprintf(&sb, "%s\n\n", strings.TrimSpace(rootNote))
	}
	wroteAny := false
	// Cumulative budget: track total rendered bytes so a large set of
	// context files can't blow the prompt (every inlined byte is re-sent on
	// every tool call). Beyond the budget, later paths degrade to a note.
	budget := &renderBudget{remaining: MaxInlineContextBytes}
	for _, p := range paths {
		if budget.exhausted() {
			fmt.Fprintf(&sb, "**Note:** context budget reached — stop inlisting further files; read `%s` from disk when needed\n\n", p)
			wroteAny = true
			continue
		}
		resolved := Resolve(p, projectDir)
		if resolved == "" {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil {
			fmt.Fprintf(&sb, "**Note:** could not read `%s`: %v\n\n", resolved, err)
			wroteAny = true
			continue
		}
		if info.IsDir() {
			wroteAny = renderDirectory(&sb, resolved, budget) || wroteAny
			continue
		}
		if info.Mode().IsRegular() {
			wroteAny = renderFile(&sb, resolved, budget) || wroteAny
			continue
		}
		fmt.Fprintf(&sb, "**Note:** `%s` is not a regular file or directory\n\n", resolved)
		wroteAny = true
	}
	if !wroteAny {
		return ""
	}
	return sb.String()
}

// renderBudget tracks the remaining cumulative-inline bytes for a context
// section. Costs are charged against the FULL rendered length (directive +
// file body + headers), because that is what actually re-enters the model on
// every turn.
type renderBudget struct {
	remaining int
}

func (b *renderBudget) charge(sb *strings.Builder, before int) {
	b.remaining -= (sb.Len() - before)
	if b.remaining < 0 {
		b.remaining = 0
	}
}

func (b *renderBudget) exhausted() bool { return b.remaining <= 0 }

// renderFile inlines a single context file's contents, capped per-file and
// against the section budget. Returns true when a section was written.
func renderFile(sb *strings.Builder, path string, budget *renderBudget) bool {
	before := sb.Len()
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(sb, "**Note:** could not read `%s`: %v\n\n", path, err)
		budget.charge(sb, before)
		return true
	}
	fmt.Fprintf(sb, "## %s\n\n```\n", path)
	if len(data) > MaxInlineFileBytes {
		sb.Write(data[:MaxInlineFileBytes])
		sb.WriteString("\n…[truncated — read the full file from disk]\n")
	} else {
		sb.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("```\n\n")
	budget.charge(sb, before)
	return true
}

// renderDirectory emits the "directory as context" block: an explicit
// instruction that the path is a directory (read every file below, do
// NOT open the directory path itself) followed by the bounded file listing.
// Returns true when a section was written.
func renderDirectory(sb *strings.Builder, path string, budget *renderBudget) bool {
	before := sb.Len()
	entries, err := WalkDir(path, MaxContextFiles)
	if err != nil {
		fmt.Fprintf(sb, "**Note:** could not list `%s`: %v\n\n", path, err)
		budget.charge(sb, before)
		return true
	}
	fmt.Fprintf(sb, "## %s (directory)\n\n", path)
	sb.WriteString("This is a directory provided as context. Read EVERY file listed below, in full, before starting your work. Use your list/glob/read tools to explore it. Do NOT attempt to open the directory path itself as a file — that errors with \"not a file\".\n\n")
	for _, e := range entries {
		rel, rerr := filepath.Rel(path, e)
		if rerr != nil {
			rel = e
		}
		fmt.Fprintf(sb, "- `%s` (%s)\n", e, rel)
	}
	if len(entries) >= MaxContextFiles {
		sb.WriteString("- … and more files (use your tools to browse the directory)\n")
	}
	sb.WriteString("\n")
	return true
}
