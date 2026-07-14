// Package assets holds go:embed directives for the Docker Compose stack,
// Atlas migrations, and the built frontend bundle. The binary embeds
// everything needed for `orchicon dev start` so that a user with just
// the binary (and Docker) gets the complete experience — no Go, Node,
// or source checkout required (AGENTS.md §Dev Control Script, §Install
// Scripts).
//
// The embed paths are relative to this file (the module root) so they
// can reference deploy/, db/, and frontend/ without `..` (which go:embed
// forbids).
package assets

import "embed"

// ComposeFS embeds the entire Docker Compose stack directory
// (deploy/compose/) — the YAML, the Postgres init SQL, the ClickHouse
// cluster config, the OTel collector config, and the SigNoz config.
//
// The compose file uses relative mounts (e.g.
// ./clickhouse-cluster.xml:/etc/clickhouse-server/config.d/cluster.xml),
// so the binary extracts this FS into a temp directory and runs
// `docker compose` from there. If only the YAML were embedded, the
// side-file mounts would silently fail (Docker creates an empty
// directory at the destination, ClickHouse never sees the cluster
// definition, the schema migrator 404s). See cmd/orchicon/dev.go.
//
//go:embed all:deploy/compose
var ComposeFS embed.FS

// MigrationsFS embeds the Atlas migration SQL files (db/migrations/*.sql).
// The migration runner reads these and applies them in order.
//
//go:embed all:db/migrations
var MigrationsFS embed.FS

// FrontendFS embeds the built frontend bundle (frontend/dist/). When the
// frontend has been built (make fe-build / npm run build), this contains
// the SPA assets. When it has not (dev builds without a frontend step),
// the directory contains only .gitkeep and the SPA serving falls back to
// the Vite dev server proxy.
//
//go:embed all:frontend/dist
var FrontendFS embed.FS

// MigrationsDir is the directory within MigrationsFS containing the
// migration SQL files.
const MigrationsDir = "db/migrations"

// FrontendDir is the directory within FrontendFS containing the SPA.
const FrontendDir = "frontend/dist"
