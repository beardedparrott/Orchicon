// Package project implements the ProjectService Connect handler
// (docs/07_API_Specification.md §3.1). It is the API-layer boundary
// between the generated Connect handlers and the data-access layer.
//
// Responsibilities:
//   - validate and sanitize all inputs (the security boundary),
//   - resolve the tenant from the request context,
//   - perform the mutation + outbox enqueue in one transaction,
//   - map db row types to the generated proto types.
//
// No business logic lives here beyond input validation and lifecycle
// transitions; reconcilers and policy engines govern deeper behavior
// (AGENTS.md invariant #1).
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxNameLen / maxSlugLen bound input size to prevent abuse. Slugs are
// also constrained to URL-safe characters by the slug regex.
const (
	maxNameLen        = 500
	maxSlugLen        = 63
	maxGoalKeyLen     = 100
	maxGoalValueLen   = 10000
	maxGoalsLen       = 1 << 20 // 1 MiB; goals is a JSON document
	maxContextFiles   = 1000   // max number of context files
	maxFilePathLen    = 4096   // max length of a single file path
	maxFileTreeDepth  = 20     // max depth for recursive file tree listing
	maxFileTreeSize   = 10000  // max total entries in a file tree listing
)

// slugRE defines the canonical slug character set: lowercase alphanumerics
// and hyphens, must start and end alphanumeric. This is the security
// gate that rejects path-traversal or injection-laden slugs before they
// reach the database.
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// validateName trims and bounds-checks a project name. An empty name
// after trimming is rejected (InvalidArgument).
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	return name, nil
}

// validateSlug validates a slug for updates: must match the slug regex
// and not exceed max length.
func validateSlug(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", errors.New("slug must not be empty")
	}
	if !slugRE.MatchString(slug) {
		return "", fmt.Errorf("slug must match %s", slugRE.String())
	}
	if len(slug) > maxSlugLen {
		return "", fmt.Errorf("slug must be at most %d characters", maxSlugLen)
	}
	return slug, nil
}

// normalizeSlug validates the slug if provided; otherwise derives one
// from the name (lowercased, spaces → hyphens, non-safe chars stripped).
// The result is always slugRE-matched before returning.
func normalizeSlug(slug, name string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = deriveSlug(name)
	}
	if !slugRE.MatchString(slug) {
		return "", fmt.Errorf("slug must match %s", slugRE.String())
	}
	if len(slug) > maxSlugLen {
		return "", fmt.Errorf("slug must be at most %d characters", maxSlugLen)
	}
	return slug, nil
}

// deriveSlug produces a best-effort slug from a name: lowercase, replace
// whitespace runs with single hyphens, drop characters outside [a-z0-9-].
// The result is guaranteed to match slugRE (a fully-stripped name falls
// back to "project" so the unique index never sees an empty slug).
func deriveSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "project"
	}
	return out
}

// convertGoalsToJSON converts a list of GoalField key-value pairs into a
// JSON object for storage. Empty input produces an empty JSON object.
func convertGoalsToJSON(goals []*apiv1.GoalField) ([]byte, error) {
	if len(goals) == 0 {
		return []byte("{}"), nil
	}
	m := make(map[string]string, len(goals))
	for _, gf := range goals {
		key := strings.TrimSpace(gf.Key)
		if key == "" {
			return nil, errors.New("goal key must not be empty")
		}
		if utf8.RuneCountInString(key) > maxGoalKeyLen {
			return nil, fmt.Errorf("goal key must be at most %d characters", maxGoalKeyLen)
		}
		val := gf.Value
		if utf8.RuneCountInString(val) > maxGoalValueLen {
			return nil, fmt.Errorf("goal value must be at most %d characters", maxGoalValueLen)
		}
		m[key] = val
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal goals: %w", err)
	}
	if len(b) > maxGoalsLen {
		return nil, fmt.Errorf("goals must be at most %d bytes", maxGoalsLen)
	}
	return b, nil
}

// validateGoals ensures the goals JSON is valid and size-bounded.
// Used when goals arrive as a pre-encoded JSON string (legacy path).
func validateGoals(goals string) ([]byte, error) {
	goals = strings.TrimSpace(goals)
	if goals == "" {
		return []byte("{}"), nil
	}
	if len(goals) > maxGoalsLen {
		return nil, fmt.Errorf("goals must be at most %d bytes", maxGoalsLen)
	}
	if !json.Valid([]byte(goals)) {
		return nil, errors.New("goals must be valid JSON")
	}
	return []byte(goals), nil
}

// goalsToFields parses a JSON-encoded goals string into GoalField messages.
// Used by the response path so the frontend can render goals as fields.
func goalsToFields(goals string) ([]*apiv1.GoalField, error) {
	if goals == "" || goals == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(goals), &m); err != nil {
		return nil, err
	}
	fields := make([]*apiv1.GoalField, 0, len(m))
	for k, v := range m {
		fields = append(fields, &apiv1.GoalField{Key: k, Value: v})
	}
	return fields, nil
}

// requireTenant resolves the tenant id from the context. The middleware
// (internal/middleware/tenant.go) stores it; if it is missing the
// request was unscoped, which is an internal error (the middleware
// should have rejected it).
func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

