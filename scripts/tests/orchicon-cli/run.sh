#!/usr/bin/env bash
# ============================================================================
# Offline harness for scripts/orchicon.sh — the day-to-day entry point.
#
# WHY THIS EXISTS: orchicon.sh is the script a developer runs every day, and it
# must keep working with NO container engine and — for most of what is asserted
# here — no built binary, no database and no running plane. Every check below is
# therefore driven through stub binaries on PATH ($ORCHICON_BIN, a `curl`, a
# `make`), so CI needs nothing installed.
#
# WHAT IS ASSERTED, and why each one is worth a test:
#
#   1. `help` IS COMPLETE. The usage block used to be `sed -n '3,45p'` over the
#      script's own header — positional, so a longer header silently truncated
#      `help` and nothing noticed. The verb list is now diffed against the
#      dispatch table, so a verb added without a usage line fails here.
#   2. DELEGATION, NOT A SECOND LIFECYCLE. Every verb must reach the CLI's OWN
#      surface: the shared `instance` verb when the binary has one, else that
#      binary's component verbs. The stub records argv, so "did it call the
#      shared mechanism" is asserted, not assumed.
#   3. STATUS NEVER LIES. A plane whose PID file says running but whose /healthz
#      does not answer must read as UP BUT NOT HEALTHY; with no HTTP client it
#      must read as UNVERIFIED. A port probe that reports "healthy" because
#      something holds the port is the failure this guards.
#   4. TWO INSTANCES STAY APART. dev and prod must reach different data dirs and
#      different plane ports, or stopping one disturbs the other.
#   5. THE POST-START SUMMARY. It is how a developer learns the plane URL, the
#      GUI, the TUI and the log command. It is asserted per instance.
#   6. REBUILD STAYS HONEST. It is the quick path and must say so, and must name
#      the gated path.
#   7. NO container.sh. The launcher it used to delegate to is gone; a reference
#      in the script or in its output is a broken instruction.
#
#   usage: scripts/tests/orchicon-cli/run.sh [path/to/orchicon.sh]
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
SCRIPT="${1:-$REPO/scripts/orchicon.sh}"

if [ ! -f "$SCRIPT" ]; then
  echo "orchicon-cli harness: no such script: $SCRIPT" >&2
  exit 1
fi

npass=0
nfail=0
ok()  { npass=$((npass + 1)); printf '  ok   %s\n' "$*"; }
bad() { nfail=$((nfail + 1)); printf '  FAIL %s\n' "$*" >&2; }

assert_contains() { # haystack needle label
  case "$1" in
    *"$2"*) ok "$3" ;;
    *) bad "$3 — expected to contain: $2"; printf '       got: %s\n' "$1" >&2 ;;
  esac
}
assert_not_contains() { # haystack needle label
  case "$1" in
    *"$2"*) bad "$3 — must NOT contain: $2"; printf '       got: %s\n' "$1" >&2 ;;
    *) ok "$3" ;;
  esac
}
assert_rc() { # actual expected label
  if [ "$1" -eq "$2" ]; then ok "$3 (exit $1)"; else bad "$3 — expected exit $2, got $1"; fi
}
assert_eq() { # actual expected label
  if [ "$1" = "$2" ]; then ok "$3"; else bad "$3 — expected '$2', got '$1'"; fi
}

