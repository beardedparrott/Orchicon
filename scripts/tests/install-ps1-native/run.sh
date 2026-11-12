#!/usr/bin/env bash
# ============================================================================
# Offline harness for scripts/install.ps1 — the NATIVE Windows installer.
#
# WHAT THE INSTALLER IS NOW: it downloads orchicon_<ver>_windows_amd64.zip, installs
# orchicon.exe + orch.exe, and runs `orchicon install` as a NATIVE Windows process.
# WSL2 is provisioned only when the operator opts into containers, and the
# three-cause Docker Desktop diagnosis is gone (the engine is provisioned by the
# installer binary itself). So this suite no longer pins "everything reaches the
# distro": it pins that the NATIVE artifact is installed, that a plain install
# never invokes wsl, that the setup is told when a human is present and when not,
# and that the ports printed are the ones RECORDED rather than a constant.
#
# WHY IT STILL EXISTS AT ALL: the Windows installer is invisible without Windows,
# and the two bugs that cost a release to find live in wsl.exe's captured output
# encoding — a distro name arriving with NUL bytes between its characters (which
# truncates at the first NUL when handed back to `wsl -d <name>`), and `$VARS` in
# a script argument being expanded by the distro's default shell before the
# target bash sees them. The container opt-in still carries a script into a
# distro, so those assertions still have a subject.
#
# WHAT IS REAL HERE: the installer's functions and file, PowerShell's decoding of
# captured native output, the argv/marshal difference between `wsl -e` and
# `wsl --`, $LASTEXITCODE propagation, a REAL zip extracted by Expand-Archive, and
# a real child process running the whole install path.
# WHAT IS EMULATED: wsl.exe (UTF-16LE on a redirected stream, WSL_UTF8=1 switching
# it to UTF-8, `--` re-parsing the command text, a localized failure for an
# unknown distro) and the orchicon.exe inside the release archive (it reports a
# version, records the environment it was handed, and writes the ports record the
# real binary writes — it never does real work).
#
# REQUIRES: pwsh (PowerShell 7+), iconv, python3 (to build the archive double).
#
#   scripts/tests/install-ps1-native/run.sh [installer.ps1]
#   scripts/tests/install-ps1-native/run.sh --baseline <other.ps1>
#   scripts/tests/install-ps1-native/run.sh --no-selfcheck
#
# Exits 0 only when every assertion passes, so it can be used as a check.
#
# EVERY STAGE IS BOUNDED, and that is a fix rather than caution. The harness's console output is
# withheld until it finishes (it is shown from the report file), so a blocked child used to produce the
# banner and then NOTHING — no output, no error, no end. Reported as "make rebuild-dev is still hanging
# on 'installer under test'", and it is un-fixable by inspection exactly because the failure is silence.
# A stage that exceeds its deadline now FAILS and prints the tail of what it had produced, so a block
# names itself. Override the bound with ORCHICON_HARNESS_STAGE_TIMEOUT (seconds).
#
# THE SUITE ALSO PROVES IT STILL BITES unless --no-selfcheck is given: it re-introduces the defect into
# a COPY (the sentinel `throw` that unwinds safely becomes the `exit` that closes the operator's session)
# and requires the harness to FAIL it. This ran only in CI before, which is how a local `make rebuild-dev`
# could pass while reporting nothing about whether the detector still detects — and a detector that
# stopped detecting is worse than no detector, because it is trusted.
# ============================================================================
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
INSTALLER="${REPO_ROOT}/scripts/install.ps1"
BASELINE=""
SELFCHECK=1

while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) BASELINE="${2:?--baseline needs a path}"; shift 2;;
    --no-selfcheck) SELFCHECK=0; shift;;
    -h|--help) sed -n '2,50p' "${BASH_SOURCE[0]}"; exit 0;;
    *) INSTALLER="$1"; shift;;
  esac
done

# Per-stage deadline. 10 minutes is far above a normal stage (the whole suite runs in ~15s), so it only
# ever fires on a genuine block.
STAGE_TIMEOUT="${ORCHICON_HARNESS_STAGE_TIMEOUT:-600}"

