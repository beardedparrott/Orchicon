// Index-integrity checking.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// EnsureAmcheck installs the amcheck extension if it is not already present.
func EnsureAmcheck(ctx context.Context, pool *Pool) (bool, error) {
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
		return false, fmt.Errorf("db: create amcheck extension: %w", err)
	}
	return true, nil
}

// ListUserBtreeIndexes returns the fully-qualified names of every btree index on user tables
// (public schema), excluding system catalogs.
func ListUserBtreeIndexes(ctx context.Context, pool *Pool) ([]string, error) {
	const q = `
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_index i ON i.indexrelid = c.oid
		JOIN pg_am a ON a.oid = c.relam
		WHERE n.nspname = 'public'
		  AND c.relkind = 'i'
		  AND a.amname = 'btree'
		ORDER BY 1`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list indexes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("db: scan index name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CheckIndex verifies a single btree index with amcheck's bt_index_parent_check.
func CheckIndex(ctx context.Context, pool *Pool, indexName string) error {
	var ok string
	err := pool.QueryRow(ctx, "SELECT bt_index_parent_check($1::regclass)", indexName).Scan(&ok)
	if err != nil {
		return fmt.Errorf("index %s corrupt: %w", indexName, err)
	}
	return nil
}

// CheckIndexIntegrity validates every user btree index and returns the names of any corrupted ones.
func CheckIndexIntegrity(ctx context.Context, pool *Pool) ([]string, error) {
	installed, err := EnsureAmcheck(ctx, pool)
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, nil
	}
	indexes, err := ListUserBtreeIndexes(ctx, pool)
	if err != nil {
		return nil, err
	}
	var corrupt []string
	for _, name := range indexes {
		if err := CheckIndex(ctx, pool, name); err != nil {
			corrupt = append(corrupt, name)
		}
	}
	return corrupt, nil
}

// ReindexIndex rebuilds a single index without blocking writes.
func ReindexIndex(ctx context.Context, pool *Pool, indexName string) error {
	if _, err := pool.Exec(ctx, "REINDEX INDEX CONCURRENTLY "+indexName); err != nil {
		return fmt.Errorf("db: reindex %s: %w", indexName, err)
	}
	return nil
}

// RepairCorruptIndexes checks all user btree indexes and rebuilds any that are corrupted.
func RepairCorruptIndexes(ctx context.Context, pool *Pool, log *slog.Logger) []string {
	qCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	corrupt, err := CheckIndexIntegrity(qCtx, pool)
	if err != nil {
		log.Warn("index integrity check unavailable", "error", err)
		return nil
	}
	if len(corrupt) == 0 {
		return nil
	}
	var repaired []string
	for _, name := range corrupt {
		log.Warn("corrupt index detected — rebuilding", "index", name)
		if err := ReindexIndex(qCtx, pool, name); err != nil {
			log.Error("reindex failed", "index", name, "error", err)
			continue
		}
		repaired = append(repaired, name)
		log.Info("corrupt index rebuilt", "index", name)
	}
	return repaired
}
