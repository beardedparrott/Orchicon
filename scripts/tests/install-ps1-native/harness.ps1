param(
  [Parameter(Mandatory=$true)][string]$Installer,
  [Parameter(Mandatory=$true)][string]$Label,
  [Parameter(Mandatory=$true)][string]$StubBin,
  [Parameter(Mandatory=$true)][string]$OutFile,
  [string]$FixtureDir = "",
  [int]$ConsoleCodePage = 936   # a non-Unicode console codepage, as on a CJK Windows
)
# Asserts how scripts/install.ps1 behaves as the NATIVE Windows installer. It loads the
# installer's functions via the AST (the installer itself is never executed in-process) and
# runs them against a wsl.exe double plus a release-archive double on $StubBin. See run.sh for
# what is real here and what is emulated. Exits non-zero if any assertion fails.
#
# WHAT CHANGED WITH THE NATIVE REWRITE, and why this suite is no longer only about encoding:
# the installer used to run the whole stack inside WSL2, so its contract was "everything
# reaches the distro intact". It now installs orchicon.exe and runs `orchicon install` as a
# native Windows process, so its contract is: it installs the NATIVE artifact, it never touches
# WSL unless containers were asked for, it tells a human-is-present setup apart from an
# unattended one, and it reads the ports the install RECORDED instead of guessing them.
#
# It ALSO pins the DEFAULT install path: an MSI is preferred when one is published, and that arm
# sits behind an availability probe, so run.sh serves a fixture MSI to reach it. Every other case
# forces the zip, which is how the MSI arm shipped unexercised — and then crashed on the first
# Windows run with a null scratch directory.
# The wsl.exe encoding assertions are still here — the container opt-in still carries scripts
# into a distro — and the deny-list now covers podman as well as docker.
$ErrorActionPreference = "Stop"
$ConsoleEncoding = [System.Text.Encoding]::GetEncoding($ConsoleCodePage)
[Console]::OutputEncoding = $ConsoleEncoding      # the host decode Windows PS 5.1 performs
$env:PATH = "${StubBin}:$env:PATH"
Remove-Item Env:WSL_UTF8 -ErrorAction SilentlyContinue
Remove-Item Env:EMU_NO_DEFAULT -ErrorAction SilentlyContinue
if (-not $FixtureDir) { $FixtureDir = Join-Path (Split-Path $StubBin -Parent) "fixture" }
$Archive = Join-Path $FixtureDir "release/orchicon_0.0.0_windows_amd64.zip"

$Report = [System.Collections.Generic.List[string]]::new()
$Fail = [System.Collections.Generic.List[string]]::new()

# BOUNDED WAITS. Reaching a verdict by waiting for ever is not a verdict, and this harness has
# places that used to do exactly that. The harness's console output is WITHHELD until it
# finishes, so a blocked child produces no output at all — indistinguishable from a broken
# suite. Every child gets a deadline; expiry KILLS it and records a FAIL that names what was
# blocked. Override for a genuinely slow host: ORCHICON_HARNESS_CHILD_TIMEOUT_MS.
$script:ChildTimeoutMs = 120000
if ($env:ORCHICON_HARNESS_CHILD_TIMEOUT_MS) {
  $parsed = 0
  if ([int]::TryParse($env:ORCHICON_HARNESS_CHILD_TIMEOUT_MS, [ref]$parsed) -and $parsed -gt 0) {
    $script:ChildTimeoutMs = $parsed
  }
}

function Wait-ProcessBounded {
  param([System.Diagnostics.Process]$Process, [string]$What)
  if ($Process.WaitForExit($script:ChildTimeoutMs)) { return $true }
  try { $Process.Kill($true) } catch { try { $Process.Kill() } catch { } }
  Check ("{0} terminates (bounded wait)" -f $What) $false `
    ("it did not exit within {0} ms and was KILLED — it is blocked, not slow. Raise ORCHICON_HARNESS_CHILD_TIMEOUT_MS if this host is genuinely slower than that." -f $script:ChildTimeoutMs)
  return $false
}
function Say($m) { $Report.Add([string]$m) }
function Check($name, $ok, $detail) {
  if ($ok) { Say ("  PASS  {0}{1}" -f $name, $(if ($detail) { " — $detail" } else { "" })) }
  else { Say ("  FAIL  {0}{1}" -f $name, $(if ($detail) { " — $detail" } else { "" })); $Fail.Add($name) }
}
function Yes($b) { if ($b) { "yes" } else { "no" } }
function Eq([string]$a, [string]$b) { [string]::Equals($a, $b, [System.StringComparison]::Ordinal) }
function CP([string]$s) {                            # make invisible code points visible
  if ($null -eq $s) { return '<null>' }
  ($s.ToCharArray() | ForEach-Object {
     $c = [int]$_; if ($c -eq 0) { '<NUL>' } elseif ($c -lt 32) { '<{0:X2}>' -f $c } else { [string]$_ }
  }) -join ''
}
function Code([string]$src) {                        # source with comment lines stripped
  (($src -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
}

$tokens = $null; $errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path $Installer).Path, [ref]$tokens, [ref]$errors)
Say ""
Say "================ $Label ================"
if ($errors.Count) {
  Say "PARSE FAIL ($($errors.Count)):"
  $errors | ForEach-Object { Say ("  L{0}: {1}" -f $_.Extent.StartLineNumber, $_.Message) }
  $Fail.Add("AST parse")
  [System.IO.File]::WriteAllText($OutFile, ($Report -join "`n"), [System.Text.Encoding]::UTF8)
  exit 1
}
$funcs = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)
Invoke-Expression (($funcs | ForEach-Object { $_.Extent.Text }) -join "`n`n")
$bashSrc = ($funcs | Where-Object Name -eq 'Invoke-WslBash').Extent.Text
Say ("installer parses clean; {0} functions; Invoke-WslBash passes -e: {1}" -f $funcs.Count, (Yes ($bashSrc -match '(?m)-e')))

# --- 1. the distro name, captured by the installer's OWN statement ----------
$namesStmt = $ast.FindAll({ param($n)
  $n -is [System.Management.Automation.Language.AssignmentStatementAst] -and $n.Extent.Text -match '^\s*\$names\s*=' }, $true)[0]
Invoke-Expression $namesStmt.Extent.Text
Say ""
Say "[1] distro-name capture"
Say ("    captured: '{0}'  (array={1}, count={2})" -f (CP ($names -join '')), ($names -is [array]), $names.Count)
Check "captured name is the distro, exactly" (Eq ($names -join '') 'Ubuntu') (CP ($names -join ''))
Check "the installer's own fallback (`$names[0]) is the name, not its first character" (Eq ([string]$names[0]) 'Ubuntu') ("got '" + (CP ([string]$names[0])) + "'")

# --- 2/3. both resolution paths -------------------------------------------
$script:Distro = ""
Ensure-Wsl -Soft | Out-Null
Say ""
Say "[2] Ensure-Wsl, default distro marked with a '*' in the verbose listing"
Check "resolves the distro via the verbose listing" (Eq $script:Distro 'Ubuntu') (CP $script:Distro)

$env:EMU_NO_DEFAULT = "1"
$script:Distro = ""
try { Ensure-Wsl -Soft | Out-Null } finally { Remove-Item Env:EMU_NO_DEFAULT -ErrorAction SilentlyContinue }
Say "[3] Ensure-Wsl, no '*' default marker (falls back to the quiet listing)"
Check "falls back to a real name" (Eq $script:Distro 'Ubuntu') (CP $script:Distro)

# --- 5. non-ASCII from the distro (UTF-8 passthrough) ----------------------
$script:Distro = 'Ubuntu'
$cn = '已安装 完成'
$got = ((Invoke-WslBash "printf '%s\n' '$cn'") -join "`n").Trim()
Say ""
Say "[5] non-ASCII printed by the distro"
Check "round-trips exactly (no mojibake)" (Eq $got $cn) ("want '$cn' got '" + (CP $got) + "'")

# --- 6. wsl's OWN localized message ---------------------------------------
$script:Distro = 'NoSuchDistro'
$got2 = ((Invoke-WslBash "true") -join "`n").Trim()
$want = "没有名为 'NoSuchDistro' 的发行版。"
Say "[6] wsl's own message (this is the 'it printed an error in Chinese' report)"
Check "round-trips exactly (no mojibake)" (Eq $got2 $want) ("want '$want' got '" + (CP $got2) + "'")

