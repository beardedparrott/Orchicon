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
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Version v0.2.0
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -InstallDir "/usr/local/bin"
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -NoSetup
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Uninstall
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Clean
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -ForceClean
#
# FAILURE BEHAVIOUR — THE WINDOW NEVER CLOSES. This script calls `exit` nowhere,
# and that is a design constraint rather than a style preference. The documented
# entry point is `irm … | iex`, and `iex` runs this file's text in the OPERATOR'S
# OWN session scope: an `exit` there does not end "the script", it ends their
# PowerShell session. The window vanished with the reason still on an unread
# buffer, and the report this fixes is a user who had to launch PowerShell from
# inside a command prompt just to see why the install had crashed. A halt instead
# unwinds to the runner at the bottom of this file (see HALT SEMANTICS), which
# prints what failed, prints the next steps, and returns to the prompt.
#
# PROGRESS — THE LONG AND INTERACTIVE STEPS STREAM. `orchicon install` pulls
# images for minutes and asks about the host ports, so its output is streamed to
# the console as it is produced rather than captured and replayed at the end. A
# captured install is indistinguishable from a hung one, and a captured PROMPT is
# a permanent hang: the question is buffered while the process blocks on the
# terminal nobody has been told to touch. See Invoke-WslBashLive.
#
# A full transcript is written to %TEMP% (see Start-InstallLog) and its path is
# printed at the start of the run and again if anything halts.
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
on Linux. The control plane and Grafana default to http://localhost:8080 and
http://localhost:3002 (8091/3003 for prod); the installer prints the ports it
actually bound, which can differ when a default was already in use.

Options:
  Env: ORCHICON_CONTROL_PORT / ORCHICON_GRAFANA_PORT
                      Host ports for the control plane and Grafana. Set inside
                      WSL before installing when another product already holds
                      a default port. Both launchers honour the same names.
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

This installer NEVER closes your PowerShell window: whatever goes wrong it prints
what failed and the next steps, then returns to the prompt with your session
intact. The long steps (image pull, stack setup) stream their output as they run,
and the setup step may ask about host ports — press ENTER to accept each default.
A full log of the run is written to %TEMP% (path printed when the run starts).
"@
    # `return`, not `exit`: see HALT SEMANTICS. -Help used to close the window too.
    return
}

