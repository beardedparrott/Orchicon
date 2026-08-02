# ============================================================================
# Orchicon installer (Windows / PowerShell — WSL2)
#
# Runs the full Linux stack inside WSL2, giving Windows users the same
# one-command install experience as Linux/macOS:
#
#   irm https://orchicon.dev/install.ps1 | iex
#
# What this script does:
#   1. Provisions / detects WSL2 and a Linux distro.
#   2. Confirms Docker is available inside the distro (Docker Desktop's
#      WSL2 integration, or Docker Engine installed in the distro).
#   3. Downloads the LINUX release binary and installs it inside WSL.
#   4. Runs `orchicon install` inside WSL: pull the published images,
#      start the runtime daemon, launch the single-container instance.
#   5. Prints the URLs to open from Windows (localhost forwarding).
#
# The runtime layer (runtime daemon, unix socket, container mounts) is
# POSIX-only; on Windows it runs inside WSL2 — there is no native Windows
# port, so this installer never downloads the Windows release binary.
#
# Options:
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Version v0.1.173
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -InstallDir "/usr/local/bin"
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -NoSetup
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Uninstall
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Clean
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -ForceClean
#
# For Linux/macOS, see scripts/install.sh or:
#   curl -fsSL https://orchicon.dev/install | bash
# ============================================================================

param(
    [string]$Version = "",
    [string]$InstallDir = "",
    [switch]$Uninstall,
    [switch]$Clean,
    [switch]$ForceClean,
    [switch]$DryRun,
    [switch]$NoSetup,
    [switch]$Help
)

$ErrorActionPreference = "Stop"

$GitHubOwner = "beardedparrott"
$GitHubRepo = "Orchicon"
$script:Distro = ""
$script:WslPresent = $false

