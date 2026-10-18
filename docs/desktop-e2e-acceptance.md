# The packaged desktop app — acceptance runbook

This is the procedure that proves the feature's central claim end to end:

> **The installed app works on a clean machine with no plane running: it names the
> URL it tried, starts the plane itself, serves its own embedded UI, keeps a live
> execution streaming, and stops and starts the plane on demand.**

Every one of those pieces can pass its own test while the packaged app is unusable:
a package that installs but links a missing webview, an offline state that renders a
blank window, a start action that returns before `/healthz` answers, a proxy that
buffers and freezes a live execution, a Stop that reports success over a running
plane. Only walking the whole path on a machine that has never had a plane on it
catches that. This is an **acceptance procedure, not a component test**. It changes
no production code; failures are filed against the owning sibling task.

Automated form: [`scripts/desktop-e2e.sh`](../scripts/desktop-e2e.sh). Run it, or
type the steps below by hand; they are the same commands, and the harness's evidence
files are named after the step numbers below.

```bash
scripts/desktop-e2e.sh --dry-run            # print the plan for every step, execute nothing
scripts/desktop-e2e.sh --preflight-only     # step 0 only (validity precondition), then stop
scripts/desktop-e2e.sh --deb ./orchicon_desktop_0.9.0_linux_amd64.deb   # the whole session
scripts/desktop-e2e.sh --platform windows   # emits the manual Windows 10 evidence sheet
```

The manual sheets are emitted **even when step 0 refuses the host**: steps 0–1's checks
are Linux's, and a Windows/macOS operator necessarily runs this on another host — so a
refusal must not swallow the sheet they came for. The tier is still recorded `SKIP`
(one row, one sheet per platform), never a pass.

Evidence lands in `qa-evidence/desktop-e2e/<UTC>/` (`results.tsv`, `defects.tsv`,
`summary.md`, `NOTES.md`, one log per step). Exit codes: `0` every assertion passed,
`1` at least one assertion failed (**each failure names the `file:line` that owns
it** — this procedure files defects against its siblings, it never patches them),
`2` the run is not a valid proof (a plane is already answering, the artifact is a dev
build, a GUI prerequisite is absent, or the tree cannot produce a package) — nothing
was proven and nothing should be reported as such.