function Write-Info { param([string]$msg) Write-Host "▸ $msg" -ForegroundColor Cyan }
function Write-Ok   { param([string]$msg) Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Warn { param([string]$msg) Write-Host "! $msg" -ForegroundColor Yellow }
function Write-Err  { param([string]$msg) Write-Host "✗ $msg" -ForegroundColor Red }

# --- HALT SEMANTICS ---------------------------------------------------------
# A halt stops the INSTALLER WITHOUT TOUCHING THE OPERATOR'S SESSION. `exit` is
# the obvious way to write this, and it is wrong here for a reason that only shows
# up in the supported invocation: `irm https://orchicon.dev/install.ps1 | iex`
# executes this file's text at the operator's own prompt, so `exit` closes the
# window and takes the diagnosis with it. (Established by measurement, not by
# reading: with `exit 1` in an iex'd script nothing after it runs and the session is
# gone; with `return`, or with the sentinel `throw` caught below, the session is
# untouched.)
#
# The report this exists for: on a Windows box with no Docker Desktop the
# installer "just closed the powershell session", and the only way to read why was
# to start PowerShell from inside a command prompt, which keeps the console alive
# when its child exits.
#
# So every stop throws the sentinel and the runner at the bottom of this file
# catches it. The string is matched BY MESSAGE, which makes the throw here and the
# catch there a PAIR: changing one without the other turns every halt into an
# "unexpected error".
function Stop-Install {
    param([string]$msg)
    if ($msg) { Write-Err $msg }
    throw "OrchiconInstallHalt"
}

# --- Transcript (a closed window is not a diagnosis) ------------------------
# THE REPORT THIS EXISTS FOR: the failure closed the window, so there was nothing
# left to read — and even with the window fixed, a long install scrolls its own
# diagnosis off the screen. So the whole run is transcribed, and the path is
# printed as soon as it starts, so it is known even if the window disappears for
# some reason this script does not control.
#
# Never fatal, and never the operator's own transcript: a host that refuses
# transcription (a non-console host) skips it, Start-Transcript fails rather than
# stealing a transcript the operator already had running, and Stop-Transcript is
# called only for a transcript WE started.
#   ORCHICON_INSTALL_LOG=<path>   choose the file
#   ORCHICON_INSTALL_LOG=off      disable it
function Start-InstallLog {
    if ($env:ORCHICON_INSTALL_LOG -eq "off") { return }
    $path = $env:ORCHICON_INSTALL_LOG
    if (-not $path) {
        $path = Join-Path ([System.IO.Path]::GetTempPath()) ("orchicon-install-{0}.log" -f (Get-Date -Format "yyyyMMdd-HHmmss"))
    }
    try {
        Start-Transcript -Path $path -Force | Out-Null
        $script:LogPath = $path
        $script:LogStarted = $true
        Write-Host "  (full log of this install: $path)" -ForegroundColor DarkGray
    } catch {
        # A host without transcription support, or a transcript already running.
        # Neither is worth failing an install over, so the log is simply absent.
        $script:LogStarted = $false
    }
}
function Stop-InstallLog {
    if (-not $script:LogStarted) { return }
    try { Stop-Transcript | Out-Null } catch { }
    $script:LogStarted = $false
}

# wsl.exe writes UTF-16LE when its output is redirected. Decoded on a non-Unicode
# console codepage that produces garbage: a localized wsl message renders as glyph
# soup, and a distro name arrives with NUL bytes between its characters
# ("U`0b`0u`0n`0t`0u"). The NULs are invisible when printed, but the name then
# truncates at the first NUL when it is handed back to `wsl -d <name>`, so every
# later call fails with "no distribution with the supplied name".
#
# Stripping the NULs recovers an ASCII name, but it cannot recover non-ASCII text:
# by the time PowerShell hands us the string the bytes are already gone. So the
# capture is made deterministic instead — WSL_UTF8=1 makes wsl.exe emit UTF-8
# rather than UTF-16LE (supported since WSL 0.64.0, 2022: the same release that
# added --exec), and the distro's own output is UTF-8 as well, so decoding the
# capture as UTF-8 is correct for BOTH streams. Clear-WslNul stays as the fallback
# for WSL older than 0.64, and for anything that still writes UTF-16.
function Clear-WslNul {
    param($Lines)
    @($Lines | ForEach-Object { [string]$_ -replace "`0", '' })
}

# Run wsl.exe and return its output with a known encoding. $LASTEXITCODE is left
# holding wsl's exit code: only a native command sets it, and neither the console
# properties nor Clear-WslNul below are native commands.
function Invoke-WslCapture {
    param([string[]]$WslArgs, [switch]$Quiet)
    $prevEncoding = [Console]::OutputEncoding
    $prevUtf8 = $env:WSL_UTF8
    # On Windows PowerShell 5.1, `2>&1` turns native stderr into ErrorRecords
    # which the script-level "Stop" preference would treat as terminating.
    # Scope "Continue" locally so wsl's chatter/errors never abort us here.
    $ErrorActionPreference = "Continue"
    $env:WSL_UTF8 = "1"
    try {
        # Best-effort: a host that refuses the console-encoding change still gets
        # the NUL fix from WSL_UTF8 alone.
        try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
        $out = if ($Quiet) { & wsl @WslArgs 2>$null } else { & wsl @WslArgs 2>&1 }
    } finally {
        # Restore both: this script runs via `irm | iex` and must not leave the
        # operator's session on a different console encoding than it found it.
        try { [Console]::OutputEncoding = $prevEncoding } catch { }
        $env:WSL_UTF8 = $prevUtf8
    }
    return (Clear-WslNul $out)
}

# --- WSL helpers ------------------------------------------------------------

# Run a bash script inside the target WSL distro. The script's output is the
# function's output; $LASTEXITCODE carries its exit code (PowerShell sets it
# automatically after the native `wsl` call, so callers read it afterwards).
function Invoke-WslBash {
    param([string]$Script)
    # `-e` is required: without it wsl re-parses the command through the
    # distro's default shell, which expands $ARCHIVE/$TMP/$# as empty before
    # the target bash sees them (tar got an empty path). `-e` execs with argv
    # preserved.
    if ($script:Distro) {
        return (Invoke-WslCapture -WslArgs @("-d", $script:Distro, "-e", "bash", "-lc", $Script))
    }
    return (Invoke-WslCapture -WslArgs @("-e", "bash", "-lc", $Script))
}

# Run a bash script inside the target WSL distro with its output STREAMED to this
# console AS IT IS PRODUCED, instead of captured and replayed when it finishes.
#
# WHY THIS EXISTS, and it is the difference between an install that looks hung and
# one that reports itself. Invoke-WslBash captures, which is right for a step whose
# output we PARSE and wrong for a step that takes minutes or asks a question:
#
#   * Capture makes a WORKING install indistinguishable from a hung one. Pulling
#     the published images is hundreds of megabytes and many minutes, and none of
#     that progress reached the operator — the window sat on one line.
#   * Capture makes a PROMPT a permanent hang. `orchicon install` asks about the
#     host ports (an empty line accepts the default) and reads the CONTROLLING
#     TERMINAL while writing the question to stdout. Captured, the question is
#     buffered and invisible while the process blocks on a terminal nobody has been
#     told to touch — which is exactly the "it is just hanging on 'Setting up the
#     full stack inside WSL'" report.
#
# TWO DELIBERATE DETAILS, both of which are why this is a separate function rather
# than a switch on the capturing one:
#
#   * NO `2>&1`. On Windows PowerShell 5.1 that turns the child's stderr into
#     ErrorRecords, which the script-level "Stop" preference then treats as
#     terminating. Left alone, stderr goes straight to this console, which is what a
#     streaming caller wants anyway.
#   * $ErrorActionPreference is scoped "Continue", so wsl's own chatter (a distro
#     notice, a version warning) cannot abort the install mid-stream.
#
# $LASTEXITCODE is set by the native call and read by the caller, exactly as with
# Invoke-WslBash. Call it as a STATEMENT: its output belongs on the console, and
# assigning it would buffer the very thing this exists to stream.
function Invoke-WslBashLive {
    param([string]$Script)
    $prevEncoding = [Console]::OutputEncoding
    $prevUtf8 = $env:WSL_UTF8
    $ErrorActionPreference = "Continue"
    $env:WSL_UTF8 = "1"
    try {
        try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
        if ($script:Distro) {
            & wsl -d $script:Distro -e bash -lc $Script
        } else {
            & wsl -e bash -lc $Script
        }
    } finally {
        try { [Console]::OutputEncoding = $prevEncoding } catch { }
        $env:WSL_UTF8 = $prevUtf8
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
        Stop-Install
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
    # `@(...)` wraps the WHOLE pipeline, not just the capture: with one distro
    # installed this leaves a bare string, and `$names[0]` on a string is its first
    # CHARACTER — the fallback below would then pick "U" out of "Ubuntu".
    $names = @(Invoke-WslCapture -WslArgs @("--list", "--quiet") -Quiet |
        Where-Object { $_ -and $_ -notmatch "no installed distributions" -and $_ -notmatch "Windows Subsystem" })
    if ($names.Count -eq 0) {
        if ($Soft) { return $false }
        Write-Err "WSL is installed but has no Linux distribution."
        Write-Host ""
        Write-Host "Next steps:" -ForegroundColor Yellow
        Write-Host "  1. wsl --install -d Ubuntu   (or: wsl --install)"
        Write-Host "  2. Complete first-run setup (create a Linux user + password)."
        Write-Host "  3. Re-run:  irm https://orchicon.dev/install.ps1 | iex"
        Stop-Install
    }

    # Prefer the default distro (marked with `*` in `wsl --list --verbose`).
    $verbose = Invoke-WslCapture -WslArgs @("--list", "--verbose") -Quiet
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
            Stop-Install
        }
    } else {
        $script:Distro = $names[0]
    }

    Write-Ok "using WSL distro: $script:Distro"
    return $true
}

