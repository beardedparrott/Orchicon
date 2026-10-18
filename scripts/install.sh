#!/usr/bin/env bash
# ============================================================================
# Orchicon installer (Linux / macOS)
#
# Usage:
#   curl -fsSL https://orchicon.dev/install | bash
#   curl -fsSL https://orchicon.dev/install | bash -s -- --version v0.2.0
#   curl -fsSL https://orchicon.dev/install | bash -s -- --install-dir /usr/local/bin
#   curl -fsSL https://orchicon.dev/install | bash -s -- --uninstall
#   curl -fsSL https://orchicon.dev/install | bash -s -- --clean
#   curl -fsSL https://orchicon.dev/install | bash -s -- --force-clean
#
# This script downloads the latest (or specified) Orchicon release binary
# from GitHub and installs it to the chosen directory. It detects OS and
# architecture automatically. Re-running the script updates to the latest
# release.
#
# For Windows, see scripts/install.ps1 or:
#   irm https://orchicon.dev/install.ps1 | iex
# ============================================================================
set -euo pipefail

# --- Defaults ---------------------------------------------------------------
GITHUB_OWNER="beardedparrott"
GITHUB_REPO="Orchicon"
# WHERE THE RELEASE ASSETS COME FROM, overridable on purpose.
#
# The default is the published release, which is what an operator gets. The override is what makes the
# install path TESTABLE: scripts/tests/install-path serves a locally-built artifact and runs this very
# script against it, end to end — download, extract, install, verify — with no release, no network and no
# Docker involved. It is also the hook for a mirror, or for pointing an install at a pre-release channel
# (`--version edge`, whose assets the default base already serves).
ASSET_BASE_URL="${ORCHICON_INSTALL_BASE_URL:-https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}/releases/download}"
INSTALL_DIR="${ORCHICON_INSTALL_DIR:-${HOME}/.local/bin}"
# WHICH STACK THIS ACTS ON. dev / test / prod are three INSTANCES of the same product on one host:
# separate container, separate ports, separate data volume, separate host state. Every instance-scoped
# action below — install, clean, force-clean, uninstall — is scoped to THIS one, and nothing else.
#
# IT IS A FLAG AND NOT ONLY THE ENV VAR, which is not sugar: the documented invocation is
# `curl … | bash -s -- …`, where an exported variable reaches CURL rather than BASH. Passing
# ORCHICON_INSTANCE=test for the shell side means rewriting the whole one-liner.
INSTANCE="${ORCHICON_INSTANCE:-dev}"
VERSION=""
PURGE=false   # --uninstall --purge: also destroy the data
UNINSTALL=false
REMOVE_BINARY=false      # --remove-binary: the executables only, and they are MACHINE-WIDE
ASSUME_YES=false         # --yes: answer the prompts affirmatively (non-interactive use)
INSTANCE_EXPLICIT=false  # set when --instance was passed, which SKIPS the instance prompt
BINARY_HELD_ELSEWHERE=false  # set when a kill declined to touch another instance's process
CLEAN=false
FORCE_CLEAN=false
DRY_RUN=false
SETUP=true
INSTALL_ORCH=true  # orch = the thin remote TUI client (skippable via --no-orch)

# --- Colors -----------------------------------------------------------------
if [ -t 1 ]; then
  B='\033[1m'; C='\033[36m'; G='\033[32m'; Y='\033[33m'; R='\033[31m'; D='\033[2m'; X='\033[0m'
else
  B=''; C=''; G=''; Y=''; R=''; D=''; X=''
fi

# ok_dry "<done>" "<would>": report what HAPPENED, or what WOULD have happened under --dry-run.
#
# A dry run that says "container removed" is worse than no dry run: the whole point is to be told what
# would change, and a destructive command reporting completed work it did not do is the message an
# operator is most likely to act on. Found by running it.
ok_dry() { if [ "$DRY_RUN" = true ]; then ok "would $2"; else ok "$1"; fi }
info()  { echo -e "${C}▸${X} $*"; }
ok()    { echo -e "${G}✓${X} $*"; }
warn()  { echo -e "${Y}!${X} $*"; }
err()   { echo -e "${R}✗${X} $*" >&2; }
die()   { err "$*"; exit 1; }

# --- Parse args -------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --version|-v)      VERSION="$2"; shift 2 ;;
    --install-dir|-d)  INSTALL_DIR="$2"; shift 2 ;;
    --instance|-i)     INSTANCE="$2"; INSTANCE_EXPLICIT=true; shift 2 ;;
    --uninstall)       UNINSTALL=true; shift ;;
    --remove-binary)   REMOVE_BINARY=true; shift ;;
    --purge)           PURGE=true; UNINSTALL=true; shift ;;  # purge IS an uninstall that destroys data
    --yes|-y)          ASSUME_YES=true; shift ;;
    --clean)           CLEAN=true; shift ;;
    --force-clean|--nuke|-f) FORCE_CLEAN=true; shift ;;
    --dry-run)         DRY_RUN=true; shift ;;
    --no-setup)        SETUP=false; shift ;;
    --no-orch)         INSTALL_ORCH=false; shift ;;
    --help|-h)
      # QUOTED DELIMITER: this is a usage TEXT, so nothing in it may be expanded or EXECUTED. It was
      # `<<EOF`, and the backticked phrases in it were therefore COMMAND SUBSTITUTIONS — so
      # `install.sh --help` ran `` `orchicon install` `` for real on any host that has one on PATH,
      # inlining that output into the usage. Printing help must not install anything.
      cat <<'EOF'
