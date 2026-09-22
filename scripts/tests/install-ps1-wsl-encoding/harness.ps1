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

Say ""
if ($Fail.Count -eq 0) { Say "RESULT: all assertions passed" }
else { Say ("RESULT: {0} assertion(s) FAILED: {1}" -f $Fail.Count, ($Fail -join '; ')) }
[System.IO.File]::WriteAllText($OutFile, ($Report -join "`n"), [System.Text.Encoding]::UTF8)
if ($Fail.Count -gt 0) { exit 1 }
