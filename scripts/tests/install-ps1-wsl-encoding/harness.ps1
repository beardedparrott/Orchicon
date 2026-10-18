param(
  [Parameter(Mandatory=$true)][string]$Installer,
  [Parameter(Mandatory=$true)][string]$Label,
  [Parameter(Mandatory=$true)][string]$StubBin,
  [Parameter(Mandatory=$true)][string]$OutFile,
  [int]$ConsoleCodePage = 936   # a non-Unicode console codepage, as on a CJK Windows
)
# Asserts how scripts/install.ps1 handles wsl.exe output. Loads the installer's
# functions via the AST (the installer itself is NEVER executed) and runs them
# against the wsl.exe double on $StubBin. See run.sh for what is real here and
# what is emulated. Exits non-zero if any assertion fails.
$ErrorActionPreference = "Stop"
$ConsoleEncoding = [System.Text.Encoding]::GetEncoding($ConsoleCodePage)
[Console]::OutputEncoding = $ConsoleEncoding      # the host decode Windows PS 5.1 performs
$env:PATH = "${StubBin}:$env:PATH"
Remove-Item Env:WSL_UTF8 -ErrorAction SilentlyContinue
Remove-Item Env:EMU_NO_DEFAULT -ErrorAction SilentlyContinue

$Report = [System.Collections.Generic.List[string]]::new()
$Fail = [System.Collections.Generic.List[string]]::new()

# BOUNDED WAITS. Reaching a verdict by waiting for ever is not a verdict, and this harness has two
# places that used to do exactly that (`Start-Process -Wait`, and a bare `$p.WaitForExit()`).
#
# WHY IT MATTERS MORE HERE THAN ELSEWHERE: the harness's console output is WITHHELD until it finishes
# (run.sh shows it only from the report file), so a blocked child produces no output at all — the
# operator sees the banner and then silence, which is indistinguishable from a broken suite, and in CI
# it burns the runner's whole budget before anyone learns why. So every child gets a deadline, and
# expiry KILLS it and records a FAIL that names what was blocked.
#
# Two minutes is ~10x the normal child cost (these children only parse the installer's functions and run
# them against the wsl double), so it does not make a slow machine flaky. Override for a genuinely slow
# host: ORCHICON_HARNESS_CHILD_TIMEOUT_MS.
$script:ChildTimeoutMs = 120000
if ($env:ORCHICON_HARNESS_CHILD_TIMEOUT_MS) {
  $parsed = 0
  if ([int]::TryParse($env:ORCHICON_HARNESS_CHILD_TIMEOUT_MS, [ref]$parsed) -and $parsed -gt 0) {
    $script:ChildTimeoutMs = $parsed
  }
}

# Wait-ProcessBounded waits for a started process, kills it on expiry, and returns whether it exited in
# time. The tree is killed, not just the process: a pwsh child can spawn the wsl double, and an orphan
# left holding the stub directory is how a later assertion fails for an unrelated reason.
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

# --- 4. the reported symptom: the Docker check -----------------------------
Say ""
Say "[4] Docker check with the captured name (the report was 'Docker is not available inside X')"
foreach ($st in @('Ubuntu', ($names -join ''))) {
  $script:Distro = $st
  $out = Invoke-WslBash "docker version --format '{{.Server.Version}}'"
  $rc = $LASTEXITCODE
  $ok = -not ($rc -ne 0 -or -not $out)
  Check ("reaches Docker using name '{0}'" -f (CP $st)) $ok "exit=$rc"
}

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
Say "[8] `$LASTEXITCODE propagation (Ensure-WslDocker reads it)"
Check "success is 0" ($rcOk -eq 0) "exit=$rcOk"
Check "an unknown distro is 1" ($rcBad -eq 1) "exit=$rcBad"
Check "a failing script's own code propagates" ($rcSeven -eq 7) "exit=$rcSeven"