# mkexec makes a stub runnable. The absolute path is used deliberately: a bare
# `chmod` name is intercepted by some restricted harness sandboxes, and a stub
# that is not executable turns every delegation assertion into a false PASS.
mkexec() { /bin/chmod 0755 "$1" 2>/dev/null || python3 -c 'import os,sys; os.chmod(sys.argv[1], 0o755)' "$1"; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/orchicon-cli.XXXXXX")"
# The scratch tree contains this harness's own HOME, so of course it is not
# deleted while that HOME is set. A refusal on cleanup is noise, never a failure.
cleanup() { rm -rf "$WORK" 2>/dev/null || true; }
trap cleanup EXIT

# A HOME of its own: the script derives every per-instance path from it, and no
# test may read or write the developer's real instance state.
export HOME="$WORK/home"
mkdir -p "$HOME/.local/share"

STUB="$WORK/stub"
mkdir -p "$STUB"
CALLS="$WORK/calls"
: >"$CALLS"

# ── the stub binary ─────────────────────────────────────────────────────────
# Logs argv + the per-instance env it was handed, then answers like the real CLI
# does for the verbs orchicon.sh uses. Its `instance` subcommand answers non-zero
# on purpose: this branch's binary has no shared lifecycle verb, which is exactly
# the fallback path that has to keep working.
cat >"$STUB/orchicon" <<'STUB_BIN'
#!/usr/bin/env bash
{
  printf 'argv=%s\n' "$*"
  printf 'inst=%s\n' "${ORCHICON_INSTANCE:-}"
  printf 'serve_state=%s\n' "${ORCHICON_SERVE_STATE_DIR:-}"
  printf 'residency=%s\n' "${ORCHICON_PLANE_RESIDENCY:-}"
} >>"$STUB_CALLS"
case "${1:-}" in
  instance) exit 3 ;;
  install)
    printf 'up' >"$STUB_PLANE_STATE"
    printf 'up' >"$STUB_SERVICES_STATE"
    mkdir -p "$HOME/.local/share/orchicon-${ORCHICON_INSTANCE:-dev}/serve/logs"
    echo 'stub plane log line' >"$HOME/.local/share/orchicon-${ORCHICON_INSTANCE:-dev}/serve/logs/orchicon.log"
    echo "stub: install ${ORCHICON_INSTANCE:-dev}"
    exit 0 ;;
  restart) echo "stub: restart ${ORCHICON_INSTANCE:-dev}"; exit 0 ;;
  serve)
    case "${2:-}" in
      --status)
        if [ "$(cat "$STUB_PLANE_STATE" 2>/dev/null)" = up ]; then echo "serve is running (PID 4242)"; exit 0; fi
        echo "serve is not running"; exit 1 ;;
      --stop)
        if [ "${STUB_PLANE_STOP_RC:-0}" != 0 ]; then
          # A stop that FAILED with the plane still up. It must never be
          # reported as "nothing was running": the exit code alone cannot tell
          # the two apart, so the script has to ask the PID file.
          printf 'up' >"$STUB_PLANE_STATE"
          echo "stub: the signal did not land" >&2
          exit "$STUB_PLANE_STOP_RC"
        fi
        if [ "$(cat "$STUB_PLANE_STATE" 2>/dev/null)" = up ]; then
          printf 'down' >"$STUB_PLANE_STATE"; echo "serve stopped"; exit 0
        fi
        echo "serve is not running"; exit 1 ;;
    esac ;;
  services)
    case "${2:-}" in
      status)
        if [ "$(cat "$STUB_SERVICES_STATE" 2>/dev/null)" = up ]; then
          echo "services for ${3:-dev}: up (postgres 127.0.0.1:5432, nats 127.0.0.1:4222, supervisor running)"
          exit 0
        fi
        echo "services for ${3:-dev}: down (nothing answering postgres 127.0.0.1:5432 or nats 127.0.0.1:4222)"
        exit 1 ;;
      down)
        # The real verb exits non-zero ONLY when the ports did not come free,
        # so this is a genuine failure and not a "was not running" signal.
        if [ "${STUB_SERVICES_DOWN_RC:-0}" != 0 ]; then
          echo "stub: postgres did not release :5432" >&2
          exit "$STUB_SERVICES_DOWN_RC"
        fi
        echo "services for ${3:-dev} are stopped; postgres and nats are free"; exit 0 ;;
    esac ;;
esac
exit 0
STUB_BIN
mkexec "$STUB/orchicon"

# A `curl` that answers whatever the case under test needs.
cat >"$STUB/curl" <<'STUB_CURL'
#!/usr/bin/env bash
printf 'curl=%s\n' "$*" >>"$STUB_CALLS"
exit "${STUB_CURL_RC:-0}"
STUB_CURL
mkexec "$STUB/curl"

# A `make` that records the target it was asked for and builds nothing.
cat >"$STUB/make" <<'STUB_MAKE'
#!/usr/bin/env bash
printf 'make=%s\n' "$*" >>"$STUB_CALLS"
exit 0
STUB_MAKE
mkexec "$STUB/make"