# run_harness <installer> <label> <report-file> <log-file>
# Bounded, and the only place a harness is invoked, so a new caller cannot forget the bound.
run_harness() {
  local installer="$1" label="$2" report="$3" log="$4"
  local rc=0
  # ORCHICON_HARNESS_FORCE_BASH_TIMEOUT is a TEST SEAM: it selects the bash watcher so the fallback below
  # is provable on a host that HAS GNU timeout. Untested fallback code is how a portability fix quietly
  # stops working, and this one only runs on a machine that is not the CI runner.
  if [ "${ORCHICON_HARNESS_FORCE_BASH_TIMEOUT:-0}" != "1" ] && command -v timeout >/dev/null 2>&1; then
    timeout "${STAGE_TIMEOUT}" "${PWSH}" -NoProfile -File "${HERE}/harness.ps1" \
      -Installer "${installer}" -Label "${label}" -StubBin "${STUB}/bin" -OutFile "${report}" \
      >"${log}" 2>&1 || rc=$?
  else
    # NO GNU `timeout` (macOS without coreutils) — bound it in bash rather than run unbounded, because
    # an unbounded stage is the exact failure this function exists to remove. A watcher sends TERM at the
    # deadline; the 143 that produces is the timeout signal, translated to timeout(1)'s 124 so the caller
    # has ONE code to reason about. NOTE: the watchdogs in harness.ps1 also bound the harness's own
    # children, so a TERM here cannot strand them.
    "${PWSH}" -NoProfile -File "${HERE}/harness.ps1" \
      -Installer "${installer}" -Label "${label}" -StubBin "${STUB}/bin" -OutFile "${report}" \
      >"${log}" 2>&1 &
    local pid=$! watcher
    ( sleep "${STAGE_TIMEOUT}"; kill -TERM "${pid}" 2>/dev/null ) &
    watcher=$!
    wait "${pid}" || rc=$?
    kill "${watcher}" 2>/dev/null || true
    wait "${watcher}" 2>/dev/null || true
    if [ "${rc}" -eq 143 ]; then rc=124; fi
  fi
  if [ "${rc}" -eq 124 ] || [ "${rc}" -eq 137 ]; then
    echo "TIMED OUT after ${STAGE_TIMEOUT}s — blocked, not slow: ${label}" >&2
    echo "  this is the failure mode the bound exists for; the last output it produced was:" >&2
    sed 's/^/    | /' "${log}" | tail -40 >&2
    return 124
  fi
  return "${rc}"
}

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
# EVERY INVOCATION IS RECORDED when asked. "Did a plain install touch WSL?" is otherwise an
# inference from PATH games; with a call log it is a fact, and the assertion that a clean Windows
# box needs no WSL becomes behavioural instead of textual.
if [ -n "${EMU_CALL_LOG:-}" ]; then printf 'wsl %s\n' "$*" >> "$EMU_CALL_LOG"; fi
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
  # EMU_NO_DOCKER / EMU_NO_DAEMON model the two shapes the installer has to tell
  # apart, AT THE INTERFACE IT ACTUALLY USES: it asks `docker version --format`
  # for a daemon and `command -v docker` for a CLI. Emulating here rather than by
  # hiding a real docker on PATH keeps the guest's coreutils intact.
  # The program is the LAST argument (`wsl -e bash -lc <program>`); `"${@:$skip}"` would also carry the
  # `-lc` and defeat an anchored match.
  guest="${!#}"
  # THE GUEST PROGRAM MAY TRAVEL BASE64-ENCODED (`printf %s <payload> | base64 -d | bash`), so decode it
  # before matching: the emulation below is about the SCRIPT the installer wrote, not about the wrapper
  # that carries it. Matching the wrapper is how this emulation silently stopped applying when the
  # transport changed — the "no docker" cases reverted to the real docker stand-in and two assertions
  # failed for a reason that had nothing to do with docker.
  if [[ "$guest" =~ ^printf\ %s\ ([A-Za-z0-9+/=]+)\ \|\ base64\ -d\ \|\ bash$ ]]; then
    guest="$(printf '%s' "${BASH_REMATCH[1]}" | base64 -d)"
  fi
  case "$guest" in
    *"command -v docker"*)
      if [ "${EMU_NO_DOCKER:-}" = "1" ]; then exit 1; fi
      echo "/usr/bin/docker"; exit 0;;
    *"docker version"*)
      if [ "${EMU_NO_DOCKER:-}" = "1" ]; then echo "bash: line 1: docker: command not found" >&2; exit 127; fi
      if [ "${EMU_NO_DAEMON:-}" = "1" ]; then
        case "$guest" in *--format*) exit 1;; esac
        echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2
        exit 1
      fi;;
  esac
  # `bash -lc` re-sources /etc/profile and resets PATH, so a docker stand-in
  # travels as an exported function (a child bash imports it from the env).
  docker() { case "$*" in *--format*) echo "29.8.0";; *) echo "Docker version 29.8.0";; esac; }
  export -f docker
  # PODMAN IS THE ENGINE THE INSTALL PROVISIONS FIRST (install_engine.go), so it needs the same
  # stand-in — and the same deny-list below. Without it a `podman machine start` reaching this
  # double would resolve the REAL podman off PATH and start a real VM.
  podman() { case "$*" in *--version*) echo "podman version 5.2.0";; *) echo "podman $*";; esac; }
  export -f podman
  cmd=("${@:$skip}")
  # RECORD WHAT THE GUEST WAS HANDED, when asked. The install scripts are multi-line bash with double
  # quotes in them, and a caller that mangles them (Windows PowerShell 5.1 quoting an inline native
  # argument) sends a script that parses to something else entirely — which is exactly the failure this
  # records: the operator's install died on `TMP="$(mktemp -d)"` arriving as `-d)`. Recording the text
  # makes that contract assertable rather than assumed.
  if [ -n "${EMU_RECORD:-}" ]; then
    last="${cmd[$(( ${#cmd[@]} - 1 ))]}"
    # The guest program travels base64-encoded (`printf %s <payload> | base64 -d | bash`), so record the
    # DECODED script — that is the text the script-writing caller is asserting about.
    if [[ "$last" =~ ^printf\ %s\ ([A-Za-z0-9+/=]+)\ \|\ base64\ -d\ \|\ bash$ ]]; then
      printf '%s' "${BASH_REMATCH[1]}" | base64 -d > "$EMU_RECORD"
    else
      printf '%s' "$last" > "$EMU_RECORD"
    fi
    exit 0
  fi
  # --- SAFETY GATE: A GUEST MUST NEVER BE ABLE TO CHANGE THE OPERATOR'S MACHINE ---
  #
  # THIS IS A CORRECTNESS GATE, NOT CAUTION. The double runs guest scripts ON THE HOST — that is how it
  # emulates WSL without Windows — and the docker stand-in below is an EXPORTED BASH FUNCTION. An exported
  # function is imported by BASH children only. A GO child does `exec.Command("docker", ...)`, resolves
  # docker off PATH, and gets THE REAL ENGINE. And a stubbed `docker run` prints a version string and
  # reports success without doing anything.
  #
  # So a guest that reaches an install path installs FOR REAL (pulling images, re-creating the operator's
  # containers and restarting their planes) while this suite reports "all assertions passed" — and a real
  # install is also indistinguishable from a hang, because it is minutes of image pull followed by a
  # prompt. That is precisely the reported symptom, and the re-created containers are in the Docker events.
  #
  # A PATH-level shim cannot be the guard: `bash -lc` re-sources /etc/profile, which RESETS PATH, so a
  # refusal stub cannot be relied on to survive into the guest. A REFUSAL on the guest text can, and it
  # fails LOUDLY, which is the point — the offending assertion breaks instead of quietly installing.
  #
  # The deny-list is deliberately narrow, so the legitimate checks (coreutils: mktemp, tar, base64, iconv)
  # are untouched: they name neither the product binaries nor a mutating docker verb.
  # MATCH THE BINARIES AS COMMAND WORDS, not as substrings. A plain `*orchicon*` test is WRONG and was
  # measured wrong: the guest that reads the instance's bound ports uses a path containing
  # `orchicon-test/ports.json`, so a substring test refuses a legitimate read and broke four [16]
  # assertions. Requiring a command boundary keeps the gate on INVOCATIONS only.
  #
  # `case` GLOBS RATHER THAN `[[ =~ ]]`. The regex form was tried first and broke the generated script:
  # bash parses unquoted parentheses inside `[[ ... ]]` itself, so the double died with "unexpected EOF
  # while looking for matching `)'" and the whole suite went red for a reason unrelated to the gate.
  # Globs have no such ambiguity, and the newline variants are needed because guest scripts are multi-line.
  # `[[:space:]]` (not a literal space) so a MULTI-LINE guest is covered: the setup script puts the
  # invocation at the start of its own line. And the trailing `[[:space:]]` is what keeps a path that
  # merely CONTAINS the name — `orchicon-test/ports.json`, `orchicon-prod` — out of the deny-list; the
  # substring form was tried and broke four legitimate reads.
  deny=""
  case "$guest" in
    orchicon[[:space:]]*|orch[[:space:]]*|*[[:space:]/]orchicon[[:space:]]*|*[[:space:]/]orch[[:space:]]*)
      deny="it invokes the Orchicon product binaries (orchicon/orch), whose Go children bypass the docker stand-in";;
  esac
  # ...AND A MUTATING DOCKER VERB, for a second, independent reason: the stand-in below answers EVERY
  # docker invocation with a version string, so `docker run` reports SUCCESS while doing nothing. A guest
  # that believes it started a container then asserts about a container that does not exist — or worse,
  # a caller reads that as the install having worked. Refusing says what actually happened.
  #
  # A QUERY IS STILL ALLOWED: `docker version --format ...` is what the installer legitimately asks, and
  # the case block above already answers it. Only verbs with side effects are refused.
  if [ -z "$deny" ]; then
    case "$guest" in
      *"docker run"*|*"docker create"*|*"docker start"*|*"docker stop"*|*"docker rm"*|*"docker pull"*|\
      *"docker volume"*|*"docker network"*|*"docker exec"*|*"docker cp"*|*"docker build"*|\
      *"docker tag"*|*"docker push"*|*"docker compose"*|*"docker kill"*|*"docker restart"*)
        deny="it MUTATES the Docker engine — and the stand-in below would report success without doing it";;
      *"podman run"*|*"podman create"*|*"podman start"*|*"podman stop"*|*"podman rm"*|*"podman pull"*|\
      *"podman volume"*|*"podman network"*|*"podman exec"*|*"podman cp"*|*"podman build"*|\
      *"podman tag"*|*"podman push"*|*"podman compose"*|*"podman kill"*|*"podman restart"*|\
      *"podman machine init"*|*"podman machine start"*|*"podman machine stop"*|*"podman machine rm"*)
        deny="it MUTATES the container engine (podman) — and the stand-in below would report success without doing it";;
    esac
  fi
  if [ -n "$deny" ]; then
    {
      echo "wsl double: REFUSED a guest command — ${deny}."
      echo "  This harness emulates wsl.exe so the installer can be exercised without Windows; it must not"
      echo "  be able to change anything on the operator's machine. Fix the ASSERTION that reached this"
      echo "  path (it should halt earlier, or pass EMU_NO_DOCKER), never this gate."
      echo "  guest (first 500 bytes):"
      printf '%s\n' "$guest" | head -c 500 | sed 's/^/    | /'
    } >&2
    exit 90
  fi

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

