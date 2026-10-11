#!/usr/bin/env bash
# ============================================================================
# install-path/run.sh — END-TO-END EXERCISE OF THE REAL INSTALL PATH.
#
# WHY THIS EXISTS. The installers are the one artifact with no test anywhere near the real code path.
# Everything else here is covered — unit tests, a cross-compile gate, a Windows harness for the
# installer's offline behaviour — but nothing ever ran `scripts/install.sh` against an actual artifact.
# So the only way an installer fault was discovered was by a user running it, which is how a broken
# installer reached two published releases in a row. The operator: "Pushing to release a broken
# installer is just not a good look... It would be nice to have a standardized method to test this."
#
# WHAT IT DOES, in order, using the SHIPPED script rather than a reimplementation of it:
#   1. builds both binaries and packages them into the release's exact asset shape
#      (`orchicon_<ver>_linux_<arch>.tar.gz` containing a directory of the same name);
#   2. serves that artifact over a local HTTP server, laid out as
#      `<base>/<version>/<asset>` — the shape the installer builds URLs from;
#   3. runs the REAL scripts/install.sh against it via ORCHICON_INSTALL_BASE_URL, with --no-setup
#      (no Docker, no images, no daemon — the download/extract/install/verify path only);
#   4. asserts the binary landed where it was asked to, that it RUNS, and that it reports the version
#      the artifact was built as — which is what proves the installer fetched and installed THE
#      artifact, not something else.
#
# NO NETWORK, NO DOCKER, NO RELEASE. That is the point: it can run on every PR in a few seconds, so the
# install path stops being something only a user exercises.
#
# SCOPE, stated rather than implied: this covers the POSIX installer (install.sh — Linux and macOS).
# install.ps1's Windows path needs WSL2 and cannot be exercised here; its offline behaviour is covered
# by scripts/tests/install-ps1-wsl-encoding, and a real end-to-end run needs a Windows runner.
#
#   scripts/tests/install-path/run.sh [--keep]
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
INSTALLER="${REPO_ROOT}/scripts/install.sh"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

PASS=0
FAIL=0
say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$*"; PASS=$((PASS + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
check(){ if [ "$2" = "1" ]; then ok "$1"; else bad "$1"; fi; }

WORK="$(mktemp -d)"
cleanup() {
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null || true
  [ -n "${SRV_PID:-}" ] && wait "$SRV_PID" 2>/dev/null || true
  [ "${KEEP}" = "1" ] || rm -rf "${WORK}"
}
trap cleanup EXIT

# A version that cannot be mistaken for a release: the assertions below read it back out of the
# installed binary, so a wrong or defaulted version is caught rather than assumed.
VERSION="v0.0.0-installpath"
VERSION_NUM="${VERSION#v}"
ARCH="$(uname -m)"; case "${ARCH}" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
ASSET="orchicon_${VERSION_NUM}_linux_${ARCH}"
ASSET_DIR="orchicon_${VERSION_NUM}_linux_${ARCH}"

say "install-path test"
say "  installer : ${INSTALLER}"
say "  asset     : ${ASSET}.tar.gz  (linux/${ARCH})"
say

# --- 1. build + package the artifact exactly as the release does -------------------
say "[1] building and packaging the artifact"
STAGE="${WORK}/stage"
mkdir -p "${STAGE}"
if ! ( cd "${REPO_ROOT}" && go build -ldflags "-X github.com/beardedparrott/orchicon/internal/version.gitTag=${VERSION}" \
        -o "${STAGE}/orchicon" ./cmd/orchicon ) 2>"${WORK}/build.log"; then
  bad "the control-plane binary builds"
  sed 's/^/      /' "${WORK}/build.log" | tail -12
  exit 1
fi
( cd "${REPO_ROOT}" && go build -ldflags "-X github.com/beardedparrott/orchicon/internal/version.gitTag=${VERSION}" \
    -o "${STAGE}/orch" ./cmd/orch ) 2>>"${WORK}/build.log" || true
ok "the binaries build"

# release.yml's shape: a directory of the asset's own name, containing both binaries.
PKG="${WORK}/pkg/${VERSION}"
mkdir -p "${PKG}/${ASSET_DIR}"
cp "${STAGE}/orchicon" "${PKG}/${ASSET_DIR}/"
[ -f "${STAGE}/orch" ] && cp "${STAGE}/orch" "${PKG}/${ASSET_DIR}/"
( cd "${PKG}" && tar -czf "${ASSET}.tar.gz" "${ASSET_DIR}" )
SIZE="$(wc -c < "${PKG}/${ASSET}.tar.gz")"
check "the artifact is packaged in the release's shape (${SIZE} bytes)" \
  "$([ "${SIZE}" -gt 1000000 ] && echo 1 || echo 0)"