Orchicon installer

Usage: install.sh [options]

Options:
  --version <tag>      Install a specific version (e.g. v0.2.0). Default: latest.
  --install-dir <dir>  Installation directory (default: ~/.local/bin).
  --instance <name>    Which instance to act on: dev (default), test, or prod. Each has
                       its own container, ports, data volume and host state, so
                       `--instance test` cannot disturb dev or prod. Applies to install AND
                       to --clean/--force-clean/--uninstall.
    --uninstall          Stop everything THIS instance runs - its plane and its container. The
                         executables are removed ONLY when no other instance still needs them,
                         because one install directory serves every instance. DATA PRESERVED.
                         ASKS WHICH INSTANCE unless --instance is given.
    --purge              The same, and ALSO destroy this instance's data: its Docker volume and
                         its host state directory. Irreversible. ASKS WHICH INSTANCE.
    --remove-binary      Just the orchicon and orch executables. Every container, volume and
                         instance's state is left alone. If something is running off them it
                         ASKS before stopping it, and declining changes nothing. ASKS WHICH
                         INSTANCE.
    --yes, -y            Answer the prompts affirmatively, for non-interactive use.
    --clean              Stop THIS instance's container, remove the old Orchicon
                         binary, then install the latest version — one-shot upgrade. Data is
                         preserved (Docker volume, BlobStore files, runtime state).
    --force-clean, --nuke
                         Wipe and start fresh for THIS instance: stop it, destroy its Docker
                         volume (database, NATS, Tempo, Loki, VictoriaMetrics, Grafana), remove
                         blob store data and runtime state, then install the latest version.
                         WARNING: all data for THIS instance is lost.
   --dry-run            Print what would happen without making changes.
   --no-setup           Install the binary only — do NOT pull images, start the
                        runtime daemon, or launch the container (default: full
                        one-command setup runs `orchicon install`).
  -h, --help           Show this help.
EOF
      exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

# --- Instance scoping -------------------------------------------------------
#
# ONE INSTANCE, NAMED ONCE, USED EVERYWHERE. The instance was already honoured by the Go installer
# and by stop_installed_plane, but three cleanup sites spelled the names out as
# `orchicon-cnt-dev orchicon-cnt-prod` — so `--clean` removed BOTH containers and `--force-clean`
# destroyed BOTH data volumes. The help text already said those flags were about "dev containers" and
# "the dev stack"; the code was the part that was wrong, and on a host with prod data that is a
# data-loss bug rather than an inconvenience.
#
# So the names are DERIVED here, once, and nothing below spells them out again.
cnt_name() { echo "orchicon-cnt-${INSTANCE}"; }
vol_name() { echo "orchicon-cnt-${INSTANCE}-data"; }
# The host-side state dir for this instance (secrets KEK, blob store, plane PID and logs).
inst_state_dir() { echo "${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon-${INSTANCE}"; }

# AN UNKNOWN INSTANCE IS REFUSED RATHER THAN DEFAULTED, because the alternative is acting on the wrong
# stack: `--instance production` silently becoming `dev` would clean up dev while the operator watched
# prod. The set is the one the installer port table defines (cmd/orchicon/install_ports.go).
validate_instance() {
  case "$INSTANCE" in
    dev|test|prod) ;;
    *) die "unknown instance: '$INSTANCE' (known: dev, test, prod)" ;;
  esac
}
# Validate, and PUBLISH THE CHOICE so the installer this script calls picks the same instance instead
# of defaulting to dev. This is the coupling that makes `--instance test` mean one thing end to end.
validate_instance
export ORCHICON_INSTANCE="$INSTANCE"

# --- Prompts, and what each instance currently has --------------------------
#
# PROMPTS READ THE CONTROLLING TERMINAL, NEVER STDIN. The documented invocation is
# `curl … | bash -s -- …`, so BASH'S STDIN IS THE SCRIPT ITSELF — a read from stdin would consume the
# rest of the installer instead of an answer. The Go port prompt learned this the hard way
# (cmd/orchicon/install_ports.go); this is the same rule, for the same reason.
prompt_tty() {
  { printf %s "$1" > /dev/tty; } 2>/dev/null || printf %s "$1"
  local line=""
  if [ -r /dev/tty ]; then
    # A FAILED READ MUST BE DISTINGUISHABLE FROM AN EMPTY ANSWER. "No terminal" and "the operator pressed
    # Enter" both produce an empty string, and conflating them is how the instance prompt silently chose
    # dev with no terminal attached — measured, before this returned a status. The caller refuses on
    # failure and takes the default only on a real, empty answer.
    IFS= read -r line < /dev/tty 2>/dev/null || return 1
  elif [ -t 0 ]; then
    IFS= read -r line || return 1
  else
    return 1
  fi
  printf '%s' "$line"
  return 0
}

have_tty() { (exec 3<>/dev/tty) 2>/dev/null; }   # OPEN it: `-r` says yes where opening fails