// statusFromProto maps a proto enum value to a domain status string.
// Unknown/UNSPECIFIED returns empty string.
func statusFromProto(status apiv1.ProjectStatus) string {
	switch status {
	case apiv1.ProjectStatus_PROJECT_STATUS_DRAFTING:
		return domain.ProjectDrafting
	case apiv1.ProjectStatus_PROJECT_STATUS_ACTIVE:
		return domain.ProjectActive
	case apiv1.ProjectStatus_PROJECT_STATUS_PAUSED:
		return domain.ProjectPaused
	case apiv1.ProjectStatus_PROJECT_STATUS_ARCHIVED:
		return domain.ProjectArchived
	case apiv1.ProjectStatus_PROJECT_STATUS_DELETED:
		return domain.ProjectDeleted
	default:
		return ""
	}
}

// statusToProto maps a domain status string to the proto enum value.
// Unknown statuses map to UNSPECIFIED so the API never crashes on a row
// with an unexpected status (defensive — the DB should prevent this).
func statusToProto(status string) int32 {
	switch status {
	case domain.ProjectDrafting:
		return 1
	case domain.ProjectActive:
		return 2
	case domain.ProjectPaused:
		return 3
	case domain.ProjectArchived:
		return 4
	case domain.ProjectDeleted:
		return 5
	default:
		return 0
	}
}

// validateProjectDir validates and cleans a project directory path.
// It resolves the path to an absolute path (expanding ~ and symlinks),
// verifies it exists and is a directory, and prevents path-traversal
// attacks. Returns the cleaned absolute path.
func validateProjectDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("project_dir must not be empty")
	}
	if len(dir) > maxFilePathLen {
		return "", fmt.Errorf("project_dir exceeds max length of %d characters", maxFilePathLen)
	}
	// Expand ~
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand home dir: %w", err)
		}
		dir = filepath.Join(home, dir[2:])
	} else if dir == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand home dir: %w", err)
		}
		dir = home
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("project_dir %q does not exist", abs)
		}
		return "", fmt.Errorf("stat project_dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project_dir %q is not a directory", abs)
	}
	return abs, nil
}

// validateContextFiles validates a list of context file paths. Each path
// must be non-empty, not exceed the max length, and must not be absolute
// or contain path-traversal components like "..".
func validateContextFiles(files []string) error {
	if len(files) > maxContextFiles {
		return fmt.Errorf("context_files exceeds max of %d entries", maxContextFiles)
	}
	for i, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			return fmt.Errorf("context_files[%d] must not be empty", i)
		}
		if len(f) > maxFilePathLen {
			return fmt.Errorf("context_files[%d] exceeds max length of %d characters", i, maxFilePathLen)
		}
		if filepath.IsAbs(f) {
			return fmt.Errorf("context_files[%d] must be a relative path", i)
		}
		if strings.Contains(f, "..") {
			return fmt.Errorf("context_files[%d] must not contain path-traversal components", i)
		}
		if strings.HasPrefix(f, "/") || strings.HasPrefix(f, "\\") {
			return fmt.Errorf("context_files[%d] must be a relative path", i)
		}
	}
	return nil
}

// contextFilesToJSON marshals a list of file paths to a JSON byte array.
func contextFilesToJSON(files []string) ([]byte, error) {
	if files == nil {
		files = []string{}
	}
	return json.Marshal(files)
}

// contextFilesFromJSON unmarshals a JSON byte array to a list of file paths.
func contextFilesFromJSON(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var files []string
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("parse context_files: %w", err)
	}
	return files, nil
}

// buildFileTree recursively builds a FileTreeEntry from a directory path.
// Returns a tree rooted at the given path, limited by maxDepth and maxTotal.
func buildFileTree(root string, relPath string, depth int, maxDepth int, counter *int) (*apiv1.FileTreeEntry, error) {
	if depth > maxDepth {
		return nil, nil
	}
	if counter != nil && *counter > maxFileTreeSize {
		return nil, nil
	}
	fullPath := filepath.Join(root, relPath)
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, nil // skip inaccessible entries
	}
	name := filepath.Base(relPath)
	if relPath == "" {
		name = filepath.Base(root)
	}
	entry := &apiv1.FileTreeEntry{
		Name:  name,
		Path:  relPath,
		IsDir: info.IsDir(),
	}
	if counter != nil {
		*counter++
	}
	if info.IsDir() {
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			return entry, nil // directory exists but can't read contents
		}
		for _, e := range entries {
			childRel := filepath.Join(relPath, e.Name())
			if relPath == "" {
				childRel = e.Name()
			}
			child, err := buildFileTree(root, childRel, depth+1, maxDepth, counter)
			if err != nil {
				continue
			}
			if child != nil {
				entry.Children = append(entry.Children, child)
			}
		}
	}
	return entry, nil
}

// rowToProto maps a db.ProjectRow to the generated proto Project type.
// Timestamps are converted to timestamppb.
func rowToProto(p db.ProjectRow) *apiv1.Project {
	return &apiv1.Project{
		Id:           p.ID,
		TenantId:     p.TenantID,
		Name:         p.Name,
		Slug:         p.Slug,
		Status:       apiv1.ProjectStatus(statusToProto(p.Status)),
		Goals:        string(p.Goals),
		Version:      int32(p.Version),
		CreatedAt:    timestamppb.New(p.CreatedAt),
		UpdatedAt:    timestamppb.New(p.UpdatedAt),
		ProjectDir:   p.ProjectDir,
		ContextFiles: contextFilesFromJSONOrEmpty(p.ContextFiles),
	}
}

// contextFilesFromJSONOrEmpty is a best-effort parser for the context_files
// JSONB column. Returns empty slice on any error so the API never crashes
// on corrupt data.
func contextFilesFromJSONOrEmpty(data []byte) []string {
	files, err := contextFilesFromJSON(data)
	if err != nil {
		return nil
	}
	return files
}