# --- 7. nothing leaks into the operator's session (`irm | iex`) ------------
Remove-Item Env:WSL_UTF8 -ErrorAction SilentlyContinue
[Console]::OutputEncoding = $ConsoleEncoding
$encBefore = [Console]::OutputEncoding.CodePage
Invoke-WslBash "true" | Out-Null
Say ""
Say "[7] session hygiene"
Check "no WSL_UTF8 left set" ($null -eq $env:WSL_UTF8)
Check "console codepage restored" ([Console]::OutputEncoding.CodePage -eq $encBefore) ("$encBefore -> " + [Console]::OutputEncoding.CodePage)

# --- 8. $LASTEXITCODE must survive the capture wrapper --------------------
$script:Distro = 'Ubuntu'; Invoke-WslBash "true" | Out-Null; $rcOk = $LASTEXITCODE
$script:Distro = 'NoSuchDistro'; Invoke-WslBash "true" | Out-Null; $rcBad = $LASTEXITCODE
$script:Distro = 'Ubuntu'; Invoke-WslBash "exit 7" | Out-Null; $rcSeven = $LASTEXITCODE
Say ""
Say "[8] `$LASTEXITCODE propagation"
Check "success is 0" ($rcOk -eq 0) "exit=$rcOk"
Check "an unknown distro is 1" ($rcBad -eq 1) "exit=$rcBad"
Check "a failing script's own code propagates" ($rcSeven -eq 7) "exit=$rcSeven"

# --- 9. THE REPORTED CRASH: a failed install must not take the session --------
# The install is normally started as `irm … | iex`, which runs the installer's text
# in the operator's OWN session — so a PowerShell `exit` in it closes the window
# with the diagnosis still unread. That is the report pinned here: on a box with no
# Docker Desktop the installer "just closed the powershell session", and the only
# way to read why was to launch PowerShell from inside a command prompt.
#
# Run in a CHILD pwsh, exactly as documented, so "did the session survive?" is a
# question about a real process rather than about this harness. run.sh --baseline
# asserts that this suite FAILS an installer whose halt is an `exit`.
$PwshExe = Join-Path $PSHOME $(if ($IsWindows) { "pwsh.exe" } else { "pwsh" })
if (-not (Test-Path $PwshExe)) { $PwshExe = (Get-Command pwsh).Source }
$Work = Split-Path $StubBin -Parent
$marker = "INSTALLER-RETURNED-TO-PROMPT"
$quoted = $Installer.Replace("'", "''")

