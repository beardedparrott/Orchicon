// Package assets holds go:embed directives for the single-container
// runtime configs, Atlas migrations, and the built frontend bundle. The
// binary embeds everything needed for `orchicon container` (the PID-1
// supervisor) and `orchicon serve` so a user with just the binary (and
// Docker) gets the complete experience — no Go, Node, or source checkout
// required (AGENTS.md §Dev Control Script, §Install Scripts).
//
// The embed paths are relative to this file (the module root) so they
// can reference deploy/, db/, and frontend/ without `..` (which go:embed
// forbids).
package assets

import "embed"

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

// ContainerFS embeds the single-container runtime configs
// (deploy/container/configs/) — Tempo/Loki/OTel-collector/Grafana configs
// with @DATA_DIR@ placeholders, plus the Grafana provisioning. The `orchicon
// container` supervisor (cmd/orchicon/container.go) writes these into the
// container data dir, substituting the data dir path, and spawns each
// process against them.
//
// NOTE: this embeds ONLY deploy/container/configs. Do NOT embed
// deploy/container/ as a whole — container.sh copies bin/orchicon there
// for the docker build, and go:embed would recursively embed the previous
// binary, doubling the binary on every rebuild (75MB -> ... -> 1.9GB).
//
//go:embed all:deploy/container/configs
var ContainerFS embed.FS

// RuntimeImageTemplatesFS embeds the stock runtime-image Dockerfiles
// (deploy/runtime/Dockerfile + variants). The Runtime Images page serves
// these read-only so users can see how a shipped image is built and copy
// the pattern for a custom image. Only the specific Dockerfiles are
// embedded (NOT the whole deploy/runtime dir — container.sh copies
// bin/orchicon there for the docker build, and go:embed would recursively
// embed the binary, doubling the size on every rebuild).
//
//go:embed deploy/runtime/Dockerfile deploy/runtime/Dockerfile.gui deploy/runtime/Dockerfile.dev
var RuntimeImageTemplatesFS embed.FS

// ThirdPartyFS embeds third-party license + notice text so the Apache-2.0
// obligations of vendored libraries ship inside the distribution (the
// binary IS the distribution). `orchicon notices` prints them.
//
//go:embed all:third_party/oidc
var ThirdPartyFS embed.FS
