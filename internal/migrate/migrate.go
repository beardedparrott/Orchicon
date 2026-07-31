// Package migrate applies embedded SQL migrations to the Postgres
// database. It is the in-binary replacement for `atlas migrate apply`
// so that `orchicon dev start` does not require the Atlas CLI on the
// user's PATH (AGENTS.md §Dev Control Script).
//
// Migrations follow the hybrid additive-only + paired down convention
// (Option C). Up migrations are additive-only (no destructive DDL).
// Each migration may have a paired _down.sql file that reverses it,
// enabling safe rollback between binary versions.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// Run applies all pending up migrations from the embedded filesystem.
// Already-applied migrations are skipped. The migrations table is
// created on first run.
func Run(ctx context.Context, pool *db.Pool, fsys fs.FS, dir string) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return fmt.Errorf("migrate: ensure table: %w", err)
	}

	applied, err := appliedMigrations(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate: list applied: %w", err)
	}

	for _, name := range upMigrationFiles(fsys, dir) {
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

// RunDown reverts migrations down to (but not including) the target
// version. The target is the migration name (timestamp prefix) to stop
// at. For example, RunDown(ctx, pool, fsys, dir,
// "20260731000000") reverts everything after that migration.
// An empty target reverts all migrations. Migrations are reverted in
// reverse order of application, using their paired _down.sql files.
func RunDown(ctx context.Context, pool *db.Pool, fsys fs.FS, dir, target string) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return fmt.Errorf("migrate: ensure table: %w", err)
	}

	appliedOrdered, err := appliedMigrationsOrdered(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate: list applied ordered: %w", err)
	}

	// Walk applied migrations in reverse order. Stop when we hit the
	// target timestamp or reach the beginning.
	for i := len(appliedOrdered) - 1; i >= 0; i-- {
		name := appliedOrdered[i]
		prefix := timestampPrefix(name)
		if target != "" && prefix <= target {
			break
		}

		downName := downName(name)
		if _, err := fs.Stat(fsys, dir+"/"+downName); err != nil {
			return fmt.Errorf("migrate: down file missing for %s: %w", name, err)
		}
		content, err := fs.ReadFile(fsys, dir+"/"+downName)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", downName, err)
		}
		if _, err := pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("migrate: apply down %s: %w", downName, err)
		}
		if err := removeMigration(ctx, pool, name); err != nil {
			return fmt.Errorf("migrate: remove %s: %w", name, err)
		}
	}

	return nil
}

// upMigrationFiles returns .sql files that are not _down files, sorted.
func upMigrationFiles(fsys fs.FS, dir string) []string {
	names, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range names {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".sql") || strings.HasSuffix(n, "_down.sql") {
			continue
		}
		files = append(files, n)
	}
	sort.Strings(files)
	return files
}

// timestampPrefix extracts the timestamp portion of a migration name
// (everything before the first underscore).
func timestampPrefix(name string) string {
	if idx := strings.IndexByte(name, '_'); idx > 0 {
		return name[:idx]
	}
	return name
}

// downName returns the paired down migration filename.
func downName(upName string) string {
	if strings.HasSuffix(upName, ".sql") {
		return upName[:len(upName)-4] + "_down.sql"
	}
	return upName + "_down"
}

func appliedMigrationsOrdered(ctx context.Context, pool *db.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT name FROM _orchicon_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func removeMigration(ctx context.Context, pool *db.Pool, name string) error {
	_, err := pool.Exec(ctx, `DELETE FROM _orchicon_migrations WHERE name = $1`, name)
	return err
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
