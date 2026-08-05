// Index-integrity checking.
//
// Field incident: a corrupted btree index (worker_versions_worker_version_idx)
// made an equality lookup return zero rows for a worker that existed — the
// planner used the index for `=`, the index was corrupt, and the row vanished.
// The workflow reconciler then failed a step dispatch ("load worker version …
// db: not found"), errored the whole pass, and rolled back an upstream step's
// completed success — leaving the run stuck "running" with a succeeded
// execution underneath. The corrupt index was caused by a hard host sleep in
// the middle of a write.
//
// This package adds a boot-time + periodic integrity check using the
// `amcheck` extension (bt_index_parent_check). A corrupt index is caught and
// rebuilt with REINDEX INDEX CONCURRENTLY before it can silently hide rows.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// EnsureAmcheck installs the amcheck extension if it is not already present.
// amcheck ships with PostgreSQL contrib; CREATE EXTENSION requires superuser
// (the single-container postgres runs as its owner). Best-effort: returns
// false (without error) when the extension cannot be created so the integrity
// check degrades gracefully rather than failing boot.
func EnsureAmcheck(ctx context.Context, pool *Pool) (bool, error) {
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
		return false, fmt.Errorf("db: create amcheck extension: %w", err)
	}
	return true, nil
}

// ListUserBtreeIndexes returns the fully-qualified names of every btree index
// on user tables (public schema), excluding system catalogs. These are the
// indexes amcheck can validate.
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

// CheckIndex verifies a single btree index with amcheck's
// bt_index_parent_check. It returns nil when the index is sound and a
// descriptive error when it is corrupted.
func CheckIndex(ctx context.Context, pool *Pool, indexName string) error {
	var ok string
	err := pool.QueryRow(ctx, "SELECT bt_index_parent_check($1::regclass)", indexName).Scan(&ok)
	if err != nil {
		return fmt.Errorf("index %s corrupt: %w", indexName, err)
	}
	return nil
}

// CheckIndexIntegrity validates every user btree index and returns the names
// of any corrupted ones. amcheck must be installed (see EnsureAmcheck); when
// it is unavailable the check returns nil (best-effort, caller logs).
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

// ReindexIndex rebuilds a single index without blocking writes. Must run on
// its own connection/transaction (REINDEX INDEX CONCURRENTLY cannot run
// inside a transaction block, so we issue it on the pool directly).
func ReindexIndex(ctx context.Context, pool *Pool, indexName string) error {
	if _, err := pool.Exec(ctx, "REINDEX INDEX CONCURRENTLY "+indexName); err != nil {
		return fmt.Errorf("db: reindex %s: %w", indexName, err)
	}
	return nil
}

// RepairCorruptIndexes checks all user btree indexes and rebuilds any that
// are corrupted. Returns the names of indexes that were repaired. Intended
// for boot + periodic sweeps; failures are reported (not fatal).
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