function Invoke-ChildInstaller {
  param([hashtable]$Env, [string]$Command, [string]$Stdin = "")
  $prev = @{}
  foreach ($k in $Env.Keys) {
    $prev[$k] = [Environment]::GetEnvironmentVariable($k)
    [Environment]::SetEnvironmentVariable($k, $Env[$k])
  }
  try {
    # THE CHILD'S STDIN IS PINNED, AND THAT IS THE WHOLE POINT OF THIS SHAPE.
    #
    # The installer ASKS for an instance (Read-Host). Run from a terminal — which is exactly how
    # `make rebuild-dev` runs this harness — Read-Host BLOCKS, and the suite hangs with no output
    # until something kills it. Feeding the child a file (empty unless a test supplies an answer)
    # makes Read-Host return immediately, so the suite behaves identically with or without a
    # terminal. Deterministic in both contexts, which is what a test must be.
    $work = Join-Path $Work ("child-" + [guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    $inFile = Join-Path $work "in.txt"
    $outFile = Join-Path $work "out.txt"
    $errFile = Join-Path $work "err.txt"
    $scriptFile = Join-Path $work "child.ps1"
    [System.IO.File]::WriteAllText($inFile, $Stdin)
    # THE COMMAND IS WRITTEN TO A FILE AND RUN WITH -File, not passed as -Command. `Start-Process
    # -ArgumentList` re-quotes the argument for the child, and the embedded double quotes come out
    # MANGLED — measured: an inner `WriteLine("x")` reached the child as `WriteLine(x)`, a parse
    # error. A file carries the text exactly.
    [System.IO.File]::WriteAllText($scriptFile, "`$ErrorActionPreference = 'Continue'" + [Environment]::NewLine + $Command)
    $proc = Start-Process -FilePath $PwshExe -ArgumentList @("-NoProfile", "-File", $scriptFile) `
      -RedirectStandardInput $inFile -RedirectStandardOutput $outFile -RedirectStandardError $errFile `
      -PassThru
    [void](Wait-ProcessBounded -Process $proc -What "a child installer run")
    $text = ""
    if (Test-Path $outFile) { $text += [System.IO.File]::ReadAllText($outFile) }
    if (Test-Path $errFile) { $text += [System.IO.File]::ReadAllText($errFile) }
    return $text
  } finally {
    foreach ($k in $prev.Keys) { [Environment]::SetEnvironmentVariable($k, $prev[$k]) }
  }
}

# A FULL NATIVE INSTALL, in a child, against the archive double. This is the shape every new
# assertion below reuses: a scratch $HOME (so the instance state dir the installer reads and the
# double writes is disposable), a scratch install dir, no network, and the wsl double's call log
# switched on so "was wsl touched?" is a fact rather than an inference.
function Invoke-InstallRun {
  param([hashtable]$Extra = @{}, [switch]$NoArchive, [string]$Stdin = "")
  $root = Join-Path $Work ("run-" + [guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Force -Path (Join-Path $root "home") | Out-Null
  New-Item -ItemType Directory -Force -Path (Join-Path $root "bin") | Out-Null
  $envLog = Join-Path $root "orchicon-env.log"
  $callLog = Join-Path $root "wsl-calls.log"
  $base = @{
    PROCESSOR_ARCHITECTURE   = "AMD64"
    HOME                     = (Join-Path $root "home")
    ORCHICON_INSTALL_LOG     = "off"
    ORCHICON_INSTANCE        = ""
    ORCHICON_INSTALL_BASE_URL = "https://example.invalid/releases"
    EMU_CALL_LOG             = $callLog
    EMU_ORCHICON_ENV_LOG     = $envLog
    EMU_PORTS_JSON           = '{"ORCHICON_CONTROL_PORT":8099}'
  }
  if (-not $NoArchive) { $base["ORCHICON_INSTALL_ARCHIVE"] = $Archive }
  foreach ($k in $Extra.Keys) { $base[$k] = $Extra[$k] }
  $installDir = Join-Path $root "bin"
  $cmd = "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Version v0.0.0 -Instance dev -InstallDir '$installDir'" + $(if ($Extra.ContainsKey("__ARGS__")) { " " + $Extra["__ARGS__"] } else { "" }) + "`n'EXITCODE=' + `$global:LASTEXITCODE`n'$marker'"
  if ($base.ContainsKey("__ARGS__")) { $base.Remove("__ARGS__") }
  $text = Invoke-ChildInstaller -Env $base -Command $cmd -Stdin $Stdin
  return [pscustomobject]@{
    Text = $text; Root = $root; Home = (Join-Path $root "home"); InstallDir = $installDir
    EnvLog = $envLog; CallLog = $callLog
    PortsRecord = (Join-Path (Join-Path $root "home") ".local/share/orchicon-dev/ports.json")
  }
}

Say ""
Say "[9] a failed install returns to the prompt (the reported crash)"
# THE FAILURE IS A DOWNLOAD FAILURE, and it is a real one for a clean Windows box: no network,
# or a corporate proxy that answers 407. It halts AFTER -Help, -DryRun and the elevation
# pre-flight, so the halt footer and the transcript are exercised on the path an operator hits.
$noNet = Invoke-InstallRun -NoArchive -Extra @{ ORCHICON_INSTALL_BASE_URL = "https://example.invalid/releases" }
Check "the session survives the halt (the installer returned)" ($noNet.Text -match $marker) $(if ($noNet.Text -match $marker) { "$marker printed" } else { "NO MARKER — the child died, as the pre-fix installer did" })
Check "the CAUSE is named: the download failed" ($noNet.Text -match 'download failed') "the report must not be a bare list of steps"
Check "the next steps are printed" ($noNet.Text -match [regex]::Escape("irm https://orchicon.dev/install.ps1 | iex"))
Check "no raw exception is dumped at the operator" ($noNet.Text -notmatch "OperationStopped|Cannot bind argument|At line:")
Check "a halt does not claim success" ($noNet.Text -notmatch "Install complete")
Check "a halt reports a non-zero outcome" ($noNet.Text -match "EXITCODE=1") (($noNet.Text -split "`n" | Where-Object { $_ -match 'EXITCODE' }) -join ' / ')
# THE HALF-INSTALLED BINARY MUST NOT BE CALLED A SUCCESS: nothing was extracted, so the
# resume advice must not name a binary that does not exist.
Check "it does not tell the operator to run a binary it never installed" (-not ($noNet.Text -match 'Resume \(idempotent')) "the footer only offers the resume command when the binary is there"

$help = Invoke-ChildInstaller -Env @{ ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-help") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Help; '$marker'"
Check "-Help returns to the prompt (it closed the window too)" ($help -match $marker)
Check "-Help still prints the usage" ($help -match "Usage: install.ps1")

$dry = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64"; HOME = (Join-Path $Work "home-dry"); ORCHICON_INSTANCE = "" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Version v0.0.0 -Instance dev -DryRun; '$marker'"
Check "-DryRun returns to the prompt" ($dry -match $marker)
Check "-DryRun still prints its plan" ($dry -match "dry-run complete")
Check "-DryRun starts no transcript (it changes nothing)" ($dry -notmatch "full log of this install")
Check "-DryRun downloads nothing and installs nothing" ($dry -notmatch "extracting and installing") "a dry run that reaches the network is not a dry run"

$logPath = Join-Path $Work "transcript.log"
$logged = Invoke-InstallRun -NoArchive -Extra @{ ORCHICON_INSTALL_LOG = $logPath; ORCHICON_INSTALL_BASE_URL = "https://example.invalid/releases" }
$logText = if (Test-Path $logPath) { Get-Content -Raw $logPath } else { "" }
Check "the run leaves a transcript where it was asked to" ((Test-Path $logPath) -and ($logText -match "Orchicon was not installed")) "the log is the evidence a closed window used to take with it"
Check "the transcript's path is printed on the way past" ($logged.Text -match [regex]::Escape($logPath))

# --- 10. THE OTHER REPORTED FAILURE: the long step must STREAM ---------------
# TWO MECHANISMS now, and both are pinned:
#
#   * the SETUP is a BARE native invocation (`& $exe install`), because a directly-invoked
#     native child inherits the console. Capturing it would buffer the progress AND the port
#     prompt, and a buffered prompt is a permanent hang. Asserted on the AST and proved to bite
#     by mutating a copy into a capture and requiring FAIL.
#   * the CONTAINER substrate probe still uses Invoke-WslBashLive, and this is the difference it
#     makes: watch a CHILD from here and look for the guest's first line WHILE IT IS STILL
#     RUNNING. Capture-and-replay cannot pass that, so the same guest is also run through
#     Invoke-WslBash and required to FAIL it.
$driver = Join-Path $Work "stream-driver.ps1"
[System.IO.File]::WriteAllText($driver, @'
param([string]$Installer, [string]$Mode)
$t = $null; $e = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path $Installer).Path, [ref]$t, [ref]$e)
$funcs = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)
Invoke-Expression (($funcs | ForEach-Object { $_.Extent.Text }) -join [Environment]::NewLine)
$script:Distro = "Ubuntu"
$guest = "echo STREAM-MARKER; sleep 4; echo LATE-MARKER"
if ($Mode -eq "capture") { $out = Invoke-WslBash $guest; Write-Output ("CAPTURE-RETURNED captured-lines=" + (@($out).Count)) }
else { Invoke-WslBashLive $guest; Write-Output "LIVE-RETURNED" }
'@, [System.Text.Encoding]::UTF8)

function Measure-Streaming {
  param([string]$Mode)
  $out = Join-Path $Work "stream-$Mode.out"
  Remove-Item $out -ErrorAction SilentlyContinue
  # Its stdin is PINNED for the same reason the installer child's is: a child that inherits a
  # terminal and then reads it blocks for ever, and the operator sees silence.
  $inFile = Join-Path $Work "stream-in.txt"
  [System.IO.File]::WriteAllText($inFile, "")
  $p = Start-Process -FilePath $PwshExe -PassThru -RedirectStandardOutput $out -RedirectStandardInput $inFile `
    -ArgumentList @("-NoProfile", "-File", $driver, "-Installer", $Installer, "-Mode", $Mode)
  $early = $false
  $deadline = (Get-Date).AddSeconds(25)
  while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 150
    $txt = ""
    if (Test-Path $out) { try { $txt = [System.IO.File]::ReadAllText($out) } catch { $txt = "" } }
    if ($txt -match "STREAM-MARKER") { $early = -not $p.HasExited; break }
    if ($p.HasExited) { break }
  }
  [void](Wait-ProcessBounded -Process $p -What "the streaming child")
  $text = ""
  $drain = (Get-Date).AddSeconds(10)
  while ((Get-Date) -lt $drain) {
    if (Test-Path $out) { try { $text = [System.IO.File]::ReadAllText($out) } catch { $text = "" } }
    if ($text -match "RETURNED") { break }
    Start-Sleep -Milliseconds 100
  }
  return [pscustomobject]@{ Early = $early; Text = $text }
}

Say ""
Say "[10] the long/interactive steps stream (the 'hanging with no progress' report)"
$liveRes = Measure-Streaming -Mode "live"
Check "the live path shows the guest's first line WHILE it is still running" $liveRes.Early ("early=" + $liveRes.Early)
Check "the live path shows the guest's output in full" (($liveRes.Text -match "LATE-MARKER") -and ($liveRes.Text -match "LIVE-RETURNED"))
$capRes = Measure-Streaming -Mode "capture"
Check "the capturing path buffers — which is why the setup must not use it" ($capRes.Early -eq $false) ("early=" + $capRes.Early)
Check "the capturing path did run the guest, and captured both its lines" (($capRes.Text -match "captured-lines=2") -and ($capRes.Text -match "CAPTURE-RETURNED")) "the output is held until the end — which is why nothing was visible"

# The setup invocation itself: a bare statement, never assigned and never piped. A single
# CommandAst `& $exe install` whose parent chain reaches the try-block directly is the shape;
# `$x = & $exe install` or `& $exe install | Out-String` is the defect.
function Test-SetupInvocationBare([string]$Source) {
  $lines = @(($Source -split "`n") | Where-Object { $_ -notmatch '^\s*#' })
  $bare = @($lines | Where-Object { $_ -match '^\s*&\s*\$exe\s+install\s*$' })
  if ($bare.Count -lt 1) { return $false }
  foreach ($l in $lines) {
    if ($l -match '=\s*\(?\s*&\s*\$exe\s+install') { return $false }
    if ($l -match '&\s*\$exe\s+install\s*\|') { return $false }
  }
  return $true
}
$ps1Src0 = Get-Content -Raw $Installer
Check "the setup is invoked BARE (a statement, not captured)" (Test-SetupInvocationBare $ps1Src0) `
  "a captured `$exe install buffers the progress and the port prompt: a working install then looks hung"
# THE DETECTOR MUST BITE: mutate the bare invocation into a capture and require the check above
# to FAIL. A detector that stopped detecting is worse than no detector, because it is trusted.
$mutated = $ps1Src0.Replace("            & `$exe install", "            `$capturedInstall = (& `$exe install | Out-String)")
Check "the bare-invocation check is proven to bite (a capture is rejected)" (($mutated -ne $ps1Src0) -and (-not (Test-SetupInvocationBare $mutated))) `
  "the mutation must apply AND be rejected, or this section proves nothing"

# --- 11. The invariants, on the AST, so a later edit cannot quietly undo them ---
Say ""
Say "[11] the invariants the native installer rests on"
$exits = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.ExitStatementAst] }, $true)
Check 'no PowerShell `exit` anywhere (a bash one inside a here-string is fine)' ($exits.Count -eq 0) ("found at line(s): " + (($exits | ForEach-Object { $_.Extent.StartLineNumber }) -join ", "))
$src11 = Get-Content -Raw $Installer
Check "the halt sentinel is still a throw/catch PAIR" ((($src11 -split "OrchiconInstallHalt").Count - 1) -eq 2) "occurrences: $(($src11 -split 'OrchiconInstallHalt').Count - 1)"
$code11 = Code $src11

# C1 — THE INTERACTIVE CLAIM. installIsInteractive() (cmd/orchicon/install_ports.go) decides
# whether the port questions may be asked by checking whether ITS OWN stdout is a terminal, and
# this wrapper's setup invocation is not a terminal. Without an explicit declaration the prompts
# would NEVER appear on Windows, silently defeating a preserved behaviour. So: the assignment
# exists, it is "1", and it sits in the block that invokes the setup.
Check "ORCHICON_INSTALL_INTERACTIVE is declared for the setup invocation" `
  (($code11 -match '\$env:ORCHICON_INSTALL_INTERACTIVE\s*=\s*"1"')) `
  "without it the installer believes nobody is watching and never offers the port questions"
$interactiveAt = $code11.IndexOf('$env:ORCHICON_INSTALL_INTERACTIVE = "1"')
$setupAt = $code11.IndexOf('& $exe install')
Check "and it is in the SAME region as the setup invocation, before it" `
  (($interactiveAt -ge 0) -and ($setupAt -gt $interactiveAt) -and (($setupAt - $interactiveAt) -lt 2000)) `
  "a flag set somewhere else in the file cannot be reviewed against the call it is for"
Check "the unattended path sets the flag to EMPTY instead (it is a claim that a person is here)" `
  ($code11 -match 'if \(\$Yes\) \{\s*\n\s*\$env:ORCHICON_INSTALL_INTERACTIVE = \$null') `
  "-Yes means nothing will answer a prompt, so claiming interactivity would hang the run"
# Criterion 3: the three-cause Docker Desktop diagnosis and its two helpers are GONE.
Check "Ensure-WslDocker and Test-DockerDesktopRunning are DELETED, not ported" `
  ((-not ($code11 -match 'Ensure-WslDocker')) -and (-not ($code11 -match 'Test-DockerDesktopRunning'))) `
  "the engine diagnosis lives in install_engine.go now; a wrapper copy could only disagree with it"
Check "nothing INSPECTS Docker Desktop any more" `
  (-not ($code11 -match 'Get-Process|com\.docker\.backend|Get-Service')) `
  "a comment or the help text may say it is no longer required, but no code may act on it"
Check "the three-cause report is gone with it" `
  (-not ($code11 -match 'no docker CLI at all|WSL Integration|not enabled for THIS distro')) `
  "that report described a requirement that no longer exists"
# Criterion 5: the native artifact, never the linux one.
Check "the asset is the windows zip" `
  (($code11 -match 'orchicon_\$\{versionNum\}_windows_\$\{arch\}\.zip') -and (-not ($code11 -match '_linux_'))) `
  "a linux asset on the native path is an install that cannot run"

# --- 12. THE SCRIPT MUST REACH THE GUEST AS WRITTEN -------------------------
# The container opt-in still carries a multi-line bash probe into a distro, and Windows PowerShell
# 5.1 does not escape embedded quotes when it builds a native command line — so wsl.exe ate every
# quote as an argument delimiter and the guest ran a DIFFERENT PROGRAM. Measured on a real Windows
# box, a quoted `mktemp` assignment arrived as a bare `-d)` and the install died with "unexpected
# EOF while looking for matching )". The transport is base64, whose alphabet has no quote.
#
# TWO ASSERTIONS: the guest RECEIVES the script byte-identical (a real round trip), and the
# command handed to wsl contains no double quote at all (the hazard itself).
Say ""
Say "[12] the script reaches the guest as written"

$record = Join-Path $Work "received-script.txt"
$prevRecord = $env:EMU_RECORD
$env:EMU_RECORD = $record
try {
  # Quotes, command substitution, a quoted pipeline, an apostrophe — the shapes the real probe uses
  # and that a quote-eating transport destroys.
  $original = @'
set -euo pipefail
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "found: $TMP" | grep -q . || true
'@
  $script:Distro = "Ubuntu"
  Invoke-WslBashLive $original | Out-Null
} finally {
  if ($null -eq $prevRecord) { Remove-Item Env:EMU_RECORD -ErrorAction SilentlyContinue }
  else { $env:EMU_RECORD = $prevRecord }
}
$expected = $original -replace "`r`n", "`n"
$received = if (Test-Path $record) { [System.IO.File]::ReadAllText($record) } else { "" }
Check "the guest receives the script BYTE-IDENTICAL to what was written" `
  ([string]::Equals($received, $expected, [System.StringComparison]::Ordinal)) `
  ("received " + $received.Length + " chars, expected " + $expected.Length)
Check "its double quotes survive (the exact thing 5.1 stripped)" `
  ($received -match [regex]::Escape('TMP="$(mktemp -d)"'))
$liveCode = Code (($funcs | Where-Object Name -eq "Invoke-WslBashLive").Extent.Text)
$capCode = Code (($funcs | Where-Object Name -eq "Invoke-WslBash").Extent.Text)
$builder = ($funcs | Where-Object Name -eq "WslBashArgs").Extent.Text
Check 'both helpers build their argv through the ONE builder (WslBashArgs)' `
  (($liveCode -match 'WslBashArgs \$Script') -and ($capCode -match 'WslBashArgs \$Script')) `
  'a second, hand-built argv is how the transport drifted before'
Check 'neither helper passes the script inline as an argument' `
  (($liveCode -notmatch '"\s*,\s*\$Script') -and ($capCode -notmatch '"\s*,\s*\$Script')) `
  'an inline `bash -lc $Script` puts the quotes back on the command line'
Check 'the builder encodes the script (base64) rather than passing it raw' `
  ($builder -match 'ConvertTo-BashPayload' -and $builder -match 'base64 -d') `
  'the payload alphabet is the reason nothing can be mangled in transit'

# --- 13. -Instance MUST SCOPE EVERYTHING, AND REFUSE AN UNKNOWN NAME --------
# Every assertion CLEARS ORCHICON_INSTANCE first: the variable is inherited from whatever plane is
# running the session, so without clearing, "the default" would be testing the ambient environment.
Say ""
Say "[13] -Instance scoping"

$scoped = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-scope") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -Instance test -DryRun"
Check "-Instance test scopes the teardown to the TEST instance only" `
  (($scoped -match 'ORCHICON_INSTANCE=test') -and ($scoped -notmatch 'ORCHICON_INSTANCE=dev') -and ($scoped -notmatch 'ORCHICON_INSTANCE=prod')) `
  ("an unscoped cleanup would take dev and prod down too; got: " + (($scoped -split "`n" | Where-Object { $_ -match 'ORCHICON_INSTANCE|instance' }) -join ' / '))
Check "and it names no container, because a host-resident plane has none" `
  (-not ($scoped -match 'orchicon-cnt')) `
  ("a container name in the plan is a step that cannot run; got: " + (($scoped -split "`n" | Where-Object { $_ -match 'cnt' }) -join ' / '))
Check "and it scopes the STATE DIRECTORY it will touch to that instance" `
  ($scoped -match 'orchicon-test') "the path it prints is the path it acts on"

$def = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-def") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -DryRun"
Check "with no flag, no environment and no terminal it REFUSES" `
  ($def -match 'no instance chosen') `
  ("got: " + (($def -split "`n" | Where-Object { $_ -match 'instance' }) -join ' / '))
$explicit = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-exp") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -Instance dev -DryRun"
Check "and -Instance dev is still honoured without any prompt" ($explicit -match 'ORCHICON_INSTANCE=dev') `
  ("got: " + (($explicit -split "`n" | Where-Object { $_ -match 'ORCHICON_INSTANCE|instance' }) -join ' / '))
$fromEnv = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = "test"; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-env") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -DryRun"
Check "the environment is still honoured when no flag is given" ($fromEnv -match 'ORCHICON_INSTANCE=test') `
  ("got: " + (($fromEnv -split "`n" | Where-Object { $_ -match 'ORCHICON_INSTANCE|instance' }) -join ' / '))
$bad = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-bad") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -Instance production -DryRun; '$marker'"
Check "an unknown instance is REFUSED, naming the known set" `
  ($bad -match "unknown instance: 'production'" -and $bad -match 'dev, test, prod') `
  "defaulting to dev would clean up the wrong stack while the operator watched prod"
Check "and it returns to the prompt rather than acting" ($bad -match $marker)
$help2 = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-h2") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Help; '$marker'"
Check "-Help documents -Instance" ($help2 -match '-Instance <name>')
Check "and -Help mentions dev, test and prod" `
  (($help2 -match 'dev \(default\)') -and ($help2 -match 'test') -and ($help2 -match 'prod'))

# --- 14. UNINSTALL MUST ACTUALLY STOP THINGS, AND MUST NOT TAKE DATA UNASKED ---
# The operator ran -Uninstall on Windows and found "the web server is still running broken": the
# binary was gone and the plane was not. `serve --stop` resolves its PID file from
# ORCHICON_SERVE_STATE_DIR, so a bare call looks in the CWD-relative default, MISSES a
# host-resident plane, reports success, and then the caller deletes the binary out from under it.
# The verbs are NATIVE now, so this is asserted on the functions themselves (extracted and run) as
# well as on the source.
Say ""
Say "[14] uninstall stops what it says it stops"

$stopSrc = ($funcs | Where-Object Name -eq 'Stop-InstancePlane').Extent.Text
$stopCode = Code $stopSrc
Check "the stop goes through 'orchicon stop' scoped to the instance" `
  (($stopCode -match '& \$exe stop') -and ($stopCode -match '\$env:ORCHICON_INSTANCE = Get-InstanceName')) `
  "a machine-wide match by process name took dev and prod's planes down"
Check "and it hands the SERVE STATE DIR explicitly" `
  ($stopCode -match 'ORCHICON_SERVE_STATE_DIR') `
  "without it the call resolves the PID file somewhere else, misses the plane, and reports success"
Check "and it never falls back to serve --stop, pkill or fuser" `
  (-not ($stopCode -match 'serve --stop|pkill|fuser')) "an instance-scoped command with a machine-wide effect is the incident this replaces"
Check "its environment changes are restored" ($stopCode -match 'finally') "a leaked ORCHICON_INSTANCE would retarget the operator's next command"
$uninstallSrc = Code $src11
Check "uninstall stops the plane BEFORE removing the binaries" `
  (($uninstallSrc.IndexOf('Stop-InstancePlane', $uninstallSrc.IndexOf('if ($Uninstall) {'))) -lt ($uninstallSrc.IndexOf('Remove-Item -LiteralPath $f -Force', $uninstallSrc.IndexOf('if ($Uninstall) {')))) `
  "deleting the executable first is what produced the broken server"
Check "data is preserved unless -Purge is given" `
  (($src11 -match 'Re-run with -Purge to remove it') -and ($uninstallSrc -match 'if \(\$Purge\)')) `
  "destroying a database is not something an uninstall may infer from 'remove the software'"
Check "the uninstall summary distinguishes preserved from destroyed data" `
  (($src11 -match 'DATA PRESERVED') -and ($src11 -match 'host state destroyed')) `
  "an uninstall must not be ambiguous about whether the database survived"
Check "-Uninstall / -RemoveBinary destroy nothing outside the install dir and the instance state dir" `
  ((-not ($uninstallSrc -match 'Remove-Item -LiteralPath \$HOME')) -and (-not ($uninstallSrc -match 'rm -rf')) -and (-not ($uninstallSrc -match 'docker (rm|stop|volume|run)'))) `
  "-ForceClean once deleted $HOME/bin and $HOME/data, directories Orchicon does not own"

$purgeHelp = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off"; HOME = (Join-Path $Work "home-ph") } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Help; '$marker'"
Check "-Help documents -Purge" ($purgeHelp -match '-Purge')
Check "and says the uninstall preserves data" ($purgeHelp -match 'DATA IS PRESERVED')

# --- 15. THE DESTRUCTIVE VERBS ARE NATIVE, CONSENTED AND INSTANCE-BOUND ------
Say ""
Say "[15] -Uninstall / -RemoveBinary / -Clean / -ForceClean"

Check "the instance is ASKED FOR, not inferred" ($uninstallSrc -match 'function Ask-Instance') `
  "inferring it from the environment with a dev default is how the wrong stack gets cleaned"
Check "a missing answer REFUSES (no reliance on switch default)" `
  ($uninstallSrc -match 'IsNullOrWhiteSpace\(\$answer\)') `
  "switch (`$null) fires no branch at all, so a default clause cannot catch a missing answer"
Check "whether -Instance was given is recorded at SCRIPT scope" `
  ($uninstallSrc -match '\$script:InstanceGiven = \$PSBoundParameters\.ContainsKey') `
  "inside a function `$PSBoundParameters describes the FUNCTION, so the check there never fired"
Check "-RemoveBinary gates the stop on CONSENT and declining changes nothing" `
  (($src11 -match 'stop this instance(?:''s)? plane, then remove the binaries') -and ($src11 -match 'nothing stopped, nothing removed')) `
  "removing a binary a plane runs from is what leaves a server answering with no binary"
Check "-RemoveBinary honours -DryRun" `
  ($uninstallSrc -match 'would remove \$exePath and \$orchPath') "a dry run that removes something is worse than no dry run"
Check "-Help documents -RemoveBinary" ($src11 -match '-RemoveBinary')
Check "-Help documents -Containers" ($src11 -match '-Containers')
Check "no bash payloads remain for the destructive verbs (they were filesystem-local to the distro)" `
  (-not ($uninstallSrc -match 'Invoke-WslBash \$removeBinaryScript|Invoke-WslBash \$uninstallScript|Invoke-WslBash \$cleanScript')) `
  "a payload that ran in the distro could not touch the native install dir it claimed to clean"

# --- 16. THE CONNECTION INFO READS THE RECORD, IT DOES NOT GUESS --------------
# Reported from a real Windows install: the operator answered 3001 for a published port, `orchicon
# install` printed 3001, and then Write-ConnectionInfo printed 3002 — one run, two contradicting
# numbers, the second being a port nothing had bound. A PROMPT cannot be predicted by a wrapper, so
# the install RECORDS what it bound and this reads it back. There is no hardcoded fallback any
# more: printing a default when the record is missing is how the guess reappears as a fact.
Say ""
Say "[16] the connection info reads the ports that were bound"

$savedHome = $HOME
$scratchHome = Join-Path $Work "home-record"
New-Item -ItemType Directory -Force -Path (Join-Path $scratchHome ".local/share/orchicon-test") | Out-Null
[System.IO.File]::WriteAllText((Join-Path $scratchHome ".local/share/orchicon-test/ports.json"),
  '{"ORCHICON_CONTROL_PORT":8092,"ORCHICON_GRAFANA_PORT":3001}')
$prevInstance16 = $script:Instance
try {
  Set-Variable -Name HOME -Value $scratchHome -Force
  $script:Instance = "test"
  $resolved = Get-ResolvedPorts
  Check "the record is read back from ~/.local/share/orchicon-<instance>/ports.json" ($null -ne $resolved) ("looked in " + (Get-InstanceStateDir))
  Check "and it carries the port the operator actually chose" `
    ($resolved -and $resolved.ORCHICON_CONTROL_PORT -eq 8092) `
    ("got control=" + $(if ($resolved) { $resolved.ORCHICON_CONTROL_PORT } else { "<none>" }))
  $sb = [System.Text.StringBuilder]::new()
  function Write-Host { param([Parameter(ValueFromRemainingArguments=$true)]$Object) [void]$sb.AppendLine(($Object -join " ")) }
  Write-ConnectionInfo
  $shown = $sb.ToString()
  Check "and the printed control URL is that port, not a default" ($shown -match "localhost:8092") `
    ("printed: " + (($shown -split "`n" | Where-Object { $_ -match 'Control plane' }) -join ' / '))
  Check "the retired Grafana URL is NOT printed, even from a pre-removal record" (-not ($shown -match "localhost:3001")) `
    "a stale key must stay inert rather than resurrect a component"
  # C2's regression: with NO record, the old code printed the per-instance default (8080/3002) and
  # that number was a lie. It must print no port at all.
  Remove-Item -LiteralPath (Join-Path $scratchHome ".local/share/orchicon-test/ports.json") -Force
  Check "with no record Get-ResolvedPorts returns nothing (there is no fallback)" ($null -eq (Get-ResolvedPorts))
  [void]$sb.Clear()
  Write-ConnectionInfo
  $noRec = $sb.ToString()
  Check "and the operator sees NO port number at all" (-not ($noRec -match 'localhost:8080|localhost:3001|localhost:3002|localhost:8091')) `
    ("printed: " + (($noRec -split "`n" | Where-Object { $_ -match 'localhost' }) -join ' / '))
  Check "and is told how to ask what it bound" ($noRec -match 'status') "the honest answer without a record is 'ask it'"
} finally {
  Set-Variable -Name HOME -Value $savedHome -Force
  $script:Instance = $prevInstance16
  Remove-Item Function:\Write-Host -ErrorAction SilentlyContinue
}
$ps1b = Get-Content -Raw $Installer
Check "Write-ConnectionInfo consults the recorded ports" `
  (($ps1b -match 'Get-ResolvedPorts') -and ($ps1b -match 'resolved\.ORCHICON_CONTROL_PORT')) `
  "a re-derivation here disagrees with the install's own output"
Check "the installer names no Grafana port or URL" (-not ($ps1b -match 'GRAFANA')) `
  "a retired component must not survive in the wrapper that prints the connection info"
Check "the dry run prints no port numbers either" (-not ((Code $ps1b) -match '8080 / 3002|dev defaults')) `
  "a plan that names a port the install may never bind is the same defect"

# --- 17. A PLAIN INSTALL ASKS FOR AN INSTANCE; THE DESTRUCTIVE VERBS REFUSE ---
Say ""
Say "[17] install asks with a fallback; destructive verbs refuse"
$code17 = Code $ps1b
Check "a plain install asks, with dev as the fallback" ($code17 -match 'Ask-Instance -Fallback "dev"') `
  "installing to the documented default is not destructive, so it may default"
Check "and an empty answer refuses without a fallback" ($code17 -match 'no instance chosen - pass -Instance') `
  "the refusal is what makes Enter safe for the destructive verbs"

# --- 18. THE WINDOWS SETUP PULLS NO IMAGE OF ITS OWN --------------------------
# The instance image was deleted: the plane and its services are host processes. Nothing may
# reintroduce a container ref into a wrapper whose install no longer creates a container.
Say ""
Say "[18] the Windows setup pulls no image of its own"
Check "the setup passes no instance image ref" (-not ($ps1b -match 'ORCHICON_CONTAINER_IMAGE')) `
  "the instance image is gone; a ref here names a container the install never creates"
Check "and it carries no registry ref and no unfilled placeholder" `
  ((-not ($ps1b -match 'ghcr\.io/')) -and (-not ($ps1b -match '__IMAGE_REF__'))) `
  "a leftover registry ref or placeholder is a pull waiting to fail against a ref that does not exist"
Check "and it pre-pulls no runtime image either" (-not ($ps1b -match 'ORCHICON_RUNTIME_IMAGE')) `
  "runtime images are on demand: the install PROMPTS for them and pulls only what was chosen"

# --- 19. EVERY DESTRUCTIVE FLAG IS INSTANCE-BOUND ---------------------------
# ENUMERATED ON PURPOSE: the flag list is the assertion's input, so a new destructive flag that
# forgets to ask must be added here to be covered — which surfaces the omission in review.
Say ""
Say "[19] every destructive flag asks for an instance"
foreach ($flag in @('$RemoveBinary', '$Uninstall', '$Clean', '$ForceClean')) {
  $start = $code17.IndexOf("if (" + $flag + ") {")
  if ($start -lt 0) { Check "$flag found in install.ps1" $false "the verb is gone, so this assertion cannot check it"; continue }
  $block = $code17.Substring($start, [Math]::Min(1600, $code17.Length - $start))
  Check "$flag asks which instance" ($block -match 'Ask-Instance') `
    "without it the verb acts on whatever instance the environment names — which is how -ForceClean could destroy state nobody chose"
}
Check "install is the only flag with a fallback" ($code17 -match 'Ask-Instance -Fallback "dev"') `
  "installing to the documented default is safe; destroying data on a default is not"

# --- 20. THE HARNESS MUST NOT BLOCK, AND THE PROMPT MUST COME LAST ------
# Reported: "I am rebuilding dev and it keeps hanging on this step" — and this step WAS this harness.
# The installer asks for an instance with Read-Host; run from a terminal Read-Host BLOCKS, so the
# suite sat there with no output until something killed it. CI never saw it because CI has no
# terminal, so the failure existed only for the person running the build.
Say ""
Say "[20] the harness cannot block, and the prompt comes after the prerequisites"
$harnessSrc = Get-Content -Raw $PSCommandPath
Check "the child's stdin is PINNED (so a prompt returns instead of blocking)" `
  (($harnessSrc -match 'RedirectStandardInput') -and ($harnessSrc -match '\[System.IO.File\]::WriteAllText\(\$inFile')) `
  "without this, Read-Host waits for a keystroke that a test will never type"
Check "the child command is passed by FILE, not inline" ($harnessSrc -match '"-NoProfile", "-File", \$scriptFile') `
  "Start-Process mangles embedded quotes in an inline -Command"
$runner20 = $ps1b.Substring($ps1b.IndexOf('RUN THE INSTALLER'))
$elevAt = $runner20.IndexOf('Assert-NotElevated')
$contAt = $runner20.IndexOf('Install-ContainersSubstrate')
$askAt  = $runner20.IndexOf('Ask-Instance -Fallback "dev"')
Check "the instance prompt comes AFTER the elevation pre-flight and the containers step" `
  (($elevAt -ge 0) -and ($askAt -gt $elevAt) -and ($contAt -ge 0) -and ($askAt -gt $contAt)) `
  ("elev=$elevAt containers=$contAt ask=$askAt — asking first makes the operator answer a question about a stack that cannot be created")
Check "the elevation guard is a PRE-FLIGHT, before any download" `
  ($elevAt -lt $runner20.IndexOf('downloading')) "Postgres refuses to run as Administrator, and that must be said before the download"

# --- 21. THE DOUBLE MUST NOT BE ABLE TO CHANGE THE OPERATOR'S MACHINE ------------
# The double RUNS guest scripts on the host — that is how it emulates WSL without Windows — and its
# container stand-ins are EXPORTED BASH FUNCTIONS. A bash child imports them; a GO child calls
# exec.Command("podman"/"docker") and gets the REAL engine. And a stubbed `podman run` PRINTS
# something and reports success without doing anything. So a guest that reaches an install path can
# install FOR REAL while this suite reports "all assertions passed" — which is also indistinguishable
# from a hang. THE GATE NOW COVERS PODMAN AS WELL AS DOCKER: podman is the engine the installer
# actually provisions first (install_engine.go), so guarding only docker left the real hazard open.
Say ""
Say "[21] the wsl double cannot change the operator's machine"
$doublePath = Join-Path $StubBin "wsl"
function Invoke-DoubleExit {
  param([string]$Guest)
  $o = Join-Path $Work ("dbl-" + [guid]::NewGuid().ToString("N"))
  $inFile = "$o.in"; $outFile = "$o.out"; $errFile = "$o.err"
  [System.IO.File]::WriteAllText($inFile, "")
  # THE GUEST IS QUOTED, and that is not cosmetic. `Start-Process -ArgumentList` joins its array and
  # RE-SPLITS on whitespace, so an unquoted multi-word guest arrives as several arguments and the
  # double's `guest="${!#}"` sees only the LAST WORD — measured: "orchicon install" arrived as
  # `install`, slipped the gate, and the real binary actually STARTED a plane here.
  $q = '"' + $Guest + '"'
  $p = Start-Process -FilePath $doublePath -PassThru `
    -ArgumentList @("-e", "bash", "-lc", $q) `
    -RedirectStandardInput $inFile -RedirectStandardOutput $outFile -RedirectStandardError $errFile
  [void](Wait-ProcessBounded -Process $p -What "the double under test")
  return $p.ExitCode
}
# EVERY DANGEROUS CASE IS MADE HARMLESS, so a regression here cannot execute the product: the binary
# is reached through a DEAD PATH (so a lookup fails) and the absolute form names a path that does not
# exist. A test that asserts "this must not run" must not be able to run it in order to find out.
foreach ($dangerous in @("PATH=/nonexistent orchicon install", "/nonexistent/bin/orchicon serve",
                         "PATH=/nonexistent orch up",
                         "docker run -d --name probe alpine true", "docker pull alpine:latest",
                         "docker volume rm orchicon-cnt-dev-data",
                         "podman run -d --name probe alpine true", "podman pull alpine:latest",
                         "podman machine init", "podman machine start", "podman volume rm probe")) {
  $rc = Invoke-DoubleExit -Guest $dangerous
  $detail = "exit=$rc (90 = refused; anything else means a guest could mutate the operator's machine)"
  Check "the double REFUSES '$dangerous'" ($rc -eq 90) $detail
}
foreach ($benign in @("echo hello", "cat /var/lib/orchicon/orchicon-prod/ports.json",
                      "mktemp -d", "docker version --format {{.Server.Version}}",
                      "command -v podman", "podman --version")) {
  $rc = Invoke-DoubleExit -Guest $benign
  Check "the double still RUNS '$benign'" ($rc -ne 90) "exit=$rc — the gate must catch command words, not paths that contain the product's name"
}
$runShSrc = Get-Content -Raw (Join-Path (Split-Path $PSCommandPath -Parent) "run.sh")
Check "the gate is present in the double (a wiring property)" ($runShSrc -match 'SAFETY GATE' -and $runShSrc -match 'exit 90') `
  "without it a guest reaching an install path installs for real, silently"
Check "and the deny-list covers podman, the engine the install provisions first" `
  (($runShSrc -match '\*"podman run"\*') -and ($runShSrc -match '\*"podman machine init"\*')) `
  "guarding only docker left the real hazard open"


# --- 22. A PLAIN INSTALL NEVER TOUCHES WSL ----------------------------------
# Criterion 1 in an assertion: on a clean Windows 11 box with no WSL and no Docker, the installer
# installs the NATIVE binaries and brings up a plane. This host cannot boot Windows, so the proxy is
# BEHAVIOURAL and exact: run the whole install path against the archive double with the wsl double's
# call log switched on, and require that wsl was never invoked. A "does it still work" test that
# quietly called `wsl --list` first would fail here.
Say ""
Say "[22] the native path never runs wsl"

$native = Invoke-InstallRun
Say ("    exit code: " + (($native.Text -split "`n" | Where-Object { $_ -match '^EXITCODE=' }) -join ' '))
Check "the install reaches the setup step" ($native.Text -match 'Install complete') ("got: " + (($native.Text -split "`n" | Where-Object { $_ -match 'installed|Install complete|not installed' }) -join ' / '))
Check "it installed orchicon.exe and orch.exe into the install dir" `
  ((Test-Path (Join-Path $native.InstallDir "orchicon.exe")) -and (Test-Path (Join-Path $native.InstallDir "orch.exe"))) `
  ("contents: " + ((Get-ChildItem $native.InstallDir -ErrorAction SilentlyContinue | ForEach-Object Name) -join ', '))
Check "it extracted the release archive RECURSIVELY (the archive wraps a directory)" `
  ((Test-Path (Join-Path $native.InstallDir "orchicon.exe")) -and ($native.Text -notmatch 'did not contain orchicon.exe')) `
  "a flat lookup misses every real release"
$wslCalls = if (Test-Path $native.CallLog) { (Get-Content -Raw $native.CallLog).Trim() } else { "" }
Check "wsl was NEVER invoked on the native path" (-not $wslCalls) ("wsl was called with: " + $wslCalls) 
Check "and the installer says no WSL is needed for a plain install" (-not ($native.Text -match 'WSL is not installed')) `
  "a plain install must not even mention a substrate it does not use"
# THE OTHER DIRECTION: -Containers DOES reach the substrate, so the previous assertion is about the
# opt-in rather than about wsl being unreachable from this file.
$containers = Invoke-InstallRun -Extra @{ ORCHICON_INSTALL_CONTAINERS = "1" }
$contCalls = if (Test-Path $containers.CallLog) { (Get-Content -Raw $containers.CallLog).Trim() } else { "" }
Check "-Containers DOES provision the substrate (the logic was reused, not deleted)" ($contCalls -match '--list') ("wsl calls: " + (($contCalls -split "`n") -join ' | '))
Check "and it reports the substrate it found" ($containers.Text -match 'WSL2 substrate ready|using WSL distro') "the opt-in has to say something about what it did"

# --- 23. AN ELEVATED SHELL GETS THE ACTIONABLE MESSAGE ----------------------
# Postgres refuses to run as Administrator. The install would otherwise acquire host binaries, start a
# runtime daemon, and fail minutes later in initdb with a message that reads like a Postgres defect.
#
# THE STUB IS INJECTED BY MUTATING A COPY, deliberately: Test-Elevated calls a Windows API that does
# not exist on this host, so there is no honest way to make it return $true in place — and a test seam
# in the shipped script for the benefit of the test would be a worse trade than a mutated copy.
Say ""
Say "[23] an elevated shell is refused with the Postgres-as-Administrator message"
$elevCopy = Join-Path $Work "install-elevated.ps1"
$elevSrc = [System.IO.File]::ReadAllText($Installer).Replace("function Test-Elevated {", "function Test-Elevated {`n    return `$true")
[System.IO.File]::WriteAllText($elevCopy, $elevSrc)
Check "the elevation stub applied to the copy" ($elevSrc -ne [System.IO.File]::ReadAllText($Installer)) "a mutation that did not apply would test the unmutated installer"
$elevRoot = Join-Path $Work ("elev-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path (Join-Path $elevRoot "home") | Out-Null
$elevRun = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64"; HOME = (Join-Path $elevRoot "home"); ORCHICON_INSTALL_LOG = "off"; ORCHICON_INSTANCE = "" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$($elevCopy.Replace("'", "''"))'))) -Version v0.0.0 -Instance dev -InstallDir '$(Join-Path $elevRoot 'bin')'; 'EXITCODE=' + `$global:LASTEXITCODE; '$marker'"
Check "it refuses, naming Administrator and Postgres" `
  (($elevRun -match 'Administrator') -and ($elevRun -match 'Postgres')) `
  ("got: " + (($elevRun -split "`n" | Where-Object { $_ -match 'Administrator|Postgres|elevated' }) -join ' / '))
