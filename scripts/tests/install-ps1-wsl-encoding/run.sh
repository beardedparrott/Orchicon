#!/usr/bin/env bash
# ============================================================================
# Offline harness for scripts/install.ps1's wsl.exe handling.
#
# WHY THIS EXISTS: the Windows installer talks to wsl.exe, and wsl.exe's
# captured output encoding is the source of two bugs that cost a release to
# find — a distro name arriving with NUL bytes between its characters (which
# then truncates at the first NUL when handed back to `wsl -d <name>`, and is
# misreported as a missing Docker), and `$VARS` in a script argument being
# expanded by the distro's default shell before the target bash sees them
# (`tar (child): : Cannot open`). Neither is visible without Windows, so this
# harness makes them visible without Windows: it runs the installer's OWN
# functions (AST-extracted, the installer is never executed) against a wsl.exe
# double.
#
# WHAT IS REAL HERE: the installer's functions, the file on disk, PowerShell's
# decoding of captured native output, the argv/marshal difference between
# `wsl -e` and `wsl --`, and $LASTEXITCODE propagation.
# WHAT IS EMULATED: wsl.exe itself. The double reproduces the DOCUMENTED
# behaviours the installer depends on — UTF-16LE on a redirected stream,
# WSL_UTF8=1 switching it to UTF-8, `--` re-parsing the command text through
# the distro's default shell, and a localized failure for an unknown distro.
# It is not a byte-exact wsl.exe clone; it is a contract, and the contract is
# what the installer is written against.
#
# REQUIRES: pwsh (PowerShell 7+) and iconv.
#
#   scripts/tests/install-ps1-wsl-encoding/run.sh [installer.ps1]
#   scripts/tests/install-ps1-wsl-encoding/run.sh --baseline <other.ps1>
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
INSTALLER="${REPO_ROOT}/scripts/install.ps1"
BASELINE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) BASELINE="${2:?--baseline needs a path}"; shift 2;;
    -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0;;
    *) INSTALLER="$1"; shift;;
  esac
done

PWSH="${PWSH:-$(command -v pwsh || true)}"
if [ -z "${PWSH}" ]; then
  echo "run.sh: pwsh (PowerShell 7+) is required but was not found on PATH" >&2
  echo "        install it, or set PWSH=/path/to/pwsh" >&2
  exit 2
fi
command -v iconv >/dev/null || { echo "run.sh: iconv is required" >&2; exit 2; }

STUB="$(mktemp -d)"
trap 'rm -rf "${STUB}" 2>/dev/null || true' EXIT   # never clobber the exit code
mkdir -p "${STUB}/bin"

# --- wsl.exe double ---------------------------------------------------------
cat > "${STUB}/wsl.src" <<'WSL_DOUBLE'
#!/usr/bin/env bash
set -u
KNOWN="Ubuntu"
LOCALIZED_NO_SUCH="没有名为 '%s' 的发行版。"

emit() {
  if [ "${WSL_UTF8:-}" = "1" ]; then printf '%s\n' "$1"
  else printf '%s\n' "$1" | iconv -f UTF-8 -t UTF-16LE; fi
}

expand_arg() {
  printf '%s' "$1" | perl -pe 's/\$\{?(\w+)\}?/defined $ENV{$1} ? $ENV{$1} : ""/ge'
}

distro=""
listmode=""
execmode=""
skip=0
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
  a="${args[$i]}"
  case "$a" in
    -d|--distribution) i=$((i+1)); distro="${args[$i]}";;
    -e|--exec) execmode="argv"; skip=$((i+2)); break;;
    --) execmode="shell"; skip=$((i+2)); break;;
    --list|-l) listmode="list";;
    --quiet|-q) listmode="${listmode}:quiet";;
    --verbose|-v) listmode="${listmode}:verbose";;
    --set-default-version) exit 0;;
  esac
  i=$((i+1))
done

if [ -n "$listmode" ]; then
  case "$listmode" in
    *quiet*) emit "$KNOWN"; exit 0;;
    *verbose*) if [ "${EMU_NO_DEFAULT:-}" = "1" ]; then emit "    $KNOWN                 Running         2"
               else emit "  * $KNOWN                 Running         2"; fi; exit 0;;
  esac
  exit 0
fi

if [ -n "$execmode" ]; then
  if [ -n "$distro" ] && [ "$distro" != "$KNOWN" ]; then
    emit "$(printf "$LOCALIZED_NO_SUCH" "$distro")" >&2
    exit 1
  fi
  # `bash -lc` re-sources /etc/profile and resets PATH, so a docker stand-in
  # travels as an exported function (a child bash imports it from the env).
  docker() { case "$*" in *--format*) echo "29.8.0";; *) echo "Docker version 29.8.0";; esac; }
  export -f docker
  cmd=("${@:$skip}")
  if [ "$execmode" = "argv" ]; then
    "${cmd[@]}"
  else
    last=$(( ${#cmd[@]} - 1 ))
    cmd[$last]="$(expand_arg "${cmd[$last]}")"
    "${cmd[@]}"
  fi
  exit $?
fi
exit 0
WSL_DOUBLE
# install(1), not cp+chmod: it writes the file and its mode in one step.
install -m 0755 "${STUB}/wsl.src" "${STUB}/bin/wsl"

echo "installer under test : ${INSTALLER}"
[ -n "${BASELINE}" ] && echo "baseline (must FAIL)  : ${BASELINE}"
echo

# Each harness writes its report to a file; its console output is held back and
# shown only when no report appeared (i.e. the harness itself crashed).
baseline_rc=0
test_rc=0
if [ -n "${BASELINE}" ]; then
  "${PWSH}" -NoProfile -File "${HERE}/harness.ps1" -Installer "${BASELINE}" \
    -Label "BASELINE (pre-fix) — every FAIL below is a real defect this fix closes" \
    -StubBin "${STUB}/bin" -OutFile "${STUB}/baseline.txt" >"${STUB}/baseline.log" 2>&1 || baseline_rc=$?
  [ -s "${STUB}/baseline.txt" ] || { echo "baseline harness produced no report:" >&2; cat "${STUB}/baseline.log" >&2; exit 3; }
  cat "${STUB}/baseline.txt"
  echo
fi

"${PWSH}" -NoProfile -File "${HERE}/harness.ps1" -Installer "${INSTALLER}" \
  -Label "INSTALLER UNDER TEST" -StubBin "${STUB}/bin" -OutFile "${STUB}/result.txt" >"${STUB}/result.log" 2>&1 || test_rc=$?
[ -s "${STUB}/result.txt" ] || { echo "harness produced no report:" >&2; cat "${STUB}/result.log" >&2; exit 3; }
cat "${STUB}/result.txt"
echo

rc=0
if [ "${test_rc}" -ne 0 ]; then
  echo "RESULT: FAILED — ${INSTALLER} did not pass (harness exit ${test_rc})" >&2
  rc=1
elif [ -n "${BASELINE}" ] && [ "${baseline_rc}" -eq 0 ]; then
  echo "RESULT: FAILED — the BASELINE passed, so this harness is not detecting the bug it exists for" >&2
  rc=1
else
  echo "RESULT: all assertions passed"
fi
exit "${rc}"