ask_yes_no() {   # ask_yes_no "<question>" -> 0 yes, 1 no
  if [ "$ASSUME_YES" = true ]; then info "$1 — yes (--yes)"; return 0; fi
  if ! have_tty; then
    warn "cannot ask (no terminal): $1"
    warn "  re-run with --yes to answer affirmatively, or --instance to skip the instance prompt"
    return 1
  fi
  local a
  if ! a="$(prompt_tty "  $1 [y/N] ")"; then
    warn "cannot ask (no terminal answered): $1"
    return 1
  fi
  case "$a" in [yY]|[yY][eE][sS]) return 0 ;; *) return 1 ;; esac
}

# WHAT AN INSTANCE CURRENTLY HAS, so the choice below is informed rather than blind.
instance_state() {
  local inst="$1" out=""
  if command -v docker >/dev/null 2>&1 && docker inspect "orchicon-cnt-$inst" >/dev/null 2>&1; then
    out="container"
  fi
  if [ -d "${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon-$inst" ]; then out="${out:+$out + }state"; fi
  printf '%s' "${out:-nothing}"
}

# ASK WHICH INSTANCE, AT THE MOMENT OF THE DESTRUCTIVE ACTION.
#
# The alternative — inferring it from ORCHICON_INSTANCE with a `dev` default — is how an operator ends
# up cleaning the wrong stack: the variable is invisible, and a shell started from inside a running
# plane INHERITS that plane's value. So the three destructive verbs ask. `--instance` skips the
# question; with no terminal and no flag it refuses rather than guessing.
ask_instance() {
  # ASK_INSTANCE_FALLBACK: WHAT AN EMPTY ANSWER MEANS.
  #
  # It differs BY VERB, and the difference is the point. For the DESTRUCTIVE verbs (uninstall, purge,
  # remove-binary) there is NO fallback: an empty answer refuses, because a reflexive Enter must never
  # select a stack to destroy. The PowerShell side already refused, so the two disagreed (measured: a bare
  # Enter in the shell picked dev and acted on it). For INSTALL there IS one, because installing to the
  # documented default is not destructive and a first run should not demand a decision it does not need.
  local fallback="${1:-}"
  if [ "$INSTANCE_EXPLICIT" = true ]; then validate_instance; export ORCHICON_INSTANCE="$INSTANCE"; return 0; fi
  if ! have_tty; then
    if [ -n "$fallback" ]; then
      INSTANCE="$fallback"; validate_instance; export ORCHICON_INSTANCE="$INSTANCE"
      info "no terminal to ask on — using '${INSTANCE}'"
      return 0
    fi
    die "which instance? pass --instance dev|test|prod (there is no terminal to ask on)"
  fi
  echo "" >&2
  echo -e "  ${B}Which instance?${X}" >&2
  echo -e "  ${D}An instance is a whole stack: its own container, ports, data volume and host state.${X}" >&2
  local n=1 inst
  for inst in dev test prod; do
    echo -e "    ${C}${n}${X}) ${B}${inst}${X}  ${D}($(instance_state "$inst"))${X}" >&2
    n=$((n + 1))
  done
  local hint; hint="${fallback:+ [${fallback}]}"
  local a
  if ! a="$(prompt_tty "  choose 1-3 or a name${hint}: ")"; then
    if [ -n "$fallback" ]; then INSTANCE="$fallback"; validate_instance; export ORCHICON_INSTANCE="$INSTANCE"; return 0; fi
    die "no terminal to ask on — pass --instance dev|test|prod"
  fi
  case "$a" in
    "")     if [ -n "$fallback" ]; then INSTANCE="$fallback"; else die "no instance chosen — pass --instance dev|test|prod"; fi ;;
    1|dev)  INSTANCE=dev ;;
    2|test) INSTANCE=test ;;
    3|prod) INSTANCE=prod ;;
    *) die "not an instance: '$a' (known: dev, test, prod)" ;;
  esac
  validate_instance
  export ORCHICON_INSTANCE="$INSTANCE"
  echo "" >&2
  info "acting on instance: ${INSTANCE}  ($(instance_state "$INSTANCE"))"
}

# --- The binaries, and why they are machine-wide ----------------------------
#
# ONE INSTALL DIRECTORY SERVES EVERY INSTANCE. The executables are not per-instance, so removing them
# while another instance's plane — or the SHARED runtime daemon, which serves all of them — is running
# from that path leaves a process on a deleted inode: a server that answers and cannot work. That is the
# bug the operator reported, and it is why nothing here removes a binary without first establishing who
# is using it.
binary_paths() {
  printf '%s\n%s\n' "$INSTALL_DIR/orchicon" "$INSTALL_DIR/orch"
}

# file_holders <path> prints the pids of the processes holding <path> — executing it, or with it open.
#
# PORTABILITY IS THE POINT, and it is the lesson scripts/install-local.sh learned for the same step:
# `fuser` is Linux-only (psmisc) and ABSENT ON macOS BY DEFAULT, and it is missing from minimal Linux
# images too. The old single-tool check returned "nobody holds it" when the TOOL was missing — which is
# exactly how a live binary gets removed out from under a running plane, the failure this exists to
# prevent. So the order is fuser -> lsof (macOS + most Linux) -> /proc (Linux with neither), and a
# missing tool now means "ask the next one", never "nobody".
file_holders() {
  local bin="$1" target fd link pid
  [ -e "$bin" ] || return 0
  if command -v fuser >/dev/null 2>&1; then
    fuser "$bin" 2>/dev/null || true
    return 0
  fi
  if command -v lsof >/dev/null 2>&1; then
    lsof -t -- "$bin" 2>/dev/null || true
    return 0
  fi
  [ -d /proc ] || return 0
  target="$(readlink -f "$bin" 2>/dev/null || printf '%s' "$bin")"
  for pid in /proc/[0-9]*; do
    pid="${pid#/proc/}"
    if [ "$pid" = "$$" ]; then continue; fi
    if [ "$(readlink "/proc/$pid/exe" 2>/dev/null || true)" = "$target" ]; then
      printf '%s\n' "$pid"
      continue
    fi
    for fd in "/proc/$pid/fd/"*; do
      link="$(readlink "$fd" 2>/dev/null || true)"
      if [ -n "$link" ] && [ "$link" = "$target" ]; then
        printf '%s\n' "$pid"
        break
      fi
    done
  done
}