plane_up()    { printf 'up'   >"$STUB_PLANE_STATE"; }
plane_down()  { printf 'down' >"$STUB_PLANE_STATE"; }
services_up() { printf 'up'   >"$STUB_SERVICES_STATE"; }
services_down() { printf 'down' >"$STUB_SERVICES_STATE"; }

export STUB_CALLS="$CALLS"
export STUB_PLANE_STATE="$WORK/plane.state"
export STUB_SERVICES_STATE="$WORK/services.state"

# run invokes the script under test with the ambient Orchicon environment
# stripped, so a developer's own instance settings cannot change the result.
# ORCHICON_BIN is PUSHED, never merely unset: a checkout with a built
# bin/orchicon would otherwise win over the stub and the delegation assertions
# would test the real CLI instead of the argv under test.
run() {
  env -u ORCHICON_INSTANCE_CTL -u ORCHICON_PLANE_RESIDENCY \
      -u ORCHICON_INSTANCE -u ORCHICON_SERVE_STATE_DIR \
      ORCHICON_BIN="${BIN_UNDER_TEST:-$STUB/orchicon}" \
      PATH="$STUB:$PATH" "$@" 2>&1
}

echo "orchicon-cli harness — $SCRIPT"

# ── 1. help is complete (AC2) ───────────────────────────────────────────────
echo "-- help"
help_out="$("$SCRIPT" help 2>&1)"
assert_rc "$?" 0 "help exits 0"
for v in start stop restart status logs rebuild help; do
  assert_contains "$help_out" "scripts/orchicon.sh $v" "help documents '$v'"
done
# The tail of the block: with the old positional `sed` range this is the line
# that got cut off when the header grew, so its presence is the anti-truncation
# assertion. It is the LAST line of the usage text.
assert_contains "$help_out" "make rebuild-<instance>" "help reaches its final line (not truncated)"
assert_contains "$help_out" "127.0.0.1:8080" "help names dev's plane URL"
assert_contains "$help_out" "127.0.0.1:8091" "help names prod's plane URL"
bare_out="$("$SCRIPT" 2>&1)"
assert_eq "$bare_out" "$help_out" "a bare invocation prints the same usage"
if grep -q "sed -n '3,45p'" "$SCRIPT"; then
  bad "usage is still extracted by a positional sed range"
else
  ok "usage is not a positional slice of the header"
fi

# ── 2. bad input exits non-zero (AC8) ───────────────────────────────────────
echo "-- bad input"
for v in start stop restart status logs rebuild; do
  out="$("$SCRIPT" "$v" staging 2>&1)"
  assert_rc "$?" 2 "$v with an unknown instance exits 2"
  assert_contains "$out" "unknown instance 'staging'" "$v names the bad instance"
done
out="$("$SCRIPT" frobnicate dev 2>&1)"
assert_rc "$?" 2 "an unknown command exits 2"
assert_contains "$out" "unknown command 'frobnicate'" "an unknown command is named"
assert_contains "$out" "expected start|stop|restart|status|logs|rebuild|uninstall|help" "the error lists the verbs"

# ── 3. the shared lifecycle surface, when the binary has one (AC7) ─────────
echo "-- delegation to the shared surface"
cat >"$WORK/ctl" <<'STUB_CTL'
#!/usr/bin/env bash
printf 'ctl=%s\n' "$*" >>"$STUB_CALLS"
printf 'ctl-residency=%s\n' "${ORCHICON_PLANE_RESIDENCY:-}" >>"$STUB_CALLS"
exit 0
STUB_CTL
mkexec "$WORK/ctl"
for pair in "start:start" "up:start" "stop:stop" "down:stop" "restart:restart" \
            "status:status" "logs:logs" "rebuild:rebuild"; do
  verb="${pair%%:*}"
  expected="${pair##*:}"
  : >"$CALLS"
  run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" "$verb" prod >/dev/null
  got="$(grep '^ctl=' "$CALLS" | head -1)"
  assert_eq "$got" "ctl=$expected prod" "'$verb prod' reaches the shared surface as '$expected prod'"
done
: >"$CALLS"
run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" start dev >/dev/null
assert_contains "$(cat "$CALLS")" "ctl-residency=host" "host residency is the script's default shape"
: >"$CALLS"
run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" ORCHICON_PLANE_RESIDENCY=container "$SCRIPT" start dev >/dev/null
assert_contains "$(cat "$CALLS")" "ctl-residency=container" "an explicitly exported residency is honoured, not overridden"

