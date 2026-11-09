// Package contextfiles is the shared implementation of Orchicon's "context paths" model.
package contextfiles

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bounds mirror the project service's input caps (internal/project/validate.go).
const (
	// MaxContextFiles is the max number of entries in a context_files list.
	MaxContextFiles = 1000
	// MaxManifestEntries caps how many files a DIRECTORY context path lists in the rendered manifest.
	MaxManifestEntries = 120
	// MaxFilePathLen is the max length of a single context path.
	MaxFilePathLen = 4096
	// MaxInlineFileBytes caps a single file's contents inlined into the worker prompt.
	MaxInlineFileBytes = 64 * 1024 // 64 KiB — a big single file stays on disk
	// MaxInlineContextBytes is the CUMULATIVE budget for ALL inlined context (file contents + directory
	// listings) rendered into one prompt section.
	MaxInlineContextBytes = 384 * 1024 // 384 KiB ≈ ~90k tokens max per context section
	// ManifestInlineMaxBytes is the per-file inline floor for the manifest renderer (RenderManifest):
	// files at or under this size are inlined so a worker can never be blind to a small, core file.
	ManifestInlineMaxBytes = 4 * 1024 // 4 KiB ≈ ~1k tokens per small file
)

// noiseDirNames are VCS/build/runtime-cache directories skipped when expanding a directory into the
// prompt.
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

// IsNoiseDir reports whether name is a VCS/build/runtime-cache directory skipped when expanding a
// directory into a listing.
func IsNoiseDir(name string) bool {
	return noiseDirNames[name]
}

// Validate checks a list of context paths.
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

// ValidateWithin runs Validate and additionally requires every path to be the project directory
// itself or a descendant of it (lexical check).
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

// ResolveWithin resolves a caller-supplied path against a project root, rejecting path traversal
// (..), absolute paths outside the root, and symlink escapes.
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

// WalkDir lists the files under root recursively, bounded to maxEntries results and skipping
// VCS/build noise directories.
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
	// SORT THE WALK.
	sort.Strings(out)
	return out, nil
}

