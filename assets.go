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

// ComposeYAML is the embedded Docker Compose stack
// (deploy/compose/docker-compose.yml).
//
//go:embed deploy/compose/docker-compose.yml
var ComposeYAML string

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
