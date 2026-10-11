#!/usr/bin/env bash
# ============================================================================
# install-path/uninstall.sh — THE UNINSTALL VERBS, TESTED WITH NOTHING REAL.
#
# WHY THIS IS A SEPARATE HARNESS, AND WHY IT IS BUILT THE WAY IT IS.
#
# These verbs remove containers, volumes, state directories and executables. Testing them the obvious
# way — point them at a real host and see what happens — is how an operator's PROD container was removed
# twice by an assistant verifying its own work. "Be careful" is not a mechanism; the mechanism is that
# this harness CANNOT reach a real resource:
#
#   * `docker` is a STUB placed first on PATH that logs its arguments and does nothing. Every container
#     and volume operation the installer makes is therefore recorded, never performed.
#   * `XDG_DATA_HOME` points at a scratch directory, so `inst_state_dir` resolves inside it. The
#     installer's answer to "does this instance exist?" is then entirely under this test's control, and
#     dev/prod are absent unless the test creates them.
#   * `--install-dir` points at a scratch directory holding FAKE binaries, so no real executable is ever
#     a candidate for removal.
#
# The assertions are then about the LOG — what the installer would have done — which is the only safe
# place to observe a destructive command.
#
# THE ASSERTION THAT MATTERS MOST is the scope check: for every verb, the recorded docker arguments must
# name ONLY the instance under test. That is the operator's absolute requirement — "when I run this with
# instance=test it must ABSOLUTELY not mess up dev and prod" — expressed as a test rather than a promise.
#
#   scripts/tests/install-path/uninstall.sh
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
INSTALLER="${REPO_ROOT}/scripts/install.sh"

PASS=0
FAIL=0
ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$*"; PASS=$((PASS + 1)); }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }

# THE WORKSPACE LIVES INSIDE THE PROJECT, not in /tmp. Two reasons: the guard that protects this host
# refuses chmod/rm outside the conversation's project (so a /tmp workspace cannot even be set up), and a
# scratch tree beside the repo is easier to inspect when a run goes wrong. `tmp/` is gitignored.
WORK="${REPO_ROOT}/tmp/install-path-uninstall.$$"
rm -rf "${WORK}"
mkdir -p "${WORK}"
trap 'rm -rf "${WORK}"' EXIT

# --- the stub: docker can only write its arguments down ----------------------
BIN="${WORK}/bin"
mkdir -p "${BIN}"
cat > "${BIN}/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG:?STUB_LOG must be set}"
case "$1" in
  inspect) exit "${STUB_INSPECT_RC:-1}" ;;   # default: no container exists
  *)       exit 0 ;;
esac
STUB
chmod +x "${BIN}/docker"

# Scratch everything the installer resolves: binaries, host state, data.
INSTALL_DIR="${WORK}/opt"
mkdir -p "${INSTALL_DIR}"
for b in orchicon orch; do printf '#!/bin/sh\necho fake\n' > "${INSTALL_DIR}/${b}"; chmod +x "${INSTALL_DIR}/${b}"; done
XDG="${WORK}/xdg"
mkdir -p "${XDG}"

# run <logfile> <args...> — invoke the REAL installer with every dangerous thing stubbed.
run() {
  local log="$1"; shift
  : > "${log}"
  env -u ORCHICON_INSTANCE \
      -u PURGE -u REMOVE_BINARY \
      PATH="${BIN}:${PATH}" STUB_LOG="${log}" XDG_DATA_HOME="${XDG}" \
      bash "${INSTALLER}" --install-dir "${INSTALL_DIR}" "$@" 2>&1
}

# mutations <log> — only the docker calls that CHANGE something. `inspect` is read-only, and the
# installer uses it legitimately to ask whether another instance still exists; flagging a read as a leak
# would be a test that cannot tell asking from acting.
mutations() {
  grep -E '^(rm|volume rm|stop)( |$)' "$1" 2>/dev/null
}

