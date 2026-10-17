# scripts/lib/teardown.sh — INSTANCE-SCOPED teardown (sourced; no shebang).
#
# Removes ONE instance and everything it owns: its host plane, its container, its
# data volume, its host data dir, and its runtime containers. Nothing else — a
# dev teardown never touches prod's or test's row of the table.
#
# DESTRUCTIVE AND NOT REVERSIBLE. The volume holds the instance's database and
# the host data dir holds the secrets KEK, so it requires an EXPLICIT
# confirmation and prints exactly what it will remove first.
#
# Sourced by scripts/orchicon.sh (the documented entry point:
# `scripts/orchicon.sh uninstall <dev|prod|test> --yes`) and by
# scripts/container.sh (its legacy `uninstall` verb delegates here).

# The instance table, the log helpers and residency_for come from instances.sh.
# Sourced here (guarded) so this file is usable on its own without double-sourcing
# anything when the caller already did it.
command -v instance_info >/dev/null 2>&1 || . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/instances.sh"

# CONFIRM_DESTROY is the in-script form of --yes (`scripts/orchicon.sh uninstall
# <inst> --yes`), kept separate from the residency positional so destroying data
# is always an explicit act. Defaulted only, never forced: ORCHICON_CONFIRM_DESTROY=1
# is the env-only form the confirmation check also accepts.
CONFIRM_DESTROY="${CONFIRM_DESTROY:-0}"

# host_plane_bin resolves the binary the HOST plane runs. Defaults to the repo
# binary the same rebuild just built, so the resident plane matches the image.
host_plane_bin() {
  echo "${ORCHICON_HOST_BIN:-$PROJECT_ROOT/bin/orchicon}"
}

# plane_stop stops ONLY this instance's host plane. ORCHICON_SERVE_STATE_DIR
# isolates the PID file per instance, so it can never kill the sibling plane.
# The services container is untouched — that is the whole point of the split.
plane_stop() {
  local inst="${1:-dev}"
  instance_info "$inst"
  local bin
  bin=$(host_plane_bin)
  if [ ! -x "$bin" ]; then
    log_dim "host plane binary missing ($bin) — nothing to stop"
    return 0
  fi
  local envargs=()
  local kv
  while IFS= read -r kv; do
    [ -n "$kv" ] && envargs+=("$kv")
  done < <(plane_env "$inst")
  # THE STOP IS VERIFIED, AND ITS FAILURE IS PROPAGATED.
  #
  # This used to run `serve --stop` and swallow ANY non-zero exit as "no detached host plane running". That
  # conflated three different outcomes — nothing was running, the stop succeeded, and THE PROCESS REFUSED TO
  # DIE — and the third one is the dangerous one. A surviving plane keeps the port, so the next start loses
  # the race and the OLD binary keeps serving; every command in the chain still reports success, and the
  # operator is left believing they are on new code. Their report: "I did that and the serve was still
  # running and I had to do a kill -9 on the PIDs... I have run into this problem a few times now where I
  # thought I was on the newest code but was still running a stale binary."
  #
  # `serve --stop` now waits, escalates to SIGKILL, and exits non-zero whenever a process survives. So its
  # exit code is `down`'s exit code: nothing-running is a WARNING (it is a legitimate no-op), a survivor is
  # an ERROR that must halt whatever asked for the stop.
  local stop_rc=0
  env -u ORCHICON_CONTAINER_MODE ${envargs[@]+"${envargs[@]}"} "$bin" serve --stop || stop_rc=$?
  case "$stop_rc" in
    0) : ;;
    *)
      # Distinguish "there was nothing to stop" from "a plane would not stop". `serve --status` answers from
      # the SAME PID file the stop resolved, so a live process there means the stop genuinely failed.
      if env -u ORCHICON_CONTAINER_MODE ${envargs[@]+"${envargs[@]}"} "$bin" serve --status >/dev/null 2>&1; then
        log_err "$inst host plane is STILL RUNNING after serve --stop — refusing to report the instance as down."
        log_err "  A surviving plane keeps the port, so the next start would lose the race and keep serving"
        log_err "  the OLD binary. Check $inst's logs, stop it manually, then retry."
        return 1
      fi
      log_dim "no detached host plane running for $inst"
      ;;
  esac
}

# stop_runtime_daemon removes every runtime container owned by ONE
# instance. Both instances share one host daemon, so the removal is
# instance-scoped (label=orchicon.instance): a dev rebuild must never
# hard-kill a live prod fire's container mid-run. Killing the DAEMON itself
# (the `runtime-stop` case below) is the only wholesale action — its next
# start resets the pool anyway.
stop_runtime_daemon() {
  local inst="${1:-dev}"
  "$(engine_cmd)" ps -a --filter label=orchicon.workflow --filter "label=orchicon.instance=$inst" --format '{{.Names}}' | while read -r name; do
    "$(engine_cmd)" rm -f "$name" >/dev/null 2>&1 && log_dim "removed runtime $name"
  done
}

