#!/usr/bin/env bash
# ============================================================================
# install-path/uninstall.sh — THE UNINSTALL VERBS, TESTED WITH NOTHING REAL.
#
# WHY THIS IS A SEPARATE HARNESS, AND WHY IT IS BUILT THE WAY IT IS.
#
# These verbs destroy an instance's state directory and remove its executables. Testing them the obvious
# way — point them at a real host and see what happens — is how an operator's PROD stack was removed twice
# by an assistant verifying its own work. "Be careful" is not a mechanism; the mechanism is that this
# harness CANNOT reach a real resource:
#
#   * `XDG_DATA_HOME` points at a scratch directory, so `inst_state_dir` resolves inside it. The
#     installer's answer to "does this instance exist?" is then entirely under this test's control, and
#     dev/prod are absent unless the test creates them.
#   * `--install-dir` points at a scratch directory holding FAKE binaries, so no real executable is ever
#     a candidate for removal.
#   * `docker`/`podman` remain on PATH as TRIPWIRES that record every argument and do nothing. The
#     installer no longer has an engine path at all, so these logs must stay EMPTY: a non-empty one is a
#     regression that put a container call back into an installer that should not have one.
#
# The assertions are therefore about the FILESYSTEM (exactly which state dirs survived) and about the
# TRIPWIRE logs (which must be empty) — the only safe places to observe a destructive command.
#
# THE ASSERTION THAT MATTERS MOST is the scope check: for every verb, only the instance under test may be
# touched, and another instance's state must be byte-identical afterwards. That is the operator's absolute
# requirement — "when I run this with instance=test it must ABSOLUTELY not mess up dev and prod" —
# expressed as a test rather than a promise.
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

# --- the tripwires: an engine call can only write its arguments down ---------
# The installer has no container-engine path any more. These stubs are kept FIRST ON PATH so that if one
# creeps back in, the call is RECORDED (and the assertions below fail) instead of landing on a real host.
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
# The teardown drives whichever engine scripts/lib/engine.sh resolves, so BOTH
# names are stubbed: a docker-only stub would make the podman case pass by
# finding no engine at all, which is the opposite of what it must prove.
cp "${BIN}/docker" "${BIN}/podman"
chmod +x "${BIN}/podman"

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