# --- Docker check (inside WSL) ----------------------------------------------
# THREE DIFFERENT FAILURES LOOK IDENTICAL FROM HERE, and each needs a different
# fix, so ask each question separately instead of printing one undifferentiated
# list of steps:
#
#   1. no docker CLI in the distro at all — Docker was never installed, or was
#      installed on Windows without ever being made visible to WSL;
#   2. a CLI that cannot reach a daemon — Docker Desktop is installed but its engine
#      is not running (the common case);
#   3. a CLI, a RUNNING Docker Desktop, and still no daemon — the WSL integration is
#      not enabled for THIS distro, so its socket is not mounted into it.
#
# The report that prompted this: "if you don't have docker desktop setup, it just
# closes the powershell session" — a crash that hid its own cause twice over, in the
# halt (now fixed: see HALT SEMANTICS) and in a list that named no cause at all.
function Ensure-WslDocker {
    Write-Info "checking Docker inside $script:Distro…"
    $daemon = (Invoke-WslBash "docker version --format '{{.Server.Version}}' 2>/dev/null" | Out-String).Trim()
    if ($LASTEXITCODE -eq 0 -and $daemon) {
        Write-Ok "Docker is available inside WSL (server $daemon)"
        return
    }

    # What the distro itself says. `command -v` answers "is there a CLI at all" in a
    # locale-independent way; the docker error text is what separates "no daemon"
    # from anything else.
    $cli = (Invoke-WslBash "command -v docker" | Out-String).Trim()
    $cliMissing = ($LASTEXITCODE -ne 0 -or -not $cli)
    $detail = (Invoke-WslBash "docker version 2>&1 | tail -4" | Out-String).Trim()
    $desktop = Test-DockerDesktopRunning

    Write-Err "Docker is not usable inside '$script:Distro'."
    Write-Host ""
    if ($cliMissing) {
        Write-Host "Cause: this WSL distro has no docker CLI at all." -ForegroundColor Yellow
        Write-Host "  1. Install Docker Desktop: https://www.docker.com/products/docker-desktop/"
        Write-Host "  2. In Docker Desktop → Settings → Resources → WSL Integration, enable the"
        Write-Host "     integration for '$script:Distro' — that is what puts the CLI on the"
        Write-Host "     distro's PATH. (Or install Docker Engine inside the distro itself.)"
    } elseif ($desktop -ne $true) {
        Write-Host "Cause: the CLI is in '$script:Distro', but there is no Docker daemon to reach." -ForegroundColor Yellow
        Write-Host "  Docker Desktop does not look like it is running on Windows (or its"
        Write-Host "  processes could not be inspected)."
        Write-Host "  1. Start Docker Desktop and wait until it reports the engine is running."
        Write-Host "  2. Then re-run this installer."
    } else {
        Write-Host "Cause: Docker Desktop is running, but '$script:Distro' cannot reach its daemon." -ForegroundColor Yellow
        Write-Host "  That is almost always the WSL integration for this distro being off."
        Write-Host "  1. Docker Desktop → Settings → Resources → WSL Integration."
        Write-Host "  2. Enable it for the default distro AND toggle '$script:Distro' on."
        Write-Host "  3. Apply & restart, then re-run this installer."
    }
    if ($detail) {
        Write-Host ""
        Write-Host "What the distro reported:" -ForegroundColor DarkGray
        ($detail -split "`r?`n") | Where-Object { $_ } | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
    }
    Write-Host ""
    Write-Host "Check it by hand inside the distro:" -ForegroundColor DarkGray
    Write-Host "  wsl -d $script:Distro -e docker version" -ForegroundColor DarkGray
    Stop-Install
}