# --- release-archive double -------------------------------------------------
# The install path extracts a REAL zip (Expand-Archive) and then RUNS what it extracted, so the
# fixture is a real archive in the shape the release workflow produces: a top-level
# orchicon_<ver>_windows_amd64/ directory holding orchicon.exe and orch.exe. The .exe double does no
# real work: it reports a version, records the environment the wrapper handed it (that is how the
# interactive/unattended distinction is observed rather than grepped), and writes the ports record
# the real binary writes.
FIXTURE="${STUB}/fixture"
mkdir -p "${FIXTURE}/build/orchicon_0.0.0_windows_amd64"
cat > "${FIXTURE}/build/orchicon_0.0.0_windows_amd64/orchicon.exe" <<'ORCHICON_EXE'
#!/usr/bin/env bash
set -u
CMD="${1:-}"
if [ -n "${EMU_ORCHICON_ENV_LOG:-}" ]; then
  {
    printf 'cmd=%s\n' "$CMD"
    printf 'INSTALL_INTERACTIVE=%s\n' "${ORCHICON_INSTALL_INTERACTIVE-<unset>}"
    printf 'ASSUME_YES=%s\n' "${ORCHICON_ASSUME_YES-<unset>}"
    printf 'INSTANCE=%s\n' "${ORCHICON_INSTANCE-<unset>}"
    printf 'SERVE_STATE_DIR=%s\n' "${ORCHICON_SERVE_STATE_DIR-<unset>}"
  } >> "${EMU_ORCHICON_ENV_LOG}"
