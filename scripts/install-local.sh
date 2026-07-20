#!/usr/bin/env bash
# install-local.sh — Build the Orchicon binary from local source and
# install it to ~/.local/bin/orchicon (or a custom path).
#
# Usage:
#   scripts/install-local.sh               # install to ~/.local/bin/orchicon
#   scripts/install-local.sh /custom/path   # install to /custom/path/orchicon
#
# Run this from the Orchicon project root. No source files are modified;
# only bin/orchicon (gitignored) and the destination are written.

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

echo "  ✓ orchicon v$(bin/orchicon version) installed at $DEST/orchicon"
echo ""
echo "Run:  orchicon start"
