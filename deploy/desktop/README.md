# Desktop packaging

Everything needed to turn the Wails shell in `cmd/orchicon-desktop/` into installable artifacts
for the three platforms. The runtime webview detection logic lives in
`internal/desktoppreflight/`.

## The per-OS runner strategy (why the release needs three runners)

Each platform's gap is different, so each platform's build is different — and a
signed macOS build **cannot** be produced on `ubuntu-latest`.

| Artifact | Runner | Why that runner |
|---|---|---|
| `.deb` + `.AppImage` | `ubuntu-22.04` | The `.deb` links against `libwebkit2gtk-4.0` (Ubuntu ≤ 23.10, Debian 12). Building on 24.04 would link 4.1 and break every older user. |
| `.app` (signed, notarised, stapled) | `macos-14` | `codesign`/`notarytool`/`stapler`/`spctl` are macOS-only, and notarisation is an Apple service call. |
| NSIS installer + portable zip + **MSI** | `windows-latest` | `wails build -webview2 download -nsis` needs makensis and the Windows toolchain; the MSI needs the WiX CLI, which is a .NET global tool no other runner in this project has. **makensis is NOT on the runner image** — every job that passes `-nsis` installs it first (`choco install nsis`, then puts `C:\Program Files (x86)\NSIS` on PATH for the build step) and asserts the installer came out, because Wails only WARNS when it is missing ("Cannot create installer: makensis not found") and exits 0. `cmd/orchicon/windows_msi_test.go` fails any workflow that asks for `-nsis` without all three. |

None of the three jobs uses an `os:`/`arch:` matrix, deliberately: the release
workflow's matrix scanner in `cmd/orchicon/ci_release_workflows_test.go` reads
every `- os:` line in the file, and a desktop matrix would make the CLI canary
comparison report a platform mismatch that is not one. Single-OS jobs avoid it.

**Desktop jobs never gate the CLI release.** The `release:` job keeps
`needs: [build, guard]` and pulls desktop artifacts with
`pattern: 'orchicon-desktop-*'` + `continue-on-error: true`, so a webkit or
signing failure cannot stop the CLI binaries from publishing.

## Files

| Path | What it is |
|---|---|
| `linux/orchicon-desktop` | The **AC3 detection point**. A POSIX-sh launcher: WebKitGTK is a dynamic-link dependency, so the loader fails before `main()` — the check must live in a script, not the binary. Installed as `/usr/bin/orchicon-desktop`, execs `/usr/lib/orchicon/orchicon-desktop`. |
| `linux/orchicon-desktop.desktop` | Menu entry (`Exec`/`TryExec` = the launcher). |
| `nfpm.yaml` | The **AC3 declaration**. `depends: libwebkit2gtk-4.0-37, libgtk-3-0`. The ABI named here must be the ABI the package's binary LINKS — so the job that builds it passes `-tags orchicon_desktop,production` and NOT `webkit2_41` (`cmd/orchicon/webkit_abi_consistency_test.go` fails the pair if they diverge). |
| `appimage/AppRun`, `appimage/build.sh` | AppImage for distros that dropped WebKitGTK 4.0 (Ubuntu 24.04+, Fedora 40+). Bundles `libwebkit2gtk` + `libjavascriptcoregtk`. **The two ABI families have different SONAMEs and they are not symmetric** — 4.0 is `libwebkit2gtk-4.0.so.37` + `libjavascriptcoregtk-4.0.so.18`; 4.1 is `libwebkit2gtk-4.1.so.0` + `libjavascriptcoregtk-4.1.so.0`. Writing `4.1.so.37` compiles, ships, and silently matches nothing. |
| `macos/entitlements.plist` | Hardened runtime + the JIT entitlements JavaScriptCore requires. No sandbox (loopback + child process). |
| `macos/sign-and-notarize.sh` | codesign → `notarytool submit --wait` → `stapler staple` → `spctl -a` **assertion**. Fails the job rather than shipping an unsigned app. |
| `windows/README-WINDOWS.txt` | Ships in the portable zip **and** the MSI: what to do if WebView2 is missing, and the Evergreen URL. |
| `windows/Orchicon.wxs` | The **MSI** — the whole product (CLI, TUI, desktop app) in one package, because Intune/SCCM/Group Policy and `msiexec /qn` cannot drive an NSIS wizard. Per-user, no elevation. Its last step runs the installed `orchicon.exe` **directly** (`FileRef` + `install --instance [INSTANCE]`) — no `cmd`, no `start`, no wrapper script — and `orchicon` allocates its own console (`cmd/orchicon/console_windows.go`) because a custom action has none. `cmd/orchicon/windows_msi_test.go` fails if a shipped binary is missing from it. |
| `windows/OrchiconSetup.wxs` | The **Burn bundle** — `OrchiconSetup.exe`, the artifact a PERSON runs. A bare MSI is the right unit of deployment (Intune/SCCM/`msiexec /qn`) but a poor experience: it draws the wizard, then hands a long download with prompts to a console it cannot report on. The bundle owns the UI and chains the MSI, so progress and the log belong to the wizard. It does **not** replace the MSI — both are published. `DisplayInternalUI="no"` stops the MSI's own wizard appearing inside it, and it passes `SKIPSETUP=1` so only one UI is ever on screen. |
| `windows/orchicon-desktop.ico` | Multi-resolution icon (`convert appicon.png -define icon:auto-resize=256,128,64,48,32,16`). Wails' PNG feeds only its own packaging; shortcuts and Add/Remove Programs need a real `.ico`. |
| `windows/wizard-banner.bmp`, `windows/wizard-dialog.bmp` | The MSI wizard's own artwork, wired as `WixUIBannerBmp` (493×58) and `WixUIDialogBmp` (493×312). **Without them WixUI silently draws its own red/black defaults** — which is what an operator reported seeing, flickering, on the first wizard page. Committed, not built (same treatment as the `.ico`): packaging must not need Python or a font, and art that changes when a font is upgraded is art that silently changes what users see. Regenerate with `python3 deploy/desktop/windows/make-wizard-bitmaps.py`. |
| `windows/make-wizard-bitmaps.py` | Regeneration tool for the two bitmaps above. Run by hand; not part of any build. |

