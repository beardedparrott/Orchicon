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
INSTALL_DIR="${ORCHICON_INSTALL_DIR:-${HOME}/.local/bin}"
VERSION=""
UNINSTALL=false
CLEAN=false
FORCE_CLEAN=false
DRY_RUN=false

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
    --uninstall)       UNINSTALL=true; shift ;;
    --clean)           CLEAN=true; shift ;;
    --force-clean|--nuke|-f) FORCE_CLEAN=true; shift ;;
    --dry-run)         DRY_RUN=true; shift ;;
    --help|-h)
      cat <<EOF
Orchicon installer

Usage: install.sh [options]

Options:
  --version <tag>      Install a specific version (e.g. v0.2.0). Default: latest.
  --install-dir <dir>  Installation directory (default: ~/.local/bin).
  --uninstall          Remove Orchicon from the install directory.
   --clean              Stop dev containers, remove the old Orchicon binary, then
                        install the latest version — one-shot upgrade. All user
                        data is preserved (Docker volumes, BlobStore files,
                        runtime state).
   --force-clean, --nuke
                        Wipe everything and start fresh: stop the dev stack,
                        destroy Docker volumes (database, NATS, ClickHouse),
                        remove blob store data and runtime state, then install
                        the latest version. WARNING: all data is lost.
   --dry-run            Print what would happen without making changes.
  -h, --help           Show this help.
EOF
      exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

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

# --- Uninstall --------------------------------------------------------------
do_uninstall() {
  local bin="$INSTALL_DIR/orchicon"

  # Stop the binary cleanly before removing it. The `dev stop`
  # internally SIGKILLs orphans too, but a defensive pkill covers
  # the case where the binary was launched manually (no PID file).
  if [ -f "$bin" ]; then
    info "stopping dev stack via '${bin} dev stop'…"
    $DRY_RUN || "$bin" dev stop 2>/dev/null || warn "dev stop failed (ignoring)"
  fi
  if command -v pkill >/dev/null 2>&1; then
    info "killing any leftover orchicon processes…"
    $DRY_RUN || pkill -9 -x orchicon 2>/dev/null || true
  fi

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
# volumes (database, NATS, ClickHouse), remove blob store data and
# runtime state, then install the latest version.
#
do_force_clean() {
  echo ""
  echo -e "${B}Orchicon — force-clean (NUKE)${X}"
  echo ""

  local bin="$INSTALL_DIR/orchicon"

  # 1. Stop dev stack via the binary (if available). The binary's
  # `orchicon dev stop` already SIGKILLs any orphan orchicon processes
  # by name, but if the binary itself was started without going through
  # `dev start` (so its PID file was never written) the cleaner below
  # is what actually frees the binary file lock.
  if [ -f "$bin" ]; then
    info "stopping dev stack via '${bin} dev stop'…"
    if [ "$DRY_RUN" = false ]; then
      "$bin" dev stop 2>/dev/null || warn "dev stop failed (ignoring)"
    else
      echo -e "  ${D}(would run: ${bin} dev stop)${X}"
    fi
  fi

  # 1b. Belt-and-suspenders: even if `dev stop` ran (or was skipped
  # because the binary is gone / corrupt), kill any remaining orchicon
  # processes by executable name. Without this step an old binary
  # process holds the mmap on $INSTALL_DIR/orchicon and the `mv` below
  # fails with "Text file busy". Idempotent and safe — `dev stop`
  # already filters its own PID, and `pkill -x` matches the executable
  # basename exactly (so worker subprocesses like `opencode` are
  # unaffected).
  if command -v pkill >/dev/null 2>&1; then
    info "killing any leftover orchicon processes…"
    if [ "$DRY_RUN" = false ]; then
      pkill -9 -x orchicon 2>/dev/null && ok "killed leftover orchicon process(es)" || true
    else
      echo -e "  ${D}(would run: pkill -9 -x orchicon)${X}"
    fi
  fi

  # 2. Nuke Docker volumes (postgres, nats, clickhouse, signoz).
  if command -v docker >/dev/null 2>&1; then
    info "destroying all orchicon Docker volumes…"
    if [ "$DRY_RUN" = false ]; then
      if docker compose -p orchicon down -v --remove-orphans 2>/dev/null; then
        ok "Docker volumes destroyed"
      else
        warn "docker compose down -v failed (containers may not exist yet)"
      fi
    else
      echo -e "  ${D}(would run: docker compose -p orchicon down -v)${X}"
    fi
  else
    warn "docker not found — skipping volume cleanup"
  fi

  # 3. Clean up local state.
  local state_dirs=("data" ".dev" "bin")
  for d in "${state_dirs[@]}"; do
    if [ -d "$d" ]; then
      info "removing ${d}/"
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
  echo -e "  ${D}• Docker volumes destroyed${X}"
  echo -e "  ${D}• BlobStore files removed${X}"
  echo -e "  ${D}• Runtime state (.dev/) removed${X}"
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

  # 1. Stop dev stack via the binary (if available).
  if [ -f "$bin" ]; then
    info "stopping dev stack via '${bin} dev stop'…"
    if [ "$DRY_RUN" = false ]; then
      "$bin" dev stop 2>/dev/null || warn "dev stop failed (ignoring)"
    else
      echo -e "  ${D}(would run: ${bin} dev stop)${X}"
    fi
  else
    # Fall back to docker compose if the binary is gone.
    if command -v docker >/dev/null 2>&1; then
      info "stopping orchicon containers via docker compose…"
      $DRY_RUN || docker compose -p orchicon down 2>/dev/null || true
    fi
  fi

  # 1b. Belt-and-suspenders orphan cleanup (same rationale as
  # do_force_clean step 1b). The `dev stop` above SIGKILLs orphans by
  # name internally, but this defensive pass catches the case where
  # the binary itself was running from a different path (so its PID
  # file was elsewhere) or was launched manually.
  if command -v pkill >/dev/null 2>&1; then
    info "killing any leftover orchicon processes…"
    if [ "$DRY_RUN" = false ]; then
      pkill -9 -x orchicon 2>/dev/null && ok "killed leftover orchicon process(es)" || true
    else
      echo -e "  ${D}(would run: pkill -9 -x orchicon)${X}"
    fi
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
  echo -e "  ${D}• ClickHouse / SigNoz (Docker volumes)${X}"
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
  local url="https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}/releases/download/${VERSION}/${asset}"

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

  # Next steps
  echo ""
  echo -e "${B}Quick start:${X}"
  echo -e "  ${D}orchicon --help           Show available commands${X}"
  echo -e "  ${D}orchicon dev start        Start the full dev environment${X}"
  echo -e "  ${D}orchicon dev status       Check what's running${X}"
  echo ""
  echo -e "${B}Note:${X} ${D}orchicon dev start requires Docker (for Postgres, NATS, SigNoz).${X}"
  echo -e "  The binary embeds the compose stack, migrations, and frontend.${X}"
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