Check "it says what to do instead (a normal PowerShell)" ($elevRun -match 'NORMAL PowerShell|not .Run as administrator') "a refusal without a next step is not actionable"
Check "it halts BEFORE downloading or installing anything" `
  (-not ($elevRun -match 'downloading|extracting and installing')) "the whole point is that nothing is half-done"
Check "the session survives and the outcome is non-zero" (($elevRun -match $marker) -and ($elevRun -match 'EXITCODE=1')) `
  (($elevRun -split "`n" | Where-Object { $_ -match 'EXITCODE|RETURNED' }) -join ' / ')

# THE OTHER DIRECTION, and it is a real case rather than symmetry: an elevated prompt is where
# people habitually remove software. Postgres refuses to run as Administrator, but -Uninstall
# STARTS nothing — it stops what runs and deletes two files — so a blanket refusal would answer
# "remove Orchicon" with a message about a program that never runs. The install dir here holds
# no binaries, so the verb reports "nothing to remove" and touches nothing.
$elevUninstall = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64"; HOME = (Join-Path $elevRoot "home"); ORCHICON_INSTALL_LOG = "off"; ORCHICON_INSTANCE = "" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$($elevCopy.Replace("'", "''"))'))) -Uninstall -Instance dev -InstallDir '$(Join-Path $elevRoot 'bin')'; 'EXITCODE=' + `$global:LASTEXITCODE"
Check "an elevated -Uninstall is NOT refused — the guard is about starting Postgres, not removing it" `
  (-not ($elevUninstall -match 'running as Administrator')) `
  ("got: " + (($elevUninstall -split "`n" | Where-Object { $_ -match 'Administrator|Uninstalled|Data preserved|nothing to remove' }) -join ' / '))

