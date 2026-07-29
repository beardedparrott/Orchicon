#!/usr/bin/env bash
# install-dev.sh — Build the Orchicon dev binary from local source.
#
# Produces bin/orchicon-dev for the dev instance. Does not install
# to ~/.local/bin — that is the prod instance's binary path.
#
# Run this from the Orchicon project root.

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

echo "▸ Building frontend…"
npm run --silent --prefix frontend build

echo "▸ Building binary…"
make build-dev --silent

echo ""
echo "  ✓ orchicon v$(bin/orchicon-dev version) built at bin/orchicon-dev"
echo ""
echo "Run:  ./bin/orchicon-dev start"
echo "  (migrations apply automatically on start)"
