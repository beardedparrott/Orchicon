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
#   scripts/install-prod.sh                          # install to ~/.local/bin/orchicon-prod
#   scripts/install-prod.sh /custom/path              # install to /custom/path/orchicon-prod
#   scripts/install-prod.sh --force /custom/path      # stop, install, then restart
#   scripts/install-prod.sh --force                   # stop, install, then restart
#
# The --force flag stops the running prod instance before replacing the
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
make build-prod --silent

# Check if the destination binary is currently in use (running process).
if fuser "$DEST/orchicon-prod" >/dev/null 2>&1; then
  if [ "$FORCE" = true ]; then
    echo "▸ Stopping prod instance (binary in use)…"
    scripts/dev-prod.sh stop 2>/dev/null || true
    sleep 2
    # If still in use after graceful stop, force-kill only the binary holder.
    if fuser "$DEST/orchicon-prod" >/dev/null 2>&1; then
      echo "  ! Graceful stop did not release binary, sending SIGKILL"
      fuser -k "$DEST/orchicon-prod" 2>/dev/null || true
      sleep 1
    fi
  else
    echo ""
    echo "  ! $DEST/orchicon-prod is currently in use (running instance)."
    echo "  ! Use --force to stop the instance, install, and restart automatically."
    echo "  ! Or stop it manually first:  scripts/dev-prod.sh stop"
    echo ""
    exit 1
  fi
fi

echo "▸ Installing to $DEST/orchicon-prod…"
cp bin/orchicon-prod "$DEST/orchicon-prod"
chmod +x "$DEST/orchicon-prod"

echo ""
echo "  ✓ $(bin/orchicon-prod version) installed at $DEST/orchicon-prod"

if [ "$FORCE" = true ]; then
  echo "▸ Starting prod instance…"
  scripts/dev-prod.sh start 2>/dev/null
else
  echo ""
  echo "Run:  scripts/dev-prod.sh restart"
  echo "  (to restart the prod instance with the new binary)"
fi