# run_detached <logfile> <args...> — the same, in a session with NO controlling terminal.
#
# A plain `run` cannot test the "no terminal" path on a developer's machine: /dev/tty exists and the
# installer correctly WAITS at it (measured — the harness hung until its timeout). `setsid` starts a new
# session with no controlling terminal, which is the state the path is written for. Note that
# `[ -r /dev/tty ]` still answers yes there while every OPEN fails, which is why the installer tests
# openability instead of readability.
run_detached() {
  local log="$1"; shift
  : > "${log}"
  env -u ORCHICON_INSTANCE \
      PATH="${BIN}:${PATH}" STUB_LOG="${log}" XDG_DATA_HOME="${XDG}" \
      setsid bash "${INSTALLER}" --install-dir "${INSTALL_DIR}" "$@" < /dev/null 2>&1
}

# only_instance <log> <instance> — did every MUTATING docker call name ONLY this instance?
only_instance() {
  local log="$1" inst="$2" other
  for other in dev prod test; do
    [ "${other}" = "${inst}" ] && continue
    if mutations "${log}" | grep -qE "orchicon-cnt-${other}([^a-z]|$)"; then
      printf '    leaked: %s\n' "$(mutations "${log}" | grep -E "orchicon-cnt-${other}" | head -2 | tr '\n' ' ')"
      return 1
    fi
  done
  return 0
}

echo "install-path: uninstall verbs (docker stubbed; nothing real is touched)"
echo

# --- 1. --instance test touches ONLY test ------------------------------------
echo "[1] --instance test is scoped to test"
# `dev` IS FAKED, not touched: with real state present the installer must conclude another instance still
# needs the executables and leave them alone. This is the assertion that encodes the operator's
# requirement, and it works only because XDG_DATA_HOME is scratch.
#
# REAL STATE, NOT AN EMPTY DIRECTORY: an instance is something that exists, and a directory holding only
# the install's own ports record (or nothing at all) is not one — see section [9].
mkdir -p "${XDG}/orchicon-test" "${XDG}/orchicon-dev/real-state"
printf 'x' > "${XDG}/orchicon-dev/real-state/marker"
OUT="$(run "${WORK}/l1.log" --uninstall --instance test)"
if grep -q "orchicon-cnt-test" "${WORK}/l1.log"; then ok "the test container is the one acted on"; else bad "the test container is acted on"; fi
if only_instance "${WORK}/l1.log" test; then ok "NO dev or prod container appears in the docker log"; else bad "another instance leaked into the docker log"; fi
if printf '%s' "${OUT}" | grep -q "Data preserved"; then ok "data is preserved by default"; else bad "data is preserved by default"; fi
if [ -f "${INSTALL_DIR}/orchicon" ]; then ok "the binaries are LEFT while other instances exist"; else bad "the binaries are left while other instances exist"; fi

# --- 2. --purge ALSO removes only test's data -------------------------------
echo ""
echo "[2] --purge scopes the destruction to test"
OUT="$(run "${WORK}/l2.log" --purge --instance test)"
if mutations "${WORK}/l2.log" | grep -q "orchicon-cnt-test-data"; then ok "the test volume is destroyed"; else bad "the test volume is destroyed"; fi
if only_instance "${WORK}/l2.log" test; then ok "and no other instance was mutated"; else bad "another instance was mutated"; fi
if printf '%s' "${OUT}" | grep -q "Re-run with --purge"; then bad "purge still claims data was preserved"; else ok "purge does not claim data was preserved"; fi

# --- 3. An unknown instance is refused -------------------------------------
echo ""
echo "[3] an unknown instance is refused"
OUT="$(run "${WORK}/l3.log" --uninstall --instance production)"
if printf '%s' "${OUT}" | grep -q "unknown instance"; then ok "refused, naming the valid set"; else bad "refused"; fi
if [ ! -s "${WORK}/l3.log" ]; then ok "and it touched nothing (empty docker log)"; else bad "it ran docker anyway"; fi

# --- 4. No terminal and no --instance: REFUSE, never default ----------------
echo ""
echo "[4] no terminal and no --instance refuses rather than defaulting"
OUT="$(run_detached "${WORK}/l4.log" --uninstall)"
if printf '%s' "${OUT}" | grep -qE "no terminal to ask on|which instance\? pass"; then ok "refuses instead of picking a default"; else bad "refuses instead of picking a default"; fi
if [ ! -s "${WORK}/l4.log" ]; then ok "and it touched nothing"; else bad "it acted without an answer"; fi