# binary_users <bin> prints "pid:instance" for every process holding it. The instance field is EMPTY for
# a process that carries none, which is the case for the shared runtime daemon.
binary_users() {
  local bin="$1" pid inst
  [ -e "$bin" ] || return 0
  for pid in $(file_holders "$bin"); do
    inst="$(tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | sed -n 's/^ORCHICON_INSTANCE=//p' | head -1)"
    printf '%s:%s\n' "$pid" "$inst"
  done
}

# all_binary_users aggregates both executables, deduped.
all_binary_users() {
  local bin out=""
  for bin in $(binary_paths); do
    out="$out$(binary_users "$bin")"$'\n'
  done
  printf '%s' "$out" | sed '/^$/d' | sort -u
}

# instance_exists <name> — a container or host state means the instance is real.
instance_exists() {
  local inst="$1" dir
  if command -v docker >/dev/null 2>&1 && docker inspect "orchicon-cnt-$inst" >/dev/null 2>&1; then return 0; fi
  dir="${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon-$inst"
  [ -d "$dir" ] || return 1
  # A STATE DIR CONTAINING ONLY `ports.json` IS NOT AN INSTANCE. The installer records the ports it bound
  # there (cmd/orchicon writeResolvedPorts), and that record alone must not make an instance look present:
  # if it did, --uninstall would conclude another instance still needs the executables and leave them
  # behind for ever. Anything ELSE in the directory is the instance's own state and does count.
  [ -n "$(find "$dir" -mindepth 1 -maxdepth 1 ! -name ports.json -print -quit 2>/dev/null)" ]
}

# other_instances_remain — is any instance other than this one still present?
other_instances_remain() {
  local inst
  for inst in dev test prod; do
    [ "$inst" = "$INSTANCE" ] && continue
    instance_exists "$inst" && return 0
  done
  return 1
}

# stop_service_for <instance> stops ONE instance's plane and container. Used only after the operator has
# explicitly agreed to stop services.
stop_service_for() {
  local inst="$1" bin="$INSTALL_DIR/orchicon"
  local sd="${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon-$inst/serve"
  if [ -f "$bin" ]; then
    if [ "$DRY_RUN" = false ]; then ORCHICON_SERVE_STATE_DIR="$sd" "$bin" serve --stop >/dev/null 2>&1 || true; else printf '  (would stop the %s plane)\n' "$inst"; fi
  fi
  if command -v docker >/dev/null 2>&1; then
    if [ "$DRY_RUN" = false ]; then
      docker rm -f "orchicon-cnt-$inst" >/dev/null 2>&1 && printf 'removed container orchicon-cnt-%s\n' "$inst" || true
    else
      printf '  (would remove container orchicon-cnt-%s)\n' "$inst"
    fi
  fi
}

# remove_binaries removes both executables. It does NOT stop anything: the caller must have settled who
# is using them first, because that is the part that can break a running system.
remove_binaries() {
  local bin
  for bin in $(binary_paths); do
    if [ -f "$bin" ]; then
      info "removing $bin"
      $DRY_RUN || rm -f "$bin"
      ok_dry "${bin##*/} removed" "${bin##*/} be removed"
    else
      warn "not found: $bin (nothing to remove)"
    fi
  done
}

# --- Helpers ---
detect_os() {
  local os; os="$(uname -s)"
  case "$os" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       die "unsupported OS: $os (use install.ps1 on Windows)" ;;
  esac
}

detect_arch() {
  local arch; arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *)              die "unsupported architecture: $arch" ;;
  esac
}

