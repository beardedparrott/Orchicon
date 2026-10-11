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
UNINSTALL=false
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
    --instance|-i)     INSTANCE="$2"; shift 2 ;;
    --uninstall)       UNINSTALL=true; shift ;;
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
  --uninstall          Stop THIS instance and remove Orchicon from the install directory. The
                       data volume is left in place, and the command says where it is.
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

# --- Helpers ----------------------------------------------------------------
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
# busy". `fuser` targets the FILE, so a plane running from any other path is
# unreachable by CONSTRUCTION rather than by naming convention — this is the same
# technique scripts/install-local.sh already uses for exactly this step.
#
# When fuser is unavailable it declines to act and says so, rather than falling
# back to a name match: killing the wrong process is worse than a warning.
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

  if ! command -v fuser >/dev/null 2>&1; then
    warn "fuser not available — cannot tell whether ${bin} is still in use."
    warn "  Deliberately not falling back to a process-NAME match, which would"
    warn "  also kill Orchicon instances running from other paths."
    return 0
  fi
  if fuser "$bin" >/dev/null 2>&1; then
    if [ "$DRY_RUN" = false ]; then
      fuser -k "$bin" 2>/dev/null && ok "released ${bin} (stopped the process holding it)" || true
    else
      echo -e "  ${D}(would run: fuser -k ${bin})${X}"
    fi
  fi
}

# --- Uninstall --------------------------------------------------------------
do_uninstall() {
  local bin="$INSTALL_DIR/orchicon"

  # Stop the plane that runs the installed binary, and free the file itself.
  # Scoped to THAT path — see stop_installed_plane for why this must not be a
  # process-name match.
  stop_installed_plane

  if [ -f "$bin" ]; then
    info "removing $bin"
    $DRY_RUN || rm -f "$bin"
    ok "Orchicon uninstalled"
  else
    warn "orchicon not found in $INSTALL_DIR — nothing to remove"
  fi
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
    if "$bin" install; then
      echo ""
      ok "Install complete — Orchicon is running."
    else
      warn "Full-stack setup did not complete. The binary is installed — run '${bin} install' to finish, or see the docs."
      echo -e "  ${D}https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}#readme${X}"
    fi
  else
    echo ""
    echo -e "${B}Installed. Next step:${X}"
    echo -e "  ${D}orchicon install    Pull images, start the runtime daemon + container${X}"
  fi

  echo ""
  echo -e "${B}Documentation:${X} ${D}https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}#readme${X}"
}

# --- Run --------------------------------------------------------------------
if [ "$UNINSTALL" = true ]; then
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