# --- 9. THE REPORTED CRASH: a failed install must not take the session --------
# The install is normally started as `irm … | iex`, which runs the installer's text
# in the operator's OWN session — so a PowerShell `exit` in it closes the window
# with the diagnosis still unread. That is the report pinned here: on a box with no
# Docker Desktop the installer "just closed the powershell session", and the only
# way to read why was to launch PowerShell from inside a command prompt (which keeps
# its console alive when the child exits).
#
# Run in a CHILD pwsh, exactly as documented, so "did the session survive?" is a
# question about a real process rather than about this harness. On the pre-fix
# installer the child dies at the Docker check and never prints the marker, which is
# what run.sh --baseline asserts this suite does.
$PwshExe = Join-Path $PSHOME $(if ($IsWindows) { "pwsh.exe" } else { "pwsh" })
if (-not (Test-Path $PwshExe)) { $PwshExe = (Get-Command pwsh).Source }
$Work = Split-Path $StubBin -Parent

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
    # `make rebuild-dev` runs this harness — Read-Host BLOCKS, and the suite hangs with no output until
    # something kills it. That is what happened: `make rebuild-dev` sat on this step indefinitely.
    #
    # Feeding the child a file (empty unless a test supplies an answer) makes Read-Host return
    # immediately, so the suite behaves identically with or without a terminal. Deterministic in both
    # contexts, which is what a test must be.
    $work = Join-Path $Work ("child-" + [guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    $inFile = Join-Path $work "in.txt"
    $outFile = Join-Path $work "out.txt"
    $errFile = Join-Path $work "err.txt"
    $scriptFile = Join-Path $work "child.ps1"
    [System.IO.File]::WriteAllText($inFile, $Stdin)
    # A fresh session's own preference, so what the child does is the installer's
    # doing and not a harness setting leaking in.
    #
    # THE COMMAND IS WRITTEN TO A FILE AND RUN WITH -File, not passed as -Command. `Start-Process
    # -ArgumentList` re-quotes the argument for the child, and the embedded double quotes come out of it
    # MANGLED — measured: an inner `WriteLine("x")` reached the child as `WriteLine(x)`, a parse error. A
    # file carries the text exactly, and as a bonus it is inspectable while debugging a failing run.
    [System.IO.File]::WriteAllText($scriptFile, "`$ErrorActionPreference = 'Continue'" + [Environment]::NewLine + $Command)
    $proc = Start-Process -FilePath $PwshExe -ArgumentList @("-NoProfile", "-File", $scriptFile) `
      -RedirectStandardInput $inFile -RedirectStandardOutput $outFile -RedirectStandardError $errFile `
      -PassThru
    [void](Wait-ProcessBounded -Process $proc -What "a child installer run")
    # BOTH STREAMS, concatenated: the harness asserts about stderr text too ("no raw exception is dumped"),
    # and the previous shape merged them with 2>&1.
    $text = ""
    if (Test-Path $outFile) { $text += [System.IO.File]::ReadAllText($outFile) }
    if (Test-Path $errFile) { $text += [System.IO.File]::ReadAllText($errFile) }
    return $text
  } finally {
    foreach ($k in $prev.Keys) { [Environment]::SetEnvironmentVariable($k, $prev[$k]) }
  }
}
$quoted = $Installer.Replace("'", "''")
$marker = "INSTALLER-RETURNED-TO-PROMPT"

Say ""
Say "[9] a failed install returns to the prompt (the reported crash)"
$noDocker = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64"; EMU_NO_DOCKER = "1"; ORCHICON_INSTALL_LOG = "off" } `
  -Command "iex (Get-Content -Raw '$quoted'); '$marker'"
Check "the session survives the halt (the iex'd installer returned)" ($noDocker -match $marker) $(if ($noDocker -match $marker) { "$marker printed" } else { "NO MARKER — the child died, as the pre-fix installer did" })
Check "the CAUSE is named: no docker CLI in the distro" ($noDocker -match "no docker CLI at all") "the report must not be a bare list of steps"
Check "the next steps are printed" ($noDocker -match [regex]::Escape("irm https://orchicon.dev/install.ps1 | iex"))
Check "no raw exception is dumped at the operator" ($noDocker -notmatch "OperationStopped|Cannot bind argument|At line:")
Check "a halt does not claim success" ($noDocker -notmatch "Install complete")

$noDaemon = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64"; EMU_NO_DAEMON = "1"; ORCHICON_INSTALL_LOG = "off" } `
  -Command "iex (Get-Content -Raw '$quoted'); '$marker'"
Check "a CLI with an unreachable daemon also returns to the prompt" ($noDaemon -match $marker)
Check "that case quotes the distro's error, not the wrong cause" (($noDaemon -match "Cannot connect to the Docker daemon") -and ($noDaemon -notmatch "no docker CLI at all"))

