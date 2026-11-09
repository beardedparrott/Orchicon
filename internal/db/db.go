// Package db is the data-access layer.
package db

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// defaultPoolMaxConns is the FLOOR for the connection pool, and it is deliberately not derived from
	// NumCPU alone.
	defaultPoolMaxConns = 32

	// envPoolMaxConns overrides the ceiling for operators who know their host's Postgres limits.
	envPoolMaxConns = "ORCHICON_DB_MAX_CONNS"
)

// resolvePoolMaxConns decides the pool ceiling and reports which rule decided it (for tests and
// debugging).
func resolvePoolMaxConns(dsn string, numCPU int, getenv func(string) string) (int32, string) {
	if v := strings.TrimSpace(getenv(envPoolMaxConns)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return int32(n), "env:" + envPoolMaxConns
		}
	}
	if strings.Contains(dsn, "pool_max_conns") {
		return 0, "dsn:pool_max_conns"
	}
	n := numCPU * 4
	if n < defaultPoolMaxConns {
		n = defaultPoolMaxConns
	}
	return int32(n), "default"
}

// Pool wraps a pgxpool.Pool and is the single handle to Postgres for
// the whole control plane. Concurrent goroutines share one pool.
type Pool struct {
	*pgxpool.Pool
}

// Open creates a pool against the given DSN with sane defaults.
func Open(ctx context.Context, dsn string) (*Pool, error) {
	// A TEST BINARY MAY NEVER OPEN THE PRODUCTION DATABASE.
	if err := guardAgainstProdDSN(dsn); err != nil {
		return nil, err
	}
	// AND IT MAY NOT OPEN ONE A RUNNING PLANE IS SERVING EITHER — not only a prod-labelled one.
	if err := guardAgainstLivePlaneDSN(dsn); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	// THE POOL IS SIZED HERE, NOT INHERITED FROM NumCPU.
	if maxConns, _ := resolvePoolMaxConns(dsn, runtime.NumCPU(), os.Getenv); maxConns > 0 {
		cfg.MaxConns = maxConns
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
	// set_config with is_local=true sets the variable for the duration of this transaction only.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("db: set tenant context: %w", err)
	}
	return &TenantTx{Tx: tx, tenantID: tenantID}, nil
}
