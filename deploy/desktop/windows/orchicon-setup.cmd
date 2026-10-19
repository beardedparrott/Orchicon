@echo off
REM ============================================================================
REM  orchicon-setup.cmd - the MSI's last step: hand off to the REAL installer.
REM
REM  WHY A WRAPPER AND NOT AN ExeCommand. The setup needs two answers from the
REM  wizard (which instance, which runtime images) carried into the child's
REM  ENVIRONMENT, and it must open in a VISIBLE console so its progress and its
REM  port questions can be seen and answered. Expressing that inline in WiX means
REM  nested quoting inside `cmd /c set ... && start ...` - exactly the class of
REM  quoting that once mangled every quoted script in install.ps1. A file with %1
REM  and %2 has no such failure mode.
REM
REM  IT DELEGATES AND ADDS NOTHING. No port choice, no image choice, no ordering:
REM  those belong to `orchicon install`, which is idempotent and resumable and is
REM  what the one-line ps1 install runs too. A second copy of that logic here is
REM  how the MSI and the CLI start disagreeing about what "installed" means.
REM ============================================================================
setlocal

REM %1 = instance (dev|prod|test), %2 = runtime image selection (1,3 / all / none).
REM Omitted arguments leave the variable UNSET rather than empty, so the CLI's own
REM default applies instead of a blank value it would refuse as garbage.
if not "%~1"=="" set "ORCHICON_INSTANCE=%~1"
if not "%~2"=="" set "ORCHICON_INSTALL_RUNTIME_IMAGES=%~2"

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