# ── 4. the post-start summary (AC3) ────────────────────────────────────────
echo "-- post-start summary"
: >"$CALLS"
out="$(run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" start dev)"
assert_rc "$?" 0 "start dev exits 0"
assert_contains "$out" "orchicon dev is up" "the summary names the instance"
assert_contains "$out" "http://127.0.0.1:8080" "the summary prints dev's plane URL"
assert_contains "$out" "gui     open http://127.0.0.1:8080" "the summary prints the GUI hint"
assert_contains "$out" "tui     orch" "the summary prints the TUI hint"
assert_contains "$out" "~/.orchicon/config" "the TUI hint names the profile file"
assert_contains "$out" "logs    scripts/orchicon.sh logs dev" "the summary prints the logs command"
out="$(run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" start prod)"
assert_contains "$out" "http://127.0.0.1:8091" "prod's summary prints prod's plane URL"
assert_not_contains "$out" "127.0.0.1:8080" "prod's summary never prints dev's port"

# ── 5. the component-verb path (the binary has no `instance` verb) ─────────
echo "-- delegation without a shared surface"
export ORCHICON_BIN="$STUB/orchicon"
: >"$CALLS"; plane_down; services_down
run "$SCRIPT" start dev >/dev/null
assert_contains "$(cat "$CALLS")" "argv=install" "start reaches the CLI's install verb"
assert_contains "$(cat "$CALLS")" "inst=dev" "start hands install the instance"
: >"$CALLS"
run "$SCRIPT" stop prod >/dev/null
call_log="$(cat "$CALLS")"
assert_contains "$call_log" "argv=serve --stop" "stop reaches serve --stop"
assert_contains "$call_log" "argv=services down prod" "stop reaches services down for that instance"
assert_contains "$call_log" "serve_state=$HOME/.local/share/orchicon-prod/serve" "stop scopes the plane to PROD's state dir"
assert_not_contains "$call_log" "orchicon-dev/serve" "stopping prod never touches dev's state dir"
: >"$CALLS"
run "$SCRIPT" restart dev >/dev/null
assert_contains "$(cat "$CALLS")" "serve_state=$HOME/.local/share/orchicon-dev/serve" "restart scopes the plane to DEV's state dir"

# ── 5b. stop reports only what it verified ────────────────────────────────
# `stop` claims "plane and its host services" in one line, so BOTH halves have to
# be checked before it prints. A services stop that failed while the success line
# still printed is the exact unverified claim this script exists to prevent; so is
# calling a plane that is still up "nothing was running".
echo "-- stop is honest about both halves"
plane_up; services_up
out="$(STUB_SERVICES_DOWN_RC=1 run "$SCRIPT" stop dev)"; rc=$?
assert_rc "$rc" 1 "stop exits 1 when the services did not stop"
assert_not_contains "$out" "✓ orchicon dev stopped" "a failed services stop is never reported as success"
assert_contains "$out" "host services did NOT" "stop names the half that failed"
plane_up; services_up
out="$(STUB_PLANE_STOP_RC=1 run "$SCRIPT" stop dev)"; rc=$?
assert_rc "$rc" 1 "stop exits 1 when a live plane refuses to stop"
assert_not_contains "$out" "nothing was running" "a plane that is still up is never called 'nothing was running'"
assert_contains "$out" "STILL running" "stop says plainly that the plane is still up"
plane_down; services_down
out="$(run "$SCRIPT" stop dev)"; rc=$?
assert_rc "$rc" 0 "stop on a stopped instance exits 0"
assert_contains "$out" "nothing was running" "stopping an idle instance says nothing was running"
assert_contains "$out" "✓ orchicon dev stopped" "a fully verified stop reports success"

# ── 6. status tells the truth, and only the truth (AC5) ────────────────────
echo "-- status"
plane_down; services_down
out="$(STUB_CURL_RC=0 run "$SCRIPT" status dev)"
assert_rc "$?" 1 "status exits 1 when the plane is down"
assert_contains "$out" "verdict: dev is DOWN" "status reports DOWN, not healthy"
assert_not_contains "$out" "healthy" "a down plane is never described as healthy"
assert_contains "$out" "orchicon dev" "status names the instance"