# --- 24. THE SECOND INSTALL ASKS NOTHING, AND THE PORTS ARE THE RECORD'S -----
# Ports are RECORD-FIRST now (install_ports.go resolveInstallPorts): an instance that already has a
# ports.json keeps those ports silently, and a genuine collision names the other instance or the
# foreign holder. The wrapper's job is to print what was RECORDED — the documented bug was "the
# operator answered 3001 for a port, this installer printed 3001, and then install.ps1 re-printed a
# hardcoded 3002", which is now fixed at the source, so the re-print must read the record.
#
# The record here deliberately differs from EVERY per-instance default (dev 8080, prod 8091, test
# 8092), so a survivor of the old fallback cannot pass by coincidence.
Say ""
Say "[24] the ports printed are the ones the install recorded"

$second = Invoke-InstallRun
# THE RECORD IS READ BY THE INSTALLER FROM THE INSTANCE STATE DIR, and what it PRINTS must come from
# there rather than from a constant. The read itself is asserted in [16] (Get-ResolvedPorts against a
# scratch record); here the point is the NEGATIVE half of C2, which needs no record at all: with no
# record this instance has nothing recorded, and the installer must print NO port number rather than
# falling back to a per-instance default. THAT fallback was the reported bug (operator answered 3001,
# the installer re-printed 3002).
Check "with no record the installer prints NO port number" `
  (($second.Text -match 'no port record yet') -and (-not ($second.Text -match 'localhost:8080|localhost:8091|localhost:8092|localhost:3002'))) `
  ("printed: " + (($second.Text -split "`n" | Where-Object { $_ -match 'localhost|Control plane|no port record' }) -join ' / '))