# --- 5. --remove-binary: binaries only, nothing else ------------------------
echo ""
echo "[5] --remove-binary removes only the executables"
rm -rf "${XDG}/orchicon-dev"   # no other instance exists now, so the binaries ARE a candidate
OUT="$(run "${WORK}/l5.log" --remove-binary --instance test)"
if [ ! -e "${INSTALL_DIR}/orchicon" ] && [ ! -e "${INSTALL_DIR}/orch" ]; then ok "both executables were removed"; else bad "both executables were removed"; fi
if [ ! -s "${WORK}/l5.log" ]; then ok "and NO docker command was issued (containers untouched)"; else bad "it ran docker: $(head -1 "${WORK}/l5.log")"; fi

# --- 6. --remove-binary asks before stopping anything ----------------------
echo ""
echo "[6] --remove-binary asks before taking services down"
for b in orchicon orch; do printf '#!/bin/sh\necho fake\n' > "${INSTALL_DIR}/${b}"; chmod +x "${INSTALL_DIR}/${b}"; done
# A holder that reports itself as the prod instance, exactly like the real accident.
( exec 3< "${INSTALL_DIR}/orchicon"; sleep 20 ) &
HOLDER=$!
sleep 1
OUT="$(run_detached "${WORK}/l6.log" --remove-binary --instance test)"
if printf '%s' "${OUT}" | grep -q "cannot ask"; then ok "with no terminal it does not assume consent"; else bad "with no terminal it does not assume consent"; fi
if [ -f "${INSTALL_DIR}/orchicon" ]; then ok "and the binaries are left in place"; else bad "it removed binaries without an answer"; fi
kill "${HOLDER}" 2>/dev/null || true

# --- 7. --yes + --dry-run: consent given, and NOTHING is actually done ----------
# THIS IS THE ASSERTION THAT WOULD HAVE CAUGHT THE WORST BUG IN THIS FILE. A helper stopped services
# without honouring --dry-run, so a "dry" run removed the operator's PROD container: the binaries were
# protected and the container was not. So the test gives explicit consent, in dry-run, with a holder
# present, and requires that (a) the stop is REPORTED and (b) the binaries are still there afterwards.
echo ""
echo "[7] --yes consents, --dry-run still does not act"
for b in orchicon orch; do printf '#!/bin/sh\necho fake\n' > "${INSTALL_DIR}/${b}"; chmod +x "${INSTALL_DIR}/${b}"; done
env ORCHICON_INSTANCE=test bash -c "exec 3< '${INSTALL_DIR}/orchicon'; sleep 20" &
HOLDER=$!
sleep 1
OUT="$(run "${WORK}/l7.log" --remove-binary --instance test --yes --dry-run)"
if printf '%s' "${OUT}" | grep -qE "would stop|would remove container"; then
  ok "it reports the stop it would perform"
else
  bad "it reports the stop it would perform"
fi
if [ -f "${INSTALL_DIR}/orchicon" ] && [ -f "${INSTALL_DIR}/orch" ]; then
  ok "and DRY RUN left both binaries in place"
else
  bad "dry run removed the binaries anyway"
fi
if grep -q . <(mutations "${WORK}/l7.log"); then
  bad "DRY RUN ISSUED A MUTATION: $(mutations "${WORK}/l7.log" | tr '\n' ' ')"
else
  ok "and the stub log records NO container mutation at all"
fi
kill "${HOLDER}" 2>/dev/null || true

# --- 8. DRY RUN MUST NOT CLAIM IT DID ANYTHING ------------------------------
# A dry run that reports "container removed" is worse than no dry run: the operator is told work happened
# that did not, on the one command whose whole purpose is to say what WOULD change.
echo ""
echo "[8] --dry-run says what it would do, never what it did"
rm -rf "${XDG}/orchicon-test" && mkdir -p "${XDG}/orchicon-test"
OUT="$(run "${WORK}/l8.log" --uninstall --instance test --dry-run)"
if printf '%s' "${OUT}" | grep -q "would remove the test container"; then ok "reports the container as a WOULD"; else bad "reports the container as a would"; fi
if printf '%s' "${OUT}" | grep -qE "✓ *test container removed|✓ *[a-z]+ removed"; then
  bad "AND IT ALSO CLAIMS A COMPLETED REMOVAL"
else
  ok "and never claims a completed removal"
fi
if grep -q . <(mutations "${WORK}/l8.log"); then bad "dry run issued a docker mutation"; else ok "and issued no docker mutation"; fi