**A `SKIP` is never a pass.** The harness counts SKIPs separately in `summary.md`,
because most of these claims are absence-shaped ("the plane is down", "the banner is
not shown", "the view is not frozen") and are trivially true of a capture that failed
to run. `make desktop-e2e-assertions-test` pins that rule offline, with no app, no
plane and no display.

---

## What a clean machine means here

A **clean machine** is one on which this app has never been installed and no Orchicon
plane is running or has ever run for the invoking user:

| OS | Clean means |
|---|---|
| Linux | a fresh VM (WebKitGTK present or absent per the variant under test), no `orchicon-desktop` package, no `~/.local/share/orchicon-*`, nothing listening on the control port |
| macOS | a fresh VM with no `/Applications/Orchicon.app`, no `~/Library/Application Support/Orchicon`, no plane |
| Windows 10 | a fresh VM with no `Orchicon` entry in *Apps & features*, no WebView2 runtime pre-installed (the point of the WebView2 bootstrapper variant), no plane |

**A dev build is not admissible.** `desktop/build/bin/orchicon-desktop` /
`cmd/orchicon-desktop/build/bin/orchicon-desktop` is the wails dev output; it is not
what a user installs, it resolves its frontend from the source tree, and it cannot
prove the packaging, the embedded UI, or the WebView2/WebKitGTK provisioning paths.
The run must launch the **installed** path:

- Linux `.deb`: `/usr/bin/orchicon-desktop` launcher → `/usr/lib/orchicon/orchicon-desktop`
- Linux AppImage: `<image>.AppImage` → `AppRun`
- macOS: `/Applications/Orchicon.app/Contents/MacOS/Orchicon`
- Windows: `%LOCALAPPDATA%\Orchicon\orchicon-desktop.exe` (per-user install)

---

## Step 0 — the validity precondition

If any of these fail, the run proves nothing and the harness exits `2` before it
touches the machine:

```bash
# no plane answering (the whole point of the claim)
curl -fsS --max-time 2 "http://127.0.0.1:${ORCHICON_CONTROL_PORT:-8080}/healthz"   # must FAIL
# the artifact is a package, and the installed binary is the packaged one
dpkg -s orchicon-desktop          # Linux/deb: reports the version being released
ls -l /usr/lib/orchicon/orchicon-desktop
# the webview the package declares
ldconfig -p | grep -E 'libwebkit2gtk-4\.[01]\.so\.37'   # Linux/WebKitGTK tier
# the GUI automation the Linux automated tier needs
command -v Xvfb xdotool import                            # absence ⇒ exit 2, never a PASS
```

The control port comes from the shared table (`internal/controlport`), never a
hardcoded 8080 in the app; the harness honours `--control-port`.

`Xvfb` is the display the harness **starts and owns** for the whole session: the app
is launched onto it and every screenshot is taken from it, so a capture can never
evidence a different screen. `xvfb-run -a` is deliberately not used — it picks its own
server number and exports `DISPLAY` only to the child it launches, which would leave
the harness shooting a guessed display, and it needs `xauth`, which minimal hosts lack.

---

## Step 1 — install the packaged app (AC1)

```bash
sudo apt install ./orchicon_desktop_<version>_linux_amd64.deb
dpkg -L orchicon-desktop | grep -E '/usr/(bin|lib/orchicon)/'
sha256sum /usr/lib/orchicon/orchicon-desktop
```

**Proof:** `dpkg -L` shows both packaged paths, `dpkg -s` reports the released
version, and the `sha256sum` of the installed binary is recorded. Launching is step
2's job; a package that installs but never opens a window fails there, not here.

macOS and Windows: install from the `.dmg`/`.zip` and the installer respectively, then
run the same steps by hand and attach screenshots (AC9). The harness emits the sheet.

---

## Step 2 — with no plane running, the offline state names the URL it tried (AC2)

Launch the installed app and do **not** start a plane. The expectation is an
**actionable offline panel that names the URL it tried** — not a blank window, not a
bare stack trace.

Three independent observations count as proof, and the app's own word is only one of
them:

1. **The proxy's honest failure body** names the plane URL it could not reach —
   `internal/desktop/proxy.go` (`the plane at <url> is unreachable — is it running?`),
   reachable through the shell origin's `/healthz` passthrough:
   ```bash
   curl -s http://<shell-origin>/healthz    # 502, body contains the plane URL
   ```
2. **The shell's own log** line `desktop: plane version unavailable: <err>`, with the
   bound `Info` reporting `planeTag: "unreachable"` (`internal/desktop/shell.go`,
   `internal/desktop/info.go`).
3. **The rendered panel** shows `Nothing answers at <triedUrl>` where
   `triedUrl = current?.URL ?? info?.planeURL` (`frontend/src/routes/manage-server.tsx`).
   The panel's testids are `plane-state`, `plane-detail`, `plane-start`.

**FAIL** if the URL is absent, generic (`"the server"`, `"something went wrong"`), or
if the action is not offered. Evidence: screenshot + captured 502 body + the log line.

---

## Step 3 — the offline action starts the plane and waits for `/healthz` (AC3)

Click **Start &lt;instance&gt;** in the offline panel. That calls the bound
`plane.Start(instance)` → `planecontrol.Start`, which launches the plane detached and
**waits for `/healthz`** before returning; the page's success string
(`"started and answering /healthz"`) is derived only from that Go-side OK.

Independent verification — never the app's report:

```bash
curl -fsS http://127.0.0.1:${ORCHICON_CONTROL_PORT}/healthz    # 200
curl -fsS http://127.0.0.1:${ORCHICON_CONTROL_PORT}/versionz   # non-empty
ss -ltnp | grep ":${ORCHICON_CONTROL_PORT} "                   # the listening PID
```

The listening PID must equal the `StatusDTO.PID` the control service reports
(`internal/planebind/service.go`), and the UI must hand over to the normal app (not
stay on the offline panel). **FAIL** if the plane comes up but the UI does not, or if
the action returns success while `/healthz` still refuses.

---

## Step 4 — the UI is the app's embedded build, and a version mismatch is surfaced (AC4)

```bash
curl -s http://<shell-origin>/ | grep -c '__ORCHICON_DESKTOP__'   # 1 — injected by the shell
curl -s http://<plane-url>/    | grep -c '__ORCHICON_DESKTOP__'   # 0 — the plane never injects it
```

The shell serves `index.html` with the injected `__ORCHICON_DESKTOP__` script
(`internal/desktop/spa.go`); the plane's own frontend cannot contain it. That is the
proof the window is showing the app's **embedded** build, version-coherent with the
binary, and not the plane's UI by accident.

Coherence: `Info.appTag` vs the installed binary's `orchicon version` vs the plane's
`/versionz`.

**A mismatch must be surfaced, and proving it requires forcing one.** Run the app with
`--plane` pointed at a plane of a *different* tag and assert the banner renders
(`frontend/src/components/version-mismatch-banner.tsx`) and the injected JSON carries
`versionMismatch: true` (`internal/desktop/info.go` sets it only when both tags are
known and differ). A run that only ever sees matching tags proves nothing here and is
recorded SKIP.

---

## Step 5 — server-streaming keeps updating (AC5 — the transport risk)

This is the failure this item exists to catch: **a view that loads and then silently
freezes.** Three layers, all three recorded:

1. **Static invariants, gated offline:** `WriteTimeout: 0` on the shell's HTTP server
   (`internal/desktop/shell.go`) and `FlushInterval: -1` on the proxy
   (`internal/desktop/proxy.go`). A nonzero write deadline truncates
   `ExecutionService/StreamExecutionEvents` mid-run; a buffering proxy yields one lump.
2. **Go-level live proof:** `ORCHICON_DESKTOP_PLANE_URL=http://… go test -tags
   desktop_e2e ./internal/desktop -run TestLive -v` (`internal/desktop/e2e_live_test.go`,
   "three events spread over time"). If this fails, the app-level failure is a
   **transport** defect, not a UI one — that disambiguation is why both are run.
3. **Webview-level proof (the gap):** start a real workflow and sample the **visible**
   event list at `t0`, `t0+10s`, `t0+20s`, while independently sampling the execution's
   own event count through the plane's API. **PASS only if both grow.** A capture that
   froze, or that yielded exactly once, FAILs — that is the specific failure mode.

---

## Step 6 — unary data views load from the plane (AC6)

Navigate to `/projects` and `/work-items` and compare the rendered rows to the plane's
own count for the same session (the same Connect procedure the UI calls).

An **empty table PASSes nothing**: empty-rendered-while-the-plane-is-also-empty is
SKIP; empty-rendered-while-the-plane-has-rows is FAIL, filed against the failing
query in `frontend/src/api/`.

---

## Step 7 — stop and start from the app, both independently verified (AC7)

```bash
# after clicking Stop
curl -fsS --max-time 2 http://127.0.0.1:${ORCHICON_CONTROL_PORT}/healthz   # must FAIL
kill -0 <pid-from-step-3>                                                  # must be gone
# after clicking Start again
curl -fsS --max-time 2 http://127.0.0.1:${ORCHICON_CONTROL_PORT}/healthz   # 200
```

The wording the app shows comes from `frontend/src/lib/manage-server.ts`
(`stopped`, `started and answering /healthz`, and the honest
`left-alone-unidentified` outcome — an app that claims to have stopped a plane it could
not identify is a defect, not a pass).

---

## Step 8 — Windows 10 and the Linux webview variants (AC8)

**Windows 10 (WebView2).** The installer must provision WebView2 — either the Evergreen
bootstrapper (`deploy/desktop/windows/README-WINDOWS.txt`) or a precise instruction. A
blank window is a FAIL. The absence path is proven by capturing the preflight dialog's
message (`internal/desktoppreflight` `Report.String()`), which names the remedy URL.

**Linux (WebKitGTK).** A host missing `libwebkit2gtk-4.{0,1}.so.37` must be told so
clearly by the launcher's plain-`sh` message (`deploy/desktop/linux/orchicon-desktop`),
not die in a loader error. Prove it by running the launcher with the webkit sonames
filtered out of the loader's view and capturing its message; the `.deb`'s declared
dependency and the AppImage path (`deploy/desktop/appimage/AppRun`) cover distros that
dropped 4.0.

**macOS.** Gatekeeper/notarisation: the app must open without a right-click ceremony, or
say precisely what to do (`deploy/desktop/macos/sign-and-notarize.sh` is the owning
path).

---

## The SKIP rule, and how failure becomes a defect (AC9, AC10)

- **SKIP means unproven, never passed.** A step with no display, no installed package,
  no second tagged plane, or no reachable workflow is recorded SKIP with the host
  limitation named in the step log and in `NOTES.md`.
- **A FAIL never edits the owning code.** The harness writes
  `step<TAB>assertion<TAB>status<TAB>evidence<TAB>owner file:line` into `results.tsv`
  and `id<TAB>owning-task<TAB>evidence<TAB>summary` into `defects.tsv`. The fix belongs
  to the sibling task that owns that file.
- **Known defect D1 (filed against the packaging task).** Packaging expects the shell at
  repo-root `desktop/` (`Makefile` desktop targets, `deploy/desktop/nfpm.yaml`
  `src: desktop/build/bin/orchicon-desktop`, `.github/workflows/release.yml`), while the
  shell lives at `cmd/orchicon-desktop/`. On a correctly merged tree
  `make desktop-package-linux` cannot succeed, so step 0 exits `2` and records D1 rather
  than reporting a step PASS off a package it did not build or install.

## Acceptance-criteria coverage

| AC | Where it is proven |
|---|---|
| 1 install + launch on Linux, macOS, Windows | step 0–1 (Linux automated), step 8 + manual sheets (macOS/Windows) |
| 2 actionable offline state naming the URL | step 2, three independent observations |
| 3 offline action starts the plane, waits `/healthz`, hands over | step 3, PID + `/healthz` + `/versionz` |
| 4 embedded UI, mismatch surfaced | step 4, injected script + forced mismatch |
| 5 streaming keeps updating | step 5, webview sampling + plane count + Go live test + static invariants |
| 6 unary data views | step 6, rendered rows vs plane counts |
| 7 stop/start, independently verified | step 7, `/healthz` + `kill -0` |
| 8 Windows 10 WebView2 / Linux WebKitGTK | step 8, preflight message + launcher message |
| 9 evidence attached | `qa-evidence/desktop-e2e/<UTC>/` |
| 10 failures filed, not patched | `defects.tsv` rule + D1 |

## A note on what this harness cannot do for you

It cannot install anything on macOS or Windows from Linux, and it cannot conjure a
clean machine. Those tiers emit a **manual evidence sheet** and record SKIP-with-reason
until a human attaches the screenshots. Manufacturing a PASS there would be worse than
useless: it would hide exactly the platform bug this item exists to find.