# Test-DockerDesktopRunning reports whether Docker Desktop looks alive ON THE
# WINDOWS SIDE: $true, $false, or $null when we cannot tell. It only picks which
# fix to print, so an unreadable process list must not become a WRONG instruction —
# the caller treats anything that is not $true as "not running, or cannot tell" and
# says so.
function Test-DockerDesktopRunning {
    try {
        $procs = @(Get-Process -Name "Docker Desktop", "com.docker.backend", "dockerd" -ErrorAction SilentlyContinue)
        return ($procs.Count -gt 0)
    } catch {
        return $null
    }
}

# --- Connection info (Windows-visible URLs) ----------------------------------
# The instance name comes from the environment (dev by default). Resolved here,
# ONCE, so the connection info and the setup-failure hints cannot disagree about
# which instance this is.
function Get-InstanceName {
    if ($env:ORCHICON_INSTANCE) { return $env:ORCHICON_INSTANCE }
    return "dev"
}

function Write-ConnectionInfo {
    $instance = Get-InstanceName
    # Resolve the ports the SAME way the installer does (cmd/orchicon/install_ports.go
    # and scripts/container.sh instance_info): the per-instance default, with
    # ORCHICON_CONTROL_PORT / ORCHICON_GRAFANA_PORT overriding it when set. These
    # numbers used to be hardcoded right here, which printed a URL pointing at a port
    # this install never bound as soon as an operator moved off a busy default.
    if ($instance -eq "prod") { $ctrl = 8091; $graf = 3003 } else { $ctrl = 8080; $graf = 3002 }
    if ($env:ORCHICON_CONTROL_PORT) { $ctrl = $env:ORCHICON_CONTROL_PORT }
    if ($env:ORCHICON_GRAFANA_PORT) { $graf = $env:ORCHICON_GRAFANA_PORT }
    Write-Host ""
    Write-Host "Open from Windows (WSL2 forwards localhost):" -ForegroundColor White
    Write-Host "  Control plane: http://localhost:$ctrl" -ForegroundColor Cyan
    Write-Host "  Grafana:       http://localhost:$graf" -ForegroundColor Cyan
    Write-Host "  (if a default port was already in use, 'orchicon install' suggested the next" -ForegroundColor DarkGray
    Write-Host "   free one — its own output lists the ports it actually bound)" -ForegroundColor DarkGray
    Write-Host "  Note: if the URLs do not answer, check Windows Defender Firewall,"
    Write-Host "  or add a port forward: netsh interface portproxy add v4tov4 listenport=$ctrl"
    Write-Host "  listenaddress=127.0.0.1 connectport=$ctrl connectaddress=<WSL-IP>" -ForegroundColor DarkGray
}