// Render builds the prompt section for a list of context paths.
func Render(rootNote string, paths []string, projectDir string) string {
	var sb strings.Builder
	if len(paths) == 0 {
		return ""
	}
	if strings.TrimSpace(rootNote) != "" {
		fmt.Fprintf(&sb, "%s\n\n", strings.TrimSpace(rootNote))
	}
	wroteAny := false
	// Cumulative budget: track total rendered bytes so a large set of context files can't blow the
	// prompt (every inlined byte is re-sent on every tool call).
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

// ManifestPolicy selects how a manifest treats a file that is too big to inline.
type ManifestPolicy int

const (
	// ManifestPolicyContext is the CONTEXT-FILE policy: a file over ManifestInlineMaxBytes becomes a
	// path+size entry with a "read on demand" instruction.
	ManifestPolicyContext ManifestPolicy = iota
	// ManifestPolicySkill is the SKILL policy: file contents are ALWAYS inlined (bounded only by the
	// hard per-file cap), and a directory's skill files are inlined too rather than merely listed.
	ManifestPolicySkill
)

// RenderManifest is the context-by-reference renderer.
func RenderManifest(rootNote string, paths []string, projectDir string) string {
	return RenderManifestWithPolicy(rootNote, paths, projectDir, ManifestPolicyContext)
}

// RenderManifestWithPolicy is RenderManifest with the inline policy made explicit.
func RenderManifestWithPolicy(rootNote string, paths []string, projectDir string, policy ManifestPolicy) string {
	var sb strings.Builder
	if len(paths) == 0 {
		return ""
	}
	if strings.TrimSpace(rootNote) != "" {
		fmt.Fprintf(&sb, "%s\n\n", strings.TrimSpace(rootNote))
		if policy == ManifestPolicySkill {
			// A skill is INSTRUCTIONS, not context to be sampled — so the text says "read and follow", not
			// "read only what you need", which would invite the agent to skip the very rules it was given.
			sb.WriteString("The following skills are available. Their contents are included below — **read them and follow them**: they are the operator's standing instructions for how to do work in this project.\n\n")
		} else {
			sb.WriteString("The following context is available inside the project directory. Small, essential files are inlined below; everything else is listed as a manifest of path + size. **Read only the files your task needs** using your read/grep tools — do not dump large files wholesale. This keeps your context lean and fast.\n\n")
		}
	}
	wroteAny := false
	budget := &renderBudget{remaining: MaxInlineContextBytes}
	initLen := sb.Len()
	// DETERMINISTIC ORDER.
	for _, p := range sortedByResolved(paths, projectDir) {
		if budget.exhausted() {
			fmt.Fprintf(&sb, "**Note:** manifest budget reached — read `%s` from disk only if needed\n\n", p)
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
			budget.charge(&sb, initLen)
			continue
		}
		if info.IsDir() {
			wroteAny = renderDirectoryManifest(&sb, resolved, budget, policy) || wroteAny
			continue
		}
		if info.Mode().IsRegular() {
			// SKILL FILES ARE ALWAYS INLINED.
			if policy == ManifestPolicySkill || info.Size() <= int64(ManifestInlineMaxBytes) {
				wroteAny = renderFile(&sb, resolved, budget) || wroteAny
			} else {
				before := sb.Len()
				fmt.Fprintf(&sb, "- `%s` (%d bytes) — **read this file on demand** if the task needs it\n", resolved, info.Size())
				budget.charge(&sb, before)
				wroteAny = true
			}
			continue
		}
		fmt.Fprintf(&sb, "**Note:** `%s` is not a regular file or directory\n\n", resolved)
		wroteAny = true
		budget.charge(&sb, initLen)
	}
	if !wroteAny {
		return ""
	}
	return sb.String()
}

// renderDirectoryManifest lists a directory's entries (bounded) as a
// manifest with sizes, telling the worker to read only what it needs.
func renderDirectoryManifest(sb *strings.Builder, path string, budget *renderBudget, policy ManifestPolicy) bool {
	before := sb.Len()
	// BOUNDED BY THE PROMPT BUDGET, not the input cap — see MaxManifestEntries.
	entries, err := WalkDir(path, MaxManifestEntries)
	if err != nil {
		fmt.Fprintf(sb, "**Note:** could not list `%s`: %v\n\n", path, err)
		budget.charge(sb, before)
		return true
	}

	// A SKILLS DIRECTORY INLINES ITS FILES.
	if policy == ManifestPolicySkill {
		fmt.Fprintf(sb, "## %s (skills — follow these)\n\n", path)
		for _, e := range entries {
			if budget.exhausted() {
				fmt.Fprintf(sb, "**Note:** skills budget reached — read `%s` from disk\n\n", e)
				break
			}
			if st, serr := os.Stat(e); serr == nil && st.Mode().IsRegular() {
				renderFile(sb, e, budget)
			}
		}
		sb.WriteString("\n")
		budget.charge(sb, before)
		return true
	}

	fmt.Fprintf(sb, "## %s (directory — read on demand)\n\n", path)
	sb.WriteString("Directory context. Read the specific files you need for the task; do not read the whole directory. The listing below is a manifest (path + size):\n\n")
	for _, e := range entries {
		st, serr := os.Stat(e)
		size := int64(0)
		if serr == nil {
			size = st.Size()
		}
		fmt.Fprintf(sb, "- `%s` (%d bytes)\n", e, size)
	}
	if len(entries) >= MaxManifestEntries {
		fmt.Fprintf(sb, "- … and more: this listing is capped at %d entries. Use `glob` (by pattern) or "+
			"`grep` (by content) to find what you need, and `read` for the files you pick.\n", MaxManifestEntries)
	}
	sb.WriteString("\n")
	budget.charge(sb, before)
	return true
}

// renderBudget tracks the remaining cumulative-inline bytes for a context section.
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

// renderDirectory emits the "directory as context" block: an explicit instruction that the path is
// a directory (read every file below.
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
