#!/usr/bin/env bash
# ============================================================================
# Build the Orchicon AppImage.
#
# WHY AN APPIMAGE AT ALL. The .deb declares libwebkit2gtk-4.0-37, which exists on
# Ubuntu <= 23.10 and Debian 12 — but Ubuntu 24.04, Fedora 40 and everything after
# them dropped the 4.0 series. Those users install this instead: the AppImage
# CARRIES libwebkit2gtk, so no system package decision is involved.
#
# Inputs (produced by `wails build` in the same job):
#   cmd/orchicon-desktop/build/bin/orchicon-desktop   the app binary
#   desktop/build/appicon.png            the icon
# Output:
#   dist/desktop/orchicon_desktop_<ver>_linux_amd64.AppImage
#
# The pinned appimagetool digest is env-overridable: `APPIMAGETOOL_SHA256=<hex>`
# turns a missing/mismatched digest into a hard failure, which is what a release
# job should run with. Left unset, the script prints the digest it measured and
# proceeds, so a first build is possible without a chicken-and-egg chicken. Set
# it once in the workflow and the download is pinned from then on.
# ============================================================================
set -euo pipefail

VERSION="${ORCHICON_VERSION:-0.0.0-dev}"
VERSION="${VERSION#v}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUT="${ROOT}/dist/desktop"
APPIMAGETOOL_URL="https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage"
BIN="${ROOT}/cmd/orchicon-desktop/build/bin/orchicon-desktop"
ICON="${ROOT}/cmd/orchicon-desktop/build/appicon.png"

[ -x "$BIN" ] || { echo "error: $BIN not found — run \`wails build\` first" >&2; exit 1; }
mkdir -p "$OUT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
APPDIR="$WORK/Orchicon.AppDir"
mkdir -p "$APPDIR/usr/bin" "$APPDIR/usr/lib/orchicon" "$APPDIR/usr/share/applications" \
         "$APPDIR/usr/share/icons/hicolor/512x512/apps"

# --- the app ---------------------------------------------------------------
install -m 0755 "$BIN" "$APPDIR/usr/lib/orchicon/orchicon-desktop"
install -m 0755 "$ROOT/deploy/desktop/appimage/AppRun" "$APPDIR/AppRun"
install -m 0644 "$ROOT/deploy/desktop/linux/orchicon-desktop.desktop" \
  "$APPDIR/orchicon-desktop.desktop"
install -m 0644 "$ROOT/deploy/desktop/linux/orchicon-desktop.desktop" \
  "$APPDIR/usr/share/applications/orchicon-desktop.desktop"
if [ -f "$ICON" ]; then
  install -m 0644 "$ICON" "$APPDIR/orchicon-desktop.png"
  install -m 0644 "$ICON" "$APPDIR/usr/share/icons/hicolor/512x512/apps/orchicon-desktop.png"
fi

# --- the webview, bundled --------------------------------------------------
# Only the two libraries that make the 4.0 series unavailable on a new distro are
# copied: webkit itself and the JavaScriptCore it links to. GTK3 and the rest are
# left to the system, because every desktop that can run a GTK app already has
# them; bundling the whole GTK stack is what appimage-builder exists for, and
# hand-rolling it is how an AppImage ends up subtly broken.
# THE SONAMEs ARE NOT SYMMETRIC, and that is not a typo to normalise away:
#   WebKitGTK 4.0 -> libwebkit2gtk-4.0.so.37 + libjavascriptcoregtk-4.0.so.18
#   WebKitGTK 4.1 -> libwebkit2gtk-4.1.so.0  + libjavascriptcoregtk-4.1.so.0
# This list used to ask for `libwebkit2gtk-4.1.so.37`, which no distribution ships,
# so on a 4.1-only host nothing matched, the copy loop was a no-op, and the build
# died on the "no libwebkit2gtk found to bundle" guard below.
for so in libwebkit2gtk-4.0.so.37 libjavascriptcoregtk-4.0.so.18 libwebkit2gtk-4.1.so.0 \
          libjavascriptcoregtk-4.1.so.0; do
  found="$(ldconfig -p 2>/dev/null | awk -v n="$so" '$1 == n { print $NF; exit }')"
  if [ -n "${found:-}" ]; then
    cp -L "$found" "$APPDIR/usr/lib/orchicon/"
    echo "bundled: $so"
  fi
done
if ! ls "$APPDIR/usr/lib/orchicon"/libwebkit2gtk-* >/dev/null 2>&1; then
  echo "error: no libwebkit2gtk found to bundle — this AppImage would not start on a distro" >&2
  echo "       without WebKitGTK. Install libwebkit2gtk-4.0-dev (or -4.1-dev) first." >&2
  exit 1
fi

# --- appimagetool ----------------------------------------------------------
TOOL="$WORK/appimagetool"
echo "fetching appimagetool"
curl -fsSL --retry 3 -o "$TOOL" "$APPIMAGETOOL_URL"
chmod +x "$TOOL"
MEASURED="$(sha256sum "$TOOL" | awk '{print $1}')"
if [ -n "${APPIMAGETOOL_SHA256:-}" ]; then
  if [ "$MEASURED" != "$APPIMAGETOOL_SHA256" ]; then
    echo "error: appimagetool sha256 mismatch" >&2
    echo "  expected: $APPIMAGETOOL_SHA256" >&2
    echo "  measured: $MEASURED" >&2
    exit 1
  fi
else
  echo "note: APPIMAGETOOL_SHA256 is unset — measured $MEASURED"
  echo "      pin that value in the release workflow to make this download verifiable"
fi

# --- emit ------------------------------------------------------------------
ASSET="orchicon_desktop_${VERSION}_linux_amd64.AppImage"
export ARCH=x86_64
# `--no-appstream` because the AppStream metadata is optional here and a network
# fetch of appstreamcli during a release build is a failure waiting to happen.
(cd "$WORK" && "$TOOL" --no-appstream "$APPDIR" "$OUT/$ASSET")
chmod +x "$OUT/$ASSET"
echo "built: $OUT/$ASSET"
