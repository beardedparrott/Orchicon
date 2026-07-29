#!/usr/bin/env bash
# install-dev.sh — Build the Orchicon binary from local source to bin/.
#
# The binary is placed in bin/orchicon (the default dev binary path).
# It does not install to ~/.local/bin — that is the prod instance's
# binary. For daily iteration: make build && scripts/dev.sh start.
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
