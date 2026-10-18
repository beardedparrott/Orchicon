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
if [ "${RC}" -ne 0 ] && grep -q "unknown instance" <<<"${OUT}"; then
  ok "an unknown instance is refused, naming the known set"
else
  bad "an unknown instance is refused (rc=${RC}, said: $(printf '%s' "${OUT}" | head -1))"
fi
if grep -q "dev, test, prod" <<<"${OUT}"; then
  ok "the refusal names the valid choices"
else
  bad "the refusal names the valid choices"
fi

# And the instance must actually scope the acting: a dry-run force-clean for `test` may name ONLY test.
OUT="$(bash "${INSTALLER}" --force-clean --instance test --dry-run 2>&1 || true)"
if grep -q "orchicon-cnt-test" <<<"${OUT}" && ! grep -qE "orchicon-cnt-(dev|prod)" <<<"${OUT}"; then
  ok "--instance test scopes the acting to the test container alone"
else
  bad "--instance test scopes the acting (named: $(grep -o 'orchicon-cnt-[a-z]*' <<<"${OUT}" | sort -u | tr '\n' ' '))"
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
if grep -q "using 'dev'" <<<"${OUT}"; then
  ok "with no terminal it takes the documented default (dev) rather than blocking"
else
  bad "with no terminal it takes the documented default — got: $(printf '%s' "${OUT}" | head -2 | tr '\n' ' ')"
fi
OUT="$(detached --instance test --no-setup)"
if grep -q "Installed" <<<"${OUT}"; then
  ok "--instance test is honoured without a prompt"
else
  bad "--instance test is honoured (got: $(printf '%s' "${OUT}" | head -2 | tr '\n' ' '))"
fi
if grep -qi "Which instance" <<<"${OUT}"; then
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