check_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# stop_installed_plane stops the control plane that runs the INSTALLED binary,
# and frees that file so it can be replaced (mv) or removed (rm).
#
# IT MUST NEVER KILL BY PROCESS NAME, and that is the whole point of this
# function existing. It used to run `pkill -9 -x orchicon`, which matches the
# executable BASENAME — so an uninstall of the INSTALLED copy also killed any
# other Orchicon plane on the machine, including instances running from a
# DIFFERENT path (a development checkout's bin/orchicon). On the host this was
# written for, `scripts/install.sh --uninstall` would have SIGKILLed the dev and
# prod planes, which run from bin/orchicon, leaving their work items mid-flight.
#
# The job is narrower than "stop Orchicon": it is "release THIS file". An old
# process holding the binary's mmap is what makes the mv/rm fail with "Text file
# busy". The check targets the FILE (file_holders), so a plane running from any
# other path is unreachable by CONSTRUCTION rather than by naming convention —
# this is the same technique scripts/install-local.sh already uses for exactly
# this step.
#
# It never degrades to a name match: killing the wrong process is worse than a
# warning. Where not even a file-level check is possible (no fuser, no lsof, no
# /proc) it declines to act and says so.
stop_installed_plane() {
  local bin="$INSTALL_DIR/orchicon"
  [ -f "$bin" ] || return 0

  # Best effort, and only when the instance's own state dir can be derived: the
  # PID file is PER INSTANCE (scripts/container.sh passes the state dir as
  # ORCHICON_SERVE_STATE_DIR), so a bare `serve --stop` would look in the
  # cwd-relative default `.dev` and miss a host-resident plane.
  local inst="${ORCHICON_INSTANCE:-dev}"
  local state_dir="${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon-${inst}/serve"
  if [ "$DRY_RUN" = false ]; then
    ORCHICON_SERVE_STATE_DIR="$state_dir" "$bin" serve --stop 2>/dev/null \
      || warn "serve --stop found no running plane for instance '${inst}' (ignoring)"
  else
    echo -e "  ${D}(would run: ORCHICON_SERVE_STATE_DIR=${state_dir} ${bin} serve --stop)${X}"
  fi

  # KILL ONLY THIS INSTANCE'S PROCESSES, NEVER THE FILE'S OTHER USERS.
  #
  # This used to be `fuser -k "$bin"`, which kills EVERY process holding the executable regardless of
  # which instance it belongs to. With two instances running from one installed path, `--instance test
  # --uninstall` therefore took dev and prod's planes down with it — an instance-scoped command with a
  # machine-wide effect. (It was not firing only because the layouts happened to differ.)
  #
  # The instance is in each process's environment, so the decision reads it instead of guessing: ours are
  # killed, and anything else is NAMED AND LEFT ALONE. The shared runtime daemon carries no instance — it
  # serves all of them — so it is left too, and the caller is told the binaries may still be in use.
  binary_users "$bin" || true
  local ours="" others=""
  while IFS=: read -r _pid _inst; do
    [ -n "$_pid" ] || continue
    if [ -z "$_inst" ] || [ "$_inst" = "$inst" ]; then ours="$ours $_pid"; else others="$others $_pid($_inst)"; fi
  done <<EOF
$(binary_users "$bin")
EOF
  if [ -n "${ours# }" ]; then
    if [ "$DRY_RUN" = false ]; then
      # shellcheck disable=SC2086
      kill $ours 2>/dev/null && ok "stopped this instance's process(es) holding ${bin}" || true
    else
      echo -e "  ${D}(would kill${ours})${X}"
    fi
  fi
  if [ -n "${others# }" ]; then
    warn "other processes are still holding ${bin}:${others}"
    warn "  left running on purpose — they belong to other instances, and stopping them is not this"
    warn "  command's to do. The binary will be left in place while they need it."
    BINARY_HELD_ELSEWHERE=true
  fi
}

# --- Uninstall --------------------------------------------------------------
do_uninstall() {
  # WHICH INSTANCE, ASKED AT THE MOMENT OF THE DESTRUCTIVE ACTION. Inferring it from ORCHICON_INSTANCE
  # with a `dev` default is how an operator cleans the wrong stack: the variable is invisible, and a
  # shell started from inside a running plane inherits that plane's value. --instance skips the question.
  ask_instance

  local bin="$INSTALL_DIR/orchicon"

  # 1. THIS INSTANCE'S PLANE, through its own state dir (and only its own processes — see
  #    stop_installed_plane).
  stop_installed_plane

  # 2. THIS INSTANCE'S CONTAINER. The old uninstall never touched it, so the services stack kept running
  #    after an "uninstall".
  if command -v docker >/dev/null 2>&1; then
    info "removing the ${INSTANCE} container…"
    $DRY_RUN || docker rm -f "$(cnt_name)" >/dev/null 2>&1 || true
    ok_dry "${INSTANCE} container removed ($(cnt_name))" "remove the ${INSTANCE} container ($(cnt_name))"
  fi

  # 3. DATA, only when asked. Destroying a database is not something "remove the software" may imply, so
  #    the destructive half is --purge and it says so plainly.
  if [ "$PURGE" = true ]; then
    if command -v docker >/dev/null 2>&1; then
      info "removing the ${INSTANCE} data volume…"
      $DRY_RUN || docker volume rm "$(vol_name)" >/dev/null 2>&1 || true
      ok_dry "data volume removed ($(vol_name))" "remove the data volume ($(vol_name))"
    fi
    local sd; sd="$(inst_state_dir)"
    if [ -d "$sd" ]; then
      info "removing host state ${sd}…"
      $DRY_RUN || rm -rf "$sd"
      ok_dry "host state removed ($sd)" "remove the host state ($sd)"
    fi
  else
    echo ""
    echo -e "  ${D}Data preserved:${X}"
    echo -e "  ${D}• instance volume:  $(vol_name)${X}"
    echo -e "  ${D}• instance state:   $(inst_state_dir)${X}"
    echo -e "  ${D}Re-run with --purge to remove both (irreversible).${X}"
  fi

  # 4. THE BINARIES LAST, AND ONLY IF NOTHING ELSE NEEDS THEM. They are machine-wide: one install serves
  #    every instance, so removing them while dev or prod still runs from them would leave those planes
  #    on a deleted inode — the very bug this command exists to stop causing.
  if [ "$BINARY_HELD_ELSEWHERE" = true ] || other_instances_remain; then
    warn "leaving the binaries in place: other instances still use them"
    warn "  orchicon: $INSTALL_DIR/orchicon"
    [ "$PURGE" = true ] || warn "  every container, volume and state for '${INSTANCE}' is gone, so uninstalling it"
    warn "  the executables is a machine-wide action — use --remove-binary once no instance needs them."
  else
    info "no other instance remains — removing the binaries"
    remove_binaries
  fi
  exit 0
}

