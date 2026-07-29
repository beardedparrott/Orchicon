#!/usr/bin/env bash
# install-local.sh — Build the Orchicon binary from local source and
# install both orchicon-dev and orchicon-prod binaries.
#
# Usage:
#   scripts/install-local.sh               # install both to ~/.local/bin/
#   scripts/install-local.sh /custom/path   # install to /custom/path/
#
# Run this from the Orchicon project root.

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

DEST="${1:-$HOME/.local/bin}"
mkdir -p "$DEST"

echo "▸ Building frontend…"
npm run --silent --prefix frontend build

echo "▸ Building binaries (standard + dev + prod)…"
make build --silent && make build-dev --silent && make build-prod --silent

echo "▸ Installing to $DEST/orchicon…"
cp bin/orchicon "$DEST/orchicon"
chmod +x "$DEST/orchicon"

echo "▸ Installing to $DEST/orchicon-dev…"
cp bin/orchicon-dev "$DEST/orchicon-dev"
chmod +x "$DEST/orchicon-dev"

echo "▸ Installing to $DEST/orchicon-prod…"
cp bin/orchicon-prod "$DEST/orchicon-prod"
chmod +x "$DEST/orchicon-prod"

echo ""
echo "  ✓ $(bin/orchicon version) installed"
echo "    std:  $DEST/orchicon"
echo "    dev:  $DEST/orchicon-dev"
echo "    prod: $DEST/orchicon-prod"
echo ""
echo "Run:  orchicon start        (default dev instance)"
echo "      orchicon-prod start   (prod instance)"