## The Windows MSI

Built by the WiX CLI (`dotnet tool install --global wix`), so **it can only be built on a
Windows host with the .NET SDK** — `make desktop-package-windows` refuses with that
sentence rather than failing inside a missing command.

What it deliberately does **not** do, each for a measured reason:

- **It does not bundle the service binaries.** Those are 542 MB (otelcol-contrib 293,
  loki 105, tempo 74, postgres 34, victoria-metrics 24, nats 16) and would version-lock
  to the installer. `orchicon install` acquires them at setup, exactly as the one-line
  `install.ps1` does.
- **It DOES bundle `orchicon-linux-amd64`** (~100 MB uncompressed — the single biggest
  thing in the package, and the reason it roughly doubles). That is the binary a runtime
  container executes, and it is not a third-party service: it is the same program as
  `orchicon.exe`, built from the same commit in the same job. A Windows executable cannot
  be PID 1 of a Linux container, so without it an MSI install can never run one — and
  unlike the service binaries there is nothing for setup to acquire it from, which is why
  the MSI carried none for four releases and every MSI install failed at container create.
  It is installed beside `orchicon.exe`, which is the runtime daemon's first search
  location (`internal/runtime/exe_resolve.go`).

  The size is felt by every Windows route at once, because they all carry this file now:
  the MSI, the Burn bundle (`Compressed="yes"`, so it embeds the MSI) and the portable zip.
  The alternative — fetching it during setup — is the design that failed repeatedly: a
  second download that only warned when it failed, and a version free to drift from the
  daemon that mounts it.
- **It does not run the setup as an MSI dialog sequence.** An MSI custom action has no
  console, so `installIsInteractive()` is false and any prompt writes into nothing and
  blocks for ever. Setup runs `orchicon install` in a real console launched from the
  last step — which is what "the normal questions and the runtime-image download" means
  in practice, and it cannot drift from the CLI.
- **It is not per-machine.** A per-machine install would put the binaries under one
  identity and run the services as another, which is the one arrangement the product
  does not support. Per-user also means no UAC prompt.

**The install itself is unverified by CI.** `windows-desktop-msi` in `ci.yml` proves the
Windows desktop app compiles, that WiX accepts the `.wxs`, and that a non-trivial MSI
comes out (it fails below 50 MB, because WiX exits 0 for an empty package). It does not
run the MSI — that needs a real machine.

## Building locally

```sh
make desktop-deps            # pinned wails CLI + the apt hint for Linux
make desktop-preflight-test  # the webview detection tests (no webview needed)
make desktop-build           # the shell binary for this host, into bin/
make desktop-package-linux   # .deb + .AppImage, into dist/desktop/ (Linux only)
```

## Release secrets the macOS job needs

`desktop-macos` runs `deploy/desktop/macos/sign-and-notarize.sh`, which fails the job
rather than shipping an unsigned `.app`. Set these repository secrets before the first
release that includes the desktop artifacts:

| Secret | What it is |
|---|---|
| `MACOS_CERT_P12` | base64 of the Developer ID Application `.p12` |
| `MACOS_CERT_PASSWORD` | that certificate's password |
| `MACOS_NOTARY_APPLE_ID` | the Apple ID used for `notarytool` |
| `MACOS_NOTARY_TEAM_ID` | its Apple team ID |
| `MACOS_NOTARY_PASSWORD` | an app-specific password (not the Apple ID password) |

None of the desktop targets are part of `make ci` on purpose: `ci` is the Go +
frontend gate and must stay runnable on a plain runner. The detection *logic* is
covered by the normal `go test ./...` gate, which is where it belongs.

## Known gaps

- **Windows code signing is not done** (no certificate). The installer works but
  SmartScreen warns; the docs say so.
- **`APPIMAGETOOL_SHA256` is unset by default.** Set it in the release workflow to
  make the appimagetool download verifiable against a pinned digest.
- The AppImage bundles webkit and JavaScriptCore only; GTK3 and the base desktop
  libraries come from the system, which every GTK-capable desktop already has.
