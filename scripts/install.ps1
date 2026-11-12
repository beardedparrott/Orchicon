# ============================================================================
# Orchicon installer (Windows / PowerShell — NATIVE)
#
# Installs the native Windows build of Orchicon and brings this machine's plane
# up as a native Windows process:
#
#   irm https://orchicon.dev/install.ps1 | iex
#
# What this script does:
#   1. Refuses to run from an ELEVATED shell, before it downloads anything.
#      Postgres refuses to run as Administrator, so an elevated install would
#      fail three steps later with an initdb error nobody can act on; the guard
#      says what to do instead.
#   2. Downloads orchicon_<version>_windows_amd64.zip and installs orchicon.exe
#      and orch.exe into %LOCALAPPDATA%\Programs\Orchicon — a per-user
#      directory, so the install needs no administrator rights.
#   3. Runs `orchicon.exe install`: acquires the host service binaries (Postgres,
#      NATS), starts the runtime daemon and launches THIS instance's plane as
#      host processes of the invoking user. It OFFERS the optional runtime
#      images (multi-select, default none, each with its size) and pulls only
#      what was chosen — nothing by default.
#   4. Prints the URLs to open, read back from the ports the install RECORDED.
#
# NO WSL2 AND NO DOCKER DESKTOP ARE REQUIRED. The plane and its services are host
# processes of the invoking user, and the container engine (podman first, then
# docker) is provisioned and diagnosed by `orchicon install` itself — see
# cmd/orchicon/install_engine.go — rather than by this wrapper. This script no
# longer probes for an engine at all: the three-cause "no CLI / no daemon / no
# WSL integration" report it used to print described a requirement that no
# longer exists.
#
# THIS SCRIPT NEVER TOUCHES WSL. Runtime containers reach the container engine
# through `podman machine`, which podman creates and owns, so a Windows host needs
# no distro of its own and `wsl --install` is not a step in this install. The WSL
# detection and script-transport logic that used to live here went with the
# WSL-first architecture it served — see the note where it was.
#
# -Desktop IS THE ONE THING THIS SCRIPT INSTALLS THAT IS NOT A HOST BINARY OF THE
# STACK, and it is worth being precise about it: the Orchicon desktop app IS a
# native Windows binary (a Wails/WebView2 shell), so it cannot be installed by the
# same copy-into-InstallDir step the CLI binaries use. With -Desktop this script
# downloads the WINDOWS desktop installer and runs it; the app then talks to the
# plane on the same host over loopback, exactly as the browser UI does.
#
# Options:
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Version v0.2.0
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -InstallDir "D:\tools\Orchicon"
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -NoContainers   # engine-free host
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -NoSetup
#   & ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -NoVerify
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
# PROGRESS — THE LONG AND INTERACTIVE STEPS STREAM. `orchicon install` acquires
# host binaries (minutes on a first install) and asks about the host ports and the
# optional runtime images, so it is invoked BARE — `& $exe install` — and a
# directly-invoked native child inherits this console, so its output appears as it
# is produced. That is the whole mechanism: NOTHING HERE CAPTURES IT. A captured
# install (`$out = & $exe install`, `| Out-String`, `Tee-Object -Variable`) is
# indistinguishable from a hung one, and a captured PROMPT is a permanent hang.
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
    # ACCEPTED, AND NO LONGER MEANS ANYTHING POSITIVE: containers are the default now, so this
    # switch exists only so a script or a documented command that passes it keeps working. It is
    # deliberately not an error — silently breaking a command someone copied is worse than
    # accepting a redundant flag.
    [switch]$Containers,
    # THE OPT-OUT. For a host that must stay engine-free (locked-down, policy-managed, a CI guest).
    [switch]$NoContainers,
    [switch]$Uninstall,
    [switch]$Clean,
    [switch]$ForceClean,
    [switch]$DryRun,
    [switch]$NoSetup,
    [switch]$NoVerify,
    [switch]$RemoveBinary,
    [switch]$Yes,
    [switch]$Purge,
    [switch]$Help,
    [switch]$Desktop,
    [string]$Instance = ""
)

$ErrorActionPreference = "Stop"

# STRICT MODE, VERSION 1.0: reading an unassigned variable is an ERROR.
#
# THIS IS THE CLASS GUARD for the defect that shipped. The MSI arm used $tmpdir before
# anything assigned it, and PowerShell silently reads an unassigned variable as $null — so the
# failure surfaced one line later as
#   Cannot bind argument to parameter 'Path' because it is null
# which reads as a broken install rather than as an unset variable, and sent the operator to
# report a crash that named nothing. Under this mode the same mistake fails AT THE USE:
#   The variable '$tmpdir' cannot be retrieved because it has not been set.
#
# VERSION 1.0 ON PURPOSE, and it is the narrowest mode that catches this. Later versions also
# prohibit non-existent object PROPERTIES and out-of-bounds array indexes — a much wider net
# than this fix should cast over a 1400-line installer operators run.
Set-StrictMode -Version 1.0

$GitHubOwner = "beardedparrott"
$GitHubRepo = "Orchicon"
# (The WSL distro/state fields that used to live here are gone with the WSL path.)
# INITIALISED, not merely assigned-on-use. Under Set-StrictMode (above) reading an unassigned
# variable is an error, and the transcript state is only set when a transcript is actually
# started — so every run with ORCHICON_INSTALL_LOG=off used to read it unset. Found by turning
# strict mode on: six harness assertions failed on exactly this.
$script:LogStarted = $false
$script:LogPath = ""
# CONTAINERS ARE ON BY DEFAULT, AND THE OPT-OUT IS THE DIRECTION THAT HAS TO BE EXPLICIT.
#
# The product decision, in the operator's words: "I want container support out of the box.
# People shouldn't need to pass a parameter… People like to be spoon fed. It should install by
# default and be running and ready to go."
#
# That is the right default for a different reason too: execution MODE is a per-project and
# per-work-item setting the operator switches at will. A host that never provisioned the engine
# cannot honour a switch to container mode — it can only fail later, when a run needs a
# container the host cannot make. Provisioning at install is what makes the switch real.
#
# -NoContainers (or ORCHICON_INSTALL_CONTAINERS=0) opts out for a host that must stay
# engine-free: a locked-down desktop, a policy-managed machine, a CI guest. Everything else
# installs the engine and gets containers ready.
$script:ContainerOptIn = -not ([bool]$NoContainers -or $env:ORCHICON_INSTALL_CONTAINERS -eq "0")
# The linux/amd64 binary a runtime container runs, set when one is installed. Reported as a verdict
# at the end of the install: without it, container execution cannot work on this host.
$script:ContainerBinary = $null

if ($Help) {
    Write-Host @"
Orchicon installer (Windows — native)

Usage: install.ps1 [options]

Orchicon's plane and its services (Postgres, NATS) run as native Windows host
processes of the user who installs them. This script installs the Windows build
(orchicon.exe + orch.exe) into %LOCALAPPDATA%\Programs\Orchicon and runs the
one-command setup, which starts this instance's plane and prints the URL it bound.

When the release publishes an MSI, this script installs from THAT instead: the
same binaries, wrapped so Windows' own deployment tooling (Intune, SCCM, Group
Policy, `msiexec /qn`) can use it, and carrying the desktop app and its shortcuts.
The MSI is per-user and needs no elevation. -Yes and -NoSetup deliberately keep the
zip path, because an MSI's wizard cannot be driven unattended.

WSL2 and Docker Desktop are NOT required. Container support IS installed by
default (podman, whose machine brings its own Linux VM), so no WSL distro of your
own is needed — this installer never runs `wsl`.

Options:
  -Version <tag>      Install a specific version (e.g. v0.1.173). Default: latest.
  -Instance <name>    Which instance to act on: dev (default), test, or prod. Each has its
                      own plane, ports, host services and host state, so -Instance test
                      cannot disturb dev or prod. Applies to install AND to
                      -Clean/-ForceClean/-Uninstall.
  -InstallDir <dir>   Where to put orchicon.exe and orch.exe
                      (default: %LOCALAPPDATA%\Programs\Orchicon).
  -NoContainers       Skip the container engine (podman) and its Linux substrate entirely.
                      Containers are provisioned BY DEFAULT, so a run that needs one just works;
                      pass this only on a host that must stay engine-free (locked-down,
                      policy-managed, a CI guest). ORCHICON_INSTALL_CONTAINERS=0 does the same.
  -Containers        (accepted, no longer needed) Containers are the default; kept so existing
                      commands keep working. The engine (podman) is provisioned by
                      `orchicon install`, which also creates and starts its machine.
  -NoSetup            Install the binary only — do NOT start the host services, the
                      runtime daemon, or the plane.
  -Desktop            ALSO install the desktop app (a GUI client for this plane).
                      The app is a native Windows binary, so this is the one step that
                      runs an installer rather than copying files. That installer
                      carries the WebView2 Evergreen bootstrapper, so a Windows 10
                      machine provisions the runtime during setup instead of opening
                      a blank window (Windows 11 has it already). Fully optional —
                      the app is a client, and the web UI at the same URL does
                      everything it does. Without it, the install prints where to get
                      the app.
  -Uninstall          Stop everything THIS instance runs - its plane and its host services
                      (Postgres, NATS) - and remove the Orchicon binaries. DATA IS PRESERVED
                      (the instance's host state: database, blob store, runtime state), and
                      the summary says where it is.
  -Purge              With -Uninstall: ALSO destroy that data - the instance's host state
                      directory (database, blob store, runtime state). Irreversible. Without
                      -Uninstall this does nothing.
  -RemoveBinary       Remove just the orchicon and orch executables. Every instance's state
                      is left alone. If something is running off them it asks before
                      stopping it, and declining changes nothing.
  -Yes                Answer the prompts affirmatively, for non-interactive use. That
                      includes the optional runtime images (it answers: all of them).
                      It also means the setup is NOT told a human is present, so it
                      asks nothing.
  -Clean              Stop this instance's plane and host services, remove the old
                      binaries, then install the latest version — one-shot upgrade. All
                      user data is preserved (database, BlobStore files, runtime state).
  -ForceClean         Wipe everything for THIS instance and start fresh: stop it, destroy
                      its data directory (database, BlobStore files, runtime state),
                      remove local state, then install the latest version. WARNING: all
                      data for this instance is lost.
  -DryRun             Print what would happen without making changes.
  -Help               Show this help.

The setup step also OFFERS the optional runtime images (runtime base ~1 GB, GUI +150 MB,
  dev +1.5-2.5 GB). It is a multi-select that defaults to NONE: pressing ENTER pulls no image,
  and that is a complete install - the plane runs and a project in local mode needs none.

This installer NEVER closes your PowerShell window: whatever goes wrong it prints
what failed and the next steps, then returns to the prompt with your session
intact. The long steps (binary acquisition, stack setup) stream their output as they run,
and the setup step may ask about host ports — press ENTER to accept each default.
A full log of the run is written to %TEMP% (path printed when the run starts).
"@
    # `return`, not `exit`: see HALT SEMANTICS. -Help used to close the window too.
    return
}