# no_engine_calls <log> — the tripwire log must be EMPTY. There is no container engine in the install
# path, so any call at all is the regression, not just a mutating one; `inspect` used to be excused because
# asking "does another instance exist" was legitimate, and that question is now answered from the state dir.
no_engine_calls() {
  [ -s "$1" ] && return 1
  return 0
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

# other_state_intact <manifest> <dir>... — the sibling instances' state dirs are byte-identical to the
# manifest taken before the verb ran. This is the scope assertion, on the filesystem rather than on a log:
# a state dir that changed is a leak, whatever the output claimed.
manifest() {
  local dir
  for dir in "$@"; do
    [ -d "${dir}" ] || continue
    find "${dir}" -type f -print0 2>/dev/null | sort -z | xargs -0 -r sha256sum 2>/dev/null
  done
}
other_state_intact() {
  local before="$1"; shift
  [ "$(manifest "$@")" = "$(cat "${before}")" ]
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
mkdir -p "${XDG}/orchicon-test/serve" "${XDG}/orchicon-dev/real-state"
printf 'kek' > "${XDG}/orchicon-test/serve/kek"
printf 'x' > "${XDG}/orchicon-dev/real-state/marker"
manifest "${XDG}/orchicon-dev" > "${WORK}/dev.manifest"
OUT="$(run "${WORK}/l1.log" --uninstall --instance test)"
if printf '%s' "${OUT}" | grep -q "orchicon-test"; then ok "the test instance is the one acted on"; else bad "the test instance is acted on"; fi
if other_state_intact "${WORK}/dev.manifest" "${XDG}/orchicon-dev"; then ok "dev's state is byte-identical (no other instance was touched)"; else bad "another instance's state dir changed"; fi
if no_engine_calls "${WORK}/l1.log"; then ok "and NO container engine was called at all"; else bad "it called an engine: $(head -1 "${WORK}/l1.log")"; fi
if printf '%s' "${OUT}" | grep -q "Data preserved"; then ok "data is preserved by default"; else bad "data is preserved by default"; fi
if [ -f "${INSTALL_DIR}/orchicon" ]; then ok "the binaries are LEFT while other instances exist"; else bad "the binaries are left while other instances exist"; fi

# --- 2. --purge ALSO removes only test's data -------------------------------
echo ""
echo "[2] --purge scopes the destruction to test"
OUT="$(run "${WORK}/l2.log" --purge --instance test)"
if [ ! -d "${XDG}/orchicon-test" ]; then ok "the test instance's data dir is destroyed"; else bad "the test instance's data dir survived --purge"; fi
if printf '%s' "${OUT}" | grep -q "destroys\|destroying"; then ok "and it says it is destroying data before it does"; else bad "the destruction is not announced before it happens"; fi
if other_state_intact "${WORK}/dev.manifest" "${XDG}/orchicon-dev"; then ok "and no other instance was mutated"; else bad "another instance was mutated"; fi
if no_engine_calls "${WORK}/l2.log"; then ok "and still no container engine"; else bad "it called an engine: $(head -1 "${WORK}/l2.log")"; fi
if printf '%s' "${OUT}" | grep -q "Re-run with --purge"; then bad "purge still claims data was preserved"; else ok "purge does not claim data was preserved"; fi

# --- 3. An unknown instance is refused -------------------------------------
echo ""
echo "[3] an unknown instance is refused"
OUT="$(run "${WORK}/l3.log" --uninstall --instance production)"
if printf '%s' "${OUT}" | grep -q "unknown instance"; then ok "refused, naming the valid set"; else bad "refused"; fi
if no_engine_calls "${WORK}/l3.log"; then ok "and it touched nothing (empty tripwire log)"; else bad "it ran an engine anyway"; fi

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
if no_engine_calls "${WORK}/l5.log"; then ok "and NO container engine was called (nothing but the executables was touched)"; else bad "it ran an engine: $(head -1 "${WORK}/l5.log")"; fi

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
if no_engine_calls "${WORK}/l7.log"; then
  ok "and the tripwire log records NO engine call at all"
else
  bad "DRY RUN CALLED AN ENGINE: $(head -1 "${WORK}/l7.log")"
fi
kill "${HOLDER}" 2>/dev/null || true

# --- 8. DRY RUN MUST NOT CLAIM IT DID ANYTHING ------------------------------
# A dry run that reports "container removed" is worse than no dry run: the operator is told work happened
# that did not, on the one command whose whole purpose is to say what WOULD change.
echo ""
echo "[8] --dry-run says what it would do, never what it did"
rm -rf "${XDG}/orchicon-test" && mkdir -p "${XDG}/orchicon-test"
OUT="$(run "${WORK}/l8.log" --uninstall --instance test --dry-run)"
if printf '%s' "${OUT}" | grep -qE "would run: ORCHICON_INSTANCE=test .*stop"; then ok "reports the stop as a WOULD"; else bad "does not report the stop it would perform"; fi
if printf '%s' "${OUT}" | grep -qE "✓ *test container removed|✓ *[a-z]+ removed"; then
  bad "AND IT ALSO CLAIMS A COMPLETED REMOVAL"
else
  ok "and never claims a completed removal"
fi
if no_engine_calls "${WORK}/l8.log"; then ok "and issued no engine call"; else bad "dry run called an engine"; fi

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

# --- 13. THE KILL IS FILE-TARGETED AND INSTANCE-FILTERED ---------------------
# Two planes running from the SAME installed path, one per instance. `--instance test --uninstall` must stop
# test, leave dev ALIVE *and name it*, and leave the binaries in place. This is the regression that had a
# `pkill -9 -x orchicon` / `fuser -k "$bin"` in this step: either one takes every instance down, so an
# instance-scoped command had a machine-wide effect.
#
# A SHELL SCRIPT CANNOT STAND IN FOR THE PLANE. file_holders reads /proc/<pid>/exe, which for a script is the
# INTERPRETER (/bin/bash) — so a script-based stand-in would be reached by the wrong path entirely, and a test
# of the wrong path is how this class of bug survives. The stand-in is therefore COMPILED: it sleeps (holding
# the file as its own executable) and exits 0 on `stop`, like the real binary's stop verb.
echo ""
echo "[13] uninstalling test leaves dev's plane alive, and names it"

FP="${WORK}/fakeplane"
mkdir -p "${FP}"
cat > "${FP}/main.go" <<'GO'
package main

import (
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stop" {
		os.Exit(0)
	}
	for {
		time.Sleep(100 * time.Millisecond)
	}
}
GO
# Build to a scratch name and move it into place: `go build -o <existing non-object file>` refuses, and the
# install dir already holds a stand-in script from the sections above.
if ! ( cd "${FP}" && go mod init fakeplane >/dev/null 2>&1 && go build -o "${FP}/orchicon-built" . \
         && mv -f "${FP}/orchicon-built" "${INSTALL_DIR}/orchicon" && chmod +x "${INSTALL_DIR}/orchicon" ) \
     2>"${WORK}/fakeplane-build.log"; then
  bad "the compiled stand-in plane builds"
  tail -5 "${WORK}/fakeplane-build.log" | sed 's/^/      /'
else
  ok "the compiled stand-in plane builds (two instances, one installed path)"
  rm -rf "${XDG}/orchicon-dev" "${XDG}/orchicon-test" "${XDG}/orchicon-prod"
  mkdir -p "${XDG}/orchicon-dev/serve" "${XDG}/orchicon-test/serve"
  printf 'dev'  > "${XDG}/orchicon-dev/serve/kek"
  printf 'test' > "${XDG}/orchicon-test/serve/kek"
  # A third plane from a DIFFERENT PATH — a development checkout's bin/orchicon, the case the in-file
  # incident history names. It holds a different file, so it must be unreachable by construction.
  mkdir -p "${WORK}/devcheckout"
  cp "${INSTALL_DIR}/orchicon" "${WORK}/devcheckout/orchicon"
  ORCHICON_INSTANCE=dev  "${INSTALL_DIR}/orchicon" & DEV_PID=$!
  ORCHICON_INSTANCE=test "${INSTALL_DIR}/orchicon" & TEST_PID=$!
  ORCHICON_INSTANCE=dev  "${WORK}/devcheckout/orchicon" & OTHER_PATH_PID=$!
  sleep 1
  OUT="$(run "${WORK}/l13.log" --uninstall --instance test)"
  if ! kill -0 "${DEV_PID}" 2>/dev/null; then
    bad "the dev plane from the SAME path was killed by --instance test"
  else
    ok "the dev plane from the same installed path is still ALIVE"
  fi
  if kill -0 "${TEST_PID}" 2>/dev/null; then
    bad "the test plane survived its own uninstall"
  else
    ok "the test plane was stopped"
  fi
  if printf '%s' "${OUT}" | grep -q "still holding" && printf '%s' "${OUT}" | grep -q "(${DEV_PID}(dev))\|(dev)"; then
    ok "and the surviving process is NAMED, with its instance, rather than killed"
  else
    bad "the surviving process is not named (said: $(printf '%s' "${OUT}" | grep -a 'holding' | head -1))"
  fi
  if kill -0 "${OTHER_PATH_PID}" 2>/dev/null; then
    ok "and a plane running from a DIFFERENT path is untouched (unreachable by construction)"
  else
    bad "a plane running from a different path was killed"
  fi
  if [ -f "${INSTALL_DIR}/orchicon" ] && printf '%s' "${OUT}" | grep -qF "${INSTALL_DIR}/orchicon"; then
    ok "and the binaries are left in place, with the path named in the message"
  else
    bad "the binaries were removed (or the path was not named) while dev still needs them"
  fi
  if other_state_intact "${WORK}/l13-dev.manifest" "${XDG}/orchicon-dev" 2>/dev/null || \
     [ -f "${XDG}/orchicon-dev/serve/kek" ]; then
    ok "and dev's data dir is intact"
  else
    bad "dev's data dir was touched"
  fi
  kill "${DEV_PID}" "${OTHER_PATH_PID}" 2>/dev/null || true
  wait "${DEV_PID}" "${OTHER_PATH_PID}" 2>/dev/null || true
fi

# --- 11. THE RE-HOMED TEARDOWN: scripts/orchicon.sh uninstall ------------------
# The instance teardown used to be `scripts/instance-ports.sh uninstall <inst> --yes`,
# and that script owned the instance table. Both moved: the table is
# scripts/lib/instances.sh and the documented teardown verb is
# `scripts/orchicon.sh uninstall <inst> --yes`. The PROMISE did not move, so it is
# re-asserted here against the new entry point — instance-scoped, refused without
# confirmation, printing what it removes before it removes it, never touching the
# legacy dev/prod PG_VOLUME, and driving whichever engine ORCHICON_ENGINE names.
ORCH="${REPO_ROOT}/scripts/orchicon.sh"

# tear <logfile> <engine> <env...> -- <args...> — the real verb, with HOME
# redirected so the instance's host data dir (which holds the KEK) resolves
# INSIDE the scratch tree. That is the whole safety story: the verb is real, the
# instance it can reach is not.
tear() {
  local log="$1" engine="$2"; shift 2
  : > "${log}"
  env -u ORCHICON_CONFIRM_DESTROY \
      HOME="${WORK}/home" PATH="${BIN}:${PATH}" STUB_LOG="${log}" \
      ORCHICON_ENGINE="${engine}" ORCHICON_HOST_BIN="${WORK}/no-such-orchicon" \
      bash "${ORCH}" uninstall "$@" 2>&1
}

echo ""
echo "[11] scripts/orchicon.sh uninstall is instance-scoped, confirmed and engine-aware"
mkdir -p "${WORK}/home/.local/share/orchicon-test/serve"
printf 'kek' > "${WORK}/home/.local/share/orchicon-test/serve/kek"
mkdir -p "${WORK}/home/.local/share/orchicon-dev" "${WORK}/home/.local/share/orchicon-prod"
printf 'dev' > "${WORK}/home/.local/share/orchicon-dev/keep"
printf 'prod' > "${WORK}/home/.local/share/orchicon-prod/keep"

# (a) NO CONFIRMATION ⇒ NOTHING HAPPENS. The log must be EMPTY (not "only reads"):
# a destructive verb that inspects before refusing is already on the wrong side of
# the line the incident history draws.
OUT="$(tear "${WORK}/l11a.log" docker test)"
RC=$?
if [ "${RC}" -ne 0 ]; then ok "no --yes is refused (exit ${RC})"; else bad "no --yes was ACCEPTED"; fi
if printf '%s' "${OUT}" | grep -q "volume — DELETED"; then ok "and it prints what it would remove first"; else bad "it does not print what it would remove"; fi
if [ ! -s "${WORK}/l11a.log" ]; then ok "and issued NO engine command at all"; else bad "it ran the engine anyway: $(head -1 "${WORK}/l11a.log")"; fi
if [ -f "${WORK}/home/.local/share/orchicon-test/serve/kek" ]; then ok "and the instance's KEK is untouched"; else bad "it deleted data without confirmation"; fi

# (b) ORCHICON_CONFIRM_DESTROY=1 is the env-only confirmation form.
: > "${WORK}/l11b.log"
OUT="$(env HOME="${WORK}/home" PATH="${BIN}:${PATH}" STUB_LOG="${WORK}/l11b.log" \
       ORCHICON_ENGINE=docker ORCHICON_CONFIRM_DESTROY=1 ORCHICON_HOST_BIN="${WORK}/no-such-orchicon" \
       bash "${ORCH}" uninstall test 2>&1)"
RC=$?
if [ "${RC}" -eq 0 ]; then ok "ORCHICON_CONFIRM_DESTROY=1 proceeds (exit 0)"; else bad "ORCHICON_CONFIRM_DESTROY=1 did not proceed (exit ${RC})"; fi
if grep -q "orchicon-cnt-test-data" "${WORK}/l11b.log"; then ok "and removes the test instance's volume"; else bad "the test volume was not removed"; fi
if [ ! -e "${WORK}/home/.local/share/orchicon-test" ]; then ok "and removes the test instance's host data dir"; else bad "the host data dir survived"; fi

# (c) SCOPE: only `test`. dev/prod containers, volumes AND the legacy compose
# volumes must be absent from the engine log — the last one is the documented
# refusal: PG_VOLUME predates the single-container layout and may hold data this
# instance never owned.
LEAK="$(grep -E 'orchicon-cnt-(dev|prod)|orchicon_(prod_)?postgres-data' "${WORK}/l11b.log" 2>/dev/null | head -3 | tr '\n' ' ')"
if [ -z "${LEAK}" ]; then ok "no sibling container, volume or legacy PG volume appears"; else bad "a sibling leaked into the engine log: ${LEAK}"; fi
if [ -f "${WORK}/home/.local/share/orchicon-dev/keep" ] && [ -f "${WORK}/home/.local/share/orchicon-prod/keep" ]; then
  ok "dev and prod data dirs are untouched"
else
  bad "a sibling instance's data dir was touched"
fi

# (d) ENGINE-AWARE: pinning podman must drive podman and NOT docker.
: > "${WORK}/l11d.log"
env HOME="${WORK}/home" PATH="${BIN}:${PATH}" STUB_LOG="${WORK}/l11d.log" \
    ORCHICON_ENGINE=podman ORCHICON_CONFIRM_DESTROY=1 ORCHICON_HOST_BIN="${WORK}/no-such-orchicon" \
    bash "${ORCH}" uninstall test >/dev/null 2>&1
if grep -q "orchicon-cnt-test" "${WORK}/l11d.log"; then ok "ORCHICON_ENGINE=podman drives the engine for the teardown"; else bad "podman was not driven"; fi

# (e) AN UNKNOWN INSTANCE IS REFUSED, never defaulted: the verb this repo
# documents must not pick one for you.
OUT="$(tear "${WORK}/l11e.log" docker production)"
RC=$?
if [ "${RC}" -ne 0 ] && printf '%s' "${OUT}" | grep -q "unknown instance"; then ok "an unknown instance is refused"; else bad "an unknown instance was not refused"; fi
if [ ! -s "${WORK}/l11e.log" ]; then ok "and nothing was touched"; else bad "it acted on an instance nobody named"; fi

# --- 12. clean-docker: engine-aware, and still scoped -------------------------
# The other half of the same engine switch. The assertions are the ones the
# incident history demands: it prunes through the resolved engine, it prunes by
# our label only, and it NEVER prunes volumes — no engine has an ownership filter
# for them, so there is no predicate that makes a volume prune ours.
CD="$(make -s -n -f "${REPO_ROOT}/Makefile" clean-docker ORCHICON_ENGINE=podman 2>/dev/null)"
if printf '%s' "${CD}" | grep -q "podman image prune -f --filter \"dangling=true\""; then ok "clean-docker drives the resolved engine (podman)"; else bad "clean-docker still hardcodes docker"; fi
if printf '%s' "${CD}" | grep -q "podman container prune -f --filter \"label=orchicon-instance\""; then ok "and scopes container pruning to OUR label"; else bad "the container prune is not label-scoped"; fi
if printf '%s' "${CD}" | grep -q "volume prune"; then bad "clean-docker prunes volumes (host-wide, unanchored)"; else ok "and never prunes volumes (no ownership filter exists)"; fi
CD_NONE="$(env -u ORCHICON_ENGINE PATH="/usr/bin:/bin" make -s -n -f "${REPO_ROOT}/Makefile" clean-docker 2>/dev/null)"
if printf '%s' "${CD_NONE}" | grep -q "no container engine found"; then ok "and a missing engine is a LOUD SKIP, not a failure"; else bad "a missing engine is not handled"; fi

echo ""
if [ "${FAIL}" -eq 0 ]; then
  echo "RESULT: all ${PASS} assertion(s) passed"
  exit 0
fi
echo "RESULT: ${FAIL} assertion(s) FAILED (passing: ${PASS})"
exit 1