# --- Remove the binaries only ------------------------------------------------
#
# THE EXPLICIT MACHINE-WIDE ACTION. It touches no container, volume or instance state — only the two
# executables — and it ASKS before taking anything down, because removing a binary that a plane or the
# shared runtime daemon is running from is what produces a server that answers and cannot work.
do_remove_binary() {
  ask_instance

  local users; users="$(all_binary_users)"
  if [ -z "$users" ]; then
    info "nothing is running off the binaries"
    remove_binaries
    exit 0
  fi

  echo ""
  warn "a process is running off these binaries:"
  printf '%s\n' "$users" | while IFS=: read -r pid inst; do
    [ -n "$pid" ] || continue
    printf '    pid %-8s instance %s\n' "$pid" "${inst:-<none — the shared runtime daemon>}"
  done

  if ! ask_yes_no "There is a plane running off this binary. Would you like to stop all services first before removing?"; then
    info "nothing stopped, nothing removed"
    exit 0
  fi

  # Stop each instance that appears in the user list (plane + container), then anything still holding the
  # file — which is how the shared runtime daemon is reached, since it carries no instance.
  local seen="" inst
  for inst in $(printf '%s\n' "$users" | cut -d: -f2 | sort -u); do
    [ -n "$inst" ] && seen="$seen $inst"
  done
  for inst in dev test prod; do
    case " $seen " in *" $inst "*) stop_service_for "$inst" ;; esac
  done
  local pid
  for pid in $(printf '%s\n' "$users" | cut -d: -f1); do
    [ -n "$pid" ] || continue
    $DRY_RUN || kill "$pid" 2>/dev/null || true
  done
  ok_dry "services stopped" "stop the services"
  remove_binaries
  exit 0
}


# --- Force-clean then install -----------------------------------------------
#
# Wipe everything and start fresh: stop the dev stack, destroy Docker
# volumes (database, NATS, Tempo, Loki, VictoriaMetrics, Grafana),
# runtime state, then install the latest version.
#
do_force_clean() {
  echo ""
  echo -e "${B}Orchicon — force-clean (NUKE)${X}"
  echo ""

  # WHICH INSTANCE, ASKED LIKE EVERY OTHER DESTRUCTIVE VERB.
  #
  # -ForceClean DESTROYS a data volume, so a default here is the most dangerous of all — and it was the one
  # verb that neither asked nor was scoped: it took whatever ORCHICON_INSTANCE held (or `dev`), which on a
  # host with prod data is a data-loss path an operator never chose. Reported: "I just did a -ForceClean on
  # my windows host and it just started working and didn't ask me which instance I wanted to do it on. EVERY
  # option should be instance bound and operate as such."
  ask_instance
  local bin="$INSTALL_DIR/orchicon"

  # 1. Stop the plane that runs the installed binary, and free that file for the
  # `mv` below. Scoped to the file — see stop_installed_plane for why this must
  # NOT be a process-name match.
  stop_installed_plane

  # 2. Remove the single-container instances and their data volumes
  # (postgres, nats, tempo, loki, victoriametrics, grafana live under
  # /var/lib/orchicon in the instance volumes).
  if command -v docker >/dev/null 2>&1; then
      info "removing the ${INSTANCE} instance and its data volume…"
    if [ "$DRY_RUN" = false ]; then
        docker rm -f "$(cnt_name)" 2>/dev/null || true
        docker volume rm "$(vol_name)" 2>/dev/null || true
        ok "${INSTANCE} instance + volume removed ($(cnt_name), $(vol_name))"
    else
        echo -e "  ${D}(would run: docker rm -f $(cnt_name); docker volume rm $(vol_name))${X}"
    fi
  else
    warn "docker not found — skipping container cleanup"
  fi

  # 3. Clean up local state — ANCHORED TO THE STATE DIRECTORY, never the caller's cwd.
  #
  # THESE WERE BARE RELATIVE NAMES and that was a data-loss bug for anyone who ran the
  # installer from a project directory. `rm -rf "data" ".dev" "bin"` resolves against the
  # CURRENT WORKING DIRECTORY, and `bin/` and `data/` are names ordinary projects have —
  # so `--force-clean` from a project root deleted that project's `bin/` and `data/`, and
  # any `.dev/` it had. On Windows the same list ran after `cd "$HOME"`, which bounded it to
  # `$HOME/bin`, `$HOME/data`, `$HOME/.dev` — still directories Orchicon does not own.
  #
  # The state this step exists to clear lives UNDER the Orchicon state dir; the README
  # documents the layout as "Runtime state, PID files, logs (.dev/), blob store (data/)" at
  # ~/.local/share/orchicon/. So it is named absolutely now, and XDG_DATA_HOME is honoured.
  #
  # `bin` IS DELIBERATELY NOT IN THE LIST. The binary is `$INSTALL_DIR/orchicon` and step 4
  # removes it by ABSOLUTE path; nothing of ours has ever lived in a cwd-relative `bin`.
    local state_dir; state_dir="$(inst_state_dir)"
    # `data` is this instance-owned state. The old list also removed a CWD-RELATIVE `.dev`, which is
    # not an Orchicon-owned path at all — the same mistake the Windows installer made with $HOME/.dev.
    for d in "$state_dir/data"; do
    if [ -d "$d" ]; then
      info "removing ${d}"
      $DRY_RUN || rm -rf "$d"
      ok "$d removed"
    fi
  done

  # 4. Remove the old binary.
  if [ -f "$bin" ]; then
    info "removing $bin"
    $DRY_RUN || rm -f "$bin"
    ok "old binary removed"
  fi

  # 5. Summary.
  echo ""
  echo -e "${G}All data wiped — ready for a fresh start${X}"
echo -e "  ${D}• The ${INSTANCE} data volume destroyed ($(vol_name))${X}"
  echo -e "  ${D}• BlobStore files removed${X}"
echo -e "  ${D}• Host state removed ($(inst_state_dir))${X}"
  echo -e "  ${D}• Old binary removed${X}"
  echo ""
  echo -e "${B}Now installing latest version…${X}"
  echo ""
}