# --- 2. serve it at <base>/<version>/<asset> --------------------------------------
say ""
say "[2] serving it over HTTP"
( cd "${WORK}/pkg" && exec python3 -m http.server 0 --bind 127.0.0.1 >"${WORK}/srv.log" 2>&1 ) &
SRV_PID=$!
PORT=""
for _ in $(seq 1 50); do
  PORT="$(sed -n 's/.*port \([0-9]\{2,\}\).*/\1/p' "${WORK}/srv.log" | head -1)"
  [ -n "${PORT}" ] && break
  sleep 0.1
done
if [ -z "${PORT}" ]; then
  # Older/newer http.server builds announce differently; fall back to a fixed port.
  kill "${SRV_PID}" 2>/dev/null || true
  PORT=8721
  ( cd "${WORK}/pkg" && exec python3 -m http.server "${PORT}" --bind 127.0.0.1 >"${WORK}/srv.log" 2>&1 ) &
  SRV_PID=$!
  sleep 1
fi
BASE="http://127.0.0.1:${PORT}"
if curl -fsS "${BASE}/${VERSION}/${ASSET}.tar.gz" -o /dev/null 2>/dev/null; then
  ok "the artifact is reachable at ${BASE}/${VERSION}/${ASSET}.tar.gz"
else
  bad "the artifact is reachable over HTTP (port ${PORT})"
  sed 's/^/      /' "${WORK}/srv.log" | head -5
  exit 1
fi

# --- 3. run the REAL installer against it ------------------------------------------
say ""
say "[3] running the real installer against it"
DEST="${WORK}/bin"
LOG="${WORK}/install.log"
set +e
# --instance test, because the installer now ASKS which instance (it has a tty here, so an unattended run
# would otherwise block on the question). Passing it is the documented way to skip the prompt.
ORCHICON_INSTALL_BASE_URL="${BASE}" bash "${INSTALLER}" \
  --version "${VERSION}" --install-dir "${DEST}" --instance test --no-setup >"${LOG}" 2>&1
RC=$?
set -e
if [ "${RC}" -eq 0 ]; then
  ok "install.sh exits 0 (--no-setup, so no Docker is involved)"
else
  bad "install.sh exits 0 (got ${RC})"
  sed 's/^/      /' "${LOG}" | tail -20
fi
# It must have gone through the URL we served, not the default base.
if grep -q "${BASE}/${VERSION}/${ASSET}.tar.gz" "${LOG}"; then
  ok "it downloaded from the served URL (the base override is honoured)"
else
  bad "it downloaded from the served URL"
  grep -i 'download' "${LOG}" | sed 's/^/      /' | head -3
fi

# --- 4. the installed thing must work, and be the artifact we served ---------------
say ""
say "[4] the installed binary"
if [ -x "${DEST}/orchicon" ]; then
  ok "orchicon was installed to the requested directory"
else
  bad "orchicon was installed to the requested directory"
  ls -la "${DEST}" 2>/dev/null | sed 's/^/      /' | head -5
fi
GOT="$("${DEST}/orchicon" version 2>/dev/null | head -1 || true)"
case "${GOT}" in
  *"${VERSION}"*) ok "it runs and reports the version it was built as (${VERSION})" ;;
  *) bad "it runs and reports ${VERSION} — got \"${GOT}\"" ;;
esac
# The companion client rides the same archive; its absence is a packaging regression, not a spec.
if [ -x "${DEST}/orch" ]; then
  ok "the orch companion client was installed alongside it"
else
  bad "the orch companion client was installed alongside it"
fi

# --- 5. --instance must scope the acting, and refuse an unknown name --------------
# The shell installer had no instance flag at all, so `--clean` and `--force-clean` spelled the names out
# as `orchicon-cnt-dev orchicon-cnt-prod` — they removed BOTH containers and destroyed BOTH data volumes
# while the help text said they were about dev. `ORCHICON_INSTANCE` existed but cannot be used with the
# documented `curl … | bash -s --` form at all: an exported variable reaches CURL, not BASH.
say ""
say "[5] --instance scoping"

if bash "${INSTALLER}" --help 2>&1 | grep -q -- "--instance"; then
  ok "--help documents --instance"
else
  bad "--help documents --instance"
fi
if bash "${INSTALLER}" --help 2>&1 | grep -q 'orchicon install'; then
  ok "--help prints its text literally (backticks are not executed)"
else
  bad "--help prints its text literally — an unquoted heredoc would run backticked help as commands"
fi