# remove_host_data_dir deletes an instance's host state dir, INCLUDING the parts
# the services container wrote as root.
#
# A plain `rm -rf` is not enough, and its failure is misleading. The supervisor
# runs as root, so the config it generates and the postgres data it owns land in
# the host dir owned by root / uid 70. An unprivileged `rm -rf` removes the
# user-owned files and ERRORS on the rest — and because uninstall runs under
# `set -e`, that non-zero aborts the script MID-TEARDOWN, after the container and
# volume are already gone, leaving the job half-done and the operator wrongly
# told it failed.
#
# So the root-owned remainder is removed by a helper container. It mounts ONLY
# the data dir (not its parent) and its command is fixed, so it cannot reach
# anything the uninstall was not already deleting. This needs no sudo, which is
# the point: the files were created by the container, so the container is the
# thing that can clear them.
remove_host_data_dir() {
  rm -rf "$HOST_DATA_DIR" 2>/dev/null || true
  if [ ! -d "$HOST_DATA_DIR" ]; then
    return 0
  fi
  if ! engine_cmd >/dev/null 2>&1; then
    log_warn "could not fully remove $HOST_DATA_DIR (contains root-owned files) and no container engine (docker/podman) is available"
    log_warn "  remove it as root:  sudo rm -rf $HOST_DATA_DIR"
    return 1
  fi
  log_dim "removing root-owned files in $HOST_DATA_DIR via a helper container"
  # The dot-globs match dotfiles without matching '.' or '..'.
  if ! "$(engine_cmd)" run --rm -v "$HOST_DATA_DIR:/target" alpine \
      sh -c 'rm -rf /target/..?* /target/.[!.]* /target/*' >/dev/null 2>&1; then
    log_warn "helper container could not clear $HOST_DATA_DIR"
    log_warn "  remove it as root:  sudo rm -rf $HOST_DATA_DIR"
    return 1
  fi
  # Now empty (or user-owned only), so an unprivileged removal succeeds. It is
  # still checked rather than assumed: a partial helper run must not be reported
  # as a clean teardown.
  rm -rf "$HOST_DATA_DIR" 2>/dev/null || true
  if [ -d "$HOST_DATA_DIR" ]; then
    log_warn "$HOST_DATA_DIR still present — remove it as root:  sudo rm -rf $HOST_DATA_DIR"
    return 1
  fi
  return 0
}

# uninstall_instance removes ONE instance and everything it owns: its host plane,
# its container, its data volume, its host data dir, and its runtime containers.
#
# DESTRUCTIVE AND NOT REVERSIBLE. The volume holds the instance's database and
# the host data dir holds the secrets KEK — so removing the data dir makes every
# secret encrypted with it unrecoverable even if a database backup is restored.
# It therefore requires an EXPLICIT confirmation (`uninstall <inst> --yes`, or
# ORCHICON_CONFIRM_DESTROY=1) and prints exactly what it will remove first. A
# teardown that deletes a database and a KEK on a typo is the same defect class
# as the unanchored cleanups this repo has had to fix before.
#
# INSTANCE-SCOPED, like every other destructive path here: it touches only the
# named instance's container, volume, data dir and labelled runtime containers.
# It deliberately does NOT remove PG_VOLUME — for dev and prod that is a LEGACY
# compose volume predating the single-container layout, which may hold data this
# instance never owned (and which `down` has always preserved).
uninstall_instance() {
  local inst="${1:-dev}"
  instance_info "$inst" || return 1

  local pg_note="none (its database lives in the instance volume)"
  if [ "$PG_VOLUME" != "fresh" ]; then
    pg_note="$PG_VOLUME — legacy volume, NOT removed"
  fi

  echo ""
  echo -e "${C_BOLD}Uninstall instance: $inst${C_RESET}"
  echo -e "  ${C_DIM}plane (host process)${C_RESET}     $inst"
  echo -e "  ${C_DIM}container${C_RESET}                $NAME"
  echo -e "  ${C_DIM}volume — DELETED${C_RESET}         $VOLUME"
  echo -e "  ${C_DIM}host data dir — DELETED${C_RESET}  $HOST_DATA_DIR"
  echo -e "  ${C_DIM}runtime containers${C_RESET}       label=orchicon.instance=$inst"
  echo -e "  ${C_DIM}postgres volume${C_RESET}          $pg_note"
  echo ""

  if [ "${ORCHICON_CONFIRM_DESTROY:-}" != "1" ] && [ "$CONFIRM_DESTROY" != "1" ]; then
    log_warn "refusing to delete data without confirmation"
    echo -e "  ${C_DIM}re-run:  $0 uninstall $inst --yes${C_RESET}"
    echo -e "  ${C_DIM}or set:  ORCHICON_CONFIRM_DESTROY=1${C_RESET}"
    return 1
  fi

  # 1. Stop this instance's plane. plane_stop is PID-file scoped per instance, so
  #    it cannot reach a sibling's plane.
  plane_stop "$inst"

  # 2. Remove the container (and with it the publishes).
  if "$(engine_cmd)" ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
    "$(engine_cmd)" rm -f "$NAME" >/dev/null && log_ok "removed container $NAME"
  else
    log_dim "container $NAME not present"
  fi

  # 3. Reap this instance's runtime containers (instance-scoped; see
  #    stop_runtime_daemon). Without this they would outlive the instance that
  #    owns them and never be claimed by anyone.
  stop_runtime_daemon "$inst"

  # 4. The instance's own volume.
  if "$(engine_cmd)" volume inspect "$VOLUME" >/dev/null 2>&1; then
    "$(engine_cmd)" volume rm "$VOLUME" >/dev/null && log_ok "removed volume $VOLUME"
  else
    log_dim "volume $VOLUME not present"
  fi

  # 5. The host state dir (KEK, blobs, plane PID/logs).
  #    Routed through remove_host_data_dir: the services container wrote part of
  #    it as root, and a bare `rm -rf` both fails on that part and (under set -e)
  #    aborts the teardown AFTER the container and volume were already removed.
  if [ -d "$HOST_DATA_DIR" ]; then
    if remove_host_data_dir; then
      log_ok "removed host data dir $HOST_DATA_DIR"
    else
      log_warn "host data dir $HOST_DATA_DIR was not fully removed (see above)"
    fi
  else
    log_dim "host data dir $HOST_DATA_DIR not present"
  fi

  log_ok "instance $inst uninstalled"
}