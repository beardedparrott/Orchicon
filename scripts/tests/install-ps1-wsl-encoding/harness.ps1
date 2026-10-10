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
  param([hashtable]$Env, [string]$Command)
  $prev = @{}
  foreach ($k in $Env.Keys) {
    $prev[$k] = [Environment]::GetEnvironmentVariable($k)
    [Environment]::SetEnvironmentVariable($k, $Env[$k])
  }
  try {
    # A fresh session's own preference, so what the child does is the installer's
    # doing and not a harness setting leaking in.
    $full = "`$ErrorActionPreference = 'Continue'; $Command"
    return (& $PwshExe -NoProfile -Command $full 2>&1 | Out-String)
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
  -Command "& ([scriptblock]::Create((Get-Content -Raw '$quoted'))) -Version v0.0.0 -DryRun; '$marker'"
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
  $p = Start-Process -FilePath $PwshExe -PassThru -RedirectStandardOutput $out `
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
  $p.WaitForExit()
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

Say ""
if ($Fail.Count -eq 0) { Say "RESULT: all assertions passed" }
else { Say ("RESULT: {0} assertion(s) FAILED: {1}" -f $Fail.Count, ($Fail -join '; ')) }
[System.IO.File]::WriteAllText($OutFile, ($Report -join "`n"), [System.Text.Encoding]::UTF8)
if ($Fail.Count -gt 0) { exit 1 }