if ($Help) {
    Write-Host @"
Orchicon installer (Windows — runs the stack inside WSL2)

Usage: install.ps1 [options]

Orchicon's runtime layer is POSIX-only; on Windows the whole stack runs
inside a WSL2 Linux distro. This script provisions/detects WSL2, installs
the Linux binary inside the distro, and runs the one-command setup there.
WSL2 forwards localhost, so the UIs open from Windows at the same URLs as
on Linux (http://localhost:8080 control plane, http://localhost:3002 Grafana).

Options:
  -Version <tag>      Install a specific version (e.g. v0.1.173). Default: latest.
  -InstallDir <dir>   WSL install directory for the binary (default: ~/.local/bin).
  -NoSetup            Install the binary only — do NOT pull images, start the
                      runtime daemon, or launch the container.
  -Uninstall          Stop the WSL container instances, remove the WSL-installed
                      binary. The WSL distro itself is left intact.
  -Clean              Stop the container instances, remove the old binary, then
                      install the latest version — one-shot upgrade. All user
                      data is preserved (Docker volumes, BlobStore files,
                      runtime state).
  -ForceClean         Wipe everything and start fresh: stop the stack, destroy
                      the instance data volumes, remove local state, then install
                      the latest version. WARNING: all data is lost.
  -DryRun             Print what would happen without making changes.
  -Help               Show this help.
"@
    exit 0
}

function Write-Info { param([string]$msg) Write-Host "▸ $msg" -ForegroundColor Cyan }
function Write-Ok   { param([string]$msg) Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Warn { param([string]$msg) Write-Host "! $msg" -ForegroundColor Yellow }
function Write-Err  { param([string]$msg) Write-Host "✗ $msg" -ForegroundColor Red }
function Die        { param([string]$msg) Write-Err $msg; exit 1 }

# --- WSL helpers ------------------------------------------------------------

# Run a bash script inside the target WSL distro. The script's output is the
# function's output; $LASTEXITCODE carries its exit code (PowerShell sets it
# automatically after the native `wsl` call, so callers read it afterwards).
function Invoke-WslBash {
    param([string]$Script)
    # On Windows PowerShell 5.1, `2>&1` turns native stderr into ErrorRecords
    # which the script-level "Stop" preference would treat as terminating.
    # Scope "Continue" locally so wsl's chatter/errors never abort us here.
    $ErrorActionPreference = "Continue"
    if ($script:Distro) {
        & wsl -d $script:Distro -- bash -lc $Script 2>&1
    } else {
        & wsl -- bash -lc $Script 2>&1
    }
}

# Translate a Windows path (C:\...) to the /mnt/... path WSL sees it at.
# Docker Desktop's WSL2 integration automounts Windows drives at /mnt/<drive>.
function ConvertTo-WslPath {
    param([string]$Path)
    if ($Path -match '^[A-Za-z]:[/\\]') {
        $drive = $Path.Substring(0, 1).ToLower()
        $rest = $Path.Substring(2) -replace '\\', '/'
        return "/mnt/$drive$rest"
    }
    return $Path
}

# --- WSL2 provisioning ------------------------------------------------------
#
# Detects WSL + a Linux distro and stores it in $script:Distro. Exits with
# clear next steps when WSL2 or a distro is missing. With -Soft, returns
# $false instead of exiting (used by -Uninstall on machines with no WSL).
function Ensure-Wsl {
    param([switch]$Soft)

    # Already resolved for this run (Clean/ForceClean call it, then the main
    # path does again) — don't re-detect or re-print.
    if ($script:Distro) { return $true }

    $wsl = Get-Command "wsl" -ErrorAction SilentlyContinue
    if ($null -eq $wsl) {
        if ($Soft) { return $false }
        Write-Err "WSL is not installed."
        Write-Host ""
        Write-Host "Next steps (run from an elevated PowerShell, then reboot):" -ForegroundColor Yellow
        Write-Host "  1. wsl --install"
        Write-Host "  2. Reboot, then re-run:  irm https://orchicon.dev/install.ps1 | iex"
        exit 1
    }
    $script:WslPresent = $true

    # If WSL2's kernel/virtualization is missing, this prints guidance and
    # returns non-zero. A distro already on WSL2 makes it redundant, so a
    # failure here is non-fatal.
    if (-not $DryRun) {
        & wsl --set-default-version 2 2>$null | Out-Null
        if ($LASTEXITCODE -ne 0) {
            Write-Warn "could not set WSL2 as the default version — if your distro is already WSL2 this is fine"
        }
    }

    # List distros. The quiet listing may include a header on older WSL
    # versions; filter those out.
    $names = @(& wsl --list --quiet 2>$null) |
        Where-Object { $_ -and $_ -notmatch "no installed distributions" -and $_ -notmatch "Windows Subsystem" }
    if ($names.Count -eq 0) {
        if ($Soft) { return $false }
        Write-Err "WSL is installed but has no Linux distribution."
        Write-Host ""
        Write-Host "Next steps:" -ForegroundColor Yellow
        Write-Host "  1. wsl --install -d Ubuntu   (or: wsl --install)"
        Write-Host "  2. Complete first-run setup (create a Linux user + password)."
        Write-Host "  3. Re-run:  irm https://orchicon.dev/install.ps1 | iex"
        exit 1
    }

    # Prefer the default distro (marked with `*` in `wsl --list --verbose`).
    $verbose = @(& wsl --list --verbose 2>$null)
    $defaultLine = $verbose | Where-Object { $_ -match '^\s*\*' } | Select-Object -First 1
    if ($defaultLine -and $defaultLine -match '^\s*\*\s*(\S+)\s+\S+\s+(\d+)') {
        $script:Distro = $Matches[1]
        if ($Matches[2] -ne "2") {
            if ($Soft) { return $false }
            Write-Err "The default distro '$script:Distro' is WSL1 — Orchicon needs WSL2."
            Write-Host ""
            Write-Host "Next steps:" -ForegroundColor Yellow
            Write-Host "  1. wsl --set-version $script:Distro 2"
            Write-Host "  2. Re-run:  irm https://orchicon.dev/install.ps1 | iex"
            exit 1
        }
    } else {
        $script:Distro = $names[0]
    }

    Write-Ok "using WSL distro: $script:Distro"
    return $true
}

# --- Docker check (inside WSL) ----------------------------------------------
function Ensure-WslDocker {
    Write-Info "checking Docker inside WSL…"
    $out = Invoke-WslBash "docker version --format '{{.Server.Version}}'"
    if ($LASTEXITCODE -ne 0 -or -not $out) {
        Write-Err "Docker is not available inside $script:Distro."
        Write-Host ""
        Write-Host "Setup:" -ForegroundColor Yellow
        Write-Host "  1. Install Docker Desktop (https://www.docker.com/products/docker-desktop/)"
        Write-Host "  2. In Docker Desktop → Settings → Resources → WSL Integration, enable the"
        Write-Host "     integration for '$script:Distro' (or install Docker Engine inside it)."
        Write-Host "  3. Restart Docker Desktop, then re-run:  irm https://orchicon.dev/install.ps1 | iex"
        exit 1
    }
    Write-Ok "Docker is available inside WSL (server $($out | Select-Object -First 1))"
}

# --- Connection info (Windows-visible URLs) ----------------------------------
function Write-ConnectionInfo {
    $instance = if ($env:ORCHICON_INSTANCE) { $env:ORCHICON_INSTANCE } else { "dev" }
    Write-Host ""
    Write-Host "Open from Windows (WSL2 forwards localhost):" -ForegroundColor White
    if ($instance -eq "prod") {
        Write-Host "  Control plane: http://localhost:8091" -ForegroundColor Cyan
        Write-Host "  Grafana:       http://localhost:3003" -ForegroundColor Cyan
    } else {
        Write-Host "  Control plane: http://localhost:8080" -ForegroundColor Cyan
        Write-Host "  Grafana:       http://localhost:3002" -ForegroundColor Cyan
    }
    Write-Host "  Note: if the URLs do not answer, check Windows Defender Firewall,"
    Write-Host "  or add a port forward: netsh interface portproxy add v4tov4 listenport=8080"
    Write-Host "  listenaddress=127.0.0.1 connectport=8080 connectaddress=<WSL-IP>" -ForegroundColor DarkGray
}

# --- Defaults ---------------------------------------------------------------
# The binary now installs INSIDE WSL, so the install dir is a WSL path.
if (-not $InstallDir) {
    $InstallDir = "~/.local/bin"
}
if ($InstallDir -match '^[A-Za-z]:[/\\]') {
    Write-Warn "-InstallDir is a Windows path ('$InstallDir') but the binary installs inside WSL."
    Write-Warn "  Using a WSL path instead (e.g. '/usr/local/bin'). Continuing with ~/.local/bin."
    $InstallDir = "~/.local/bin"
}

# --- Uninstall --------------------------------------------------------------
if ($Uninstall) {
    if (-not (Ensure-Wsl -Soft)) {
        Write-Warn "WSL not found or no distro — nothing to uninstall"
        exit 0
    }
    $bin = Join-Path $InstallDir "orchicon"
    $uninstallScript = @'
set +e
BIN="__INSTALL_DIR__"
BIN="${BIN/#\~/$HOME}/orchicon"
if [ -x "$BIN" ]; then "$BIN" serve --stop >/dev/null 2>&1; fi
docker stop orchicon-cnt-dev orchicon-cnt-prod >/dev/null 2>&1
if [ -f "$BIN" ]; then rm -f "$BIN" && echo "removed $BIN"; else echo "orchicon not found in $BIN — nothing to remove"; fi
true
'@
    $uninstallScript = $uninstallScript.Replace('__INSTALL_DIR__', $InstallDir)
    if ($DryRun) {
        Write-Info "would run inside WSL:"
        Write-Host "  wsl -d $script:Distro -- bash -lc 'docker stop orchicon-cnt-dev orchicon-cnt-prod'"
        Write-Host "  wsl -d $script:Distro -- bash -lc 'rm -f $bin'"
    } else {
        Invoke-WslBash $uninstallScript
        Write-Ok "Orchicon uninstalled — the WSL distro is left intact"
    }
    exit 0
}

# --- Clean (stop stack, remove binary, then install latest; data kept) ------
if ($Clean) {
    Write-Host ""
    Write-Host "Orchicon — clean" -ForegroundColor White
    Write-Host ""
    Ensure-Wsl | Out-Null
    $cleanScript = @'
set +e
BIN="__INSTALL_DIR__"
BIN="${BIN/#\~/$HOME}/orchicon"
if [ -x "$BIN" ]; then "$BIN" serve --stop >/dev/null 2>&1; fi
pkill -9 -x orchicon 2>/dev/null
docker rm -f orchicon-cnt-dev orchicon-cnt-prod >/dev/null 2>&1
rm -f "$BIN"
true
'@
    $cleanScript = $cleanScript.Replace('__INSTALL_DIR__', $InstallDir)
    if ($DryRun) {
        Write-Info "would run inside WSL: stop the container instances, remove the old binary (data preserved)"
    } else {
        Invoke-WslBash $cleanScript
        Write-Ok "Infrastructure cleaned — all user data preserved"
        Write-Host ""
        Write-Host "Now installing latest version…" -ForegroundColor White
        Write-Host ""
    }
}

# --- Force-clean (nuke volumes + state, then install latest) ----------------
if ($ForceClean) {
    Write-Host ""
    Write-Host "Orchicon — force-clean (NUKE)" -ForegroundColor White
    Write-Host ""
    Ensure-Wsl | Out-Null
    $forceCleanScript = @'
set +e
BIN="__INSTALL_DIR__"
BIN="${BIN/#\~/$HOME}/orchicon"
if [ -x "$BIN" ]; then "$BIN" serve --stop >/dev/null 2>&1; fi
pkill -9 -x orchicon 2>/dev/null
docker rm -f orchicon-cnt-dev orchicon-cnt-prod >/dev/null 2>&1
docker volume rm orchicon-cnt-dev-data orchicon-cnt-prod-data >/dev/null 2>&1
cd "$HOME" || true
rm -rf data .dev bin .local/share/orchicon
rm -f "$BIN"
true
'@
    $forceCleanScript = $forceCleanScript.Replace('__INSTALL_DIR__', $InstallDir)
    if ($DryRun) {
        Write-Info "would run inside WSL: stop the stack, destroy instance data volumes, remove local state (ALL DATA LOST)"
    } else {
        Invoke-WslBash $forceCleanScript
        Write-Ok "All data wiped — ready for a fresh start"
        Write-Host ""
        Write-Host "Now installing latest version…" -ForegroundColor White
        Write-Host ""
    }
}

# --- Detect arch ------------------------------------------------------------
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64"   { "amd64" }
    "ARM64"   { "arm64" }
    default   { Write-Err "unsupported architecture: $env:PROCESSOR_ARCHITECTURE"; exit 1 }
}

# --- WSL2 + Docker provisioning (main install path) --------------------------
Ensure-Wsl | Out-Null
if (-not $DryRun) {
    Ensure-WslDocker
}

# --- Resolve version --------------------------------------------------------
if (-not $Version -or $Version -eq "latest") {
    Write-Info "fetching latest release version…"
    try {
        $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$GitHubOwner/$GitHubRepo/releases/latest"
    } catch {
        Die "could not reach GitHub releases API: $($_.Exception.Message)"
    }
    $Version = $release.tag_name
    if (-not $Version) { Die "could not determine latest version" }
}

Write-Info "installing Orchicon $Version for linux/$arch inside WSL"

# --- Build download URL -----------------------------------------------------
# Windows runs the LINUX binary inside WSL2 — never the Windows asset.
$asset = "orchicon_$($Version -replace '^v','')_linux_$arch.tar.gz"
$url = "https://github.com/$GitHubOwner/$GitHubRepo/releases/download/$Version/$asset"

# --- Download (Windows side; WSL sees it via /mnt/<drive>) -------------------
if ($DryRun) {
    Write-Info "planned steps:"
    Write-Host "  1. download $asset (linux/$arch)"
    Write-Host "  2. extract + install to $InstallDir/orchicon inside WSL"
    if (-not $NoSetup) {
        Write-Host "  3. run 'orchicon install' inside WSL (pull images, start daemon, launch container)"
        Write-Host "  4. open http://localhost:8080 (control plane) / http://localhost:3002 (Grafana)"
    }
    Write-Ok "dry-run complete — no changes made"
    exit 0
}

$tmpdir = Join-Path $env:TEMP "orchicon-install-$(Get-Random)"
New-Item -ItemType Directory -Path $tmpdir -Force | Out-Null
$archive = Join-Path $tmpdir $asset
$wslArchive = ConvertTo-WslPath $archive

Write-Info "downloading $url"
try {
    Invoke-WebRequest -Uri $url -OutFile $archive
} catch {
    Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
    Die "download failed: $($_.Exception.Message)"
}

# --- Install inside WSL -----------------------------------------------------
$installScript = @'
set -euo pipefail
INSTALL_DIR="__INSTALL_DIR__"
ARCHIVE="__ARCHIVE__"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
tar -xzf "$ARCHIVE" -C "$TMP"
BIN="$(find "$TMP" -type f -name orchicon -perm -u+x 2>/dev/null | head -1)"
if [ -z "$BIN" ]; then
  echo "could not find orchicon binary in archive" >&2
  exit 1
fi
INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
mkdir -p "$INSTALL_DIR"
mv "$BIN" "$INSTALL_DIR/orchicon"
chmod +x "$INSTALL_DIR/orchicon"
"$INSTALL_DIR/orchicon" version
rm -rf "$TMP"
'@
$installScript = $installScript.Replace('__INSTALL_DIR__', $InstallDir).Replace('__ARCHIVE__', $wslArchive)

Write-Info "extracting and installing inside WSL…"
Invoke-WslBash $installScript
if ($LASTEXITCODE -ne 0) {
    Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
    Die "install inside WSL failed (exit $LASTEXITCODE)"
}

Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue

# --- Verify ----------------------------------------------------------------
$verifyScript = @'
set -euo pipefail
INSTALL_DIR="__INSTALL_DIR__"
INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
"$INSTALL_DIR/orchicon" version
'@
$verifyScript = $verifyScript.Replace('__INSTALL_DIR__', $InstallDir)
$verify = Invoke-WslBash $verifyScript
if ($LASTEXITCODE -eq 0 -and $verify) {
    Write-Ok "Orchicon $Version installed successfully"
} else {
    Write-Warn "binary installed but could not verify — run 'orchicon version' inside WSL to check"
}

# --- PATH hint (inside WSL) -------------------------------------------------
$pathScript = @'
set -euo pipefail
INSTALL_DIR="__INSTALL_DIR__"
INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) echo "on" ;;
  *) echo "off" ;;