# An unknown instance must be REFUSED, not defaulted: silently becoming dev would clean up dev while
# the operator watched prod.
set +e
OUT="$(bash "${INSTALLER}" --instance production --no-setup --dry-run 2>&1)"
RC=$?
set -e
if [ "${RC}" -ne 0 ] && printf '%s' "${OUT}" | grep -q "unknown instance"; then
  ok "an unknown instance is refused, naming the known set"
else
  bad "an unknown instance is refused (rc=${RC}, said: $(printf '%s' "${OUT}" | head -1))"
fi
if printf '%s' "${OUT}" | grep -q "dev, test, prod"; then
  ok "the refusal names the valid choices"
else
  bad "the refusal names the valid choices"
fi

# And the instance must actually scope the acting: a dry-run force-clean for `test` may name ONLY test.
OUT="$(bash "${INSTALLER}" --force-clean --instance test --dry-run 2>&1 || true)"
if printf '%s' "${OUT}" | grep -q "orchicon-cnt-test" && ! printf '%s' "${OUT}" | grep -qE "orchicon-cnt-(dev|prod)"; then
  ok "--instance test scopes the acting to the test container alone"
else
  bad "--instance test scopes the acting (named: $(printf '%s' "${OUT}" | grep -o 'orchicon-cnt-[a-z]*' | sort -u | tr '\n' ' '))"
fi

# --- 6. INSTALL ASKS WHICH INSTANCE (detached, so the no-terminal path is the one exercised)
say ""
say "[6] the installer asks which instance"

# DETACHED: `setsid` gives the child no controlling terminal, which is the state the no-terminal path is
# written for. With a terminal present the installer correctly WAITS at the prompt — a plain run here would
# hang the harness (measured), which is why the main invocation above passes --instance.
detached() {
  # ORCHICON_INSTALL_BASE_URL, or the installer resolves "latest" from the real GitHub API and downloads
  # from the internet — which is both slow and not what this harness is for.
  env -u ORCHICON_INSTANCE ORCHICON_INSTALL_BASE_URL="${BASE}" setsid bash "${INSTALLER}" \
    --version "${VERSION}" --install-dir "${DEST}-d" "$@" < /dev/null 2>&1
}
OUT="$(detached --no-setup)"
if printf '%s' "${OUT}" | grep -q "using 'dev'"; then
  ok "with no terminal it takes the documented default (dev) rather than blocking"
else
  bad "with no terminal it takes the documented default — got: $(printf '%s' "${OUT}" | head -2 | tr '\n' ' ')"
fi
OUT="$(detached --instance test --no-setup)"
if printf '%s' "${OUT}" | grep -q "Installed"; then
  ok "--instance test is honoured without a prompt"
else
  bad "--instance test is honoured (got: $(printf '%s' "${OUT}" | head -2 | tr '\n' ' '))"
fi
if printf '%s' "${OUT}" | grep -qi "Which instance"; then
  bad "--instance test still prompted — the flag must skip the question"
else
  ok "and it does not prompt when the instance is given"
fi
# The destructive verbs must NOT have a fallback: verified in the uninstall harness, which owns them.
if bash "${INSTALLER}" --help 2>&1 | grep -q -- "--instance"; then
  ok "--help documents --instance"
else
  bad "--help documents --instance"
fi

# --- 7. THE INSTALLER MUST STAY EXECUTABLE -----------------------------------
# Not cosmetic: `make install-dry-run` and `make install-uninstall` run `scripts/install.sh` directly, so a
# lost executable bit breaks them with "Permission denied" (exit 126) — which is exactly what happened when
# a scripted edit rewrote the file through a temporary one and the mode went with it. A git mode change is
# invisible in a diff of the CONTENT, so it needs its own detector.
say ""
say "[7] the installer is executable"

if [ -x "${INSTALLER}" ]; then
  ok "scripts/install.sh is executable (the make targets run it directly)"
else
  bad "scripts/install.sh lost its executable bit — 'make install-dry-run' will fail with Permission denied"
fi
# And git must AGREE, or a fresh clone is broken even where this working tree is fine.
if git -C "${REPO_ROOT}" ls-files -s scripts/install.sh 2>/dev/null | grep -q '^100755'; then
  ok "and git records it as 100755 (so a fresh clone is correct too)"
else
  bad "git records scripts/install.sh as non-executable — a fresh clone would break the make targets"
fi

say ""
if [ "${FAIL}" -eq 0 ]; then
  say "RESULT: all ${PASS} assertion(s) passed"
  exit 0
fi
say "RESULT: ${FAIL} assertion(s) FAILED (passing: ${PASS})"
[ "${KEEP}" = "1" ] && say "  work dir kept: ${WORK}"
exit 1