# --- Clean then install -----------------------------------------------------
#
# Stop dev containers, remove the old binary, then install the latest
# version — one-shot upgrade. All user data is preserved (Docker volumes,
# BlobStore files, runtime state).
#
do_clean() {
  echo ""
  echo -e "${B}Orchicon — clean${X}"
  echo ""

  # WHICH INSTANCE, ASKED LIKE EVERY OTHER DESTRUCTIVE VERB.
  #
  # -ForceClean DESTROYS a data volume, so a default here is the most dangerous of all — and it was the one
  # verb that neither asked nor was scoped: it took whatever ORCHICON_INSTANCE held (or `dev`), which on a
  # host with prod data is a data-loss path an operator never chose. Reported: "I just did a -ForceClean on
  # my windows host and it just started working and didn't ask me which instance I wanted to do it on. EVERY
  # option should be instance bound and operate as such."
  ask_instance
  local bin="$INSTALL_DIR/orchicon"

  # 1. Stop the plane that runs the installed binary. Scoped to that file — see
  # stop_installed_plane for why this must not be a process-name match.
  stop_installed_plane

  # 1b. If the binary is already gone, the only thing still holding this
  # instance's ports is its container — fall back to stopping that.
  if [ ! -f "$bin" ] && command -v docker >/dev/null 2>&1; then
    info "stopping orchicon container instances…"
      $DRY_RUN || docker rm -f "$(cnt_name)" 2>/dev/null || true
  fi

  # 2. Remove the old binary.
  if [ -f "$bin" ]; then
    info "removing $bin"
    $DRY_RUN || rm -f "$bin"
    ok "old binary removed"
  else
    warn "orchicon not found in $INSTALL_DIR — nothing to remove"
  fi

  # 3. Show summary then proceed to install.
  echo ""
  echo -e "${G}Infrastructure cleaned — all user data preserved${X}"
  echo ""
  echo -e "${B}Data preserved:${X}"
  echo -e "  ${D}• Postgres database (Docker volume)${X}"
  echo -e "  ${D}• NATS JetStream messages (Docker volume)${X}"
  echo -e "  ${D}• Telemetry stack (Docker volumes)${X}"
  echo -e "  ${D}• BlobStore files (./data/blobs)${X}"
  echo -e "  ${D}• Runtime state (.dev/)${X}"
  echo ""
  echo -e "${B}Now installing latest version…${X}"
  echo ""
}

