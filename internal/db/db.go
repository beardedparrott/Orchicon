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
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// defaultPoolMaxConns is the FLOOR for the connection pool, and it is deliberately not derived from
	// NumCPU alone. The plane pins one connection per reconciler kind for the process lifetime (7 today),
	// dispatch takes one per in-flight item (4 by default), and every HTTP request needs one; 32 leaves
	// room for all three with headroom to spare, on any host size.
	defaultPoolMaxConns = 32

	// envPoolMaxConns overrides the ceiling for operators who know their host's Postgres limits.
	envPoolMaxConns = "ORCHICON_DB_MAX_CONNS"
)

// resolvePoolMaxConns decides the pool ceiling and reports which rule decided it (for tests and debugging).
//
// Return 0 means "leave whatever the DSN parsed" — an explicit `pool_max_conns` in the connection string is
// an operator's deliberate choice and must not be overridden by a default.
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
	// A TEST BINARY MAY NEVER OPEN THE PRODUCTION DATABASE. This is the one chokepoint every database handle in
	// the tree passes through, which is why the guard lives here rather than in a helper a test could skip — see
	// testguard.go for the incident that put it here, and for how to opt out.
	if err := guardAgainstProdDSN(dsn); err != nil {
		return nil, err
	}
	// AND IT MAY NOT OPEN ONE A RUNNING PLANE IS SERVING EITHER — not only a prod-labelled one. The README tells
	// operators to point ORCHICON_TEST_DSN at the DEV instance's database, which on a workstation is the database
	// the live dev plane's reconcilers are actively scanning; the two converge on the same rows and produce
	// `db: version conflict` failures that look like test bugs. See testguard.go.
	if err := guardAgainstLivePlaneDSN(dsn); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	// THE POOL IS SIZED HERE, NOT INHERITED FROM NumCPU.
	//
	// pgx's default is `max(4, NumCPU)`, and on a small host that is SMALLER THAN THE PLANE'S OWN
	// PINNED-CONNECTION FLOOR. leaderElection (internal/reconciler/manager.go) holds a dedicated
	// *pgxpool.Conn per reconciler kind for the process lifetime — pg_try_advisory_lock is session-scoped,
	// so 7 kinds means 7 connections that never return. Above that, dispatch holds one per in-flight item
	// (DispatchConcurrency, default 4) and every HTTP request needs one.
	//
	// MEASURED on a 6-CPU macOS host: pool = max(4,6) = 6, the seven reconcilers pinned all of them, and
	// every DB-backed request then blocked FOREVER — sign-up and local-login hung until the client gave up
	// and the server logged `db: begin tx: context canceled`, while /healthz returned 200 (it touches no
	// database) and the UI loaded. The plane looked healthy and could not create an account, so nobody
	// could log in. The same code on a 12-16 CPU workstation leaves headroom, which is why this surfaced
	// only on a small host: a 2-4 vCPU cloud VM or a small container reaches it too.
	//
	// The comment on config.DispatchConcurrency already required "the pool's max_conns must comfortably
	// exceed it". Nothing ever set max_conns, so the requirement was documented and unenforced.
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
