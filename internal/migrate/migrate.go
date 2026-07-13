// Package migrate applies embedded SQL migrations to the Postgres
// database. It is the in-binary replacement for `atlas migrate apply`
// so that `orchicon dev start` does not require the Atlas CLI on the
// user's PATH (AGENTS.md §Dev Control Script).
//
// The runner is deliberately simple: it tracks applied migrations in a
// `_orchicon_migrations` table and executes each SQL file in lexical
// (timestamp-ordered) order. Migrations are forward-only
// (AGENTS.md invariant #9).
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// Run applies all pending migrations from the embedded filesystem. It is
// idempotent: already-applied migrations are skipped. The migrations
// table is created on first run.
func Run(ctx context.Context, pool *db.Pool, fsys fs.FS, dir string) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return fmt.Errorf("migrate: ensure table: %w", err)
	}

	names, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}

	// Filter and sort .sql files. Atlas migration filenames start with a
	// timestamp (e.g. 20260712192105_initial_schema.sql) so lexical sort
	// matches application order.
	var sqlFiles []string
	for _, e := range names {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			sqlFiles = append(sqlFiles, e.Name())
		}
	}
	sort.Strings(sqlFiles)

	applied, err := appliedMigrations(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate: list applied: %w", err)
	}

	for _, name := range sqlFiles {
		if applied[name] {
			continue
		}
		content, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}
		if err := recordMigration(ctx, pool, name); err != nil {
			return fmt.Errorf("migrate: record %s: %w", name, err)
		}
	}

	return nil
}

func ensureMigrationsTable(ctx context.Context, pool *db.Pool) error {
	const q = `CREATE TABLE IF NOT EXISTS _orchicon_migrations (
		name text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`
	_, err := pool.Exec(ctx, q)
	return err
}

func appliedMigrations(ctx context.Context, pool *db.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT name FROM _orchicon_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		m[name] = true
	}
	return m, rows.Err()
}

func recordMigration(ctx context.Context, pool *db.Pool, name string) error {
	_, err := pool.Exec(ctx, `INSERT INTO _orchicon_migrations (name) VALUES ($1) ON CONFLICT DO NOTHING`, name)
	return err
}
