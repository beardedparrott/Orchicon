#!/usr/bin/env bash
# install-local.sh — Build the Orchicon binary from local source and
# install all three binaries (standard, dev, prod).
#
# Usage:
#   scripts/install-local.sh                         # install to ~/.local/bin/
#   scripts/install-local.sh --force                 # stop running instances, install, restart
#   scripts/install-local.sh /custom/path            # install to /custom/path/
#   scripts/install-local.sh --force /custom/path    # stop, install, restart
#
# Without --force, the install will fail with "Text file busy" if any of
# the destination binaries are currently in use by a running instance.

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

echo "▸ Building binaries…"
make build --silent && make build-dev --silent && make build-prod --silent

# Check each binary and stop running instances if --force.
for bin_name in orchicon orchicon-dev orchicon-prod; do
  if fuser "$DEST/$bin_name" >/dev/null 2>&1; then
    if [ "$FORCE" = true ]; then
      echo "▸ Stopping $bin_name instance (binary in use)…"
      case "$bin_name" in
        orchicon)      "$DEST/orchicon" stop 2>/dev/null || true ;;
        orchicon-dev)  "$DEST/orchicon-dev" stop 2>/dev/null || true ;;
        orchicon-prod) scripts/dev-prod.sh stop 2>/dev/null || true ;;
      esac
      sleep 2
      if fuser "$DEST/$bin_name" >/dev/null 2>&1; then
        echo "  ! Graceful stop did not release $bin_name, sending SIGKILL"
        fuser -k "$DEST/$bin_name" 2>/dev/null || true
        sleep 1
      fi
    else
      echo ""
      echo "  ! $DEST/$bin_name is currently in use (running instance)."
      echo "  ! Use --force to stop instances, install, and restart automatically."
      echo "  ! Or stop it manually first."
      echo ""
      exit 1
    fi
  fi
done

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

if [ "$FORCE" = true ]; then
  echo "▸ Restarting instances…"
  "$DEST/orchicon-dev" start 2>/dev/null &
  echo "  ✓ Dev instance starting (background)"
  echo "  Note: Restart prod separately: scripts/dev-prod.sh start"
else
  echo ""
  echo "Run:  orchicon start        (default dev instance)"
  echo "      orchicon-prod start   (prod instance)"
fi
