// Package db is the data-access layer.
//
// Per docs/09_Database_Schema.md §1, all SQL flows through this package.
// It owns the pgxpool connection, sets the tenant context per
// transaction (app.tenant_id session variable for RLS), and exposes
// typed accessors for each table group. No raw SQL is permitted outside
// this package (AGENTS.md invariant #4).
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgxpool.Pool and is the single handle to Postgres for
// the whole control plane. Concurrent goroutines share one pool.
type Pool struct {
	*pgxpool.Pool
}

// Open creates a pool against the given DSN with sane defaults.
func Open(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	return &Pool{Pool: p}, nil
}

// Close releases the connection pool.
func (p *Pool) Close() {
	if p.Pool != nil {
		p.Pool.Close()
	}
}

// WithTenant sets the app.tenant_id session variable for the duration
// of a transaction so RLS policies (docs/09_Database_Schema.md §8.5)
// enforce tenant isolation as a backstop to the data-access layer.
//
// Usage:
//
//	tx, err := pool.BeginTx(ctx)
//	defer tx.Rollback(ctx)
//	err = pool.WithTenant(tx, tenantID)(func(tx pgx.Tx) error { ... })
type TenantTx struct {
	pgx.Tx
	tenantID string
}

// BeginTenantTx starts a transaction scoped to a tenant.
func (p *Pool) BeginTenantTx(ctx context.Context, tenantID string) (*TenantTx, error) {
	tx, err := p.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("db: begin tx: %w", err)
	}
	// set_config with is_local=true sets the variable for the duration of
	// this transaction only. We use the function form (not SET LOCAL)
	// because SET does not accept parameterized values in pgx — using
	// set_config keeps the tenant id parameterized so it is never
	// string-interpolated into SQL (AGENTS.md security standards).
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("db: set tenant context: %w", err)
	}
	return &TenantTx{Tx: tx, tenantID: tenantID}, nil
}