fi
case "$CMD" in
  version) echo "Orchicon v0.0.0 (double)"; exit 0;;
  stop) exit 0;;
  install)
    echo "orchicon install (double): acquiring the host service binaries…"
    inst="${ORCHICON_INSTANCE:-dev}"
    dir="$HOME/.local/share/orchicon-${inst}"
    mkdir -p "$dir"
    printf '%s\n' "${EMU_PORTS_JSON:-{\"ORCHICON_CONTROL_PORT\":9097}}" > "$dir/ports.json"
    echo "orchicon install (double): done"
    exit "${EMU_ORCHICON_INSTALL_RC:-0}";;
esac
exit 0
ORCHICON_EXE
install -m 0755 "${FIXTURE}/build/orchicon_0.0.0_windows_amd64/orchicon.exe" "${FIXTURE}/build/orchicon_0.0.0_windows_amd64/orch.exe"
# THE LINUX CONTAINER BINARY SHIPS IN THE ARCHIVE. A runtime container is Linux and runs
# `orchicon runtime-supervisor`, so a Windows host's archive carries a linux/amd64 build from the same
# commit. The fixture carries one WITH THE ELF MAGIC, so the installer's format check is exercised
# against the real shape instead of a file it must reject.
printf '\177ELF\002\001\001\000' > "${FIXTURE}/build/orchicon_0.0.0_windows_amd64/orchicon-linux-amd64"
chmod 0755 "${FIXTURE}/build/orchicon_0.0.0_windows_amd64/orchicon-linux-amd64"
python3 - "${FIXTURE}" <<'FIXTURE_ZIP'
import os, sys, zipfile
root = sys.argv[1]
src = os.path.join(root, "build")
out = os.path.join(root, "release", "orchicon_0.0.0_windows_amd64.zip")
os.makedirs(os.path.dirname(out), exist_ok=True)
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for dirpath, _dirnames, filenames in os.walk(src):
        for name in filenames:
            full = os.path.join(dirpath, name)
            z.write(full, os.path.relpath(full, src))