Check "NOT a hardcoded per-instance default" `
  (-not ($second.Text -match 'localhost:8080|localhost:8091|localhost:8092|localhost:3002')) `
  "re-printing a constant was the reported defect: one run, two contradicting numbers"
Check "a second install for the same instance is not treated as a failure" ($second.Text -match 'Install complete') "reusing the record is correct behaviour, not a broken prompt"
# THE INTERACTIVE CLAIM, OBSERVED RATHER THAN GREPPED: the double records the environment it was
# handed, which is the only way to see what the child really received.
$envOfSetup = if (Test-Path $second.EnvLog) { Get-Content -Raw $second.EnvLog } else { "" }
Check "the interactive install handed the setup ORCHICON_INSTALL_INTERACTIVE=1" `
  ($envOfSetup -match 'INSTALL_INTERACTIVE=1') `
  ("the setup saw: " + (($envOfSetup -split "`n" | Where-Object { $_ -match 'INSTALL_INTERACTIVE|ASSUME' }) -join ' / '))
Check "and it did NOT also claim to assume yes" (-not ($envOfSetup -match 'ASSUME_YES=1')) `
  "the two flags answer different questions"
$unattended = Invoke-InstallRun -Extra @{ __ARGS__ = "-Yes" }
$envUnattended = if (Test-Path $unattended.EnvLog) { Get-Content -Raw $unattended.EnvLog } else { "" }
Check "an UNATTENDED install (-Yes) does NOT claim a human is present" `
  (-not ($envUnattended -match 'INSTALL_INTERACTIVE=1')) `
  ("the setup saw: " + (($envUnattended -split "`n" | Where-Object { $_ -match 'INSTALL_INTERACTIVE' }) -join ' / '))
