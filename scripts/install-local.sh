#!/usr/bin/env bash
# install-local.sh — Build the Orchicon binary from local source and install it.
#
# The binary is the control plane. The SERVICES it needs (Postgres, NATS,
# telemetry) are host processes of the invoking user, and
# `scripts/orchicon.sh` starts both halves for one instance; see the guidance
# printed at the end.
#
# Usage:
#   scripts/install-local.sh                         # install to ~/.local/bin/
#   scripts/install-local.sh --force                 # stop the running plane, install, restart serve
#   scripts/install-local.sh /custom/path            # install to /custom/path/
#
# Without --force, the install fails rather than replacing a binary a running
# plane is executing.

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

# in_use reports whether a running plane is executing $DEST/orchicon.
#
# PORTABILITY IS THE POINT. This used to be `fuser`, which is Linux-only and
# ABSENT ON macOS BY DEFAULT — so on a Mac the check silently never fired and the
# install either overwrote a live binary or died with a bare "Text file busy".
#
#   pgrep -f "<path> serve"  is the portable primary: pgrep exists on Linux AND
#                            macOS, and the command line of the plane we install
#                            is exactly `orchicon serve [--detach]`, so the path
#                            followed by `serve` identifies it.
#   fuser                    kept as a guarded LINUX second opinion — it sees a
#                            process holding the file as data, which the command
#                            line does not. It never runs alone, and its absence
#                            is not an error.
#
# Where neither tool exists (a minimal container with no procps and no psmisc)
# in_use answers "no", and the install then fails LOUDLY on "Text file busy"
# rather than silently replacing a live binary.
in_use() {
  if pgrep -f "$DEST/orchicon serve" >/dev/null 2>&1; then
    return 0
  fi
  if [ "$(uname -s)" = "Linux" ] && command -v fuser >/dev/null 2>&1; then
    fuser "$DEST/orchicon" >/dev/null 2>&1 && return 0
  fi
  return 1
}

# stop_running kills ONLY this destination's plane, never an instance's sibling:
# the pattern is the installed path followed by `serve`, so a plane started from
# another prefix (a checkout's bin/orchicon, or a second instance's copy) is
# untouched. SIGKILL is the second step, after the graceful stop had its chance.
stop_running() {
  "$DEST/orchicon" serve --stop 2>/dev/null || true
  sleep 2
  if in_use; then
    echo "  ! Graceful stop did not release the binary, sending SIGKILL"
    pkill -f "$DEST/orchicon serve" 2>/dev/null || true
    sleep 1
  fi
}

echo "▸ Building frontend…"
npm run --silent --prefix frontend build

echo "▸ Building binary…"
make build --silent

if [ -e "$DEST/orchicon" ] && in_use; then
  if [ "$FORCE" = true ]; then
    echo "▸ Stopping running orchicon (binary in use)…"
    stop_running
  else
    echo ""
    echo "  ! $DEST/orchicon is currently in use (running plane)."
    echo "  ! Use --force to stop it, install, and restart automatically."
    echo "  ! Or stop it manually first: $DEST/orchicon serve --stop"
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
echo "Run the full stack on THIS host (services + plane; no image, no instance container):"
echo "      make instance-build && make instance-up   (dev instance)"
echo "  or  scripts/orchicon.sh start dev | start prod | start test"
echo "  then  scripts/orchicon.sh status            (what is up on this host, per instance)"
echo ""
echo "The three instances are separate stacks on one host — separate ports,"
echo "database and data dir — so stopping one never touches the others."
echo "Remove ONE instance and its data (destructive, prints what it deletes):"
echo "      scripts/orchicon.sh uninstall test --yes"
echo ""
echo "Headless plane (external Postgres/NATS, no instance container):"
echo "      orchicon serve --detach"
echo ""
