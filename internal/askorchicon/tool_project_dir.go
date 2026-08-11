package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// These tools give Ask Orchicon (both conversation modes) filesystem
// context for a project's project_dir — the one directory a worker is
// guaranteed to be able to operate in. Ask Orchicon runs on a
// directory-less session, so this tool surface is the ONLY route to a
// project's files (the no-project execution guard blocks general-purpose
// file tools). Both tools are read-only (Mutating: false) and resolve
// every path through contextfiles.ResolveWithin, which rejects `..`
// escapes, absolute out-of-root paths, and symlink escapes — see the
// always-run traversal tests in internal/contextfiles.

// listProjectDirEntry is one entry in a list_project_dir response. Types
// are derived from Lstat (never following symlinks), so a symlinked entry
// is reported as "symlink" and its target is NOT listed or descended into.
type listProjectDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // dir | file | symlink | other
	Size int64  `json:"size"` // bytes for regular files, 0 otherwise
}

type listProjectDirResult struct {
	ProjectID string                `json:"project_id"`
	Path      string                `json:"path"`
	Entries   []listProjectDirEntry `json:"entries"`
	Truncated bool                  `json:"truncated"`
}

// toolListProjectDir lists the top-level entries of a project's
// project_dir (or a subdirectory of it). The listing is shallow — the
// model drills down by re-invoking with a subpath — and is capped at
// contextfiles.MaxContextFiles entries (truncated=true beyond it), with
// VCS/build noise directories skipped.
func toolListProjectDir(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	root, err := projectDirForTool(ctx, pool, params.ProjectID)
	if err != nil {
		return nil, err
	}
	dir := root
	if strings.TrimSpace(params.Path) != "" {
		dir, err = contextfiles.ResolveWithin(root, params.Path)
		if err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is a file, not a directory — use read_project_file to read it", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read directory %q: %w", dir, err)
	}
	result := listProjectDirResult{
		ProjectID: params.ProjectID,
		Path:      dir,
		Entries:   make([]listProjectDirEntry, 0, len(entries)),
	}
	for _, e := range entries {
		// Skip VCS/build/runtime-cache directories (os.ReadDir does not
		// follow symlinks, so a symlink named ".git" is NOT skipped here —
		// it is reported as a symlink).
		if e.IsDir() && contextfiles.IsNoiseDir(e.Name()) {
			continue
		}
		if len(result.Entries) >= contextfiles.MaxContextFiles {
			result.Truncated = true
			break
		}
		full := filepath.Join(dir, e.Name())
		entry := listProjectDirEntry{Name: e.Name(), Path: full}
		// Lstat, never Stat: report symlinks as-is and never descend.
		fi, err := os.Lstat(full)
		if err != nil {
			continue // raced with deletion; skip
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			entry.Type = "symlink"
		case fi.IsDir():
			entry.Type = "dir"
		case fi.Mode().IsRegular():
			entry.Type = "file"
			entry.Size = fi.Size()
		default:
			entry.Type = "other"
		}
		result.Entries = append(result.Entries, entry)
	}
	return json.Marshal(result)
}

type readProjectFileResult struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
}

// toolReadProjectFile reads a single file inside a project's project_dir,
// bounded to maxBytes (default contextfiles.MaxInlineFileBytes, clamped to
// [1, MaxInlineFileBytes]). The JSON envelope (not raw text) lets the model
// distinguish real content from a truncation marker.
func toolReadProjectFile(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Path      string `json:"path"`
		MaxBytes  int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if strings.TrimSpace(params.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	maxBytes := params.MaxBytes
	if maxBytes == 0 {
		maxBytes = contextfiles.MaxInlineFileBytes
	}
	if maxBytes < 1 {
		maxBytes = 1
	}
	if maxBytes > contextfiles.MaxInlineFileBytes {
		maxBytes = contextfiles.MaxInlineFileBytes
	}
	root, err := projectDirForTool(ctx, pool, params.ProjectID)
	if err != nil {
		return nil, err
	}
	file, err := contextfiles.ResolveWithin(root, params.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(file)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", file, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%q is a directory — use list_project_dir to explore it", file)
	}
	// Note: a symlink target is intentionally not rejected here —
	// ResolveWithin already proved any followable symlink stays inside the
	// root, and os.ReadFile follows it (or errors for a broken link).
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", file, err)
	}
	result := readProjectFileResult{
		ProjectID: params.ProjectID,
		Path:      file,
		Bytes:     len(data),
	}
	if len(data) > maxBytes {
		result.Content = string(data[:maxBytes])
		result.Truncated = true
	} else {
		result.Content = string(data)
	}
	return json.Marshal(result)
}

// projectDirForTool fetches the project within the caller's tenant and
// returns its project_dir, erroring when the project is unknown or has no
// directory configured. The transaction is read-only and rolled back.
func projectDirForTool(ctx context.Context, pool *db.Pool, projectID string) (string, error) {
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer ttx.Rollback(ctx)
	project, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(project.ProjectDir)
	if root == "" {
		return "", fmt.Errorf("project %q has no project_dir configured — set one with create_project_directory or update_project", projectID)
	}
	return root, nil
}