$help = Invoke-ChildInstaller -Env @{ ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Help; '$marker'"
Check "-Help returns to the prompt (it closed the window too)" ($help -match $marker)
Check "-Help still prints the usage" ($help -match "Usage: install.ps1")

$dry = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Version v0.0.0 -Instance dev -DryRun; '$marker'"
Check "-DryRun returns to the prompt" ($dry -match $marker)
Check "-DryRun still prints its plan" ($dry -match "dry-run complete")
Check "-DryRun starts no transcript (it changes nothing)" ($dry -notmatch "full log of this install")

$logPath = Join-Path $Work "transcript.log"
$logged = Invoke-ChildInstaller -Env @{ PROCESSOR_ARCHITECTURE = "AMD64"; EMU_NO_DOCKER = "1"; ORCHICON_INSTALL_LOG = $logPath } `
  -Command "iex (Get-Content -Raw '$quoted'); '$marker'"
$logText = if (Test-Path $logPath) { Get-Content -Raw $logPath } else { "" }
Check "the run leaves a transcript where it was asked to" ((Test-Path $logPath) -and ($logText -match "Orchicon was not installed")) "the log is the evidence a closed window used to take with it"
Check "the transcript's path is printed on the way past" ($logged -match [regex]::Escape($logPath))

# --- 10. THE OTHER REPORTED FAILURE: the long step must STREAM ---------------
# The setup step is Invoke-WslBashLive, and this is the difference it makes: watch a
# CHILD from here and look for the guest's first line WHILE THE CHILD IS STILL
# RUNNING. Capture-and-replay cannot pass that, so the same guest script is also run
# through Invoke-WslBash (still right where we PARSE the output) and required to FAIL
# it — which is what makes this a detector rather than a coincidence.
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
  # Its stdin is PINNED for the same reason the installer child's is: a child that inherits a terminal
  # and then reads it blocks for ever, and the operator sees silence. This driver only runs the wsl
  # double, which reads the guest payload off stdin — so it is fed an empty file and never the terminal.
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
  # The child has exited, but its redirected output is copied by a helper, so a read
  # taken the instant WaitForExit returns can miss the last lines. Wait for the
  # driver's own final line rather than sleeping a guessed amount.
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
Say "[10] the long/interactive step streams (the 'hanging with no progress' report)"
$liveRes = Measure-Streaming -Mode "live"
Check "the live path shows the guest's first line WHILE it is still running" $liveRes.Early ("early=" + $liveRes.Early)
Check "the live path shows the guest's output in full" (($liveRes.Text -match "LATE-MARKER") -and ($liveRes.Text -match "LIVE-RETURNED"))
$capRes = Measure-Streaming -Mode "capture"
Check "the capturing path buffers — which is why the setup step must not use it" ($capRes.Early -eq $false) ("early=" + $capRes.Early)
Check "the capturing path did run the guest, and captured both its lines" (($capRes.Text -match "captured-lines=2") -and ($capRes.Text -match "CAPTURE-RETURNED")) "the output is held until the end — which is why nothing was visible"

# --- 11. The invariants, on the AST, so a later edit cannot quietly undo them -
Say ""
Say "[11] the invariants this fix rests on"
$exits = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.ExitStatementAst] }, $true)
Check 'no PowerShell `exit` anywhere (a bash one inside a here-string is fine)' ($exits.Count -eq 0) ("found at line(s): " + (($exits | ForEach-Object { $_.Extent.StartLineNumber }) -join ", "))
$liveCalls = @($ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] -and $n.GetCommandName() -eq "Invoke-WslBashLive" }, $true))
Check "exactly the two long/interactive steps stream (extract + setup)" ($liveCalls.Count -eq 2) "call sites: $($liveCalls.Count)"
$src = Get-Content -Raw $Installer
Check "the halt sentinel is still a throw/catch PAIR" ((($src -split "OrchiconInstallHalt").Count - 1) -eq 2) "occurrences: $(($src -split 'OrchiconInstallHalt').Count - 1)"

# --- 12. THE SCRIPT MUST REACH THE GUEST AS WRITTEN -------------------------
# The install and uninstall scripts are multi-line bash WITH DOUBLE QUOTES, and Windows PowerShell 5.1
# does not escape embedded quotes when it builds a native command line — so wsl.exe ate every quote as an
# argument delimiter and the guest ran a DIFFERENT PROGRAM. Measured on a real Windows box, a quoted
# `mktemp` assignment arrived as a bare `-d)` and the install died with "unexpected EOF while looking for
# matching )" — while the Docker check (single-quoted, so quote-free) kept working. That asymmetry sent
# two rounds of diagnosis down the wrong path (capture-vs-live, then splat-vs-inline); the real variable
# is whether the script contains a quote. The transport is now base64, whose alphabet has no quote.
#
# TWO ASSERTIONS, and they cover what can and cannot be seen from this host:
#   * THE CONTRACT — the script the guest RECEIVES is byte-identical to what the caller wrote. This is a
#     real round trip through the encoding, decodable here, so it is a genuine behavioural test.
#   * THE HAZARD IS ABSENT — the command handed to wsl contains no double quote at all. If someone
#     "simplifies" the transport back to an inline script, this fails even though the round trip above
#     would still pass (pwsh 7 on Linux delivers both forms intact — measured; only 5.1 mangles).
Say ""
Say "[12] the script reaches the guest as written"

$record = Join-Path $Work "received-script.txt"
$prevRecord = $env:EMU_RECORD
$env:EMU_RECORD = $record
try {
  # Quotes, command substitution, a quoted pipeline, a SIGKILL builtin, an apostrophe — the shapes that
  # the real scripts use and that a quote-eating transport destroys.
  $original = @'
set -euo pipefail
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="$(find "$TMP" -type f -name orchicon -perm -u+x 2>/dev/null | head -1)"
echo "found: $BIN" | grep -q . || true
'@
  $script:Distro = "Ubuntu"
  Invoke-WslBashLive $original | Out-Null
} finally {
  if ($null -eq $prevRecord) { Remove-Item Env:EMU_RECORD -ErrorAction SilentlyContinue }
  else { $env:EMU_RECORD = $prevRecord }
}
# LF-normalised on the way in, so the expected text is the original with CRLF folded away.
$expected = $original -replace "`r`n", "`n"
$received = if (Test-Path $record) { [System.IO.File]::ReadAllText($record) } else { "" }
Check "the guest receives the script BYTE-IDENTICAL to what was written" `
  ([string]::Equals($received, $expected, [System.StringComparison]::Ordinal)) `
  ("received " + $received.Length + " chars, expected " + $expected.Length +
   "; first diff at " + $(if ($received -eq $expected) { "none" } else {
     $n = 0; while ($n -lt [Math]::Min($received.Length, $expected.Length) -and $received[$n] -eq $expected[$n]) { $n++ }
     "index $n" }))
