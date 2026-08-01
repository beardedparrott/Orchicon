#!/usr/bin/env bash
# install-local.sh — Build the Orchicon binary from local source and install
# it. The single container (scripts/container.sh) is the full-stack
# deployment; the binary itself is the control plane + PID-1 supervisor.
#
# Usage:
#   scripts/install-local.sh                         # install to ~/.local/bin/
#   scripts/install-local.sh --force                 # stop running binary, install, restart serve
#   scripts/install-local.sh /custom/path            # install to /custom/path/
#
# Without --force, the install will fail with "Text file busy" if the
# destination binary is currently in use by a running instance.

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

FORCE=false
DEST="$HOME/.local/bin"
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=true ;;
    *)       DEST="$arg" ;;
  esac
done

mkdir -p "$DEST"

echo "▸ Building frontend…"
npm run --silent --prefix frontend build

echo "▸ Building binary…"
make build --silent

if fuser "$DEST/orchicon" >/dev/null 2>&1; then
  if [ "$FORCE" = true ]; then
    echo "▸ Stopping running orchicon (binary in use)…"
    "$DEST/orchicon" serve --stop 2>/dev/null || true
    sleep 2
    if fuser "$DEST/orchicon" >/dev/null 2>&1; then
      echo "  ! Graceful stop did not release the binary, sending SIGKILL"
      fuser -k "$DEST/orchicon" 2>/dev/null || true
      sleep 1
    fi
  else
    echo ""
    echo "  ! $DEST/orchicon is currently in use (running instance)."
    echo "  ! Use --force to stop it, install, and restart automatically."
    echo "  ! Or stop it manually first: orchicon serve --stop"
    echo ""
    exit 1
  fi
fi

echo "▸ Installing to $DEST/orchicon…"
cp bin/orchicon "$DEST/orchicon"
chmod +x "$DEST/orchicon"

echo ""
echo "  ✓ $(bin/orchicon version) installed"

echo ""
echo "Run the full stack with the single container:"
echo "      make container-build && make container-up   (dev instance)"
echo "  or  scripts/container.sh up dev | up prod"
echo ""
echo "Headless control plane (external Postgres/NATS):"
echo "      orchicon serve --detach"