# --- 9. A LONE ports.json IS NOT AN INSTANCE --------------------------------
# `orchicon install` records the ports it bound in the instance state dir, and that record must not make a
# scratch install look like a live instance: if it did, --uninstall would conclude another instance still
# needs the executables and would leave them behind for ever.
echo ""
echo "[9] an install's ports record alone does not count as an instance"
rm -rf "${XDG}/orchicon-test" "${XDG}/orchicon-dev"
mkdir -p "${XDG}/orchicon-dev" && printf '{"ORCHICON_CONTROL_PORT":8080}\n' > "${XDG}/orchicon-dev/ports.json"
mkdir -p "${XDG}/orchicon-test" && printf '{"ORCHICON_CONTROL_PORT":8092}\n' > "${XDG}/orchicon-test/ports.json"
for b in orchicon orch; do printf '#!/bin/sh\necho fake\n' > "${INSTALL_DIR}/${b}"; chmod +x "${INSTALL_DIR}/${b}"; done
OUT="$(run "${WORK}/l9.log" --uninstall --instance test)"
if [ ! -e "${INSTALL_DIR}/orchicon" ]; then
  ok "the binaries were removed (dev's ports record is not an instance)"
else
  bad "the binaries were LEFT because a ports.json looked like an instance"
fi
# ...but real state still counts, so a genuine dev instance protects the binaries.
mkdir -p "${XDG}/orchicon-dev/real-state" && printf 'x' > "${XDG}/orchicon-dev/real-state/marker"
for b in orchicon orch; do printf '#!/bin/sh\necho fake\n' > "${INSTALL_DIR}/${b}"; chmod +x "${INSTALL_DIR}/${b}"; done
OUT="$(run "${WORK}/l9b.log" --uninstall --instance test)"
if [ -f "${INSTALL_DIR}/orchicon" ]; then
  ok "and real dev state DOES protect them (the other half of the rule)"
else
  bad "real dev state did not protect the binaries"
fi

# --- 10. EVERY DESTRUCTIVE VERB IS INSTANCE-BOUND ---------------------------
# The operator: "EVERY option should be instance bound and operate as such." It was not: --force-clean and
# --clean took whatever ORCHICON_INSTANCE held (or `dev`) without asking, so the ONE verb that DESTROYS A
# DATA VOLUME was also the one that could act on an instance nobody chose. Reported after a -ForceClean went
# ahead on its own.
#
# THE VERB LIST IS ENUMERATED HERE ON PURPOSE: a new destructive verb that forgets to ask must be added to
# this list to be covered, which makes the omission visible in review rather than discoverable in production.
echo ""
echo "[10] every destructive verb asks, and refuses without an answer"

for verb in --uninstall --purge --remove-binary --clean --force-clean; do
  # Detached, so there is no terminal to answer on: the verb must REFUSE rather than default.
  OUT="$(env -u ORCHICON_INSTANCE setsid bash "${INSTALLER}" --install-dir "${INSTALL_DIR}" "${verb}" < /dev/null 2>&1)"
  if printf '%s' "${OUT}" | grep -qE "no instance chosen|which instance\? pass"; then
    ok "${verb} refuses with no instance and no terminal"
  else
    bad "${verb} did NOT refuse — it acted on an instance nobody chose (said: $(printf '%s' "${OUT}" | grep -aE 'acting on|instance' | head -1))"
  fi
done

# And install is the ONE verb that may default, because installing to the documented default is not
# destructive. If install ever stops defaulting, that is a deliberate change, not a silent one.
OUT="$(env -u ORCHICON_INSTANCE ORCHICON_INSTALL_BASE_URL="http://127.0.0.1:1" setsid bash "${INSTALLER}" \
  --install-dir "${INSTALL_DIR}-d" --no-setup < /dev/null 2>&1)"
if printf '%s' "${OUT}" | grep -q "using 'dev'"; then
  ok "install still takes the documented default (it is the only verb that may)"
else
  bad "install no longer defaults to dev (got: $(printf '%s' "${OUT}" | head -2 | tr '\n' ' '))"
fi

echo ""
if [ "${FAIL}" -eq 0 ]; then
  echo "RESULT: all ${PASS} assertion(s) passed"
  exit 0
fi
echo "RESULT: ${FAIL} assertion(s) FAILED (passing: ${PASS})"
exit 1
