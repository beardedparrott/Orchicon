# ============================================================================
# Orchicon installer (Windows / PowerShell)
#
# Usage:
#   irm https://orchicon.dev/install.ps1 | iex
#
# Or with options:
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Version v0.2.0
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -InstallDir "C:\bin"
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Uninstall
#
# For Linux/macOS, see scripts/install.sh or:
#   curl -fsSL https://orchicon.dev/install | bash
# ============================================================================

param(
    [string]$Version = "",
    [string]$InstallDir = "",
    [switch]$Uninstall,
    [switch]$DryRun,
    [switch]$Help
)

$ErrorActionPreference = "Stop"

$GitHubOwner = "beardedparrott"
$GitHubRepo = "Orchicon"

if ($Help) {
    Write-Host @"
Orchicon installer (Windows)

Usage: install.ps1 [options]

Options:
  -Version <tag>      Install a specific version (e.g. v0.2.0). Default: latest.
  -InstallDir <dir>   Installation directory (default: `$HOME\.local\bin).
  -Uninstall          Remove Orchicon from the install directory.
  -DryRun             Print what would happen without making changes.
  -Help               Show this help.
"@
    exit 0
}

function Write-Info { param([string]$msg) Write-Host "▸ $msg" -ForegroundColor Cyan }
function Write-Ok   { param([string]$msg) Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Warn { param([string]$msg) Write-Host "! $msg" -ForegroundColor Yellow }
function Write-Err  { param([string]$msg) Write-Host "✗ $msg" -ForegroundColor Red }

# Default install dir
if (-not $InstallDir) {
    $InstallDir = Join-Path $HOME ".local\bin"
}

# --- Uninstall ---
if ($Uninstall) {
    $bin = Join-Path $InstallDir "orchicon.exe"
    if (Test-Path $bin) {
        Write-Info "removing $bin"
        if (-not $DryRun) { Remove-Item $bin -Force }
        Write-Ok "Orchicon uninstalled"
    } else {
        Write-Warn "orchicon not found in $InstallDir — nothing to remove"
    }
    exit 0
}

# --- Detect arch ---
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64"   { "amd64" }
    "ARM64"   { "arm64" }
    default   { Write-Err "unsupported architecture: $env:PROCESSOR_ARCHITECTURE"; exit 1 }
}

# --- Resolve version ---
if (-not $Version -or $Version -eq "latest") {
    Write-Info "fetching latest release version…"
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$GitHubOwner/$GitHubRepo/releases/latest"
    $Version = $release.tag_name
    if (-not $Version) { Write-Err "could not determine latest version"; exit 1 }
}

Write-Info "installing Orchicon $Version for windows/$arch"

# --- Build download URL ---
$asset = "orchicon_$($Version -replace '^v','')_windows_$arch.zip"
$url = "https://github.com/$GitHubOwner/$GitHubRepo/releases/download/$Version/$asset"

# --- Download ---
$tmpdir = Join-Path $env:TEMP "orchicon-install-$(Get-Random)"
New-Item -ItemType Directory -Path $tmpdir -Force | Out-Null
$archive = Join-Path $tmpdir $asset

Write-Info "downloading $url"
Invoke-WebRequest -Uri $url -OutFile $archive

# --- Extract ---
Write-Info "extracting…"
Expand-Archive -Path $archive -DestinationPath $tmpdir -Force

# --- Install ---
$bin = Join-Path $InstallDir "orchicon.exe"
Write-Info "installing to $bin"

if (-not $DryRun) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Move-Item -Path (Join-Path $tmpdir "orchicon.exe") -Destination $bin -Force
}

# --- Cleanup ---
Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue

# --- Verify ---
if (-not $DryRun) {
    $result = & $bin version 2>$null
    if ($result) {
        Write-Ok "Orchicon $Version installed successfully"
    } else {
        Write-Warn "binary installed but could not verify — run '$bin version' to check"
    }
} else {
    Write-Ok "dry-run complete — no changes made"
}

# --- PATH hint ---
$pathDirs = $env:PATH -split ';' | Where-Object { $_ -eq $InstallDir }
if (-not $pathDirs -and -not $DryRun) {
    Write-Warn "Orchicon was installed to $InstallDir which is not on your PATH."
    Write-Host "  Add it via System Properties → Environment Variables, or run:"
    Write-Host "  [Environment]::SetEnvironmentVariable('PATH', `$env:PATH + ';$InstallDir', 'User')"
}

# --- Next steps ---
Write-Host ""
Write-Host "Quick start:" -ForegroundColor White
Write-Host "  orchicon --help           Show available commands" -ForegroundColor DarkGray
Write-Host "  orchicon dev start        Start the full dev environment" -ForegroundColor DarkGray
Write-Host "  orchicon dev status       Check what's running" -ForegroundColor DarkGray
Write-Host ""
Write-Host "Documentation: https://github.com/$GitHubOwner/$GitHubRepo#readme" -ForegroundColor DarkGray