Check "and it answers the prompts with ORCHICON_ASSUME_YES instead" ($envUnattended -match 'ASSUME_YES=1') `
  "otherwise a -Yes run sits at the runtime-image prompt with nothing to answer it"
Check "the instance name reaches the setup" ($envUnattended -match 'INSTANCE=dev') "the setup must act on the instance the operator chose"

Say ""
Say "[25] a PUBLISHED MSI is actually used (the arm that shipped broken)"
# THE ARM HAD NEVER RUN UNDER TEST, and it crashed on its first real Windows run:
#   Cannot bind argument to parameter 'Path' because it is null
#   At line:1179  $msiPath = Join-Path $tmpdir $msiAsset
# — the scratch directory was created BELOW the branch that used it, and PowerShell reads an
# unassigned variable as $null without complaint.
#
# EVERY OTHER CASE HERE FORCES THE ZIP. They all pass ORCHICON_INSTALL_ARCHIVE (the offline
# seam), and that seam sets $useMsi = false — so the MSI preference, which is the DEFAULT path
# for a real Windows install, was exercised by nobody. The arm is also behind an availability
# probe, so reaching it needs an HTTP authority that answers HEAD 200; run.sh serves one.
#
# WHAT THIS ASSERTS, and why each part is necessary:
#   * the MSI arm is TAKEN  — otherwise this is another zip test wearing an MSI label;
#   * it reaches the LAUNCH  — which happens only after the download and after every
#     `Join-Path $tmpdir ...` in the argument construction, i.e. after the lines that crashed;
#   * no unassigned-variable crash — the reported defect, named so a regression reads as itself;
#   * the session survives   — a halt that closes the operator's window is its own reported bug.
# It does NOT install: there is no msiexec here, and the installer must say so rather than
# claim the installer is defective.
if ($env:ORCHICON_TEST_MSI_BASE_URL) {
  $msi = Invoke-InstallRun -NoArchive -Extra @{ ORCHICON_INSTALL_BASE_URL = $env:ORCHICON_TEST_MSI_BASE_URL }
  Check "the MSI arm is TAKEN when one is published" `
    ($msi.Text -match 'from orchicon_0\.0\.0_windows_amd64\.msi') `
    (($msi.Text -split "`n" | Where-Object { $_ -match 'installing Orchicon|zip \(same binaries' }) -join ' / ')
  Check "the scratch directory exists before the arm uses it (the reported crash)" `
    (-not ($msi.Text -match 'Cannot bind argument')) `
    "the crash is 'Cannot bind argument to parameter Path because it is null'"
  Check "the MSI is downloaded and its msiexec arguments built" `
    ($msi.Text -match 'running the installer') `
    "reaching this line means every Join-Path on the scratch directory succeeded"
  Check "a missing msiexec is NAMED, not reported as a defect in the installer" `
    (($msi.Text -match 'could not start msiexec') -or ($msi.Text -match 'the MSI exited')) `
    "Windows Installer can be disabled by policy, so this must say what happened"
  Check "the session survives the halt" ($msi.Text -match $marker) `
    $(if ($msi.Text -match $marker) { "$marker printed" } else { "NO MARKER — the child died" })
} else {
  Check "the MSI arm is exercised" $false `
    "ORCHICON_TEST_MSI_BASE_URL is not set. run.sh normally serves the fixture; without it the MSI preference — the DEFAULT path on Windows, and the arm that crashed in the field — is untested."
}