Check "its double quotes survive (the exact thing 5.1 stripped)" `
  ($received -match [regex]::Escape('TMP="$(mktemp -d)"'))

# The hazard check: what is handed to wsl must contain no double quote, wherever the script came from.
$liveCode = (($funcs | Where-Object Name -eq "Invoke-WslBashLive").Extent.Text -split "`n" |
  Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
$capCode = (($funcs | Where-Object Name -eq "Invoke-WslBash").Extent.Text -split "`n" |
  Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
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
# dev / test / prod are three INSTANCES on one host, and the container names used to be spelled out as
# `orchicon-cnt-dev orchicon-cnt-prod` — so -Uninstall stopped BOTH stacks and -ForceClean destroyed BOTH
# data volumes, while the help text claimed the flags were about dev. On a host with prod data that is a
# data-loss bug, and an operator testing a canary would have been the one to find it.
#
# Every assertion CLEARS ORCHICON_INSTANCE first: the variable is inherited from whatever plane is running
# the session (this harness was written inside the prod plane's environment, where it is `prod`), so
# without clearing, "the default" would be testing the ambient environment rather than the default.
Say ""
Say "[13] -Instance scoping"

$scoped = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -Instance test -DryRun"
Check "-Instance test names ONLY the test container" `
  (($scoped -match 'orchicon-cnt-test') -and ($scoped -notmatch 'orchicon-cnt-prod') -and ($scoped -notmatch 'orchicon-cnt-dev')) `
  "an unscoped cleanup would touch dev and prod too"

# NOT `dev` ANY MORE, and that is deliberate: the verb ASKS for the instance, and with no terminal it
# refuses rather than picking one. Inferring `dev` was how an operator could clean the wrong stack.
$def = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -DryRun"
Check "with no flag, no environment and no terminal it REFUSES" `
  (($def -match 'no instance chosen') -and ($def -notmatch 'orchicon-cnt-dev')) `
  ("got: " + (($def -split "`n" | Where-Object { $_ -match 'instance|orchicon-cnt' }) -join ' / '))
$explicit = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -Instance dev -DryRun"
Check "and -Instance dev is still honoured without any prompt" ($explicit -match 'orchicon-cnt-dev')`
  ("got: " + (($explicit -split "`n" | Where-Object { $_ -match 'orchicon-cnt' }) -join ' / '))

$fromEnv = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = "test"; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -DryRun"
Check "the environment is still honoured when no flag is given" ($fromEnv -match 'orchicon-cnt-test')

$bad = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Uninstall -Instance production -DryRun; '$marker'"
Check "an unknown instance is REFUSED, naming the known set" `
  ($bad -match "unknown instance: 'production'" -and $bad -match 'dev, test, prod') `
  "defaulting to dev would clean up the wrong stack while the operator watched prod"
Check "and it returns to the prompt rather than acting" ($bad -match $marker)

$help2 = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Help; '$marker'"
Check "-Help documents -Instance" ($help2 -match '-Instance <name>')
Check "and -Help mentions dev, test and prod" `
  (($help2 -match 'dev \(default\)') -and ($help2 -match 'test') -and ($help2 -match 'prod'))

# --- 14. UNINSTALL MUST ACTUALLY STOP THINGS, AND MUST NOT TAKE DATA UNASKED -------
# The operator ran -Uninstall on Windows and found "the web server is still running broken": the binary
# was gone and the plane was not. The cause was the trap the SHELL installer already documents and fixes —
# `serve --stop` resolves its PID file from ORCHICON_SERVE_STATE_DIR, so a bare call looks in the
# CWD-relative default, MISSES a host-resident plane, reports success, and then deletes the binary out
# from under it. The container was only ever `docker stop`ped, so it survived an "uninstall" too.
#
# These are SOURCE assertions because the uninstall is a script the installer BUILDS and hands to wsl
# (not a function this harness can call), so the property is in the generated text — the same reason the
# other wiring checks in this repo read source.
Say ""
Say "[14] uninstall stops what it says it stops"

$ps1Src = Get-Content -Raw $Installer
Check "the uninstall script passes the instance's state dir to serve --stop" `
  ($ps1Src -match 'ORCHICON_SERVE_STATE_DIR=\"\$STATE_DIR\" \"\$BIN\" serve --stop') `
  "a bare serve --stop misses the host-resident plane and leaves a web server running with no binary"
Check "and it derives that state dir from the INSTANCE, not a fixed path" `
  ($ps1Src -match 'orchicon-\$\{INSTANCE\}/serve') `
  "the state dir is per instance; a fixed one would stop the wrong plane"
Check "it REMOVES the container rather than only stopping it" `
  ($ps1Src -match 'docker rm -f \"\$CONTAINER\"') `
  "docker stop left the services stack running after an uninstall"
Check "data is preserved unless -Purge is given" `
  ($ps1Src -match 'Re-run with -Purge to remove both' -and $ps1Src -match '\$PURGE\" = \"1\"') `
  "destroying a database is not something an uninstall may infer from 'remove the software'"
# SCOPED TO THE UNINSTALL SCRIPT: there is a second `rm -f "$BIN"` in the remove-binary script, and an
# unscoped IndexOf was comparing against that one instead — an assertion about the wrong text.
# `$uninstallScript` MUST BE ESCAPED: unescaped it interpolates to empty in this scope (the harness never
# runs the installer), so the search string became " = @'" and matched the remove-binary script instead —
# an assertion about the wrong block, which is how it reported a failure that was not there.
$a1 = $ps1Src.IndexOf("`$uninstallScript = @'")
$a2 = $ps1Src.IndexOf("'@", $a1)
$uninstallOnly = $ps1Src.Substring($a1, $a2 - $a1)
Check "the binary is removed LAST, after the plane that runs from it is stopped" `
  ($uninstallOnly.IndexOf('ORCHICON_SERVE_STATE_DIR="$STATE_DIR"') -lt $uninstallOnly.IndexOf('rm -f "$BIN"')) `
  "deleting the executable first is what produced the broken server"

$purgeHelp = Invoke-ChildInstaller -Env @{ ORCHICON_INSTANCE = ""; ORCHICON_INSTALL_LOG = "off" } `
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Help; '$marker'"
Check "-Help documents -Purge" ($purgeHelp -match '-Purge')
Check "and says the uninstall preserves data" ($purgeHelp -match 'DATA IS PRESERVED')

# --- 15. THE WINDOWS UNINSTALL VERBS ------------------------------------------
# The posix side of this is behaviourally tested (scripts/tests/install-path/uninstall.sh, with docker
# stubbed so nothing real can be touched). Windows cannot be: the uninstall runs INSIDE WSL, and this host
# has none. So these are SOURCE assertions about the script the installer BUILDS — the same approach the
# other wiring checks here take, and honest about what it does and does not prove.
Say ""
Say "[15] -Uninstall / -RemoveBinary / -Purge"

$ps1 = Get-Content -Raw $Installer
$ps1Code = (($ps1 -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"

Check "the instance is ASKED FOR, not inferred" ($ps1Code -match 'function Ask-Instance') `
  "inferring it from the environment with a dev default is how the wrong stack gets cleaned"
Check "and both destructive verbs call it" `
  (([regex]::Matches($ps1Code, 'Ask-Instance\s*$', 'Multiline')).Count -ge 2) `
  "-Uninstall and -RemoveBinary must each ask"
# `switch ($null)` fires NO branch in PowerShell — not even `default` — so a missing answer needs an
# explicit test or it silently keeps the instance already set (measured: it acted on dev with no answer).
Check "a missing answer REFUSES (no reliance on switch default)" `
  ($ps1Code -match 'IsNullOrWhiteSpace\(\$answer\)') `
  "switch (`$null) fires no branch at all, so a default clause cannot catch a missing answer"
Check "whether -Instance was given is recorded at SCRIPT scope" `
  ($ps1Code -match '\$script:InstanceGiven = \$PSBoundParameters\.ContainsKey') `
  "inside a function `$PSBoundParameters describes the FUNCTION, so the check there never fired"
Check "the remove-binary script gates the stop on CONSENT" `
  ($ps1Code -match 'CONSENT="1"' -and $ps1Code -match 'nothing stopped, nothing removed') `
  "removing a binary a plane runs from is what leaves a server answering with no binary"
Check "and gates the removal itself on DRYRUN" `
  ($ps1Code -match 'DRYRUN="1"') `
  "a dry run that removes something is worse than no dry run"
Check "-Help documents -RemoveBinary" ($ps1 -match '-RemoveBinary')
Check "-Help documents -Purge" ($ps1 -match '-Purge')
Check "the uninstall summary distinguishes preserved from destroyed data" `
  (($ps1 -match 'DATA PRESERVED') -and ($ps1 -match 'volume and host state removed')) `
  "an uninstall must not be ambiguous about whether the database survived"

# --- 16. THE CONNECTION INFO READS THE BOUND PORTS, IT DOES NOT GUESS --------
# Reported from a real Windows install: the operator answered 3001 for a published port, `orchicon install`
# printed 3001, and then Write-ConnectionInfo printed 3002 — one run, two contradicting numbers, the second
# being a port nothing had bound. A PROMPT cannot be predicted by a wrapper, so the installer now RECORDS
# what it bound and this reads it back. Behavioural where it can be: the function is extracted from the
# installer (this harness already Invoke-Expressions every function from it) and called against a scratch
# record.
#
# THE RECORD STILL CARRIES A RETIRED KEY ON PURPOSE. 3001 IS ORCHICON_GRAFANA_PORT: Grafana and its port
# were removed from the plane, and an EXISTING ports.json keeps that key because nothing rewrites the file.
# No code reads it any more, so the checks below assert the CONTROL port is read back and that the retired
# Grafana URL is NOT printed — a stale key must stay inert rather than resurrect a component.
Say ""
Say "[16] the Windows connection info reads the ports that were bound"

$inst = Join-Path $Work "inststate"
New-Item -ItemType Directory -Force -Path (Join-Path $inst "orchicon-test") | Out-Null
[System.IO.File]::WriteAllText((Join-Path $inst "orchicon-test/ports.json"),
  '{"ORCHICON_CONTROL_PORT":8092,"ORCHICON_GRAFANA_PORT":3001}')

$prevXdg = $env:XDG_DATA_HOME
$prevInstance = $script:Instance
$prevDistro = $script:Distro
$env:XDG_DATA_HOME = $inst
$script:Distro = "Ubuntu"
$script:Instance = "test"
try {
  $resolved = Get-ResolvedPorts
  Check "the record is read back" ($null -ne $resolved) "the wrapper must read, not re-derive"
  Check "and it carries the port the operator actually chose" `
    ($resolved -and $resolved.ORCHICON_CONTROL_PORT -eq 8092) `
    ("got control=" + $(if ($resolved) { $resolved.ORCHICON_CONTROL_PORT } else { "<none>" }))

  # What the operator actually SEES, which is the thing that was wrong.
  $sb = [System.Text.StringBuilder]::new()
  function Write-Host { param([Parameter(ValueFromRemainingArguments=$true)]$Object) [void]$sb.AppendLine(($Object -join " ")) }
  Write-ConnectionInfo
  $shown = $sb.ToString()
  Check "and the printed control URL is that port, not the default" ($shown -match "localhost:8092") `
    ("printed: " + (($shown -split "`n" | Where-Object { $_ -match 'Control plane' }) -join ' / '))
  Check "the retired Grafana URL is NOT printed, even from a pre-removal record" `
    (-not ($shown -match "localhost:3001")) `
    ("printed: " + (($shown -split "`n" | Where-Object { $_ -match 'localhost:3001' }) -join ' / '))

  # The fallback still exists for an install from before the record did, and must not be the ONLY source.
  Remove-Item (Join-Path $inst "orchicon-test/ports.json") -Force
  Check "with no record it falls back to the per-instance default" ($null -eq (Get-ResolvedPorts))
} finally {
  $env:XDG_DATA_HOME = $prevXdg
  $script:Instance = $prevInstance
  $script:Distro = $prevDistro
  Remove-Item Function:\Write-Host -ErrorAction SilentlyContinue
}

# The wiring: Write-ConnectionInfo must consult the record, not just the defaults.
$ps1b = Get-Content -Raw $Installer
Check "Write-ConnectionInfo consults the recorded ports" `
  (($ps1b -match 'Get-ResolvedPorts') -and ($ps1b -match 'resolved\.ORCHICON_CONTROL_PORT')) `
  "the defaults are a fallback for an older install, not the source of truth"
# AND IT NO LONGER KNOWS ABOUT GRAFANA AT ALL — the port row, the prompt and the printed URL were removed
# with the component, so a stale ORCHICON_GRAFANA_PORT key in an existing record reaches no consumer.
Check "the installer names no Grafana port or URL" (-not ($ps1b -match 'GRAFANA')) `
  "a retired component must not survive in the wrapper that prints the connection info"

# --- 17. A PLAIN INSTALL ASKS FOR AN INSTANCE; THE DESTRUCTIVE VERBS REFUSE ---
# The two disagreeing was itself the bug (a bare Enter picked dev in PowerShell's sibling, the shell
# installer). Now: INSTALL passes a fallback, the destructive verbs pass none.
Say ""
Say "[17] install asks with a fallback; destructive verbs refuse"

$code = (($ps1b -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
Check "a plain install asks, with dev as the fallback" ($code -match 'Ask-Instance -Fallback "dev"') `
  "installing to the documented default is not destructive, so it may default"
Check "the destructive verbs ask with NO fallback" `
  (-not ($code -match 'Ask-Instance -Fallback' -and $code -match '\$Uninstall\) \{\s*\n\s*Ask-Instance -Fallback')) `
  "a reflexive Enter must never select a stack to destroy"
Check "and an empty answer refuses without a fallback" `
  ($code -match 'no instance chosen - pass -Instance') `
  "the refusal is what makes Enter safe for the destructive verbs"

# --- 18. THE WINDOWS SETUP NAMES THE VERSION'S IMAGE --------------------------
# The same rule as the shell installer's, and the same failure it prevents: with binaries only, a canary
# install put a canary BINARY over the last RELEASED image, so a test of "does re-installing converge the
# container?" had nothing newer to converge to.
Say ""
Say "[18] the Windows setup names the version's image"

$ps1c = Get-Content -Raw $Installer
Check "the setup script passes ORCHICON_CONTAINER_IMAGE" `
  ($ps1c -match 'ORCHICON_CONTAINER_IMAGE="__IMAGE_REF__"') `
  "without it the stack comes from whatever :latest the host already has"
Check "and the ref is built from the version, lowercased for ghcr" `
  (($ps1c -match '\$imageRepo = "ghcr.io/"') -and ($ps1c -match '\.ToLower\(\)')) `
  "ghcr rejects an uppercase repository name, and GITHUB_REPO is 'Orchicon'"
Check "the placeholder is substituted, not shipped literally" `
  ($ps1c -match "\.Replace\('__IMAGE_REF__'") `
  "a literal __IMAGE_REF__ would be an invalid image ref at install time"
# The RUNTIME images must NOT move with the version: nothing publishes orchicon-runtime:<version>, so
# tagging them would make an install fail on a ref that does not exist.
Check "the runtime images are left on their default tag" `
  (-not ($ps1c -match 'ORCHICON_RUNTIME_IMAGE="__')) `
  "only the instance image is versioned; the runtime engine is not"

# --- 19. EVERY DESTRUCTIVE FLAG IS INSTANCE-BOUND ---------------------------
# The operator: "EVERY option should be instance bound and operate as such." -Clean and -ForceClean were the
# exception: neither asked, so the verb that DESTROYS A DATA VOLUME took whatever ORCHICON_INSTANCE held (or
# `dev`). Reported after a -ForceClean went ahead on its own on a Windows host.
#
# ENUMERATED ON PURPOSE: the flag list is the assertion's input, so a new destructive flag that forgets to
# ask must be added here to be covered — which surfaces the omission in review.
Say ""
Say "[19] every destructive flag asks for an instance"

$ps1c = (($ps1Src -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
foreach ($flag in @('$RemoveBinary', '$Uninstall', '$Clean', '$ForceClean')) {
  # The flag's block, from its `if (` to the next flag's, so the assertion is about THAT verb.
  $start = $ps1c.IndexOf("if (" + $flag + ") {")
  if ($start -lt 0) { bad "$flag not found in install.ps1"; continue }
  $block = $ps1c.Substring($start, [Math]::Min(1800, $ps1c.Length - $start))
  Check "$flag asks which instance" ($block -match 'Ask-Instance') `
    "without it the verb acts on whatever instance the environment names — which is how -ForceClean could destroy a volume nobody chose"
}
# And install is the ONE flag that may default (with a fallback), because it is the only non-destructive one.
Check "install is the only flag with a fallback" `
  (($ps1c -match 'Ask-Instance -Fallback "dev"')) `
  "installing to the documented default is safe; destroying a volume on a default is not"

# --- 20. THE HARNESS MUST NOT BE ABLE TO BLOCK, AND THE PROMPT MUST COME LAST ------
# Reported: "I am rebuilding dev and it keeps hanging on this step" — and this step WAS this harness. The
# installer asks for an instance with Read-Host; run from a terminal (which is exactly how `make rebuild-dev`
# runs it) Read-Host BLOCKS, so the suite sat there with no output until something killed it. CI never saw
# it because CI has no terminal: the failure existed only for the person running the build.
#
# A HANG IS A WIRING PROPERTY, so it is asserted as one — there is nothing to observe at runtime that would
# have caught it here, since the bug is precisely that nothing happens.
Say ""
Say "[20] the harness cannot block, and the prompt comes after the prerequisites"

$harnessSrc = Get-Content -Raw $PSCommandPath
Check "the child's stdin is PINNED (so a prompt returns instead of blocking)" `
  (($harnessSrc -match 'RedirectStandardInput') -and ($harnessSrc -match '\[System.IO.File\]::WriteAllText\(\$inFile')) `
  "without this, Read-Host waits for a keystroke that a test will never type"
Check "the child command is passed by FILE, not inline" `
  ($harnessSrc -match '"-NoProfile", "-File", \$scriptFile') `
  "Start-Process mangles embedded quotes in an inline -Command (measured: WriteLine(`"x`") arrived as WriteLine(x))"

# And the installer itself must ask for an instance only once the prerequisites are known good.
$code20 = (($ps1Src -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
$dock = $code20.IndexOf("Ensure-WslDocker")
$ask = $code20.IndexOf('Ask-Instance -Fallback "dev"')
Check "the instance prompt comes AFTER the Docker check" `
  (($dock -ge 0) -and ($ask -gt $dock)) `
  "asking first makes the operator answer a question about a stack that cannot be created, and buries the real problem under the answer"

# --- 21. THE DOUBLE MUST NOT BE ABLE TO CHANGE THE OPERATOR'S MACHINE ------------
# The double RUNS guest scripts on the host — that is how it emulates WSL without Windows — and its docker
# stand-in is an EXPORTED BASH FUNCTION. A bash child imports that function; a GO child (the orchicon/orch
# binaries) calls exec.Command("docker") and gets the REAL engine. And a stubbed `docker run` PRINTS a
# version string and reports success without doing anything.
#
# So a guest that reaches an install path can install FOR REAL — pulling images and re-creating the
# operator's containers — while this suite reports "all assertions passed". That is the shape of the
# reported "prod dropped and had to be restarted". A real install also looks exactly like a hang.
#
# THESE CASES ARE BEHAVIOURAL, which is the only kind of assertion that would have caught it: the gate is
# in the double, so it is tested by calling the double. Each case states a guest and the outcome required.
Say ""
Say "[21] the wsl double cannot change the operator's machine"

$doublePath = Join-Path $StubBin "wsl"
function Invoke-DoubleExit {
  param([string]$Guest)
  $o = Join-Path $Work ("dbl-" + [guid]::NewGuid().ToString("N"))
  $inFile = "$o.in"; $outFile = "$o.out"; $errFile = "$o.err"
  [System.IO.File]::WriteAllText($inFile, "")
  # THE GUEST IS QUOTED, and that is not cosmetic. `Start-Process -ArgumentList` joins its array and
  # RE-SPLITS on whitespace, so an unquoted multi-word guest arrives as several arguments and the double's
  # `guest="${!#}"` sees only the LAST WORD — measured: "orchicon install" arrived as `install`, slipped
  # the gate, and the real `/usr/local/bin/orchicon serve` case actually STARTED a plane here for 120s
  # before the watchdog killed it. Quotes make it one argument, which is what `wsl -e bash -lc <one arg>`
  # means. (Same re-quoting hazard the -Command assertions exist for.)
  $quoted = '"' + $Guest + '"'
  $p = Start-Process -FilePath $doublePath -PassThru `
    -ArgumentList @("-e", "bash", "-lc", $quoted) `
    -RedirectStandardInput $inFile -RedirectStandardOutput $outFile -RedirectStandardError $errFile
  [void](Wait-ProcessBounded -Process $p -What "the double under test")
  return $p.ExitCode
}

# EVERY DANGEROUS CASE IS MADE HARMLESS, so a regression here cannot execute the product: the binary is
# reached through a DEAD PATH (so a lookup fails) and the absolute form names a path that does not exist.
# A test that asserts "this must not run" must not be able to run it in order to find out.
foreach ($dangerous in @("PATH=/nonexistent orchicon install", "/nonexistent/bin/orchicon serve",
                         "PATH=/nonexistent orch up",
                         "docker run -d --name probe alpine true", "docker pull alpine:latest",
                         "docker volume rm orchicon-cnt-dev-data")) {
  $rc = Invoke-DoubleExit -Guest $dangerous
  # The detail is built in a VARIABLE: a parenthesized expression cannot START a continuation line in
  # PowerShell ("Unexpected token"), which is a parse error in the whole harness, not just this case.
  $detail = "exit=$rc (90 = refused; anything else means a guest could mutate the operator's machine)"
  Check "the double REFUSES '$dangerous'" ($rc -eq 90) $detail
}

# AND THE GATE MUST NOT OVER-REACH. A substring test for `orchicon` refuses the legitimate guest that reads
# the instance's bound ports, because its path contains `orchicon-test/ports.json` — measured: it broke four
# [16] assertions. The gate is on INVOCATIONS, so a path that merely contains the name must still run.
foreach ($benign in @("echo hello", "cat /var/lib/orchicon/orchicon-prod/ports.json",
                      "mktemp -d", "docker version --format {{.Server.Version}}")) {
  $rc = Invoke-DoubleExit -Guest $benign
  $detail2 = "exit=$rc — the gate must catch command words, not paths that contain the product's name"
  Check "the double still RUNS '$benign'" ($rc -ne 90) $detail2
}

$runShSrc = Get-Content -Raw (Join-Path (Split-Path $PSCommandPath -Parent) "run.sh")
Check "the gate is present in the double (a wiring property)" `
  ($runShSrc -match 'SAFETY GATE' -and $runShSrc -match 'exit 90') `
  "without it a guest reaching an install path installs for real, silently"

Say ""
if ($Fail.Count -eq 0) { Say "RESULT: all assertions passed" }
else { Say ("RESULT: {0} assertion(s) FAILED: {1}" -f $Fail.Count, ($Fail -join '; ')) }
[System.IO.File]::WriteAllText($OutFile, ($Report -join "`n"), [System.Text.Encoding]::UTF8)
if ($Fail.Count -gt 0) { exit 1 }
