package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// runProjectMountsWriter keeps the project-mounts manifest in sync with the
// projects table. Docker bind mounts are fixed at container-create time, so
// scripts/container.sh reads this manifest (from the data volume) to mount
// exactly the host dirs/files the projects reference — project_dir plus any
// context_files. "Save a project dir, then scripts/container.sh sync-mounts"
// applies the change. Only runs in container mode (ORCHICON_DATA_DIR set).
func runProjectMountsWriter(ctx context.Context, pool *db.Pool, log *slog.Logger, dataDir string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeProjectMountsManifest(ctx, pool, dataDir); err != nil {
				log.Warn("project mounts manifest update failed", "error", err)
			}
		}
	}
}

// writeProjectMountsManifest queries all project_dir + context_files values
// and rewrites the manifest file only when the set changed.
func writeProjectMountsManifest(ctx context.Context, pool *db.Pool, dataDir string) error {
	paths, err := collectProjectMountPaths(ctx, pool)
	if err != nil {
		return err
	}
	content := strings.Join(paths, "\n")
	if content != "" {
		content += "\n"
	}
	path := filepath.Join(dataDir, "project-mounts")
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return nil // unchanged
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("project mounts: mkdir: %w", err)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// collectProjectMountPaths returns the sorted unique set of host paths
// (project dirs + context files) across all projects.
func collectProjectMountPaths(ctx context.Context, pool *db.Pool) ([]string, error) {
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return nil, fmt.Errorf("project mounts: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	rows, err := ttx.Tx.Query(ctx,
		`SELECT COALESCE(project_dir, ''), COALESCE(context_files, '[]') FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("project mounts: query: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	for rows.Next() {
		var dir, ctxJSON string
		if err := rows.Scan(&dir, &ctxJSON); err != nil {
			return nil, fmt.Errorf("project mounts: scan: %w", err)
		}
		add(dir)
		var files []string
		if json.Unmarshal([]byte(ctxJSON), &files) == nil {
			for _, f := range files {
				add(f)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project mounts: rows: %w", err)
	}
	sort.Strings(paths)
	return paths, nil
}