# THE WHOLE CLASS, NOT ONE FILE. Every scripts/tests/*/run.sh is executable by convention, plus
# scripts/install.sh — and a scripted edit that rewrites a file through a temp one silently drops the mode,
# which is invisible in a diff of the content. That happened twice: to install.sh (breaking `make
# install-dry-run`), and then to this harness's own run.sh. So the check ENUMERATES the set: a new harness is
# covered the moment it exists, and a script outside the convention is a deliberate choice rather than drift.
mode_suspects="scripts/install.sh"
for d in "${REPO_ROOT}"/scripts/tests/*/run.sh; do
  [ -f "$d" ] || continue
  mode_suspects="$mode_suspects ${d#"${REPO_ROOT}/"}"
done
for f in $mode_suspects; do
  if [ ! -x "${REPO_ROOT}/$f" ]; then
    bad "$f is not executable — the convention is +x for every harness and the installer"
  elif ! git -C "${REPO_ROOT}" ls-files -s "$f" 2>/dev/null | grep -q '^100755'; then
    bad "git records $f as non-executable — a fresh clone would differ from this working tree"
  else
    ok "$f is executable, and git agrees"
  fi
done

# --- 8. THE STACK MUST COME FROM THE VERSION BEING INSTALLED -----------------
# Without this the instance ran whatever `:latest` the host already had, so a canary install put a canary
# BINARY over the last RELEASED image — and a host that already held `:latest` could never be brought
# forward. Asserted at the source because the assertion is about WHICH REF IS NAMED, and no amount of
# running the installer against a local artifact can observe that.
say ""
say "[8] the install names the version's container image"

INSTALLER_SRC="$(cat "${INSTALLER}")"
if grep -q 'ORCHICON_CONTAINER_IMAGE="\$image_ref"' <<<"${INSTALLER_SRC}"; then
  ok "the setup step passes ORCHICON_CONTAINER_IMAGE"
else
  bad "the setup step does not name the container image — the stack would come from whatever :latest the host has"
fi
if grep -q 'image_ref="ghcr.io/' <<<"${INSTALLER_SRC}"; then
  ok "and the ref is derived from the version, not hardcoded"
else
  bad "the image ref is not derived from the version"
fi
# ghcr requires a lowercase repository name, and GITHUB_REPO is "Orchicon".
if grep -q 'tr "\[:upper:\]" "\[:lower:\]"' <<<"${INSTALLER_SRC}"; then
  ok "and it is lowercased (ghcr rejects uppercase)"
else
  bad "the image ref is not lowercased — ghcr would reject 'Orchicon'"
fi

# --- 9. THE INFRASTRUCTURE BINARIES HAVE ONE RESOLVER, AND IT RECORDS --------------
# There is no image carrying postgres/nats any more, so the installer either finds them
# on the host or downloads them. Both must go through ONE mechanism that RECORDS what it
# resolved: a second resolution path (or a service proc falling back to a bare name on
# PATH) would let the installer report one postgres while the services run another, and
# a downloaded binary would be invisible to a proc that only knew the name. Asserted at
# the source because the assertion is about WHICH CALL PATH exists, which no run of the
# installer against a local artifact can observe.
say ""
say "[9] the infrastructure binaries have one resolver, and it records"

BINARIES_SRC_FILE="${REPO_ROOT}/cmd/orchicon/binaries.go"
if [ -f "${BINARIES_SRC_FILE}" ]; then
  ok "cmd/orchicon/binaries.go exists (the single acquisition mechanism)"
else
  bad "cmd/orchicon/binaries.go is missing — postgres/nats would come from PATH only"
fi
BINARIES_SRC="$(cat "${BINARIES_SRC_FILE}" 2>/dev/null || true)"
if grep -q 'func acquireInfraBinaries(' <<<"${BINARIES_SRC}"; then
  ok "it exposes one acquisition entry point (acquireInfraBinaries)"
else
  bad "acquireInfraBinaries is missing — components would resolve the binaries independently"
fi
if grep -q 'infra-binaries.json' <<<"${BINARIES_SRC}"; then
  ok "it records what it resolved"
else
  bad "the resolver does not record its result — a second component would re-derive it and could disagree"
fi
if grep -q 'checksum mismatch' <<<"${BINARIES_SRC}"; then
  ok "a checksum mismatch is a hard failure"
else
  bad "no checksum-mismatch refusal — an unverified download could be used"
fi
if grep -q 'minPostgresMajor' <<<"${BINARIES_SRC}"; then
  ok "a detected PostgreSQL is version-checked against a documented minimum"
else
  bad "no minimum PostgreSQL version — a too-old detected server would be attempted"
fi

# The CONSUMERS must read the record, not a bare name. A service proc that still
CONTAINER_SRC="$(cat "${REPO_ROOT}/cmd/orchicon/container.go")"
if grep -q 's.resolved().Postgres' <<<"${CONTAINER_SRC}" \
   && grep -q 's.resolved().Initdb' <<<"${CONTAINER_SRC}" \
   && grep -q 's.resolved().Createdb' <<<"${CONTAINER_SRC}" \
   && grep -q 's.resolved().NatsServer' <<<"${CONTAINER_SRC}"; then
  ok "every service proc execs the RESOLVED path"
else
  bad "a service proc does not use the resolved path — a DOWNLOADED binary would be invisible to it"
fi
# The EXACT invocation literals the bare-name version used. Checking the invocation
# (not the word) matters: "postgres" also appears as a log-field key and a subdir name,
# and matching those would be a false failure that teaches the operator to ignore this
# check.
bare=""
for literal in '"initdb", "-D"' '"postgres", "-D"' 'exec.CommandContext(ctx, "createdb"' 'command: "nats-server"'; do
  if grep -qF "${literal}" <<<"${CONTAINER_SRC}"; then
    bare="${bare}
  ${literal}"
  fi
done
if [ -z "${bare}" ]; then
  ok "no bare infrastructure-binary name is exec'd (the resolved paths replaced every one)"
else
  bad "a bare infrastructure-binary invocation survives — a DOWNLOADED binary would be invisible to it:${bare}"
fi

# THE RESOLVED BIN DIR MUST REACH THE INSTANCE. A host-side resolution is only
# THE RESOLVED BINARIES ARE NEVER RE-DOWNLOADED, and the mount that used to guarantee it is gone.
#
# The bind mount existed because the SERVICES used to run in a container: a binary downloaded under the
# instance's host state dir is not on a container's filesystem, so without the mount the supervisor would
# resolve (and download) a second copy. The services are now HOST processes of the invoking user, so there
# is no container to mount into and nothing to make visible — the processes exec the resolved ABSOLUTE
# paths directly, which this suite asserts separately ("every service proc execs the RESOLVED path").
#
# So the property is now: install still RESOLVES the binaries (it must not hand a name to PATH), and it
# still points the host services at the instance's own state dir. Asserted as a source-level pin because it
# is about which arguments are constructed.
INSTALL_GO_SRC="$(cat "${REPO_ROOT}/cmd/orchicon/install.go")"
if grep -qF 'startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout)' <<<"${INSTALL_GO_SRC}"; then
  ok "the host services are started against the instance's own state dir"
else
  bad "the host services are not started from the instance's state dir — a DOWNLOADED binary would be invisible to them"
fi
if grep -qF 'binDir("/var/lib/orchicon")' <<<"${INSTALL_GO_SRC}"; then
  bad "install.go still constructs a container MOUNT for the infra binaries — the instance no longer runs in a container"
else
  ok "no container mount is constructed for the infra binaries (the instance is host processes)"
fi

# Documentation of the minimum version, because it is a support promise.
if grep -q 'Minimum supported PostgreSQL' "${REPO_ROOT}/ARCHITECTURE.md"; then
  ok "ARCHITECTURE.md documents the minimum supported PostgreSQL"
else
  bad "the minimum supported PostgreSQL version is not documented"
fi

say ""
say "[10] the desktop app (--desktop)"

# THE DESKTOP APP IS OPT-IN, SO BOTH HALVES NEED PROVING: with --desktop the app must actually land where
# this script already writes, and WITHOUT it the user must be told where to get it rather than left to
# find it. The asset is served from the same fake release as the CLI archive, so the URL shape the
# installer builds is exercised for real (no network, no real release).
if [ "${ARCH}" = "amd64" ]; then
  DESKTOP_ASSET="orchicon_desktop_${VERSION_NUM}_linux_${ARCH}.AppImage"
  # A stand-in the installer only has to download, chmod and place: the real artifact is a Wails build.
  printf '#!/bin/sh\necho fake-appimage\n' > "${PKG}/${VERSION}/${DESKTOP_ASSET}"
  chmod +x "${PKG}/${VERSION}/${DESKTOP_ASSET}"

  DDEST="${WORK}/bin-desktop"
  DLOG="${WORK}/install-desktop.log"
  set +e
  ORCHICON_INSTALL_BASE_URL="${BASE}" bash "${INSTALLER}" \
    --version "${VERSION}" --install-dir "${DDEST}" --instance test --no-setup --desktop >"${DLOG}" 2>&1
  DRC=$?
  set -e
  check "install.sh --desktop exits 0 (got ${DRC})" "$([ "${DRC}" -eq 0 ] && echo 1 || echo 0)"
  [ "${DRC}" -ne 0 ] && sed 's/^/      /' "${DLOG}" | tail -12
  if grep -q "${BASE}/${VERSION}/${DESKTOP_ASSET}" "${DLOG}"; then
    ok "it downloaded the desktop app from the served release (the published asset name is the contract)"
  else
    bad "it downloaded the desktop app from the served release"
    grep -i 'desktop' "${DLOG}" | sed 's/^/      /' | head -4
  fi
  if [ -x "${DDEST}/orchicon-desktop" ]; then
    ok "the desktop app landed in the install dir, executable (no sudo, no /usr/local)"
  else
    bad "the desktop app landed in the install dir, executable"
    ls -la "${DDEST}" 2>/dev/null | sed 's/^/      /' | head -5
  fi
else
  say "  SKIP  linux/${ARCH}: the desktop AppImage is amd64-only (documented; install.sh says so)"
fi

# WITHOUT the flag the install must NAME where the app comes from — that line is AC5.
NLOG="${WORK}/install-nodesktop.log"
ORCHICON_INSTALL_BASE_URL="${BASE}" bash "${INSTALLER}" \
  --version "${VERSION}" --install-dir "${WORK}/bin-nodesktop" --instance test --no-setup >"${NLOG}" 2>&1 || true
if grep -q 'Desktop app (optional)' "${NLOG}"; then
  ok "without --desktop it still says how to get the app (nobody is left to find it)"
else
  bad "without --desktop it names where to get the app"
fi

# AND THE QUARANTINE RULE, at the source: a released .app that Gatekeeper rejects is a release bug, so the
# installer ASSERTS with spctl and reports it — it never strips the flag to make the warning go away.
INSTALLER_FOR_DESKTOP="$(cat "${INSTALLER}")"
if grep -q 'spctl -a -t exec' <<<"${INSTALLER_FOR_DESKTOP}"; then
  ok "install.sh asserts Gatekeeper acceptance with spctl"
else
  bad "install.sh does not assert Gatekeeper acceptance — a blocked .app would look installed and then not start"
fi
if grep -v '^\s*#' <<<"${INSTALLER_FOR_DESKTOP}" | grep -q 'xattr -d'; then
  bad "install.sh STRIPS the quarantine flag — that hides a bad release instead of reporting it"
else
  ok "install.sh never strips the quarantine flag (comment-only mentions do not count)"
fi

say ""
if [ "${FAIL}" -eq 0 ]; then
  say "RESULT: all ${PASS} assertion(s) passed"
  exit 0
fi
say "RESULT: ${FAIL} assertion(s) FAILED (passing: ${PASS})"
[ "${KEEP}" = "1" ] && say "  work dir kept: ${WORK}"
exit 1
