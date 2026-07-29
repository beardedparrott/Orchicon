#!/usr/bin/env bash
# install-prod.sh — Build the Orchicon binary from local source and
# install it to ~/.local/bin/orchicon (the prod instance binary path).
#
# Usage:
#   scripts/install-prod.sh                 # install to ~/.local/bin/orchicon
#   scripts/install-prod.sh /custom/path     # install to /custom/path/orchicon
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
make build --silent

echo "▸ Installing to $DEST/orchicon…"
cp bin/orchicon "$DEST/orchicon"
chmod +x "$DEST/orchicon"

echo ""
echo "  ✓ orchicon v$(bin/orchicon version) installed at $DEST/orchicon"
echo ""
echo "Run:  scripts/dev-prod.sh restart"
echo "  (to restart the prod instance with the new binary)"