FIXTURE_ZIP
test -f "${FIXTURE}/release/orchicon_0.0.0_windows_amd64.zip" || { echo "run.sh: fixture archive was not built" >&2; exit 2; }

# --- a PUBLISHED MSI, so the MSI arm can be taken ----------------------------
# THE ARM THAT SHIPPED BROKEN. install.ps1 prefers an MSI when one is published, but every
# other case here sets ORCHICON_INSTALL_ARCHIVE (the offline seam), which forces the zip —
# so the MSI arm had NEVER executed under test and reached a real operator unexercised. It
# crashed on the first Windows run ("Cannot bind argument to parameter 'Path' because it is
# null": the scratch directory was created BELOW the branch that used it).
#
# The arm is behind an availability PROBE, so reaching it needs an HTTP authority that answers
# HEAD 200 for <base>/<version>/<msi-asset>. That is all this is: a fixture MSI on a local
# listener. The file is never installed — there is no msiexec on this runner — and the case
# asserts the installer gets AS FAR AS LAUNCHING it, which is the whole of the arm's setup.
MSI_DIR="${FIXTURE}/msi"
mkdir -p "${MSI_DIR}/v0.0.0"
# Not a valid package on purpose: if this ever ran where msiexec exists it must fail fast
# rather than open an installer UI.
printf 'not an msi\n' > "${MSI_DIR}/v0.0.0/orchicon_0.0.0_windows_amd64.msi"

# PORT 0, THEN REPORT IT: a fixed port collides on a busy runner, and a retry loop is another
# thing that can hang. The server writes the port it got and the harness reads it.
MSI_PORT_FILE="${STUB}/msi-port.txt"
python3 - "${MSI_DIR}" "${MSI_PORT_FILE}" >/dev/null 2>&1 <<'MSI_SERVER' &
import http.server, socketserver, sys, os
root, portfile = sys.argv[1], sys.argv[2]
os.chdir(root)
class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a): pass
httpd = socketserver.TCPServer(("127.0.0.1", 0), Quiet)
with open(portfile, "w") as f:
    f.write(str(httpd.server_address[1]))
httpd.serve_forever()
MSI_SERVER
MSI_SERVER_PID=$!
trap 'kill "${MSI_SERVER_PID}" 2>/dev/null || true; rm -rf "${STUB}" 2>/dev/null || true' EXIT

for _ in $(seq 1 50); do
  [ -s "${MSI_PORT_FILE}" ] && break
  sleep 0.1
