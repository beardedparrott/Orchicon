#!/usr/bin/env bash
# wix-check.sh — validate the Windows MSI's WiX sources WITHOUT a 5-minute CI round.
#
# WHY THIS EXISTS. Every fault in the wizard work so far was an XML schema error — WIX0048,
# WIX0001, WIX0004 twice, WIX0400 — and each cost a `windows-latest` round trip to find,
# because WiX is a .NET tool and this project had no dotnet. It does now, under .dev/tools.
#
# WHAT IT CHECKS: the COMPILE phase, i.e. every schema rule about every element and attribute.
# That is the whole class of error this exists for.
#
# WHAT IT CANNOT CHECK: binding. On Linux WiX rejects every Directory/@Name as "not a relative
# path" (WIX0389) — including in a minimal MSI — so no package can be produced. Cross-reference
# questions (does that dialog ID exist? does the sequence resolve?) still need windows-latest.
#
#     scripts/dev/wix-check.sh
#
# Exits non-zero only on an error that is NOT one of the known Linux artifacts, so it can be
# used as a pre-push check.

set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOTNET_ROOT="${DOTNET_ROOT:-$REPO/.dev/tools/dotnet}"
export DOTNET_ROOT
export PATH="$DOTNET_ROOT:$HOME/.dotnet/tools:$PATH"
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1

if ! command -v dotnet >/dev/null 2>&1; then
  cat >&2 <<'MSG'
error: dotnet was not found.

  WiX is a .NET global tool, so it needs a runtime. Install both under .dev/tools:

    curl -fsSL -o /tmp/dotnet-install.sh https://dot.net/v1/dotnet-install.sh
    bash /tmp/dotnet-install.sh --channel 9.0 --install-dir "$PWD/.dev/tools/dotnet" --no-path
    DOTNET_ROOT="$PWD/.dev/tools/dotnet" PATH="$PWD/.dev/tools/dotnet:$PATH" \
      dotnet tool install --global wix --version 5.0.2
MSG
  exit 2
fi
if ! command -v wix >/dev/null 2>&1; then
  echo "error: the wix CLI was not found. Install the PINNED version:" >&2
  echo "  dotnet tool install --global wix --version 5.0.2" >&2
  exit 2
fi

# The pinned UI extension has to be present before -ext can name it (WiX's own store, not NuGet).
wix extension add -g WixToolset.UI.wixext/5.0.2 >/dev/null 2>&1 || true

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Stand-ins for the binaries: the compile phase only needs the PATHS to resolve, not the bytes.
mkdir -p "$WORK/src"
for f in orchicon.exe orch.exe orchicon-desktop.exe README-WINDOWS.txt \
         LICENSE.rtf orchicon-desktop.ico wizard-banner.bmp wizard-dialog.bmp; do
  : > "$WORK/src/$f"
done

SRC="$REPO/deploy/desktop/windows"
OUT="$WORK/out.msi"
LOG="$WORK/build.log"

wix build \
  "$SRC/Orchicon.wxs" \
  -arch x64 -ext WixToolset.UI.wixext \
  -d "Version=0.0.0" -d "SourceDir=$WORK/src" \
  -bindvariable "WixUILicenseRtf=$WORK/src/LICENSE.rtf" \
  -o "$OUT" >"$LOG" 2>&1 || true

# AN EMPTY LOG IS NOT A PASS. It means WiX never ran (a broken invocation, a missing tool), and
# reporting "OK" there is worse than failing — this check exists to remove blind rounds, not add
# them. (Found by its own negative control: a stray command prefix made WiX never execute and
# the script cheerfully printed OK.)
if ! grep -qE 'error WIX|Warning|warning WIX' "$LOG" && [ ! -s "$LOG" ]; then
  echo "FAIL — wix produced no output at all, so nothing was checked:" >&2
  sed 's/^/  /' "$LOG" >&2
  exit 1
fi

# KNOWN LINUX ARTIFACTS. Both appear for develop too, which builds cleanly on Windows — so
# neither says anything about these sources:
#   WIX0389  Directory/@Name "is not a relative path" — every directory, including a minimal MSI
#   WIX0027  a Windows '\' separator read as an illegal filename character
NEW="$(grep -oE 'error WIX[0-9]+' "$LOG" \
        | grep -vE 'WIX0389|WIX0027' | sort | uniq -c || true)"

if [ -n "$NEW" ]; then
  echo "FAIL — schema errors that are NOT Linux artifacts:" >&2
  grep -E "error WIX" "$LOG" | grep -vE 'WIX0389|WIX0027' | sed 's/^/  /' >&2
  exit 1
fi

echo "OK — no schema errors (the only reports were the known Linux path artifacts:"
echo "     WIX0389 for Directory/@Name and WIX0027 for '\\', both of which develop shows too)."
echo "     Binding still needs windows-latest; this checks the COMPILE phase only."