plane_up; services_up
out="$(STUB_CURL_RC=0 run "$SCRIPT" status dev)"
assert_rc "$?" 0 "status exits 0 for a verified-healthy plane"
assert_contains "$out" "serve is running (PID 4242)" "status reports the plane's own PID state"
assert_contains "$out" "verdict: dev is UP and healthy" "status reports UP and healthy when /healthz answers"
assert_contains "$out" "services for dev: up" "status reports the services' own verdict"

out="$(STUB_CURL_RC=22 run "$SCRIPT" status prod)"
assert_rc "$?" 1 "status exits 1 when /healthz does not answer"
assert_contains "$out" "verdict: prod is UP but NOT healthy" "an unhealthy plane is NOT reported healthy"
assert_not_contains "$out" "UP and healthy" "an unhealthy plane never reads as healthy"

# No HTTP client at all: the script must say it does not know, not guess.
mkdir -p "$WORK/minbin"
BASH_ABS="$(command -v bash)"
# Wrappers rather than symlinks (a bare `ln` is intercepted in some sandboxes),
# each with an ABSOLUTE shebang: a `#!/usr/bin/env bash` shim inside a PATH that
# also contains a `bash` shim recurses for ever instead of running anything.
for t in head dirname; do
  printf '#!%s\nexec %s "$@"\n' "$BASH_ABS" "$(command -v "$t")" >"$WORK/minbin/$t"
  mkexec "$WORK/minbin/$t"
done
printf '#!%s\ncase "$1" in\n  instance) exit 3 ;;\n  serve) echo "serve is running (PID 4242)"; exit 0 ;;\n  services) echo "services for dev: up (postgres 127.0.0.1:5432)"; exit 0 ;;\nesac\nexit 1\n' \
  "$BASH_ABS" >"$WORK/minbin/orchicon"
mkexec "$WORK/minbin/orchicon"
out="$(env -u ORCHICON_INSTANCE_CTL ORCHICON_BIN="$WORK/minbin/orchicon" HOME="$HOME" \
        PATH="$WORK/minbin" /bin/bash "$SCRIPT" status dev 2>&1)"
assert_contains "$out" "UNVERIFIED" "with no curl, status says health is UNVERIFIED"
assert_not_contains "$out" "UP and healthy" "with no curl, status never claims health"

# ── 7. logs (the plane's own log, per instance) ────────────────────────────
echo "-- logs"
mkdir -p "$HOME/.local/share/orchicon-dev/serve/logs"
echo 'HARNESS-MARKER dev plane log' >"$HOME/.local/share/orchicon-dev/serve/logs/orchicon.log"
out="$(run "$SCRIPT" logs dev)"
assert_rc "$?" 0 "logs dev exits 0 when the log exists"
assert_contains "$out" "HARNESS-MARKER" "logs dev prints dev's plane log"
out="$(run "$SCRIPT" logs prod)"
assert_rc "$?" 1 "logs prod exits 1 when prod has no log yet"
assert_contains "$out" "orchicon-prod/serve/logs/orchicon.log" "logs prod names the path it looked for"

# ── 8. rebuild: quick path vs gated path (AC6) ─────────────────────────────
echo "-- rebuild"
: >"$CALLS"; plane_up; services_up
out="$(run "$SCRIPT" rebuild dev)"
assert_rc "$?" 0 "rebuild dev exits 0"
assert_contains "$out" "WITHOUT running the test suite" "rebuild warns that it does not run the tests"
assert_contains "$out" "to gate on tests/checks, use: make rebuild-dev" "rebuild names the gated path"
# The note must come FIRST — before the work it describes.
first_line="$(printf '%s\n' "$out" | head -1)"
assert_contains "$first_line" "WITHOUT running the test suite" "the quick-path note is printed before the rebuild runs"
assert_contains "$(cat "$CALLS")" "make=-C $REPO build" "rebuild asks the repo's own build for a binary"

