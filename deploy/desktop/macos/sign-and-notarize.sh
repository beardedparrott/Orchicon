#!/usr/bin/env bash
# ============================================================================
# Sign, notarise and STAPLE the Orchicon .app, then PROVE Gatekeeper accepts it.
#
# Run only on a macOS runner (macos-14). Every secret is required: a missing one
# fails the job loudly rather than shipping an unsigned app that Gatekeeper will
# silently kill on a user's machine — that is the macOS "gap" this packaging
# exists to close (AC1).
#
# Inputs (env):
#   MACOS_CERT_P12        base64 of the Developer ID Application .p12
#   MACOS_CERT_PASSWORD   its password
#   MACOS_KEYCHAIN_PASS   password for the throwaway keychain (default: random)
#   MACOS_NOTARY_APPLE_ID the Apple ID used for notarytool
#   MACOS_NOTARY_TEAM_ID  the team ID
#   MACOS_NOTARY_PASSWORD an app-specific password (NOT the Apple ID password)
#   APP_PATH              path to the .app (default: desktop/build/bin/Orchicon.app)
#   ORCHICON_VERSION      version tag; names the output zip
# Output:
#   dist/desktop/orchicon_desktop_<ver>_darwin_universal.zip (stapled)
# ============================================================================
set -euo pipefail

: "${MACOS_CERT_P12:?MACOS_CERT_P12 is required — refusing to ship an unsigned .app}"
: "${MACOS_CERT_PASSWORD:?MACOS_CERT_PASSWORD is required}"
: "${MACOS_NOTARY_APPLE_ID:?MACOS_NOTARY_APPLE_ID is required for notarisation}"
: "${MACOS_NOTARY_TEAM_ID:?MACOS_NOTARY_TEAM_ID is required for notarisation}"
: "${MACOS_NOTARY_PASSWORD:?MACOS_NOTARY_PASSWORD is required for notarisation}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
APP_PATH="${APP_PATH:-$ROOT/desktop/build/bin/Orchicon.app}"
VERSION="${ORCHICON_VERSION:-0.0.0-dev}"
VERSION="${VERSION#v}"
OUT="${ROOT}/dist/desktop"
ENTITLEMENTS="$ROOT/deploy/desktop/macos/entitlements.plist"
SIGN_ID="${MACOS_SIGN_IDENTITY:-Developer ID Application}"

[ -d "$APP_PATH" ] || { echo "error: $APP_PATH not found — run \`wails build -platform darwin/universal\` first" >&2; exit 1; }
mkdir -p "$OUT"

# --- keychain --------------------------------------------------------------
# A THROWAWAY keychain, created and deleted inside the job: importing a .p12 into
# the login keychain leaves the certificate on a hosted runner and prompts for
# codesign access. This keychain is unlocked, prepended to the search list, and
# removed by the trap.
KEYCHAIN="$RUNNER_TEMP/orchicon-signing.keychain-db"
KEYCHAIN_PASS="${MACOS_KEYCHAIN_PASS:-$(openssl rand -hex 16)}"
security create-keychain -p "$KEYCHAIN_PASS" "$KEYCHAIN"
security set-keychain-settings -lut 21600 "$KEYCHAIN"
security unlock-keychain -p "$KEYCHAIN_PASS" "$KEYCHAIN"
echo "$MACOS_CERT_P12" | base64 --decode > "$RUNNER_TEMP/cert.p12"
security import "$RUNNER_TEMP/cert.p12" -k "$KEYCHAIN" -P "$MACOS_CERT_PASSWORD" \
  -T /usr/bin/codesign -T /usr/bin/security
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$KEYCHAIN_PASS" "$KEYCHAIN" >/dev/null
security list-keychains -d user -s "$KEYCHAIN" $(security list-keychains -d user | tr -d '"')
trap 'security delete-keychain "$KEYCHAIN" 2>/dev/null || true; rm -f "$RUNNER_TEMP/cert.p12"' EXIT

# --- sign ------------------------------------------------------------------
# Deep + runtime + timestamp, with the JIT entitlements JavaScriptCore needs. The
# nested frameworks are signed by --deep; a Wails app is a single binary plus its
# bundle resources, so there is no deeper nesting to worry about.
echo "signing $APP_PATH"
codesign --force --deep --options runtime --timestamp \
  --entitlements "$ENTITLEMENTS" --sign "$SIGN_ID" "$APP_PATH"
codesign --verify --strict --verbose=2 "$APP_PATH"

# --- notarise --------------------------------------------------------------
# notarytool --wait BLOCKS until Apple answers: "Invalid" fails the job here, and
# "Accepted" is the only path to the staple below.
ZIP="$OUT/orchicon_desktop_${VERSION}_darwin_universal.zip"
ditto -c -k --keepParent "$APP_PATH" "$ZIP"
echo "submitting $ZIP for notarisation (this takes a few minutes)"
xcrun notarytool submit "$ZIP" \
  --apple-id "$MACOS_NOTARY_APPLE_ID" \
  --team-id "$MACOS_NOTARY_TEAM_ID" \
  --password "$MACOS_NOTARY_PASSWORD" \
  --wait

# --- staple ----------------------------------------------------------------
# THE STAPLE IS THE WHOLE POINT: it attaches the ticket to the bundle, so a user
# on a machine that is offline (or behind a filtering proxy that cannot reach
# Apple) still passes Gatekeeper. A notarised-but-unstapled app only works while
# Apple is reachable at first launch.
xcrun stapler staple "$APP_PATH"
xcrun stapler validate "$APP_PATH"

# Re-zip AFTER stapling: the notarisation upload consumed the pre-staple zip.
rm -f "$ZIP"
ditto -c -k --keepParent "$APP_PATH" "$ZIP"

# --- assert Gatekeeper accepts it ------------------------------------------
# `spctl` is the same check Gatekeeper runs on the user's machine. If it fails,
# the artifact is not ship-able and the job must fail: an unsigned .app reaching
# a release is exactly AC1's failure mode.
echo "verifying Gatekeeper assessment"
if ! spctl -a -vvv -t exec "$APP_PATH"; then
  echo "error: Gatekeeper assessment FAILED — the .app would be blocked on a clean Mac" >&2
  exit 1
fi
# Belt and braces: the signature must be the Developer ID one, not ad-hoc.
codesign -dv --verbose=4 "$APP_PATH" 2>&1 | grep -q "Developer ID Application" || {
  echo "error: the .app is not signed with a Developer ID Application certificate" >&2
  exit 1
}

echo "signed, notarised, stapled: $ZIP"
