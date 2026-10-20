Orchicon - portable Windows build
=================================

WHAT THIS IS
  Orchicon is an AI workflow orchestrator. This zip contains the desktop app
  (orchicon-desktop.exe) without an installer. The app is a CLIENT: it talks to
  an Orchicon plane on this machine at http://127.0.0.1:8080 and does NOT start
  one. If no plane is running, it says so and names the address it tried.

  Start a plane first, from the full install (the MSI, or install.ps1):

      "%LOCALAPPDATA%\Programs\Orchicon\orchicon.exe" install   (once)
      "%LOCALAPPDATA%\Programs\Orchicon\orchicon.exe" start

  The command line is still the canonical entry point (orchicon install /
  orchicon start / orchicon status), but you do not need it to use the app once
  a plane is up.

FIRST RUN: WEBVIEW2
  Orchicon draws its window with Microsoft Edge WebView2, the same runtime
  Microsoft Edge uses. It is preinstalled on Windows 11.

  On Windows 10 it may be missing. If it is, Orchicon shows a dialog saying so
  instead of opening an empty window. To fix it, install the Evergreen
  bootstrapper (about 2 MB, no reboot):

      https://go.microsoft.com/fwlink/p/?LinkId=2124703

  The INSTALLER (orchicon_desktop_<version>_windows_amd64_installer.exe) does
  this for you; this portable zip does not, because it cannot run an installer
  as part of an unzip.

WINDOWS SMARTSCREEN
  This build is not code-signed (no certificate), so SmartScreen may say
  "Windows protected your PC". Choose "More info" then "Run anyway".

UNINSTALL
  Delete the folder. Nothing is written to the registry by the portable build;
  your data lives in the Orchicon plane's own data directory.

MORE
  https://github.com/beardedparrott/Orchicon
  USERGUIDE.md, section 3, "The desktop app".
