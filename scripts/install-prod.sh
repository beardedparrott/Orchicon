#!/usr/bin/env bash
# install-prod.sh — Build the Orchicon prod binary from local source and
# install it to ~/.local/bin/orchicon-prod, which is the binary path
# for the prod instance (scripts/dev-prod.sh).
#
# This builds from your local source checkout — same as install-dev.sh,
# but targets a different path so dev rebuilds don't clobber the prod
# binary.
#
# Usage:
#   scripts/install-prod.sh                 # install to ~/.local/bin/orchicon-prod
#   scripts/install-prod.sh /custom/path     # install to /custom/path/orchicon-prod
#
# After installing, restart the prod instance:
#   scripts/dev-prod.sh restart
#
# Run this from the Orchicon project root.

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

DEST="${1:-$HOME/.local/bin}"
mkdir -p "$DEST"

echo "▸ Building frontend…"
npm run --silent --prefix frontend build

echo "▸ Building binary…"
make build-prod --silent

echo "▸ Installing to $DEST/orchicon-prod…"
cp bin/orchicon-prod "$DEST/orchicon-prod"
chmod +x "$DEST/orchicon-prod"

echo ""
echo "  ✓ orchicon v$(bin/orchicon-prod version) installed at $DEST/orchicon-prod"
echo ""
echo "Run:  scripts/dev-prod.sh restart"
echo "  (to restart the prod instance with the new binary)"