# --- RUN THE INSTALLER ------------------------------------------------------
# THE RUNNER, and the only place that decides what the operator sees after a stop.
# The body below sits in a try/catch for three reasons, in order of importance:
#
#   1. A halt (Stop-Install) must NOT take the session with it. Unwinding to this
#      catch is what turns "the window vanished" into "it printed why, and returned to
#      the prompt". Mechanism and the report behind it: see HALT SEMANTICS.
#   2. The failure footer lives in ONE place, so every halt reads the same way and a
#      halt path added later cannot forget to print the next steps.
#   3. An UNEXPECTED error (a defect here, a hostile environment) is reported with
#      its position, instead of scrolling past as a raw exception.
#
# The body is NOT a function, deliberately: `return` still ends a mode (-Uninstall,
# -DryRun) while try/catch introduces no new scope, so everything the body sets stays
# readable in the catch — which the footer needs ($script:Distro, $InstallDir).
$script:ExitCode = 0
try {
    # Start the transcript FIRST, so everything below is in it. Skipped for a dry
    # run, which changes nothing and does not need a log file.
    if (-not $DryRun) { Start-InstallLog }
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
            return
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
            Write-Host "  wsl -d $script:Distro -e bash -lc 'docker stop orchicon-cnt-dev orchicon-cnt-prod'"
            Write-Host "  wsl -d $script:Distro -e bash -lc 'rm -f $bin'"
        } else {
            Invoke-WslBash $uninstallScript
            Write-Ok "Orchicon uninstalled — the WSL distro is left intact"
        }
        # `return`, not `exit`: this path SUCCEEDS, and it used to close the window on
        # the way out (see HALT SEMANTICS).
        return
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
# ANCHORED TO THE STATE DIRECTORY, NEVER A RELATIVE NAME. This read
# `cd "$HOME"; rm -rf data .dev bin .local/share/orchicon`, which deleted
# `$HOME/bin`, `$HOME/data` and `$HOME/.dev` — directories Orchicon does not own,
# and `~/bin` in particular is where people keep their own binaries. The `.local`
# entry was ALSO relative, so it never touched the real state dir at $HOME/.local.
rm -rf "$HOME/.local/share/orchicon"
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
        # A halt, not an exit: there is nothing this script can do about an unknown
        # arch, and the operator needs to read the DETECTED value rather than watch the
        # window vanish on them.
        default   { Stop-Install "unsupported architecture: $env:PROCESSOR_ARCHITECTURE (expected AMD64 or ARM64)" }
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
            Stop-Install "could not reach GitHub releases API: $($_.Exception.Message)"
        }
        $Version = $release.tag_name
        if (-not $Version) { Stop-Install "could not determine the latest version — api.github.com returned no tag" }
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
            Write-Host "  4. open the control plane / Grafana on the ports the installer prints"
            Write-Host "     (dev defaults: 8080 / 3002; prod: 8091 / 3003)"
        }
        Write-Ok "dry-run complete — no changes made"
        return
    }

    # [System.IO.Path]::GetTempPath() rather than $env:TEMP. TEMP is always set on
    # Windows, but the installer should not CRASH where it is not, and crashing here
    # is a bad failure: the error is "Cannot bind argument to parameter 'Path'
    # because it is null", raised from a string join, which reads as a defect in the
    # install rather than as a missing environment variable. (Found by running the
    # installer's failure paths under a stripped environment.)
    $tmpdir = Join-Path ([System.IO.Path]::GetTempPath()) "orchicon-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpdir -Force | Out-Null
    $archive = Join-Path $tmpdir $asset
    $wslArchive = ConvertTo-WslPath $archive

    Write-Info "downloading $url"
    try {
        Invoke-WebRequest -Uri $url -OutFile $archive
    } catch {
        Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
        Stop-Install "download failed: $($_.Exception.Message)"
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
ORCH="$(find "$TMP" -type f -name orch -perm -u+x 2>/dev/null | head -1)"
if [ -n "$ORCH" ]; then
  mv "$ORCH" "$INSTALL_DIR/orch"
  chmod +x "$INSTALL_DIR/orch"
  echo "orch (remote TUI client) installed: $INSTALL_DIR/orch"
else
  echo "orch companion binary not found in archive (older release?) — skipping"
fi
"$INSTALL_DIR/orchicon" version
rm -rf "$TMP"
'@
    $installScript = $installScript.Replace('__INSTALL_DIR__', $InstallDir).Replace('__ARCHIVE__', $wslArchive)

    Write-Info "extracting and installing inside WSL (output streams below)…"
    # STREAMED, not captured: a truncated download or a distro without room fails here,
    # and the reason belongs where the operator can see it rather than in a buffer.
    Invoke-WslBashLive $installScript
    if ($LASTEXITCODE -ne 0) {
        Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
        Stop-Install "install inside WSL failed (exit $LASTEXITCODE)"
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
    Write-Info "verifying the installed binary…"
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
        # SAY WHAT IS ABOUT TO HAPPEN, INCLUDING THE QUESTION. `orchicon install` pulls
        # the published images (minutes, hundreds of megabytes on a first install) and
        # asks about the host ports. Both facts are load-bearing: without the first, a
        # working pull is indistinguishable from a hang; without the second, an operator
        # staring at a still screen never learns that the installer is waiting for them.
        Write-Host "  · pulling the published images — the slow part (minutes on a first install)" -ForegroundColor DarkGray
        Write-Host "  · starting the runtime daemon, then launching the instance" -ForegroundColor DarkGray
        Write-Host "  · it asks about host ports: press ENTER to accept each default" -ForegroundColor DarkGray
        Write-Host "  Its output is shown live below." -ForegroundColor DarkGray
        Write-Host ""
        $setupScript = @'
set -euo pipefail
INSTALL_DIR="__INSTALL_DIR__"
INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
# Orchicon ships its own runtime engine, so NO adapter CLI is required — a
# fresh install is complete on its own. External adapters (opencode and future
# ones) are optional: installed in the distro by the operator and mounted into
# the containers at runtime, never baked in. Mentioned only when present.
if command -v opencode >/dev/null 2>&1 || [ -x "$HOME/.opencode/bin/opencode" ]; then
  echo "  opencode found in this WSL distro — optional, and will be used as a" >&2
  echo "  runtime when a model ref asks for it." >&2
  echo "" >&2
fi
"$INSTALL_DIR/orchicon" install
'@
        $setupScript = $setupScript.Replace('__INSTALL_DIR__', $InstallDir)
        # LIVE, not captured. This is THE step that takes minutes and asks questions:
        # capturing it is what made a working install look hung, and an invisible port
        # prompt look like a dead terminal. See Invoke-WslBashLive.
        Invoke-WslBashLive $setupScript
        $setupExit = $LASTEXITCODE
        if ($setupExit -eq 0) {
            Write-Ok "Install complete — Orchicon is running."
            Write-ConnectionInfo
        } else {
            # The BINARY is installed; only the stack setup failed. Report the exit code,
            # say where the reason will be, and give the exact command that resumes —
            # `orchicon install` is idempotent and picks up where this stopped.
            $instance = Get-InstanceName
            Write-Warn "Full-stack setup did not complete (exit $setupExit). The binary itself is installed at:"
            Write-Host "  $InstallDir/orchicon"
            Write-Host "  Resume inside WSL (idempotent — it picks up where this stopped):"
            Write-Host "    wsl -d $script:Distro -e bash -lc '$InstallDir/orchicon install'" -ForegroundColor Cyan
            Write-Host "  Where the reason will be:"
            Write-Host "    · the output above — the failing step prints its own reason"
            Write-Host "    · docker logs --tail 50 orchicon-cnt-$instance   (the stack's services)"
            Write-Host "    · the runtime daemon log: /tmp/orchicon-runtime/runtime-daemon.log in the distro"
            # A non-zero outcome for a wrapper: the binary is in place, the stack is not.
            $script:ExitCode = 1
        }
    }

    Write-Host ""
} catch {
    $script:ExitCode = 1
    Write-Host ""
    if ($_.Exception.Message -eq "OrchiconInstallHalt") {
        # The halt printed its own cause and its own fix, above. All that is left is
        # what an operator needs next: the knowledge that those messages are still on
        # screen (the window did NOT close — the whole point of this runner), and the
        # exact way back in.
        Write-Host "Orchicon was not installed." -ForegroundColor Red
    } else {
        # NOT a halt: a defect in this installer, or an environment it misjudges.
        # Say what we have and point at the log rather than at the operator.
        Write-Err "Orchicon installer stopped with an unexpected error:"
        Write-Host "  $($_.Exception.Message)"
        if ($_.InvocationInfo -and $_.InvocationInfo.PositionMessage) {
            Write-Host "  $($_.InvocationInfo.PositionMessage)" -ForegroundColor DarkGray
        }
        Write-Host "  This is a defect in the installer rather than something you did — please"
        Write-Host "  report it with the log below:"
        Write-Host "    https://github.com/$GitHubOwner/$GitHubRepo/issues" -ForegroundColor Cyan
    }
    Write-Host ""
    Write-Host "  This window was left open on purpose: the installer never closes it, so" -ForegroundColor DarkGray
    Write-Host "  the messages above stay readable. What was already done is left in place —" -ForegroundColor DarkGray
    Write-Host "  a re-run is idempotent and picks up where this stopped." -ForegroundColor DarkGray
    Write-Host ""
    Write-Host "  Fix what is reported above, then:"
    Write-Host "    PowerShell:  irm https://orchicon.dev/install.ps1 | iex" -ForegroundColor Cyan
    if ($script:Distro -and $InstallDir) {
        Write-Host "    WSL:         wsl -d $script:Distro -e bash -lc '$InstallDir/orchicon install'" -ForegroundColor Cyan
    }
    if ($script:LogPath) {
        Write-Host ""
        Write-Host "  full log of this run: $script:LogPath" -ForegroundColor DarkGray
    }
} finally {
    # ALWAYS — on a halt, on an early return, and on success. Leaving the operator's
    # session transcribing everything they type next would be a poor way to repay
    # their attention.
    Stop-InstallLog
    # Publish the outcome here so it also covers an early `return`. It is deliberately
    # NOT a process exit code: nothing here calls `exit` (see HALT SEMANTICS), so a
    # caller running this with `pwsh -File` still sees success — read the output, or
    # $LASTEXITCODE.
    $global:LASTEXITCODE = $script:ExitCode
}

Write-Host "Documentation: https://github.com/$GitHubOwner/$GitHubRepo#readme" -ForegroundColor DarkGray
