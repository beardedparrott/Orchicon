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
  # ONE IMPLEMENTATION: `orchicon plane stop` — the graceful, BOUNDED, VERIFIED stop in
  # internal/planecontrol, which is the SAME local mechanism the CLI, the TUI, the desktop app and
  # scripts/container.sh call. There is no lifecycle RPC; nothing here composes one.
  #
  # The three outcomes this caller must tell apart — nothing was running, the stop succeeded, and THE
  # PROCESS REFUSED TO DIE — are decided THERE, by the one owner of the PID file, because the third one is
  # the dangerous one: a surviving plane keeps the port, so the next start loses the race and the OLD binary
  # keeps serving while every command in the chain reports success. `plane stop` therefore treats
  # nothing-running as a graceful no-op and exits NON-ZERO when a process survives, so its exit code is
  # `down`'s exit code. The bash bodies that used to do this here (and in container.sh, where this function
  # lived before) are DELETED rather than kept as a "fallback": a second copy is the drift the delegation
  # exists to remove.
  ORCHICON_INSTANCE="$inst" "$bin" plane stop
  return $?
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
  echo -e "  ${C_DIM}plane (host process)${C_RESET}     $inst — stopped"
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

  # 2. (There is no plane container to remove: the plane is a host process and
  #    plane_stop above already ended it. The single-container shape that named
  #    one is retired, so an engine call here would only ever log "not present".)

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