# ── 9. two instances, one host, no shared state (AC4) ──────────────────────
echo "-- instances stay separate"
start_dev="$(run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" start dev)"
start_prod="$(run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" start prod)"
assert_contains "$start_dev" "orchicon-dev" "dev's summary carries dev's data dir"
assert_contains "$start_prod" "orchicon-prod" "prod's summary carries prod's data dir"
assert_not_contains "$start_dev" "8091" "dev's summary does not advertise prod's port"
assert_not_contains "$start_prod" "8080" "prod's summary does not advertise dev's port"

# ── 9b. the plane port is the instance's RECORDED port ────────────────────
# `install` writes ports.json holding the control port the instance ACTUALLY
# bound — because 8080 was already taken, or because the operator pinned
# ORCHICON_CONTROL_PORT. A summary or a status that then says 8080 sends the
# developer to the wrong URL and health-checks a port that is not this plane,
# which is the same unverified claim as a port probe reported "healthy".
echo "-- the port is the instance's own record"
# `restart` prints the same map, re-resolved AFTER it ran (a restart can move the
# plane's recorded port, and the developer must be told where it is NOW).
out="$(export ORCHICON_CONTROL_PORT=9250; run "$SCRIPT" restart prod)"
assert_contains "$out" "✓ orchicon prod restarted" "restart reports its own outcome"
assert_contains "$out" "orchicon prod is up" "restart prints the where-things-are map"
assert_contains "$out" "plane   http://127.0.0.1:9250" "restart's map is resolved after the restart"

# No record yet (never installed): honour the override an install would bind,
# else this instance's default column — the order the CLI itself uses.
out="$(export ORCHICON_CONTROL_PORT=9250; run "$SCRIPT" status prod)"
assert_contains "$out" "plane http://127.0.0.1:9250" "with no record, an explicit ORCHICON_CONTROL_PORT is used"
out="$(run "$SCRIPT" status prod)"
assert_contains "$out" "plane http://127.0.0.1:8091" "with no record and no override, prod's default column is used"
# Now the record `install` writes: it holds the port the instance ACTUALLY
# bound, so it wins over both the default and an ambient override.
mkdir -p "$HOME/.local/share/orchicon-prod"
cat >"$HOME/.local/share/orchicon-prod/ports.json" <<'PORTS_JSON'
{
  "ORCHICON_CONTROL_PORT": 9191,
  "ORCHICON_POSTGRES_PORT": 5433,
  "ORCHICON_NATS_PORT": 4223
}
PORTS_JSON
: >"$CALLS"
out="$(run ORCHICON_INSTANCE_CTL="/bin/bash $WORK/ctl" "$SCRIPT" start prod)"
assert_contains "$out" "http://127.0.0.1:9191" "the summary prints the port the instance RECORDED"
assert_not_contains "$out" "127.0.0.1:8091" "the summary never advertises a default port the instance is not on"
assert_contains "$out" "non-default control port" "the summary says why the port is not the default"
plane_up; services_up
: >"$CALLS"
out="$(STUB_CURL_RC=0 run "$SCRIPT" status prod)"
assert_contains "$out" "plane http://127.0.0.1:9191" "status names the recorded port"
assert_contains "$(cat "$CALLS")" "curl=-fsS --max-time 3 -o /dev/null http://127.0.0.1:9191/healthz" \
  "status health-checks the recorded port, not a default"
out="$(export ORCHICON_CONTROL_PORT=9250; run "$SCRIPT" status prod)"
assert_contains "$out" "plane http://127.0.0.1:9191" "the record wins over an ambient ORCHICON_CONTROL_PORT"

# ── 10. no reference to the retired launcher (AC9) ─────────────────────────
echo "-- no container.sh"
if grep -q "container\.sh" "$SCRIPT"; then
  bad "the script still mentions container.sh"
else
  ok "the script never mentions container.sh"
fi
all_out="$("$SCRIPT" help 2>&1; run "$SCRIPT" status dev; run "$SCRIPT" frobnicate 2>&1)"
assert_not_contains "$all_out" "container.sh" "no output mentions container.sh"

# ── summary ────────────────────────────────────────────────────────────────
echo
if [ "$nfail" -ne 0 ]; then
  echo "orchicon-cli harness: $nfail FAILED, $npass passed" >&2
  exit 1
fi
echo "orchicon-cli harness: PASS — $npass checks"