esac
'@
$pathScript = $pathScript.Replace('__INSTALL_DIR__', $InstallDir)
$pathOnPath = ((Invoke-WslBash $pathScript | Out-String).Trim() -eq "on")
if (-not $pathOnPath) {
    Write-Warn "Orchicon was installed to $InstallDir inside WSL, which is not on the distro's PATH."
    Write-Host "  Add this to ~/.bashrc (or ~/.zshrc):" -ForegroundColor Yellow
    Write-Host '  export PATH="$PATH:$HOME/.local/bin"' -ForegroundColor DarkGray
    Write-Host "  (or use the full path: $InstallDir/orchicon)" -ForegroundColor DarkGray
}

# --- One-command setup inside WSL -------------------------------------------
if ($NoSetup) {
    Write-Host ""
    Write-Host "Installed. Next step (inside WSL):" -ForegroundColor White
    Write-Host "  orchicon install    Pull images, start the runtime daemon + container" -ForegroundColor DarkGray
} else {
    Write-Host ""
    Write-Host "Setting up the full stack inside WSL (one-command install)…" -ForegroundColor White
    $setupScript = @'
set -euo pipefail
INSTALL_DIR="__INSTALL_DIR__"
INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
"$INSTALL_DIR/orchicon" install
'@
    $setupScript = $setupScript.Replace('__INSTALL_DIR__', $InstallDir)
    Invoke-WslBash $setupScript
    if ($LASTEXITCODE -eq 0) {
        Write-Ok "Install complete — Orchicon is running."
        Write-ConnectionInfo
    } else {
        Write-Warn "Full-stack setup did not complete. The binary is installed — run 'orchicon install' inside WSL to finish."
    }
}

Write-Host ""
Write-Host "Documentation: https://github.com/$GitHubOwner/$GitHubRepo#readme" -ForegroundColor DarkGray