function Write-Info { param([string]$msg) Write-Host "▸ $msg" -ForegroundColor Cyan }
function Write-Ok   { param([string]$msg) Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Warn { param([string]$msg) Write-Host "! $msg" -ForegroundColor Yellow }

# Test-IsElfBinary reports whether a file starts with the ELF magic.
# The daemon refuses anything else as a container entrypoint, so checking here turns a
# wrong or truncated download into an install-time message instead of a failed run.
function Test-IsElfBinary {
    param([string]$Path)
    try {
        $fs = [System.IO.File]::OpenRead($Path)
        try {
            $buf = New-Object byte[] 4
            if ($fs.Read($buf, 0, 4) -lt 4) { return $false }
            return ($buf[0] -eq 0x7f -and $buf[1] -eq 0x45 -and $buf[2] -eq 0x4c -and $buf[3] -eq 0x46)
        } finally { $fs.Dispose() }
    } catch { return $false }
}
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

# --- NOTHING HERE TALKS TO WSL -----------------------------------------------
#
# A WHOLE TRANSPORT LAYER USED TO LIVE HERE, AND IT IS DELETED, NOT PARKED: Invoke-WslCapture,
# Clear-WslNul, ConvertTo-BashPayload, WslBashArgs, Invoke-WslBash and Invoke-WslBashLive —
# ~120 lines whose only job was carrying a bash script into a user's WSL distro and decoding
# what came back.
#
# It was load-bearing and hard-won. Windows PowerShell 5.1 does not escape embedded double
# quotes when it flattens an argument into a native command line, so wsl.exe ate every quote as
# an argument delimiter and the guest ran a DIFFERENT PROGRAM: a quoted `mktemp` assignment
# arrived as a bare `-d)` and the install died with "unexpected EOF while looking for matching
# )". The fix was a base64 payload whose alphabet has no quote, no space and no metacharacter,
# plus `-e` so wsl stops re-parsing, plus WSL_UTF8=1 so wsl's UTF-16LE output does not arrive as
# NUL-separated glyph soup. That is why the code was careful, and it is exactly why keeping it
# "just in case" would be wrong: a future agent would find a sophisticated, tested transport
# and reasonably conclude something must use it.
#
# NOTHING DOES. The plane runs as native Windows processes and runtime containers go through
# `podman machine`, which podman creates and owns. This installer never opens a shell in a WSL
# distro, so the transport, the encoding fix and the argv builder all went with the requirement
# that produced them.
#
# IF WSL EVER COMES BACK, READ THE PARAGRAPH ABOVE FIRST — the three failures it names (quote
# stripping, wsl re-parsing, UTF-16 output) will each come back too, and they are recorded in the
# native harness's history rather than in code that no longer runs.

# --- WSL IS NOT PART OF THIS INSTALLER ANY MORE ------------------------------
#
# THIS FILE USED TO PROVISION A WSL DISTRO, because the first architecture ran the whole
# stack inside one. It does not any more: the plane is native Windows processes, and runtime
# containers go through `podman machine`, which podman creates and owns
# (internal/containerengine/ensure.go: "FIXES a stopped or absent podman MACHINE (macOS,
# Windows) … `podman machine init --now` needs no elevation"). WSL is podman's business
# there, not an operator prerequisite and not something this installer detects.
#
# REMOVED RATHER THAN KEPT DORMANT. Ensure-Wsl, Install-ContainersSubstrate and their helpers
# were unreachable while containers were opt-in, which is why nobody noticed that making
# containers the DEFAULT would make every Windows install demand a distro the platform does
# not use — the operator's report: "Why is a default install running WSL? I thought we
# discussed that podman didn't need WSL." A dead branch that a flag can revive is a trap.
#
# The container-engine DIAGNOSIS that used to live beside it is also gone; that lives in
# internal/containerengine now (see the note below), so a wrapper copy could only disagree.

# --- THE CONTAINER-ENGINE DIAGNOSIS USED TO LIVE HERE, AND IS GONE -----------
#
# `Ensure-WslDocker` and `Test-DockerDesktopRunning` are DELETED, not ported. They
# existed to tell three Docker Desktop failures apart (no CLI in the distro, a CLI
# with no daemon, a running Docker Desktop with the WSL integration off) and to
# stop an install for them. Neither premise holds any more:
#
#   * the plane and its services are host processes of the invoking user, so an
#     install needs no container engine at all; and
#   * the engine itself (podman first, then docker, including `podman machine` on
#     Windows) is provisioned BY `orchicon install` — see
#     cmd/orchicon/install_engine.go — which reports the state the machine is in
#     at the point it matters, with the fix for the engine it actually chose.
#
# A wrapper that re-implemented that diagnosis could only disagree with it.

# --- The instance, and where it keeps its state -----------------------------
# The instance name comes from the environment (dev by default). Resolved here,
# ONCE, so the connection info and the setup-failure hints cannot disagree about
# which instance this is.
function Get-InstanceName {
    if ($script:Instance) { return $script:Instance }
    if ($env:ORCHICON_INSTANCE) { return $env:ORCHICON_INSTANCE }
    return "dev"
}

# THE ONE DERIVATION OF THE INSTANCE STATE DIRECTORY. Everything that touches this
# instance's data uses it, so "which directory" cannot be answered two ways.
#
# It mirrors internal/planecontrol/state.go HostDataDir exactly:
#   filepath.Join(home, ".local", "share", "orchicon-" + instance)
# on os.UserHomeDir(). That function deliberately IGNORES ORCHICON_DATA_DIR, so a
# wrapper that honoured it would read a directory the install never writes.
function Get-InstanceStateDir {
    $share = Join-Path (Join-Path $HOME ".local") "share"
    return (Join-Path $share ("orchicon-" + (Get-InstanceName)))
}

# --- Elevation guard (Postgres refuses to run as Administrator) --------------
# THE MESSAGE BELONGS HERE, BEFORE ANYTHING IS DOWNLOADED, and this is the whole
# reason for a pre-flight: `orchicon install` would otherwise acquire the host
# binaries, start a runtime daemon and only then fail in `initdb` with a message
# about an elevated account that reads like a defect in Postgres. The Go binary
# keeps the same guard (cmd/orchicon/privilege.go) as the last line of defence;
# this is the first, and it is the one a Windows operator actually sees.
#
# WRAPPED IN try/catch RETURNING $false, deliberately: the offline suite runs THIS
# FILE on Linux, where the WindowsPrincipal type does not exist and an unguarded
# call would turn "not elevated" into an unexpected-error report.
function Test-Elevated {
    try {
        $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
        $principal = New-Object Security.Principal.WindowsPrincipal($identity)
        return [bool]$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    } catch {
        return $false
    }
}

function Assert-NotElevated {
    if (-not (Test-Elevated)) { return }
    Write-Err "this PowerShell is running as Administrator, and Orchicon cannot install from an"
    Write-Host "  elevated shell: its Postgres host service REFUSES to run as Administrator, so the"
    Write-Host "  install would fail part-way through with an error about initdb rather than about".
    Write-Host "  the shell you started."
    Write-Host ""
    Write-Host "Next steps:" -ForegroundColor Yellow
    Write-Host "  1. Close this window."
    Write-Host "  2. Open a NORMAL PowerShell (Start → PowerShell; NOT 'Run as administrator')."
    Write-Host "  3. Re-run:  irm https://orchicon.dev/install.ps1 | iex"
    Write-Host ""
    Write-Host "  Nothing was downloaded and nothing was changed." -ForegroundColor DarkGray
    Stop-Install
}

# --- Read the ports back, do not re-derive them ------------------------------
#
# `orchicon install` PROMPTS for the host ports, and a prompt is exactly the thing
# a wrapper cannot predict. Reported from a real Windows install: the operator
# answered 3001 for a published port, `orchicon install` printed 3001, and this
# block then printed 3002 — one run, two contradicting numbers, the second being a
# port nothing had bound.
#
# So the RECORD is the source. `orchicon install` writes the ports it bound to the
# instance's state dir (cmd/orchicon writeResolvedPorts) and this reads them back.
# There is NO hardcoded fallback any more: a wrapper that guesses when the record is
# missing is how the guess reappears as a fact, and the honest answer to "which port
# did it bind?" without a record is "ask it".
function Get-ResolvedPorts {
    $path = Join-Path (Get-InstanceStateDir) "ports.json"
    if (-not (Test-Path -LiteralPath $path)) { return $null }
    try { return (Get-Content -LiteralPath $path -Raw | ConvertFrom-Json) } catch { return $null }
}

function Write-ConnectionInfo {
    $resolved = Get-ResolvedPorts
    $ctrl = $null
    if ($resolved -and $resolved.ORCHICON_CONTROL_PORT) { $ctrl = $resolved.ORCHICON_CONTROL_PORT }
    Write-Host ""
    if ($ctrl) {
        Write-Host "Open in your browser:" -ForegroundColor White
        Write-Host "  Control plane: http://localhost:$ctrl" -ForegroundColor Cyan
        Write-Host "  (the port this instance actually bound — recorded in $(Join-Path (Get-InstanceStateDir) 'ports.json'))" -ForegroundColor DarkGray
        Write-Host "  Note: if it does not answer, check Windows Defender Firewall for a prompt" -ForegroundColor DarkGray
        Write-Host "  about orchicon.exe." -ForegroundColor DarkGray
    } else {
        # NO NUMBERS AT ALL. Printing a default here is the bug this replaces: it
        # would be a port this install may never have bound.
        Write-Host "Orchicon is installed, but this instance has no port record yet." -ForegroundColor White
        # BUILT BY CONCATENATION, NOT WITH \" ESCAPES: PowerShell has no backslash escape, so
        # `\"$InstallDir\\orchicon.exe\"` inside a double-quoted string ENDS the string at the
        # first quote and prints `& \ <dir>\\orchicon.exe\ text` (measured). The command this
        # prints is the one an operator copies, so it has to be the command that works.
        # `if ($InstallDir)` because this function is also called from the offline suite, where
        # the install dir is not resolved: with no dir the honest command is the one on PATH.
        $exeCmd = if ($InstallDir) { Join-Path $InstallDir 'orchicon.exe' } else { 'orchicon.exe' }
        Write-Host ('  Ask it what it bound:  & "' + $exeCmd + '" status') -ForegroundColor Cyan
    }
}

# --- Native operations on THIS instance -------------------------------------
# Stop the plane AND this instance's host services, through `orchicon stop`
# scoped by ORCHICON_INSTANCE — the same call the shell installer makes, for the
# same measured reason. A bare `serve --stop` resolves its PID file from
# ORCHICON_SERVE_STATE_DIR, so with the default it MISSED the host-resident plane,
# reported success, and the caller then deleted the binary out from under a server
# that answers and cannot work. Sending the state dir explicitly is what makes the
# call find the plane the installer actually started.
#
# Returns $true when a stop reported success, $false otherwise (including "there
# was no binary"). Never throws: it is called on teardown paths, where a failure to
# stop must not stop the teardown from telling the operator what it did.
function Stop-InstancePlane {
    $exe = Join-Path $InstallDir "orchicon.exe"
    if (-not (Test-Path -LiteralPath $exe)) { return $false }
    $prevStateDir = $env:ORCHICON_SERVE_STATE_DIR
    $prevInstance = $env:ORCHICON_INSTANCE
    $stopped = $false
    try {
        $env:ORCHICON_INSTANCE = Get-InstanceName
        $env:ORCHICON_SERVE_STATE_DIR = Join-Path (Get-InstanceStateDir) "serve"
        try {
            & $exe stop 2>$null | Out-Null
            $stopped = ($LASTEXITCODE -eq 0)
        } catch {
            $stopped = $false
        }
    } finally {
        $env:ORCHICON_SERVE_STATE_DIR = $prevStateDir
        $env:ORCHICON_INSTANCE = $prevInstance
    }
    return $stopped
}

# Add the install dir to the USER PATH, once. The per-user PowerShell/cmd PATH is
# a registry value, so this is the Windows equivalent of the shell installer's
# ~/.bashrc hint — and it is idempotent, because a repeated install that appended
# again would grow PATH without bound.
function Write-PathHint {
    # POSIX has no per-user PATH registry, and the offline suite runs this file on
    # Linux. Nothing to do there, and nothing to warn about.
    if ($env:OS -ne "Windows_NT") { return }
    $target = $InstallDir.TrimEnd('\')
    try {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @()
        if ($userPath) { $entries = @($userPath -split ';' | Where-Object { $_ }) }
        foreach ($e in $entries) {
            if ($e.TrimEnd('\') -ieq $target) { return }
        }
        $newPath = ((@($entries) + $target) | Where-Object { $_ }) -join ';'
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        Write-Ok "added $InstallDir to your user PATH"
        Write-Host "  (open a NEW terminal for 'orchicon' to resolve there; this window keeps the PATH it started with)" -ForegroundColor DarkGray
    } catch {
        Write-Warn "could not add $InstallDir to your user PATH — add it by hand to run 'orchicon' from any terminal"
    }
}

# --- The container engine ----------------------------------------------------
# IT IS PROVISIONED BY `orchicon install`, NOT HERE. This file used to carry a
# window into a user's WSL distro (Install-ContainersSubstrate -> Ensure-Wsl -> a
# bash probe inside `wsl -d <distro>`) because the FIRST architecture ran the stack
# in a distro. That architecture is gone: the plane is native, and the engine path
# runs containers through `podman machine`, which podman creates and owns
# (internal/containerengine/ensure.go). WSL is podman's implementation detail there,
# not something this installer detects, provisions, or asks the operator for.
#
# The step was REMOVED rather than kept dormant, and the reason is this very bug: it
# was unreachable while containers were opt-in, so nobody noticed that making them
# the default would make EVERY Windows install demand a distro the platform does not
# use. A dead branch that a flag can revive is a trap, not a fallback.
#
# What a container host actually gets, all from `orchicon install`:
#   detect -> offer to install podman (winget) -> `podman machine init --now` ->
#   run a trivial container to PROVE it works. That last rung is the only one that
#   answers "can this host run our work", which is why nothing here duplicates it.

# --- Desktop app (Windows side) ---------------------------------------------
#
# THE ONE THING THIS SCRIPT INSTALLS THAT IS NOT A HOST BINARY OF THE STACK: the
# desktop app is a native Windows binary by design (a Wails shell around WebView2), so
# it is installed by its own installer rather than by the copy-into-InstallDir step the
# CLI binaries use. It talks to this instance's plane on the same host over loopback,
# which is exactly what the connection info above advertises.
#
# WEBVIEW2 IS THE REASON THE INSTALLER EXISTS RATHER THAN A ZIP. `wails build -nsis
# -webview2 download` embeds the Evergreen bootstrapper in the NSIS installer, so a
# Windows 10 machine provisions the runtime during setup instead of opening a blank
# window. That is why this runs the installer instead of copying files.
function Install-DesktopApp {
    $ver = $Version
    if (-not $ver -or $ver -eq "latest") {
        try {
            $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$GitHubOwner/$GitHubRepo/releases/latest" `
                -Headers @{ "User-Agent" = "orchicon-install" }
            $ver = $release.tag_name
        } catch { $ver = "" }
    }
    if (-not $ver) {
        Write-Warn "could not determine the release version — skipping the desktop app"
        Write-Warn "  download it from https://github.com/$GitHubOwner/$GitHubRepo/releases"
        return
    }

    $stem = "orchicon_desktop_$($ver -replace '^v','')_windows_amd64"
    $url = "https://github.com/$GitHubOwner/$GitHubRepo/releases/download/$ver/${stem}_installer.exe"
    $out = Join-Path ([System.IO.Path]::GetTempPath()) "$stem-installer.exe"

    Write-Info "downloading the desktop app installer…"
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing
    } catch {
        Write-Warn "the desktop installer for $ver is not downloadable ($($_.Exception.Message))"
        Write-Warn "  get it here: https://github.com/$GitHubOwner/$GitHubRepo/releases/tag/$ver"
        return
    }

    Write-Info "running the desktop installer (silent)…"
    try {
        $proc = Start-Process -FilePath $out -ArgumentList "/S" -Wait -PassThru
        if ($proc.ExitCode -ne 0) {
            Write-Warn "the desktop installer exited $($proc.ExitCode) — the stack install is unaffected"
            return
        }
        Write-Ok "desktop app installed (Start menu, 'Orchicon')"
        Write-Info "it needs WebView2: preinstalled on Windows 11, provisioned by the installer on Windows 10"
        Write-Info "it is a client — open it with this plane running, or it starts one for you"
    } catch {
        Write-Warn "could not run the desktop installer: $($_.Exception.Message)"
        Write-Warn "  run it by hand from https://github.com/$GitHubOwner/$GitHubRepo/releases/tag/$ver"
        return
    } finally {
        Remove-Item -Path $out -Force -ErrorAction SilentlyContinue
    }
}

# Uninstall-DesktopApp removes the app the installer above put on this host. It looks in
# the locations the Wails NSIS template uses, least-privileged first, and says so plainly
# when there is nothing there — a portable unzip is uninstalled by deleting the folder.
function Uninstall-DesktopApp {
    $candidates = @(
        (Join-Path $env:LOCALAPPDATA "Programs\Orchicon\uninstall.exe"),
        (Join-Path $env:ProgramFiles "Orchicon\uninstall.exe")
    )
    if (${env:ProgramFiles(x86)}) { $candidates += (Join-Path ${env:ProgramFiles(x86)} "Orchicon\uninstall.exe") }
    foreach ($c in $candidates) {
        if ($c -and (Test-Path $c)) {
            if ($DryRun) { Write-Info "would run the desktop app uninstaller: $c"; return }
            try {
                Start-Process -FilePath $c -ArgumentList "/S" -Wait
                Write-Ok "desktop app removed"
            } catch {
                Write-Warn "could not run the desktop uninstaller ($($_.Exception.Message))"
                Write-Warn "  remove it from Settings > Apps > Installed apps"
            }
            return
        }
    }
    Write-Info "no installed desktop app found (a portable unzip is removed by deleting its folder)"
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
# readable in the catch — which the footer needs ($InstallDir, $script:ContainerOptIn).
$script:ExitCode = 0
try {
    # Start the transcript FIRST, so everything below is in it. Skipped for a dry
    # run, which changes nothing and does not need a log file.
    if (-not $DryRun) { Start-InstallLog }

    # --- SmartScreen / "saved from the internet" notice --------------------------
    # A SAVED .ps1 CARRIES A MARK-OF-THE-WEB STREAM, and that stream is what makes PowerShell (and
    # SmartScreen) treat the file as untrusted. The documented invocation writes no file at all —
    # `irm https://orchicon.dev/install.ps1 | iex` — so it never gains the mark; this is for the operator
    # who right-clicked "Save link as". The remedy is ONE command against ONE file, never a security
    # setting to lower, and it is the same instruction the guidance block at the end prints.
    if ($PSCommandPath -and (Test-Path -LiteralPath $PSCommandPath)) {
        # IN A try/catch, AND `-ErrorAction Stop` INSIDE IT. `-Stream` is a Windows-only FileSystem
        # parameter: on Linux/macOS it is not a parameter at all, and `-ErrorAction SilentlyContinue`
        # does NOT swallow a parameter-binding failure — it would surface as a terminating error and
        # take this install into the catch block, on a platform where a zone mark cannot exist. The
        # catch makes "cannot ask" mean "not marked", which is the truth on every non-Windows host and
        # on a Windows file carrying no alternate stream.
        $zone = $null
        try { $zone = Get-Item -LiteralPath $PSCommandPath -Stream Zone.Identifier -ErrorAction Stop } catch { }
        if ($zone) {
            Write-Warn "this script was saved from the internet — it carries a Zone.Identifier mark"
            Write-Warn "  PowerShell may refuse to run it, and SmartScreen may warn about it. Clear the mark on THIS file:"
            Write-Warn "    Unblock-File -LiteralPath '$PSCommandPath'"
            Write-Warn "  (or use the documented 'irm https://orchicon.dev/install.ps1 | iex', which writes no file at all)"
        }
    }

    # --- Elevated pre-flight ----------------------------------------------------
    # FIRST, before a byte is downloaded or a process is started: Postgres refuses
    # to run as Administrator, and the failure that produces arrives minutes later
    # in a subsystem the operator was not thinking about.
    #
    # SCOPED TO THE VERBS THAT START THE STACK, because that is the whole content of the
    # rule: Postgres refuses to run as Administrator, and Postgres is started by `orchicon
    # install`. -Uninstall, -RemoveBinary, -DryRun and -NoSetup start nothing — they stop
    # things, print a plan, or place two executables — so a blanket refusal would answer an
    # operator removing Orchicon from an admin prompt with a message about a program that
    # never runs. (An elevated shell is the one people habitually use to remove software.)
    $startsStack = -not ($Uninstall -or $RemoveBinary -or $DryRun -or $NoSetup)
    if ($startsStack) { Assert-NotElevated }
    # --- Defaults ---------------------------------------------------------------
    # THE NATIVE INSTALL DIR: %LOCALAPPDATA%\Programs\Orchicon is per-user, needs no
    # administrator rights, and is where Windows puts per-user applications. (The
    # fallback only fires when LOCALAPPDATA is unset, which is to say on the Linux
    # host that runs this file's offline suite.)
    if (-not $InstallDir) {
        if ($env:LOCALAPPDATA) {
            $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\Orchicon"
        } else {
            $InstallDir = Join-Path $HOME ".orchicon/bin"
        }
    }
    $InstallDir = $InstallDir.TrimEnd('\')

# --- Instance scoping -------------------------------------------------------
#
# ONE INSTANCE, NAMED ONCE, USED EVERYWHERE. The state directory used to be spelled
# out in four places, so -Uninstall stopped BOTH stacks and -ForceClean destroyed
# BOTH data directories. On a host with prod data that is a data-loss bug rather
# than an inconvenience.
#
# An unknown instance is REFUSED rather than defaulted, because the alternative is acting on the
# wrong stack: `-Instance production` quietly becoming dev would clean up dev while the operator
# watched prod. The set matches the installer port table (cmd/orchicon/install_ports.go).
$script:Instance = if ($Instance) { $Instance } elseif ($env:ORCHICON_INSTANCE) { $env:ORCHICON_INSTANCE } else { "dev" }
  # RECORDED AT SCRIPT SCOPE, deliberately: inside a function $PSBoundParameters describes the
  # FUNCTION's parameters, so Ask-Instance could never see whether -Instance was supplied from there.
  # Measured: `-RemoveBinary -Instance test` prompted anyway, because that check never fired.
  $script:InstanceGiven = $PSBoundParameters.ContainsKey('Instance')
if ($script:Instance -notin @("dev", "test", "prod")) {
    Stop-Install "unknown instance: '$script:Instance' (known: dev, test, prod)"
}

# ASK WHICH INSTANCE, AT THE MOMENT OF THE DESTRUCTIVE ACTION.
#
# Same reasoning as the shell installer's: inferring it from the environment with a `dev` default is how an
# operator cleans the wrong stack, because the variable is invisible and a shell started from inside a
# running plane inherits that plane's value. `-Instance` skips the question.
#
# READ-HOST IS NOT THE IDEAL READER - PowerShell offers no equivalent of opening /dev/tty - so this is
# deliberately conservative: an empty or failed read is treated as NO ANSWER and the command stops, rather
# than falling through to a default. For unattended use, pass -Instance.
function Ask-Instance {
    # -Fallback IS WHAT AN EMPTY ANSWER MEANS, and it differs BY VERB, exactly as the shell installer's
    # does. INSTALL passes "dev": installing to the documented default is not destructive, and a first run
    # should not demand a decision it does not need. The DESTRUCTIVE verbs pass nothing, so an empty
    # answer refuses — a reflexive Enter must never select a stack to destroy.
    param([string]$Fallback = "")
    if ($script:InstanceGiven) { return }
    if ($env:ORCHICON_INSTANCE) { return }
    Write-Host ""
    Write-Host "  Which instance?" -ForegroundColor White
    Write-Host "  An instance is a whole stack: its own plane, ports, host services and host state." -ForegroundColor DarkGray
    Write-Host "    1) dev      2) test      3) prod"
    $hint = if ($Fallback) { " [${Fallback}]" } else { " (empty cancels)" }
    $answer = $null
    try { $answer = Read-Host "  choose 1-3 or a name${hint}" } catch { $answer = $null }
    # TESTED BEFORE THE SWITCH, DELIBERATELY. In PowerShell `switch ($null)` fires NO branch at all —
    # not even `default`, because the switch enumerates its value and $null yields nothing. So a
    # `default` clause cannot catch a missing answer: it silently did nothing, left $script:Instance at
    # its earlier value, and ACTED ON dev WITH NO ANSWER GIVEN. Measured on this host.
    if ([string]::IsNullOrWhiteSpace($answer)) {
        if ($Fallback) { $script:Instance = $Fallback }
        else { Stop-Install "no instance chosen - pass -Instance dev|test|prod" }
    }
    else {
        switch ($answer) {
            "1" { $script:Instance = "dev" }
            "2" { $script:Instance = "test" }
            "3" { $script:Instance = "prod" }
            "dev" { $script:Instance = "dev" }
            "test" { $script:Instance = "test" }
            "prod" { $script:Instance = "prod" }
            default { Stop-Install "not an instance: '$answer' (known: dev, test, prod)" }
        }
    }
    Write-Info "acting on instance: $script:Instance"
}


# --- upgrade detection (parity with scripts/install.sh, section 12 of the harness) ---
#
# THE INSTALLER IS ALSO THE UPDATER. Re-running the documented command must decide for
# itself whether this is a fresh install, an upgrade, or nothing to do — the operator should
# not have to remember a flag. The rules are IDENTICAL to the shell installer's, deliberately:
# both are release-facing and a platform that behaved differently would be its own support
# burden.
#
# THERE IS NO DOWNGRADE. `-Version vOLD` can still pin an older release on purpose, but when
# the INSTALLED binary is NEWER than the release being installed we refuse: migrations are
# forward-only (`orchicon serve` runs them on boot), so moving the binary backwards can leave
# a schema it cannot read.

# Get-InstalledVersion prints the version of the installed orchicon.exe, or "" when absent.
# A binary that cannot run (wrong arch, corrupted) counts as NOT installed — the caller then
# does a fresh install, which is the recovery an operator wants.
function Get-InstalledVersion {
    $exe = Join-Path $InstallDir "orchicon.exe"
    if (-not (Test-Path -LiteralPath $exe)) { return "" }
    try {
        $out = ((& $exe version 2>$null | Out-String)).Trim()
        if ($out -match '([vV][0-9]+\.[0-9]+\.[0-9]+[^\s]*)') { return $Matches[1] }
    } catch { }
    return ""
}

# Test-VersionGreater returns $true when $A is a HIGHER semver than $B. Compares the numeric
# triple and ignores any pre-release suffix — enough to tell "newer release" from "older" and
# never mis-orders on a suffix (the shell installer's version_gt mirrors this).
function Test-VersionGreater {
    param([string]$A, [string]$B)
    if (-not $A -or -not $B) { return $false }
    $an = ($A -replace '^[vV]', '') -split '-' | Select-Object -First 1
    $bn = ($B -replace '^[vV]', '') -split '-' | Select-Object -First 1
    $ap = $an -split '\.'
    $bp = $bn -split '\.'
    for ($i = 0; $i -lt 3; $i++) {
        $x = 0; $y = 0
        if ($i -lt $ap.Count) { [void][int]::TryParse($ap[$i], [ref]$x) }
        if ($i -lt $bp.Count) { [void][int]::TryParse($bp[$i], [ref]$y) }
        if ($x -gt $y) { return $true }
        if ($x -lt $y) { return $false }
    }
    return $false
}

# --- The verbs that only take things down -----------------------------------
# ALL NATIVE NOW. These used to be bash payloads shipped into the distro, which
# meant -ForceClean's `rm -rf` ran against a DIFFERENT filesystem from the one the
# record is read from, and the paths it named (`$HOME/bin`, `$HOME/data`, a
# relative `.local/share/orchicon`) were directories Orchicon does not own. Every
# path below is either the install dir or THIS instance's state dir, and nothing
# matches a process by name.

    if ($RemoveBinary) {
        Ask-Instance
        $exePath  = Join-Path $InstallDir "orchicon.exe"
        $orchPath = Join-Path $InstallDir "orch.exe"
        if ($DryRun) {
            Write-Info "would remove $exePath and $orchPath (every instance's state is left alone)"
            if (-not (Test-Path -LiteralPath $exePath)) { Write-Warn "nothing at $exePath — nothing to remove" }
            return
        }
        # ASK BEFORE TAKING ANYTHING DOWN. Removing a binary a plane is running from
        # is what produces a server that answers and cannot work, so consent decides
        # and declining changes nothing.
        $consent = [bool]$Yes
        if (-not $Yes) {
            Write-Host ""
            Write-Host "  Remove $exePath and $orchPath?" -ForegroundColor White
            Write-Host "  If this instance's plane is running from them it has to be stopped first." -ForegroundColor DarkGray
            $answer = $null
            try { $answer = Read-Host "  stop this instance's plane, then remove the binaries? [y/N]" } catch { $answer = $null }
            $consent = [bool]($answer -and $answer.Trim().ToLower().StartsWith("y"))
        }
        if (-not $consent) {
            Write-Warn "nothing stopped, nothing removed"
            return
        }
        if (Stop-InstancePlane) { Write-Ok "stopped the $script:Instance plane and its host services" }
        foreach ($f in @($exePath, $orchPath)) {
            if (Test-Path -LiteralPath $f) {
                Remove-Item -LiteralPath $f -Force
                Write-Ok "removed $f"
            }
        }
        Write-Ok "binaries removed - every instance's state is untouched"
        return
    }

    if ($Uninstall) {
        Ask-Instance
        $exePath  = Join-Path $InstallDir "orchicon.exe"
        $orchPath = Join-Path $InstallDir "orch.exe"
        $stateDir = Get-InstanceStateDir
        if ($DryRun) {
            Write-Info "would run:"
            Write-Host "  stop this instance's plane and host services (ORCHICON_INSTANCE=$($script:Instance) orchicon.exe stop)"
            Write-Host "  remove $exePath and $orchPath"
            if ($Purge) {
                Write-Host "  destroy the $($script:Instance) host state dir (database, blob store, runtime state): $stateDir"
            } else {
                Write-Host "  keep the $($script:Instance) host state dir: $stateDir"
            }
            return
        }
        if (Stop-InstancePlane) {
            Write-Ok "stopped the $script:Instance plane and its host services"
        } else {
            Write-Host "  note: no running plane found for instance '$($script:Instance)'" -ForegroundColor DarkGray
        }
        # THE APP LIVES OUTSIDE InstallDir, so the binary loop below cannot see it: an
        # uninstall that left a working window behind (pointed at a stopped plane) is a
        # bug report.
        Uninstall-DesktopApp
        # DATA IS PRESERVED UNLESS -Purge, and the summary says which happened: an
        # uninstall must not be ambiguous about whether the database survived.
        if ($Purge) {
            if (Test-Path -LiteralPath $stateDir) {
                Remove-Item -LiteralPath $stateDir -Recurse -Force
                Write-Ok "removed host state for instance '$($script:Instance)'"
            } else {
                Write-Host "  no host state at $stateDir" -ForegroundColor DarkGray
            }
        } else {
            Write-Host ""
            Write-Host "  Data preserved:"
            Write-Host "    instance state:   $stateDir"
            Write-Host "    (the $($script:Instance) database, blob store and runtime state)"
            Write-Host "  Re-run with -Purge to remove it (irreversible)."
        }
        # BINARIES LAST, after the plane that runs from them is stopped: deleting the
        # executable first is what produced the broken server.
        $removedAny = $false
        foreach ($f in @($exePath, $orchPath)) {
            if (Test-Path -LiteralPath $f) { Remove-Item -LiteralPath $f -Force; Write-Ok "removed $f"; $removedAny = $true }
        }
        if (-not $removedAny) { Write-Host "  orchicon not found in $InstallDir - nothing to remove" -ForegroundColor DarkGray }
        if ($Purge) {
            Write-Ok "Orchicon uninstalled for '$($script:Instance)' — plane and host services stopped, host state destroyed (database included), binaries removed"
        } else {
            Write-Ok "Orchicon uninstalled for '$($script:Instance)' — plane and host services stopped, binaries removed; DATA PRESERVED (re-run with -Purge to destroy it)"
        }
        # `return`, not `exit`: this path SUCCEEDS, and it used to close the window on
        # the way out (see HALT SEMANTICS).
        return
    }

    # --- Clean (stop the stack, remove the binaries, then install latest) -------
    if ($Clean) {
        # ASKED LIKE EVERY OTHER DESTRUCTIVE VERB. Defaulting here is the most
        # dangerous of all, and it was the one verb that neither asked nor was scoped.
        Ask-Instance
        Write-Host ""
        Write-Host "Orchicon — clean" -ForegroundColor White
        Write-Host ""
        if ($DryRun) {
            Write-Info "would stop this instance's plane and host services, remove the old binaries (data preserved), then install the latest version"
        } else {
            if (Stop-InstancePlane) { Write-Ok "stopped the $script:Instance plane and its host services" }
            foreach ($f in @((Join-Path $InstallDir "orchicon.exe"), (Join-Path $InstallDir "orch.exe"))) {
                if (Test-Path -LiteralPath $f) { Remove-Item -LiteralPath $f -Force }
            }
            Write-Ok "Infrastructure cleaned — all user data preserved"
            Write-Host ""
            Write-Host "Now installing latest version…" -ForegroundColor White
            Write-Host ""
        }
    }

    # --- Force-clean (nuke THIS instance's state, then install latest) ----------
    if ($ForceClean) {
        Ask-Instance
        Write-Host ""
        Write-Host "Orchicon — force-clean (NUKE)" -ForegroundColor White
        Write-Host ""
        if ($DryRun) {
            Write-Info "would stop this instance's plane and host services, destroy $(Get-InstanceStateDir), remove the binaries (ALL DATA FOR THIS INSTANCE LOST)"
        } else {
            if (Stop-InstancePlane) { Write-Ok "stopped the $script:Instance plane and its host services" }
            # ANCHORED TO THIS INSTANCE'S OWN STATE DIRECTORY, never a relative name
            # and never the parent: `rm -rf data .dev bin .local/share/orchicon`
            # deleted $HOME/bin and $HOME/data — directories Orchicon does not own —
            # and then named a directory that is NOT the per-instance one this
            # install writes and reads back, so it reported "all data wiped" while
            # the database survived.
            $stateDir = Get-InstanceStateDir
            if (Test-Path -LiteralPath $stateDir) { Remove-Item -LiteralPath $stateDir -Recurse -Force }
            foreach ($f in @((Join-Path $InstallDir "orchicon.exe"), (Join-Path $InstallDir "orch.exe"))) {
                if (Test-Path -LiteralPath $f) { Remove-Item -LiteralPath $f -Force }
            }
            Write-Ok "All data wiped — ready for a fresh start"
            Write-Host ""
            Write-Host "Now installing latest version…" -ForegroundColor White
            Write-Host ""
        }
    }

    # --- Detect arch ------------------------------------------------------------
    $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { "amd64" }
        # THE WINDOWS RELEASE SHIPS amd64 ONLY (see .github/workflows/release.yml), and
        # Windows on ARM runs it under emulation. Saying so is the difference between
        # "it installed and works" and a support report about an asset that does not
        # exist.
        "ARM64" {
            Write-Warn "Windows on ARM: there is no arm64 build, so the amd64 build is installed and runs under emulation"
            "amd64"
        }
        # A halt, not an exit: there is nothing this script can do about an unknown
        # arch, and the operator needs to read the DETECTED value rather than watch the
        # window vanish on them.
        default   { Stop-Install "unsupported architecture: $env:PROCESSOR_ARCHITECTURE (expected AMD64 or ARM64)" }
    }

    # --- THE CONTAINER ENGINE IS PROVISIONED BY THE SETUP, NOT HERE ----------------
    #
    # THIS BLOCK USED TO CALL Install-ContainersSubstrate, WHICH ASKED FOR A USER-INSTALLED WSL DISTRO. That
    # is the WSL-FIRST architecture and it is dead: the engine path does not use a distro of ours at all. It
    # uses `podman machine`, which podman creates and owns (internal/containerengine/ensure.go: "FIXES a
    # stopped or absent podman MACHINE (macOS, Windows) … `podman machine init --now` needs no elevation").
    # WSL is podman's own implementation detail there, not something the operator installs or we detect.
    #
    # Reported by the operator as a BUG I introduced: "Why is a default install running WSL? I thought we
    # discussed that podman didn't need WSL and we were going to drop that pathway?" Correct — and it only
    # surfaced because containers became the default, so a dead branch that used to be opt-in started running
    # on every machine. A host with no WSL at all gets working containers; a host whose podman wants a
    # distro gets it from podman.
    #
    # SO THE SUBSTRATE STEP IS GONE, NOT QUIETENED. `orchicon install` (step 3 below) detects the engine,
    # offers to install podman, creates/starts its machine, and PROVES it by running a trivial container —
    # which is the only check that answers "can this host run our work".
    if ($script:ContainerOptIn) {
        if ($DryRun) {
            Write-Info "would provision/verify the container engine (podman) — the setup installs it and starts its own machine (pass -NoContainers to skip)"
        } else {
            Write-Info "container engine: provisioned by the setup step below (podman; no WSL install needed)"
        }
    } else {
        Write-Info "containers: skipped (-NoContainers)"
    }

    # A PLAIN INSTALL ASKS WHICH INSTANCE — and it asks HERE, after the prerequisites, not before them.
    #
    # It used to ask at the top of the script, so the operator chose a stack and was THEN told that a
    # prerequisite was missing. A question about which stack to create is only answerable if the stack can be
    # created, and asking first wastes the answer and buries the real problem under it.
    #
    # It is the only verb that gets a fallback (see Ask-Instance): installing to the documented default is
    # not destructive. The destructive verbs pass none.
    Ask-Instance -Fallback "dev"

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

    # WHAT IS THIS RUN? Decided BEFORE anything is downloaded or stopped, so "current" and
    # "downgrade" can exit without touching the installed binary. Mirrors the shell
    # installer's upgrade_decision exactly — one contract, two platforms.
    $haveVersion = Get-InstalledVersion
    if ($haveVersion) {
        if (Test-VersionGreater $haveVersion $Version) {
            Write-Err "the installed Orchicon ($haveVersion) is NEWER than $Version."
            Write-Host "  Downgrades are not supported: database migrations are forward-only, so an older" -ForegroundColor DarkGray
            Write-Host "  binary can meet a schema it cannot read. Re-run without -Version to install the" -ForegroundColor DarkGray
            Write-Host "  latest, or use -ForceClean to wipe this instance's data and start again." -ForegroundColor DarkGray
            throw "refusing to downgrade $haveVersion to $Version"
        }
        if ($haveVersion -eq $Version) {
            Write-Ok "Orchicon $Version is already installed - nothing to do."
            Write-Host "  binary: $(Join-Path $InstallDir 'orchicon.exe')" -ForegroundColor DarkGray
            # A no-op must be a REAL no-op: nothing stopped, nothing downloaded, nothing restarted.
            return
        }
        Write-Host ""
        Write-Host "Upgrading Orchicon $haveVersion -> $Version" -ForegroundColor White
        # A PAUSE, NOT AN AUTOMATIC BACKUP — the same reasoning as the shell installer:
        # a backup at every upgrade would leave a multi-GB artifact on every machine, and
        # almost none would ever be used. The operator is told the command instead.
        Write-Host "  Your data (database, blob store, runtime state) is preserved across an upgrade." -ForegroundColor DarkGray
        Write-Host "  To keep a restorable copy first:  orchicon db backup" -ForegroundColor DarkGray
        Write-Host ""
        if (-not $Yes) {
            $answer = $null
            try { $answer = Read-Host "  Continue with the upgrade? [Y/n]" } catch { $answer = $null }
            if ($answer -and $answer.Trim().ToLower().StartsWith("n")) {
                Write-Warn "upgrade cancelled - nothing changed"
                return
            }
        }
        # STOP THE RUNNING INSTANCE BEFORE ITS BINARY IS REPLACED. On Windows the swap is a
        # Copy-Item over a file a running process holds, which fails outright — so this is not
        # just hygiene here, it is required for the copy to succeed.
        if (Stop-InstancePlane) { Write-Ok "stopped the $script:Instance plane and its host services" }
    } else {
        Write-Info "installing Orchicon $Version for windows/$arch (native)"
    }

    # --- Build download URL -----------------------------------------------------
    # THE NATIVE WINDOWS ARTIFACT: the release matrix builds windows/amd64 as a ZIP
    # (`.github/workflows/release.yml`), containing the directory
    # orchicon_<ver>_windows_amd64/ with orchicon.exe and orch.exe. Extraction below
    # searches recursively rather than assuming the flat shape, because the archive
    # wraps that directory.
    $versionNum = $Version -replace '^v', ''
    $asset = "orchicon_${versionNum}_windows_${arch}.zip"
    # WHERE THE ASSETS COME FROM, overridable on purpose and for the same reason the shell installer's is:
    # it is what lets the install path be exercised against a locally-served artifact (a test) or a
    # pre-release channel (`-Version edge`, which the default base already serves), rather than only the
    # published release.
    $baseUrl = if ($env:ORCHICON_INSTALL_BASE_URL) { $env:ORCHICON_INSTALL_BASE_URL } else { "https://github.com/$GitHubOwner/$GitHubRepo/releases/download" }
    $url = "$baseUrl/$Version/$asset"

    # --- What a dry run says ----------------------------------------------------
    # NO PORT NUMBERS HERE. They used to be hardcoded ("dev defaults: 8080 / 3002"),
    # and a dry run that names a port the install may never bind is the same defect
    # the record was introduced to fix; the setup prints the ports it actually bound.
    if ($DryRun) {
        Write-Info "planned steps:"
        Write-Host "  1. download ${asset}(or the MSI, when this release publishes one)"
        Write-Host "  2. extract orchicon.exe and orch.exe, install them into $InstallDir"
        if ($script:ContainerOptIn) {
            Write-Host "  3. provision/verify the container engine (podman) and the WSL2 substrate it needs"
        }
        if (-not $NoSetup) {
            Write-Host "  4. run 'orchicon.exe install' (acquire host binaries, start the runtime daemon, launch this instance's plane; no image is pulled unless one is chosen)"
            Write-Host "  5. print the control-plane URL from the ports the install RECORDS"
        }
        Write-Ok "dry-run complete — no changes made"
        return
    }

    # --- THE MSI, WHEN THE RELEASE HAS ONE -------------------------------------
    #
    # WHY PREFER IT. The MSI is the package a Windows estate can DEPLOY (Intune,
    # SCCM, Group Policy, `msiexec /qn`), and it installs the desktop app too. Ran
    # from here it gives the user the native experience: a wizard, then a real
    # console walking the usual questions - which is what this script does by hand
    # today, minus the hand-assembly.
    #
    # WHY ONLY IN THIS BRANCH. Three cases keep the zip path EXACTLY as it was,
    # because each is where the MSI's wizard is the wrong tool:
    #   -Yes        unattended: the MSI would open a console nothing can answer.
    #               The zip path already solves this with ORCHICON_ASSUME_YES, and
    #               it is the path the install-path suite exercises.
    #   -NoSetup    binaries only: the zip path does that natively; the MSI would
    #               need SKIPSETUP and then leave its own idea of "installed".
    #   ORCHICON_INSTALL_ARCHIVE  the offline seam — a local ZIP. Not an MSI.
    # A release with no MSI (every release so far — none has been cut since
    # windows/amd64 joined the matrix) falls through to the zip automatically, so
    # this is inert until the first release publishes one.
    # --- The scratch directory ---------------------------------------------------
    # CREATED HERE, BEFORE EITHER BRANCH, AND THAT PLACEMENT IS THE FIX.
    #
    # This block used to sit BELOW the MSI decision, so `$useMsi` could take the MSI
    # arm and use `$tmpdir` before anything had assigned it. PowerShell does not
    # complain about reading an unassigned variable — it is simply $null — so the
    # failure surfaced one line later as
    #   Cannot bind argument to parameter 'Path' because it is null
    #   At line:1179  $msiPath = Join-Path $tmpdir $msiAsset
    # which reads as a defect in the install rather than as "this variable was never
    # set". Reported from a real Windows run of `-Version canary`, the first time the
    # MSI arm had ever executed: the branch was written behind an availability probe
    # that no test could pass, so it shipped unexercised. The harness now passes that
    # probe (see scripts/tests/install-ps1-native), which is what keeps it exercised.
    #
    # [System.IO.Path]::GetTempPath() rather than $env:TEMP. TEMP is always set on
    # Windows, but the installer should not CRASH where it is not, and crashing here
    # is a bad failure for the same reason: the error is raised from a string join
    # and names no environment variable. (Found by running the installer's failure
    # paths under a stripped environment.)
    $tmpdir = Join-Path ([System.IO.Path]::GetTempPath()) "orchicon-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpdir -Force | Out-Null

    $msiAsset = "orchicon_${versionNum}_windows_${arch}.msi"
    $useMsi = -not ($Yes -or $NoSetup -or $env:ORCHICON_INSTALL_ARCHIVE)
    if ($useMsi) {
        $msiUrl = "$baseUrl/$Version/$msiAsset"
        $msiAvailable = $false
        try {
            $probe = Invoke-WebRequest -Uri $msiUrl -Method Head -UseBasicParsing -ErrorAction Stop
            $msiAvailable = ($probe.StatusCode -eq 200)
        } catch { $msiAvailable = $false }

        if (-not $msiAvailable) {
            Write-Info "no MSI published for $Version — installing from the zip (same binaries, assembled here)"
        } else {
            Write-Info "installing Orchicon $Version from $msiAsset (per-user, no elevation)"
            $msiPath = Join-Path $tmpdir $msiAsset
            try {
                Invoke-WebRequest -Uri $msiUrl -OutFile $msiPath -UseBasicParsing
            } catch {
                Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
                Stop-Install "could not download ${msiAsset}: $($_.Exception.Message)"
            }

            # INSTANCE travels as a public MSI property; the setup wrapper receives it
            # as %1 and exports ORCHICON_INSTANCE for `orchicon install`. /norestart
            # because nothing here needs a reboot, and a silent reboot is the worst
            # thing an installer can do.
            # THE MSI LOG IS KEPT OUTSIDE THE SCRATCH DIRECTORY, WHICH IS DELETED.
            #
            # It used to be written to "$tmpdir\msi.log" and removed with tmpdir at the end of
            # the run — so the ONE record of what Windows Installer actually did was discarded
            # seconds after being produced. That was felt immediately: a report of
            # "orchicon-setup.cmd could not be found" from the wizard arrived with no way to
            # tell whether the file had failed to install or had been launched wrongly.
            # $([System.IO.Path]::GetTempPath()) is stable and per-user, so the path can be
            # PRINTED on success and quoted in a bug report later.
            $msiLog = Join-Path ([System.IO.Path]::GetTempPath()) "orchicon-msi.log"
            $msiArgs = @("/i", "`"$msiPath`"", "/norestart", "/l*v", "`"$msiLog`"",
                         "INSTANCE=$($script:Instance)")
            if ($InstallDir) { $msiArgs += "INSTALLFOLDER=`"$InstallDir`"" }

            Write-Info "running the installer (this opens the Orchicon setup console)…"
            # NAMED CAUSE WHEN MSIEXEC ITSELF WILL NOT START. Start-Process raises a
            # TERMINATING error here, so without this catch it escapes to the top-level
            # handler and the operator is told "this is a defect in the installer" — when the
            # real cause is that Windows Installer could not be launched. That happens on a
            # real machine: the Windows Installer service can be disabled by group policy, and
            # msiexec.exe can be missing from a stripped image. The zip path installs the same
            # binaries, so the useful thing to say is so.
            try {
                $proc = Start-Process -FilePath "msiexec.exe" -ArgumentList $msiArgs -Wait -PassThru
            } catch {
                Stop-Install "could not start msiexec.exe: $($_.Exception.Message)`n  Windows Installer may be disabled by policy. The zip installer installs the same binaries:
  rerun with -NoSetup for the binaries only, or fix Windows Installer."
            }
            if ($proc.ExitCode -ne 0) {
                Stop-Install "the MSI exited $($proc.ExitCode). The binaries may not be installed. MSI log: $msiLog"
            }

            # VERIFY, the same way the zip path does: run what was installed. The MSI
            # does not print where it put things, so ask the binary rather than assume
            # -InstallDir was honoured.
            $exe = if ($InstallDir) { Join-Path $InstallDir "orchicon.exe" }
                   else { Join-Path (Join-Path $env:LOCALAPPDATA "Programs\Orchicon") "orchicon.exe" }
            if (-not (Test-Path -LiteralPath $exe)) {
                Stop-Install "the MSI reported success but $exe is not there. Check its log: $msiLog"
            }
            $verify = ""
            try { $verify = ((& $exe version 2>&1 | Out-String)).Trim() } catch { $verify = "" }
            if ($verify) { Write-Ok "verified: $verify" } else { Write-Warn "could not run the installed binary to verify it" }
            Write-Ok "Installed: $exe"
            Write-Info "setup runs in the console that just opened (it asks for the host ports and the optional runtime images)"
            # NAMED ON SUCCESS TOO. It is the only record of what Windows Installer did, and a
            # path nobody is shown is a path nobody sends.
            Write-Info "Windows Installer log: $msiLog"
            Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
            return
        }
    }

    $archive = Join-Path $tmpdir $asset

    # ORCHICON_INSTALL_ARCHIVE is the offline seam, for the same reason
    # ORCHICON_INSTALL_LOG=off is one: the suite that exercises this file must be able
    # to run against a locally-built archive rather than the published release.
    if ($env:ORCHICON_INSTALL_ARCHIVE) {
        Write-Info "using the archive at $env:ORCHICON_INSTALL_ARCHIVE"
        try {
            Copy-Item -LiteralPath $env:ORCHICON_INSTALL_ARCHIVE -Destination $archive -Force
        } catch {
            Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
            Stop-Install "could not read ORCHICON_INSTALL_ARCHIVE: $($_.Exception.Message)"
        }
    } else {
        Write-Info "downloading $url"
        try {
            # -UseBasicParsing: Windows PowerShell 5.1's Invoke-WebRequest needs the
            # Internet Explorer engine otherwise, and there is none on a Server Core or
            # a hardened desktop.
            Invoke-WebRequest -Uri $url -OutFile $archive -UseBasicParsing
        } catch {
            Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
            Stop-Install "download failed: $($_.Exception.Message)"
        }
    }

    # --- VERIFY THE ARCHIVE, THEN EXTRACT IT --------------------------------------
    # THE SAME POLICY install.sh APPLIES, expressed natively so the two cannot disagree about what a
    # verified install means:
    #   * no SHA256SUMS for this version (or the file cannot be fetched) → say so and continue — older
    #     releases predate the file, and an unreachable sums file is not evidence of a corrupt download;
    #   * a hash MISMATCH → halt BEFORE anything is extracted or installed, with both hashes printed.
    if ($NoVerify) {
        Write-Info "skipping the SHA256SUMS check (-NoVerify)"
    } else {
        $sumsText = $null
        try {
            $sumsResp = Invoke-WebRequest -Uri "$baseUrl/$Version/SHA256SUMS" -UseBasicParsing
            # PS 5.1 hands back a byte[] for some content types and a string for others; both are the file.
            $sumsText = if ($sumsResp.Content -is [byte[]]) {
                [System.Text.Encoding]::UTF8.GetString($sumsResp.Content)
            } else { [string]$sumsResp.Content }
        } catch {
            Write-Info "no SHA256SUMS published for $Version — skipping checksum verification"
        }
        if ($sumsText) {
            $expected = ""
            foreach ($line in ($sumsText -split "`r?`n")) {
                # Format: "<sha256>  <asset-name>"; a leading ./ is tolerated the way the shell installer does.
                $f = $line.Trim() -split '\s+'
                if ($f.Count -ge 2) {
                    $name = $f[1] -replace '^\./', ''
                    if ($name -eq $asset) { $expected = $f[0].ToLower(); break }
                }
            }
            if (-not $expected) {
                Write-Info "SHA256SUMS does not list $asset — skipping checksum verification"
            } else {
                $actual = ""
                try { $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLower() } catch { $actual = "" }
                if (-not $actual) {
                    Write-Info "could not hash the archive — skipping checksum verification"
                } elseif ($actual -ne $expected) {
                    Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
                    Stop-Install "checksum mismatch for ${asset}: expected $expected, got $actual. The download is corrupt or is not the archive this release published. NOTHING WAS INSTALLED."
                } else {
                    Write-Ok "checksum verified against the published SHA256SUMS"
                }
            }
        }
    }

    # --- Extract and install the NATIVE binaries --------------------------------
    Write-Info "extracting and installing into $InstallDir…"
    $extractDir = Join-Path $tmpdir "extract"
    New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
    try {
        Expand-Archive -LiteralPath $archive -DestinationPath $extractDir -Force
    } catch {
        Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
        Stop-Install "could not extract ${asset}: $($_.Exception.Message)"
    }

    # RECURSIVE, because the release archive wraps a top-level
    # orchicon_<ver>_windows_<arch>/ directory.
    $foundExe = Get-ChildItem -Path $extractDir -Filter "orchicon.exe" -Recurse -File -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $foundExe) {
        Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue
        Stop-Install "the archive did not contain orchicon.exe — $asset is not a native Windows build"
    }
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $exe = Join-Path $InstallDir "orchicon.exe"
    Copy-Item -LiteralPath $foundExe.FullName -Destination $exe -Force

    $installed = @($exe)
    $foundOrch = Get-ChildItem -Path $extractDir -Filter "orch.exe" -Recurse -File -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($foundOrch) {
        $orch = Join-Path $InstallDir "orch.exe"
        Copy-Item -LiteralPath $foundOrch.FullName -Destination $orch -Force
        $installed += $orch
        Write-Ok "orch (remote TUI client) installed: $orch"
    } else {
        Write-Warn "orch companion binary not found in the archive (older release?) — skipping"
    }

    # POSIX ONLY, and only so the OFFLINE SUITE can invoke what it just installed:
    # a zip carries no exec bit, and Windows has none to restore. Never runs on the
    # platform this installer is for.
    if ($env:OS -ne "Windows_NT") {
        foreach ($f in $installed) { try { & /bin/chmod 0755 $f | Out-Null } catch { } }
    }

    # --- THE LINUX BINARY THE RUNTIME CONTAINER RUNS ------------------------------
    #
    # A runtime container is a LINUX container: it runs `orchicon runtime-supervisor`, and the
    # daemon bind-mounts the binary in rather than baking it into the image. On a Windows host
    # the daemon's own executable is a PE file, so the container's supervisor cannot start
    # without a linux/amd64 build placed beside it (internal/runtime/exe_resolve.go looks for
    # exactly this name).
    #
    # FETCHED ONLY WHEN CONTAINERS WERE ASKED FOR, and never fatal. A plain install needs no
    # Linux binary, and a failed download must not fail an install whose plane works perfectly
    # without containers — it is reported, with the remedy, and the install continues.
    if ($script:ContainerOptIn) {
        # THE DATA DIR, NOT THE INSTALL DIR. The daemon looks beside its own executable and then
        # here; a daemon started earlier from somewhere else (a locally built bin\orchicon.exe) never
        # looks in InstallDir, and an install that already has a live daemon does not restart it.
        # ORCHICON_DATA_DIR WINS, because the daemon reads it too — writing to a different directory
        # than the daemon searches would be the same failure with a different path in it.
        $dataRoot = if ($env:ORCHICON_DATA_DIR) { $env:ORCHICON_DATA_DIR }
                    elseif ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "orchicon" }
                    else { $InstallDir }
        $linuxBinDir = Join-Path $dataRoot "bin"
        $linuxDest = Join-Path $linuxBinDir "orchicon-linux-amd64"
        New-Item -ItemType Directory -Path $linuxBinDir -Force | Out-Null

        # IT IS IN THE ARCHIVE WE ALREADY HAVE. A second download of a second asset was the design
        # that kept failing — silent, and free to drift from this binary's version. The archive ships
        # it from the same commit, so there is nothing to fetch and nothing to mismatch.
        $foundLinux = Get-ChildItem -Path $extractDir -Filter "orchicon-linux-amd64*" -Recurse -File -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -eq "orchicon-linux-amd64" } | Select-Object -First 1
        if ($foundLinux) {
            Copy-Item -LiteralPath $foundLinux.FullName -Destination $linuxDest -Force
            if (Test-IsElfBinary $linuxDest) {
                $script:ContainerBinary = $linuxDest
                Write-Ok "linux runtime binary installed: $linuxDest"
            } else {
                Remove-Item -LiteralPath $linuxDest -Force -ErrorAction SilentlyContinue
                Write-Warn "the runtime binary in the archive is not a linux/amd64 executable, so it was removed"
            }
        } else {
            Write-Warn "this release's archive does not carry orchicon-linux-amd64 (an older release?)"
        }

        # NOT IN THE ARCHIVE: try the separately published asset, which is how pre-this-change
        # releases shipped it. Kept so an older version can still be installed with containers.
        if (-not $script:ContainerBinary) {
            $linuxAsset = "orchicon_${versionNum}_linux_amd64.tar.gz"
            $linuxUrl = "$baseUrl/$Version/$linuxAsset"
            $linuxArchive = Join-Path $tmpdir $linuxAsset
            Write-Info "fetching the linux runtime binary from the release…"
            $fetched = $false
            try {
                Invoke-WebRequest -Uri $linuxUrl -OutFile $linuxArchive -UseBasicParsing
                $fetched = $true
            } catch {
                Write-Warn "could not download the linux runtime binary from:"
                Write-Warn "  $linuxUrl"
                Write-Warn "  $($_.Exception.Message)"
            }
            if ($fetched) {
                $linuxExtract = Join-Path $tmpdir "linux"
                New-Item -ItemType Directory -Path $linuxExtract -Force | Out-Null
                $untarred = $false
                try {
                    & tar -xzf $linuxArchive -C $linuxExtract 2>$null
                    if ($LASTEXITCODE -eq 0) { $untarred = $true }
                } catch { }
                if ($untarred) {
                    $old = Get-ChildItem -Path $linuxExtract -Filter "orchicon" -Recurse -File -ErrorAction SilentlyContinue |
                        Where-Object { $_.Name -eq "orchicon" } | Select-Object -First 1
                    if ($old) {
                        Copy-Item -LiteralPath $old.FullName -Destination $linuxDest -Force
                        if (Test-IsElfBinary $linuxDest) {
                            $script:ContainerBinary = $linuxDest
                            Write-Ok "linux runtime binary installed: $linuxDest"
                        } else {
                            Remove-Item -LiteralPath $linuxDest -Force -ErrorAction SilentlyContinue
                            Write-Warn "the downloaded runtime binary is not a linux/amd64 executable, so it was removed"
                        }
                    }
                }
            }
        }

        # THE VERDICT, SAID ONCE. A missing container binary is not a warning among warnings: it
        # decides whether container execution works on this host at all.
        if ($script:ContainerBinary) {
            Write-Ok "container execution: ready ($script:ContainerBinary)"
        } else {
            Write-Warn "container execution: NOT READY — this host has no linux/amd64 orchicon binary, so"
            Write-Warn "  runtime containers cannot start. Everything else works. To fix it, either re-run"
            Write-Warn "  this installer, or place a linux build at:"
            Write-Warn "    $linuxDest"
            Write-Warn "  or point ORCHICON_RUNTIME_CONTAINER_EXE at one, then restart the runtime daemon."
        }
    }

    Remove-Item -Path $tmpdir -Recurse -Force -ErrorAction SilentlyContinue

    # --- Verify ----------------------------------------------------------------
    Write-Info "verifying the installed binary…"
    $verify = ""
    try { $verify = ((& $exe version 2>&1 | Out-String)).Trim() } catch { $verify = "" }
    if ($LASTEXITCODE -eq 0 -and $verify) {
        Write-Ok "Orchicon $Version installed successfully ($verify)"
    } else {
        Write-Warn "binary installed but could not verify — run '$exe version' to check"
    }

    # VERIFICATION GUIDANCE — BYTE-IDENTICAL to the block scripts/install.sh prints, deliberately: the
    # verification story must not depend on which installer the operator ran.
    # TestInstallersPrintTheSameVerificationGuidance compares the two blocks verbatim, so a change here
    # without the matching change there fails the build.
    Write-Host ""
    Write-Host @'
# >>> verification-guidance
Verifying a manual download:
  orchicon version                  # must print the version you installed
  releases publish SHA256SUMS       # both installers check it; to check by hand:
                                    #   shasum -a 256 <asset>.tar.gz    (macOS)
                                    #   sha256sum <asset>.tar.gz        (Linux)
                                    #   Get-FileHash <asset>.zip        (Windows PowerShell)
                                    # compare the result with SHA256SUMS on the release page
If macOS blocked the binary after a BROWSER download, clear the quarantine bit on that ONE file:
  xattr -d com.apple.quarantine /path/to/orchicon
  This removes the mark from one file. It does not disable Gatekeeper, and it is NOT needed when
  you install with the curl one-liner (curl sets no quarantine bit).
Windows SmartScreen, if you downloaded the zip in a BROWSER instead of running this installer:
  Unblock-File -LiteralPath .\orchicon_<version>_windows_amd64.zip   # BEFORE extracting it
  If orchicon.exe still trips "Windows protected your PC", choose More info, then Run anyway.
  This clears the mark on one archive; it does not change a Windows security setting, and it is NOT
  needed when you install with the irm one-liner, which downloads the archive itself.
# <<< verification-guidance
'@

    # --- PATH hint ---------------------------------------------------------------
    Write-PathHint

    # --- One-command setup (NATIVE, and it streams) ------------------------------
    if ($NoSetup) {
        Write-Host ""
        Write-Host "Installed. Next step:" -ForegroundColor White
        Write-Host "  orchicon install    Start the host services, the runtime daemon and this instance (Postgres and NATS from host binaries)" -ForegroundColor DarkGray
        Write-Host "  (or run it by full path: $exe install)" -ForegroundColor DarkGray
    } else {
        Write-Host ""
        Write-Host "Setting up the full stack (one-command install)…" -ForegroundColor White
        # SAY WHAT IS ABOUT TO HAPPEN, INCLUDING THE QUESTIONS. `orchicon install` acquires the
        # host service binaries (Postgres, NATS — minutes on a first install) and asks about the
        # host ports AND the optional runtime images. Both facts are load-bearing: without the
        # first, a working download is indistinguishable from a hang; without the second, an
        # operator staring at a still screen never learns that the installer is waiting for them.
        # NOTHING IS PULLED BY DEFAULT: the images are an opt-in question that answers "none".
        Write-Host "  · acquiring the host service binaries (Postgres, NATS) — the slow part on a first install" -ForegroundColor DarkGray
        Write-Host "  · starting the runtime daemon, then launching this instance's plane" -ForegroundColor DarkGray
        Write-Host "  · it asks about host ports: press ENTER to accept each default" -ForegroundColor DarkGray
        Write-Host "  · on a SECOND install for this instance it asks for no port it already recorded" -ForegroundColor DarkGray
        Write-Host "  · it offers the optional runtime images (base ~1 GB, GUI +150 MB, dev +1.5-2.5 GB):" -ForegroundColor DarkGray
        Write-Host "    press ENTER for none — a complete install pulls no image at all" -ForegroundColor DarkGray
        Write-Host "  Its output is shown live below." -ForegroundColor DarkGray
        Write-Host ""

        # THE SETUP IS INVOKED BARE, AND THE ENVIRONMENT IS THE ONLY THING SET UP
        # AROUND IT — both details are load-bearing:
        #
        #   * BARE (`& $exe install`, as a STATEMENT). A directly-invoked native child
        #     inherits this console, so its output appears as it is produced and its
        #     prompt can be answered. Capturing it (`$out = & $exe install`,
        #     `| Out-String`, `Tee-Object -Variable`) buffers the progress and the
        #     question: a working install then looks hung, and an invisible prompt is
        #     a permanent hang.
        #
        #   * ORCHICON_INSTALL_INTERACTIVE=1, AND ONLY HERE. The installer decides
        #     whether it may ask about the host ports by checking whether ITS OWN
        #     stdout is a terminal (installIsInteractive() in
        #     cmd/orchicon/install_ports.go). It refuses to prompt otherwise — because
        #     a backgrounded install that opens /dev/tty writes into whatever
        #     terminal it inherits and then blocks forever on input nobody will type.
        #     A Windows operator types into THIS script's console, so the claim "a
        #     human is present" has to be made explicitly; without this line the port
        #     questions would silently never appear. Every UNATTENDED path here
        #     (-NoSetup, -DryRun, -Yes, the teardown verbs) leaves it unset, because
        #     the flag is a claim about a person.
        #
        #   * ORCHICON_ASSUME_YES is set INSTEAD of the interactive flag under -Yes,
        #     the same env channel the shell installer uses, so a non-interactive run
        #     does not sit at the runtime-image prompt with nothing to answer it.
        $prevInteractive = $env:ORCHICON_INSTALL_INTERACTIVE
        $prevInstance    = $env:ORCHICON_INSTANCE
        $prevAssumeYes   = $env:ORCHICON_ASSUME_YES
        $prevProvision   = $env:ORCHICON_PROVISION_ENGINE
        try {
            if ($Yes) {
                $env:ORCHICON_INSTALL_INTERACTIVE = $null
                $env:ORCHICON_ASSUME_YES = "1"
            } else {
                $env:ORCHICON_INSTALL_INTERACTIVE = "1"
                $env:ORCHICON_ASSUME_YES = $null
            }
            # -Containers IS THE CONSENT. Asking again for the thing the operator just asked
            # for is not a safeguard, it is the step that silently declines: under the
            # documented `irm https://orchicon.dev/install.ps1 | iex`, PowerShell's own stdin
            # is the HTTP body, so the child (`& $exe install`) inherits a PIPE rather than a
            # console — promptTerminal() then refuses to prompt (deliberately: it must not
            # read a pipe) and podman is never installed. The operator chose containers, so
            # this env var settles it before any prompt exists.
            #
            # ORCHICON_PROVISION_ENGINE=0 (an explicit refusal) still wins: it is a hard "do
            # not install anything on my machine", and Provision checks that first.
            if ($script:ContainerOptIn) {
                $env:ORCHICON_PROVISION_ENGINE = "1"
            } else {
                $env:ORCHICON_PROVISION_ENGINE = $prevProvision
            }
            $env:ORCHICON_INSTANCE = $script:Instance
            & $exe install
            $setupExit = $LASTEXITCODE
        } finally {
            # ALWAYS, on success and on failure and on ^C: the operator's session must
            # not inherit a claim that a human is answering prompts on some other
            # process's behalf.
            $env:ORCHICON_INSTALL_INTERACTIVE = $prevInteractive
            $env:ORCHICON_INSTANCE = $prevInstance
            $env:ORCHICON_ASSUME_YES = $prevAssumeYes
            $env:ORCHICON_PROVISION_ENGINE = $prevProvision
        }
        if ($setupExit -eq 0) {
            Write-Ok "Install complete — Orchicon is running."
            Write-ConnectionInfo
        } else {
            # The BINARIES are installed; only the stack setup failed. Report the exit code,
            # say where the reason will be, and give the exact command that resumes —
            # `orchicon install` is idempotent and picks up where this stopped.
            Write-Warn "Full-stack setup did not complete (exit $setupExit). The binaries themselves are installed at:"
            Write-Host "  $InstallDir"
            Write-Host "  Resume (idempotent — it picks up where this stopped):"
            Write-Host "    & `"$exe`" install" -ForegroundColor Cyan
            Write-Host "  Where the reason will be:"
            Write-Host "    · the output above — the failing step prints its own reason"
            Write-Host "    · this instance's host services (postgres, nats) print their own reason above"
            Write-Host "    · the instance log: $(Join-Path (Get-InstanceStateDir) 'serve\logs\orchicon.log')"
            if ($script:ContainerOptIn) {
                Write-Host "    · containers: re-run 'orchicon.exe install' — it reports the engine's own reason" -ForegroundColor DarkGray
            }
            # A non-zero outcome for a wrapper: the binaries are in place, the stack is not.
            $script:ExitCode = 1
        }
    }

    # THE DESKTOP APP, opt-in. Without -Desktop this is one line naming where to get it,
    # so the user is not left to find it.
    if ($Desktop) {
        Install-DesktopApp
    } else {
        Write-Host ""
        Write-Host "  Desktop app (optional): re-run with -Desktop, or download it from" -ForegroundColor DarkGray
        Write-Host "    https://github.com/$GitHubOwner/$GitHubRepo/releases/latest" -ForegroundColor DarkGray
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
    if ($InstallDir -and (Test-Path -LiteralPath (Join-Path $InstallDir "orchicon.exe"))) {
        Write-Host ('    Setup only:  & "' + (Join-Path $InstallDir 'orchicon.exe') + '" install') -ForegroundColor Cyan
    }
    if ($script:ContainerOptIn) {
        Write-Host "    Containers:  orchicon.exe runtime-daemon --help   (the engine is the setup's: podman + its machine)" -ForegroundColor Cyan
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