# --- Main install -----------------------------------------------------------
main() {
  # ASK WHICH INSTANCE BEFORE INSTALLING, so the operator knows which stack they are creating. `dev` is
  # the fallback — pass --instance to skip the question, and it is used automatically with no terminal.
  ask_instance dev

  check_cmd curl
  check_cmd tar

  local os arch
  os="$(detect_os)"
  arch="$(detect_arch)"

  # Resolve version
  if [ -z "$VERSION" ] || [ "$VERSION" = "latest" ]; then
    info "fetching latest release version…"
    VERSION="$(curl -fsSL "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/releases/latest" \
      | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
    [ -n "$VERSION" ] || die "could not determine latest version"
  fi
  info "installing Orchicon ${B}${VERSION}${X} for ${os}/${arch}"

  # Build download URL. Release assets follow the naming convention:
  #   orchicon_<version>_<os>_<arch>.tar.gz
  local asset="orchicon_${VERSION#v}_${os}_${arch}.tar.gz"
  local url="${ASSET_BASE_URL}/${VERSION}/${asset}"

  # Download to a temp file
  local tmpdir; tmpdir="$(mktemp -d)"
  # Capture the tmpdir path into the trap at definition time (double-quoted
  # expansion) so the cleanup still works after `main` returns and the
  # function-local $tmpdir goes out of scope under `set -u`.
  trap "rm -rf '${tmpdir}'" EXIT
  local archive="$tmpdir/$asset"

  info "downloading ${D}${url}${X}"
  curl -fsSL -o "$archive" "$url" || die "download failed"

  # Extract
  info "extracting…"
  tar -xzf "$archive" -C "$tmpdir"

  # Create install dir
  if [ "$DRY_RUN" = false ]; then
    mkdir -p "$INSTALL_DIR"
  fi

  # Move binary
  local bin="$INSTALL_DIR/orchicon"
  info "installing to ${B}${bin}${X}"
  if [ "$DRY_RUN" = false ]; then
    # The release archives may wrap the binary in a top-level
    # version-os-arch/ directory (e.g. orchicon_0.1.0_linux_amd64/orchicon),
    # not lay it flat at $tmpdir/orchicon. Find by name + executable bit
    # so the installer works regardless of archive layout.
    local extracted_binary
    extracted_binary="$(find "$tmpdir" -type f -name orchicon -perm -u+x 2>/dev/null | head -1)"
    [ -n "$extracted_binary" ] || die "could not find orchicon binary in archive"
    mv "$extracted_binary" "$bin"
    chmod +x "$bin"
  fi

  # Companion install: orch (the thin remote TUI client) rides in the
  # same archive. Optional via --no-orch; failure is a warning, not fatal
  # (older releases predate the companion binary).
  if [ "$INSTALL_ORCH" = true ] && [ "$DRY_RUN" = false ]; then
    local orch_bin="$INSTALL_DIR/orch"
    local extracted_orch
    extracted_orch="$(find "$tmpdir" -type f -name orch -perm -u+x 2>/dev/null | head -1)"
    if [ -n "$extracted_orch" ]; then
      mv "$extracted_orch" "$orch_bin"
      chmod +x "$orch_bin"
      ok "orch (remote TUI client) installed: $orch_bin"
    else
      warn "orch companion binary not found in archive (older release?) — skipping"
    fi
  fi

  # Verify
  if [ "$DRY_RUN" = false ]; then
    if "$bin" version 2>/dev/null | head -1; then
      ok "Orchicon ${VERSION} installed successfully"
    else
      warn "binary installed but could not verify — run '${bin} version' to check"
    fi
  else
    ok "dry-run complete — no changes made"
  fi

  # PATH hint
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
      echo ""
      warn "Orchicon was installed to ${INSTALL_DIR} which is not on your PATH."
      echo -e "  Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
      echo -e "  ${D}export PATH=\"\$PATH:${INSTALL_DIR}\"${X}"
      ;;
  esac

  # One-command setup: pull the published images, start the runtime
  # daemon, and launch the single-container instance, then print how to
  # connect / manage it. Skip with --no-setup (headless / CI installs
  # that only want the binary).
  if [ "$SETUP" = true ] && [ "$DRY_RUN" = false ]; then
    # Orchicon ships its own runtime engine, so NO adapter CLI is required:
    # a fresh install is complete on its own. External adapters (opencode and
    # future ones) are optional — the operator installs one on the host and it
    # is mounted into the containers at runtime, never baked in. An INFO line
    # when one IS present, so the operator knows it will be used; silence
    # otherwise, because there is nothing to fix.
    if command -v opencode >/dev/null 2>&1 || [ -x "$HOME/.opencode/bin/opencode" ]; then
      echo -e "  ${D}opencode found on this host — optional, and will be used as a runtime when a model ref asks for it.${X}"
      echo ""
    fi
    echo ""
    echo -e "${B}Setting up the full stack (one-command install)…${X}"
    # SAY THAT IT ASKS QUESTIONS. `orchicon install` prompts for the host ports, so an
    # operator who does not expect a question sits at a still screen while it waits.
    # Windows had the same omission in a worse form: the prompt was CAPTURED there, so
    # the question was never visible AND the install blocked on it — see install.ps1.
    echo -e "  ${D}it asks about host ports — press ENTER to accept each default${X}"
    # THE CONTAINER IMAGE FOLLOWS THE VERSION BEING INSTALLED, so an install of X runs X. Without this the
    # stack came from whatever `:latest` happened to be on the host — which is how a canary install ended
    # up with a canary binary over the last RELEASED image, and how a host that already had `:latest` could
    # never be brought forward (the installer skipped the pull: see ensureImage in cmd/orchicon).
    #
    # ONLY THE INSTANCE IMAGE IS NAMED. The runtime base and its variants keep following IMAGE_TAG,
    # because a version has one instance image and the runtime engine is not versioned with it — naming a
    # tag here would send those refs looking for `orchicon-runtime:<version>`, which no release publishes.
    local image_ref="ghcr.io/$(printf %s "${GITHUB_OWNER}" | tr "[:upper:]" "[:lower:]")/$(printf %s "${GITHUB_REPO}" | tr "[:upper:]" "[:lower:]"):${VERSION}"
    # A version with no image of its own falls back to the floating ref, loudly, inside the installer.
    if ORCHICON_CONTAINER_IMAGE="$image_ref" "$bin" install; then
      echo ""
      ok "Install complete — Orchicon is running."
    else
      warn "Full-stack setup did not complete. The binary is installed — run '${bin} install' to finish, or see the docs."
      echo -e "  ${D}https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}#readme${X}"
    fi
  else
    echo ""
    echo -e "${B}Installed. Next step:${X}"
    echo -e "  ${D}orchicon install    Pull images, start the runtime daemon, start this instance (Postgres, NATS and the plane)${X}"
  fi

  echo ""
  echo -e "${B}Documentation:${X} ${D}https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}#readme${X}"
}

# --- Run --------------------------------------------------------------------
if [ "$REMOVE_BINARY" = true ]; then
  do_remove_binary
elif [ "$UNINSTALL" = true ]; then
  do_uninstall
elif [ "$FORCE_CLEAN" = true ]; then
  do_force_clean
  main
elif [ "$CLEAN" = true ]; then
  do_clean
  main
else
  main
fi
