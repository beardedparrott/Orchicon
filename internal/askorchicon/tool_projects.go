package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	projectpkg "github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/slug"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func makeSlug(name string) string {
	if s := slug.Slugify(name); s != "" {
		return s
	}
	return "project"
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
	out := make([]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, compactProject(p))
	}
	return json.Marshal(newCompactList(out, "get_project"))
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
		Title               string `json:"title"`
		Name                string `json:"name"`
		Goals               string `json:"goals"`
		ProjectDir          string `json:"project_dir"`
		GitStrategy         string `json:"git_strategy"`
		InitGit             bool   `json:"init_git"`
		DefaultRuntimeImage string `json:"default_runtime_image"`
		ExecutionMode       string `json:"execution_mode"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	name := params.Title
	if name == "" {
		name = params.Name
	}
	if name == "" {
		return nil, fmt.Errorf("title is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	// goals is NOT NULL: an omitted goals arg must store an empty JSON array,
	// not NULL (a create with no goals is legal and was failing at the insert).
	goalsJSON := []byte("[]")
	if params.Goals != "" {
		goalsJSON = []byte(fmt.Sprintf(`"%s"`, params.Goals))
	}
	// Omitted mode means the default (local); a bogus value is rejected here
	// instead of being silently coerced.
	executionMode, err := db.ParseExecutionMode(params.ExecutionMode, db.DefaultExecutionMode)
	if err != nil {
		return nil, err
	}
	// A STRATEGY OUTSIDE THE CLOSED SET IS REFUSED, not silently defaulted: a project quietly rewritten to
	// "local" runs differently from the one the user chose, and that shows up later as runs that cannot branch.
	gitStrategy, err := db.ParseGitStrategy(params.GitStrategy, db.DefaultGitStrategy)
	if err != nil {
		return nil, err
	}
	project, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID:          db.NewID(),
		TenantID:    tenantID,
		Name:        name,
		Slug:        makeSlug(name),
		Status:      "active",
		Goals:       goalsJSON,
		ProjectDir:  params.ProjectDir,
		GitStrategy: gitStrategy,
		DefaultRuntimeImage: func() *string {
			if v := strings.TrimSpace(params.DefaultRuntimeImage); v != "" {
				return &v
			}
			return nil
		}(),
		ExecutionMode: executionMode,
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	// THE REPOSITORY IS SET UP AFTER THE ROW IS COMMITTED, deliberately: the platform's git setup touches the
	// FILESYSTEM, and a git failure must not roll back a project the user asked for. It is REPORTED instead, so
	// the agent can say what happened and what it did about it.
	return marshalProjectWithGitSetup(ctx, project, params.InitGit)
}

// marshalProjectWithGitSetup returns the project plus the outcome of the platform's git setup when it was asked
// for. The report rides the RESULT rather than a separate call so a project cannot be created as local/pr and
// left without a repository — which is exactly how an Ask-created project came to behave differently from one
// created in the console.
func marshalProjectWithGitSetup(ctx context.Context, project db.ProjectRow, initGit bool) (json.RawMessage, error) {
	// THE PROJECT'S OWN FIELDS STAY AT THE TOP LEVEL, with git_setup added beside them. The response is the
	// project the caller asked for; wrapping it under a "project" key would break every existing consumer that
	// unmarshals the result straight into a project row — including this package's own tests — for the sake of a
	// report that is additive.
	base, berr := json.Marshal(project)
	if berr != nil {
		return nil, berr
	}
	out := map[string]any{}
	if uerr := json.Unmarshal(base, &out); uerr != nil {
		return nil, uerr
	}
	if !initGit {
		// Say what was NOT done, so the agent acts on it instead of assuming it happened.
		out["git_setup"] = map[string]any{
			"ran": false,
			"reason": "init_git was not requested — a local/pr project needs a repository before its runs can " +
				"branch",
		}
		return json.Marshal(out)
	}
	rep := projectpkg.EnsureGitState(ctx, project.ProjectDir, project.GitStrategy)
	out["git_setup"] = gitSetupReportJSON(rep)
	return json.Marshal(out)
}

// gitSetupReportJSON renders EnsureGitState's report as a small, stable shape the agent can relay verbatim.
func gitSetupReportJSON(rep projectpkg.GitSetupReport) map[string]any {
	out := map[string]any{
		"ran":     true,
		"done":    rep.Done,
		"needed":  rep.Needed,
		"summary": rep.Summary(),
	}
	if rep.GitMissing {
		out["git_missing"] = true
	}
	if rep.Err != nil {
		out["error"] = rep.Err.Error()
	}
	if rep.Skipped != "" {
		out["skipped"] = rep.Skipped
	}
	if rep.Warning != "" {
		out["warning"] = rep.Warning
	}
	return out
}

func toolUpdateProject(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID                  string  `json:"id"`
		Title               string  `json:"title"`
		Name                string  `json:"name"`
		Goals               string  `json:"goals"`
		ProjectDir          string  `json:"project_dir"`
		GitStrategy         *string `json:"git_strategy"`
		InitGit             bool    `json:"init_git"`
		DefaultRuntimeImage *string `json:"default_runtime_image"`
		ExecutionMode       *string `json:"execution_mode"`
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
	title := params.Title
	if title == "" {
		title = params.Name
	}
	if title != "" {
		update.Name = &title
	}
	if params.Goals != "" {
		g := []byte(fmt.Sprintf(`"%s"`, params.Goals))
		update.Goals = &g
	}
	if params.ProjectDir != "" {
		update.ProjectDir = &params.ProjectDir
	}
	if params.DefaultRuntimeImage != nil {
		v := strings.TrimSpace(*params.DefaultRuntimeImage)
		update.DefaultRuntimeImage = &v
	}
	if params.ExecutionMode != nil {
		// Route through the SAME parser as create: a raw switch here rejected
		// spellings create accepts (" Runtime "), so the two paths disagreed
		// about what a valid mode is. An absent/blank value is not a mode —
		// reject it rather than handing "" to the column's CHECK constraint.
		m, err := db.ParseExecutionMode(*params.ExecutionMode, "")
		if err != nil {
			return nil, err
		}
		if m == "" {
			return nil, errors.New(db.ExecutionModeInvalidMsg)
		}
		update.ExecutionMode = &m
	}
	if params.GitStrategy != nil {
		// A CHANGED STRATEGY IS VALIDATED, and it is a real change: switching to local/pr is exactly the moment a
		// project starts needing a repository it may not have.
		gs, gerr := db.ParseGitStrategy(*params.GitStrategy, "")
		if gerr != nil {
			return nil, gerr
		}
		update.GitStrategy = &gs
	}
	project, err := db.UpdateProject(ctx, ttx.Tx, tenantID, params.ID, current.Version, update)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return marshalProjectWithGitSetup(ctx, project, params.InitGit)
}

func toolArchiveProject(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
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
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	project, err := db.ArchiveProject(ctx, ttx.Tx, tenantID, params.ID, current.Version)
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
		InitGit   bool     `json:"init_git"`
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
	out := map[string]any{
		"project_dir": absPath,
		"scaffolded":  params.Scaffold,
	}
	// THE NATURAL HOME FOR git init: this tool is what CREATES the directory, so it is also the moment a
	// local/pr project stops being able to branch unless something initialises a repository. Post-commit for the
	// same reason as create_project — a git failure must not undo the directory the user asked for.
	if params.InitGit {
		out["git_setup"] = gitSetupReportJSON(projectpkg.EnsureGitState(ctx, absPath, current.GitStrategy))
	} else if db.RequiresRepository(current.GitStrategy) {
		out["git_setup"] = map[string]any{
			"ran": false,
			"reason": "init_git was not requested — this project is " + current.GitStrategy + ", which needs a " +
				"repository before its runs can branch",
		}
	}
	return json.Marshal(out)
}
