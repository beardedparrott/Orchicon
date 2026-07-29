package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func makeSlug(name string) string {
	s := strings.ToLower(name)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "project"
	}
	return s
}

func toolListProjects(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Search string `json:"search"`
		Status string `json:"status"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	projects, err := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{
		TenantID: tenantID,
		Search:   params.Search,
		Status:   params.Status,
	})
	if err != nil {
		return nil, err
	}
	// json.Marshal on a nil slice produces "null". The model reacts poorly
	// to that — "The tool returned null" — and assumes a failure. Ensure
	// an empty JSON array is returned instead.
	if projects == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(projects)
}

func toolGetProject(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	project, err := db.GetProject(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(project)
}

func toolCreateProject(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name  string `json:"name"`
		Goals string `json:"goals"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	var goalsJSON []byte
	if params.Goals != "" {
		goalsJSON = []byte(fmt.Sprintf(`"%s"`, params.Goals))
	}
	project, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID:       db.NewID(),
		TenantID: tenantID,
		Name:     params.Name,
		Slug:     makeSlug(params.Name),
		Status:   "active",
		Goals:    goalsJSON,
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(project)
}

func toolUpdateProject(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Goals      string `json:"goals"`
		ProjectDir string `json:"project_dir"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	update := db.UpdateProjectFields{}
	if params.Name != "" {
		update.Name = &params.Name
	}
	if params.Goals != "" {
		g := []byte(fmt.Sprintf(`"%s"`, params.Goals))
		update.Goals = &g
	}
	if params.ProjectDir != "" {
		update.ProjectDir = &params.ProjectDir
	}
	project, err := db.UpdateProject(ctx, ttx.Tx, tenantID, params.ID, current.Version, update)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(project)
}

func toolDeleteProject(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	if err := db.DeleteProject(ctx, ttx.Tx, tenantID, params.ID); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"status": "deleted"})
}

func toolCreateProjectDir(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string   `json:"project_id"`
		DirPath   string   `json:"dir_path"`
		Scaffold  bool     `json:"scaffold"`
		Subdirs   []string `json:"subdirs"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if params.DirPath == "" {
		return nil, fmt.Errorf("dir_path is required")
	}
	tenantID := tenant.FromContext(ctx)

	if len(params.DirPath) > 0 && params.DirPath[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expand home dir: %w", err)
		}
		params.DirPath = filepath.Join(home, params.DirPath[1:])
	}
	absPath, err := filepath.Abs(params.DirPath)
	if err != nil {
		return nil, fmt.Errorf("absolute path: %w", err)
	}
	if err := os.MkdirAll(absPath, 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}
	if params.Scaffold {
		subdirs := params.Subdirs
		if len(subdirs) == 0 {
			subdirs = []string{"src", "docs", "tests"}
		}
		for _, sub := range subdirs {
			if err := os.MkdirAll(filepath.Join(absPath, sub), 0755); err != nil {
				return nil, fmt.Errorf("create subdir %s: %w", sub, err)
			}
		}
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, params.ProjectID)
	if err != nil {
		return nil, err
	}
	if _, err := db.UpdateProject(ctx, ttx.Tx, tenantID, params.ProjectID, current.Version, db.UpdateProjectFields{
		ProjectDir: &absPath,
	}); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"project_dir": absPath,
		"scaffolded":  params.Scaffold,
	})
}