done
if [ -s "${MSI_PORT_FILE}" ]; then
  ORCHICON_TEST_MSI_BASE_URL="http://127.0.0.1:$(cat "${MSI_PORT_FILE}")"
  export ORCHICON_TEST_MSI_BASE_URL
  # The probe must really answer, or the case silently tests the zip path and proves nothing.
  if curl -sf -o /dev/null -I "${ORCHICON_TEST_MSI_BASE_URL}/v0.0.0/orchicon_0.0.0_windows_amd64.msi" 2>/dev/null \
     || python3 -c "import urllib.request,sys; urllib.request.urlopen('${ORCHICON_TEST_MSI_BASE_URL}/v0.0.0/orchicon_0.0.0_windows_amd64.msi')" 2>/dev/null; then
    echo "MSI arm fixture     : ${ORCHICON_TEST_MSI_BASE_URL}/v0.0.0/orchicon_0.0.0_windows_amd64.msi (probe answers)"
  else
    echo "run.sh: the MSI fixture listener did not answer — the MSI arm would not be exercised" >&2
    exit 2
  fi
else
  echo "run.sh: the MSI fixture listener never reported a port" >&2
  exit 2
fi

echo "installer under test : ${INSTALLER}"
[ -n "${BASELINE}" ] && echo "baseline (must FAIL)  : ${BASELINE}"
echo

# Each harness writes its report to a file; its console output is held back and
# shown only when no report appeared (i.e. the harness itself crashed).
baseline_rc=0
test_rc=0
if [ -n "${BASELINE}" ]; then
  # Announced BEFORE it runs, because its output is withheld and an unannounced quiet stage is what a
  # hang looks like.
  echo "running the baseline (must FAIL) — output follows when it finishes …"
  run_harness "${BASELINE}" "BASELINE (pre-fix) — every FAIL below is a real defect this fix closes" \
    "${STUB}/baseline.txt" "${STUB}/baseline.log" || baseline_rc=$?
  [ -s "${STUB}/baseline.txt" ] || { echo "baseline harness produced no report (rc=${baseline_rc}):" >&2; cat "${STUB}/baseline.log" >&2; exit 3; }
  cat "${STUB}/baseline.txt"
  echo
fi

echo "running the harness — output follows when it finishes (normally a few seconds) …"
run_harness "${INSTALLER}" "INSTALLER UNDER TEST" "${STUB}/result.txt" "${STUB}/result.log" || test_rc=$?
[ -s "${STUB}/result.txt" ] || { echo "harness produced no report (rc=${test_rc}):" >&2; cat "${STUB}/result.log" >&2; exit 3; }
cat "${STUB}/result.txt"
echo

# --- does this suite still DETECT? ------------------------------------------
# The mutation is CI's, moved here so a local run gets it too. It must be checked in BOTH directions: a
# mutated copy that still passes means the detector stopped detecting, and a mutation that did not apply
# at all would silently test the UNMUTATED file — a false assurance, which is the one outcome worse than
# a red suite.
selfcheck_rc=0
if [ "${SELFCHECK}" -eq 1 ]; then
  echo "self-check: re-introducing the defect into a copy (this suite must FAIL it) …"
  broken="${STUB}/install-exit.ps1"
  anchor='throw "OrchiconInstallHalt"'
  if ! grep -qF "${anchor}" "${INSTALLER}"; then
    echo "SELF-CHECK FAILED — the mutation anchor is gone: ${anchor}" >&2
    echo "  the harness detects 'exit' closing the operator's session by mutating that sentinel throw." >&2
    echo "  With the anchor missing the mutation is a no-op and the self-check would pass the UNMUTATED" >&2
    echo "  installer, which is a false assurance. Update the mutation to match the installer's halt." >&2
    exit 5
  fi
  sed "s/${anchor}/exit 1/" "${INSTALLER}" > "${broken}"
  if cmp -s "${INSTALLER}" "${broken}"; then
    echo "SELF-CHECK FAILED — the mutation did not change the file, so it proves nothing" >&2
    exit 5
  fi
  if run_harness "${broken}" "SELF-CHECK (mutated: the safe halt becomes a session-closing exit)" \
      "${STUB}/broken.txt" "${STUB}/broken.log"; then
    echo "SELF-CHECK FAILED — the harness PASSED an installer whose halt closes the operator's session." >&2
    echo "  The suite is no longer detecting the defect it exists for; its green result means nothing." >&2
    sed 's/^/    | /' "${STUB}/broken.txt" | tail -20 >&2
    exit 5
  fi
  echo "  PASS  the suite FAILS the mutated installer, so its assertions still bite"
  echo
fi

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
