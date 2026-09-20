// Command atlas-ci applies the embedded Atlas migrations to a Postgres
// database from CI — the exact engine the control plane runs on boot
// (internal/migrate.Run over assets.MigrationsFS), so the rls-check gate
// in CI enforces against the real production schema, not a vacuous empty
// database (zero tables = zero violations).
//
// Hermetic: no atlas binary on the runner, no network beyond the DB
// connection. The Postgres service container must be up before this runs
// (ci.yml starts it with a pg_isready health check).
//
// Usage: go run ./tools/atlas-ci   (DB_URL env var; falls back to the
// Makefile's default local URL when unset)
package main

import (
	"context"
	"fmt"
	"os"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atlas-ci:", err)
		os.Exit(1)
	}
}

func run() error {
	url := os.Getenv("DB_URL")
	if url == "" {
		url = "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	fmt.Println("atlas-ci: migrations applied OK")
	return nil
}