Say ""
Say "[26] upgrade detection (parity with the shell installer's section 12)"

# THE INSTALLER IS ALSO THE UPDATER on Windows, and the four decisions must match the POSIX
# installer's exactly — a platform that behaved differently would be its own support burden.
# These assert the REAL functions from install.ps1, dot-sourced into this scope.
$vg = Get-Command Test-VersionGreater -ErrorAction SilentlyContinue
Check "the installer defines a version comparator" ($null -ne $vg) `
  "without one the installer cannot tell an upgrade from a downgrade"
if ($vg) {
  Check "a higher version compares higher" (Test-VersionGreater "v10.0.0" "v9.0.0") "numeric, not lexicographic"
  Check "a pre-release sorts below its release" (-not (Test-VersionGreater "v0.0.1-pre" "v0.0.1")) "v0.0.1-pre < v0.0.1"
  Check "equal versions are not greater" (-not (Test-VersionGreater "v1.2.3" "v1.2.3")) "same version is not an upgrade"
  # The classic lexicographic trap: "1.10" must beat "1.9", which a string compare gets wrong.
  Check "1.10.0 beats 1.9.0 (the lexicographic trap)" (Test-VersionGreater "v1.10.0" "v1.9.0") "1.10 > 1.9"
}

# The refusal text must exist in the installer's source: a downgrade has to FAIL LOUDLY with the
# reason, not quietly move the binary backwards (migrations are forward-only).
$ps1 = Get-Content -Raw -LiteralPath $Installer
Check "the installer refuses a downgrade and names the reason" `
  (($ps1 -match 'Downgrades are not supported') -and ($ps1 -match 'migrations are forward-only')) `
  "a silent downgrade can leave a schema the older binary cannot read"

# The no-op must be a REAL no-op, and the backup must be offered rather than taken: a backup at
# every upgrade would leave a multi-GB artifact on every machine.
Check "an already-current install is a no-op that says so" ($ps1 -match 'is already installed - nothing to do') `
  "re-running the updater must not disturb a healthy install"
Check "the upgrade offers the backup command to the operator" ($ps1 -match 'orchicon db backup') `
  "the operator decides whether to keep a copy"
Check "no backup is created automatically on upgrade" (-not ($ps1 -match '&\s*\$exe\s+db\s+backup')) `
  "auto-backup would accumulate large artifacts on every host"
Check "the running plane is stopped before the binary is replaced" `
  ($ps1 -match '(?s)Upgrading Orchicon.*?Stop-InstancePlane') `
  "Windows cannot copy over an exe a process holds, so this is required for the swap to succeed"

Say ""
if ($Fail.Count -eq 0) { Say "RESULT: all assertions passed" }
else { Say ("RESULT: {0} assertion(s) FAILED: {1}" -f $Fail.Count, ($Fail -join '; ')) }
[System.IO.File]::WriteAllText($OutFile, ($Report -join "`n"), [System.Text.Encoding]::UTF8)
if ($Fail.Count -gt 0) { exit 1 }
