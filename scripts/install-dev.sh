#!/usr/bin/env bash
# install-dev.sh — Build the Orchicon dev binary and install to
# ~/.local/bin/orchicon-dev (or a custom path), so it's on your PATH.
#
# Usage:
#   scripts/install-dev.sh                # install to ~/.local/bin/orchicon-dev
#   scripts/install-dev.sh /custom/path   # install to /custom/path/orchicon-dev
#
# After installing, start the dev instance:
#   orchicon-dev start
#
# Run this from the Orchicon project root.

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

DEST="${1:-$HOME/.local/bin}"
mkdir -p "$DEST"

echo "▸ Building frontend…"
npm run --silent --prefix frontend build

echo "▸ Building binary…"
make build-dev --silent

echo "▸ Installing to $DEST/orchicon-dev…"
cp bin/orchicon-dev "$DEST/orchicon-dev"
chmod +x "$DEST/orchicon-dev"

echo ""
echo "  ✓ $(bin/orchicon-dev version) installed at $DEST/orchicon-dev"
echo ""
echo "Run:  orchicon-dev start"
echo "  (migrations apply automatically on start)"
