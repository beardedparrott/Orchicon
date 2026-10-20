@echo off
REM ============================================================================
REM  orchicon-setup.cmd - the MSI's last step: hand off to the REAL installer.
REM
REM  WHY A WRAPPER AT ALL. The setup needs two answers from the wizard (which
REM  instance, which runtime images) carried into the child's ENVIRONMENT, and it
REM  must open in a VISIBLE console so its progress and its port questions can be
REM  seen and answered. Inline in WiX that means nested quoting inside
REM  `cmd /c start "" ... ` - the class of quoting that once mangled every quoted
REM  script in install.ps1.
REM
REM  ...AND THAT CLASS CAME BACK ANYWAY, THROUGH ARGUMENTS. The action used to pass
REM  "[INSTANCE]" and "[RUNTIMEIMAGES]" as %1/%2. RUNTIMEIMAGES has no default - it
REM  must be UNSET rather than "none", or the setup's own prompt is skipped - so the
REM  last argument became an EMPTY QUOTED STRING, and crossing MSI's command-line
REM  building, `cmd /c` and `start` ate a quote at each end. cmd was handed
REM
REM      C:\Users\...\Orchicon\orchicon-setup.cmd"  "dev" "
REM
REM  as a COMMAND NAME and answered "'...' is not recognized as an internal or
REM  external command".
REM
REM  SO THE ANSWERS NO LONGER TRAVEL THROUGH A COMMAND LINE. The MSI writes them to
REM  HKCU\Software\Orchicon\Setup (the SetupConfig component) and this script reads
REM  them back. %1/%2 are still honoured, so the script can be run by hand exactly as
REM  before, and every value is optional.
REM
REM  IT DELEGATES AND ADDS NOTHING. No port choice, no image choice, no ordering:
REM  those belong to `orchicon install`, which is idempotent and resumable and is
REM  what the one-line ps1 install runs too. A second copy of that logic here is
REM  how the MSI and the CLI start disagreeing about what "installed" means.
REM ============================================================================
setlocal

REM %1 = instance (dev|prod|test), %2 = runtime image selection (1,3 / all / none).
REM Both are optional now; the MSI passes neither.
REM Omitted values leave the variable UNSET rather than empty, so the CLI's own
REM default applies instead of a blank value it would refuse as garbage.
if not "%~1"=="" set "ORCHICON_INSTANCE=%~1"
if not "%~2"=="" set "ORCHICON_INSTALL_RUNTIME_IMAGES=%~2"

REM THE WIZARD'S ANSWERS, FROM THE REGISTRY.
set "ORCHICON_SETUP_KEY=HKCU\Software\Orchicon\Setup"
if not defined ORCHICON_INSTANCE call :from_registry Instance ORCHICON_INSTANCE
if not defined ORCHICON_INSTALL_RUNTIME_IMAGES call :from_registry RuntimeImages ORCHICON_INSTALL_RUNTIME_IMAGES
goto :after_config

:from_registry
REM %1 = value name, %2 = the variable to set. Values are REG_SZ, so the third
REM token onward is the data; an absent key or value leaves the variable UNSET,
REM which is what the CLI wants. `2^>nul` keeps a missing key quiet: this runs on
REM every install and a scary "ERROR: The system was unable to find..." line would
REM be the first thing an operator sees.
for /f "tokens=2,*" %%A in ('reg query "%ORCHICON_SETUP_KEY%" /v %~1 2^>nul ^| findstr /i "REG_SZ"') do set "%~2=%%B"
goto :eof

:after_config

REM A HUMAN IS PRESENT: the wizard is done and this console is theirs. Without
REM this the port questions never appear - the installer only prompts when it can
REM see a terminal of its own (installIsInteractive).
set "ORCHICON_INSTALL_INTERACTIVE=1"

echo.
echo   Orchicon setup - this window asks for the host ports and the optional
echo   runtime images, then starts this instance's plane.
echo   Press ENTER at any question to take the default.
echo.

"%~dp0orchicon.exe" install
set "RC=%ERRORLEVEL%"

if not "%RC%"=="0" (
  echo.
  echo   Setup did not complete ^(exit %RC%^). The binaries ARE installed.
  echo   Resume - it is idempotent and picks up where this stopped:
  echo       "%~dp0orchicon.exe" install
  echo.
)

REM Keep the console open so the output can be read: a window that vanishes on
REM completion is the classic Windows installer complaint.
echo   You can close this window.
pause
exit /b %RC%
