#!/usr/bin/env bash
# install-dev.sh — Build the Orchicon dev binary and install to
# ~/.local/bin/orchicon-dev (or a custom path), so it's on your PATH.
#
# Usage:
#   scripts/install-dev.sh                          # install to ~/.local/bin/orchicon-dev
#   scripts/install-dev.sh /custom/path              # install to /custom/path/orchicon-dev
#   scripts/install-dev.sh --force                   # stop, install, then restart
#   scripts/install-dev.sh --force /custom/path      # stop, install, then restart
#
# The --force flag stops the running dev instance before replacing the
# binary, then restarts it with the new version. Without --force, if the
# binary is in use the install fails with "Text file busy".
#
# Run this from the Orchicon project root.

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
make build-dev --silent

# Check if the destination binary is currently in use (running process).
if fuser "$DEST/orchicon-dev" >/dev/null 2>&1; then
  if [ "$FORCE" = true ]; then
    echo "▸ Stopping dev instance (binary in use)…"
    # Try graceful stop via the binary itself.
    "$DEST/orchicon-dev" stop 2>/dev/null || true
    sleep 2
    # If still in use after graceful stop, force-kill only the binary holder.
    if fuser "$DEST/orchicon-dev" >/dev/null 2>&1; then
      echo "  ! Graceful stop did not release binary, sending SIGKILL"
      fuser -k "$DEST/orchicon-dev" 2>/dev/null || true
      sleep 1
    fi
  else
    echo ""
    echo "  ! $DEST/orchicon-dev is currently in use (running instance)."
    echo "  ! Use --force to stop the instance, install, and restart automatically."
    echo "  ! Or stop it manually first:  $DEST/orchicon-dev stop"
    echo ""
    exit 1
  fi
fi

echo "▸ Installing to $DEST/orchicon-dev…"
cp bin/orchicon-dev "$DEST/orchicon-dev"
chmod +x "$DEST/orchicon-dev"

echo ""
echo "  ✓ $(bin/orchicon-dev version) installed at $DEST/orchicon-dev"

if [ "$FORCE" = true ]; then
  echo "▸ Restarting dev instance…"
  "$DEST/orchicon-dev" start 2>/dev/null
else
  echo ""
  echo "Run:  $DEST/orchicon-dev start"
  echo "  (to start the dev instance with the new binary)"
fi
