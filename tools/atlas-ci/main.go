// Command atlas-ci applies the embedded Atlas migrations to a Postgres database from CI — the exact
// engine the control plane runs on boot (internal/migrate.Run over assets.MigrationsFS).
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