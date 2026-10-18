# Published archive — clean-machine acceptance (Linux, macOS, Windows; no Docker, no WSL)

This is the **acceptance procedure** for the platform feature's outermost surface: the artifact a real
user downloads, on a clean machine of each OS, with **no Docker, no Docker Desktop and no WSL**.

Compiling for a platform is not running on it, and a locally built binary is not the published
archive. So this procedure downloads the **published release asset**, verifies it, extracts it, runs
`orchicon install`, and then believes nothing until an independent probe agrees (`/healthz`, the
instance's own port record, a booby-trapped engine PATH).

- Harness: `scripts/published-archive-e2e.sh`
- Footprint measurement: `scripts/e2e/footprint.sh`
- No-engine/no-WSL PATH: `scripts/e2e/no-engine-path.sh`
- Offline honesty gate (CI): `make published-archive-e2e-assertions-test`

Run it once per platform. A platform that has not been run has **not** been proven — say so in the
evidence rather than leaving the gap implicit.

---

## 0. The corrections that change how this procedure is run (read first)

### C1 — do NOT test `latest`. Name a version, and prove the artifact is that version.

`releases/latest` has been many versions behind `develop` and predates the host-native work
entirely. Installing it proves the *old* shape: it either needs Docker or tests code this epic does
not own. Both outcomes are worthless, and one of them looks like a pass.

- `--version` is **required**; `latest`, `stable`, `nightly`, `main`, `develop` are refused (step 0).
- The archive name is **derived** from the release matrix (`orchicon_<version>_<os>_<arch>.tar.gz`,
  `.zip` on Windows), not copied from a transcript.
- The tag's date must be at or after the host-native merge (`ORCHICON_E2E_HOST_NATIVE_MERGE`,
  default `2026-10-18`), and the extracted binary must **report the same version** (step 2).
- **Record the version tested in the evidence.** `summary.md` does this automatically; quote it.

### C2 — the lifecycle verbs are top-level instance verbs. `serve --status` does not exist.

| procedure used to say | the verb that exists | notes |
|---|---|---|
| `orchicon install` | unchanged | also **refuses** `ORCHICON_PLANE_RESIDENCY=container` — the refusal is correct |
| `orchicon serve --status` | `orchicon plane status <instance>` | instance, residency, serving binary, drift, health |
| start/stop the plane | `orchicon start <instance>` / `orchicon stop <instance>` | `stop` is idempotent |
| `orchicon restart` | `orchicon restart <instance>` | takes an instance |

**Every verb takes an instance** — positional, `--instance <name>`, or `ORCHICON_INSTANCE`. Two
instances run side by side in a developer environment, so **every command in the evidence names the
instance it acted on**; the harness writes `[instance: <name>]` beside each logged command.

### C3 — an install with captured stdout asks NOTHING. That is correct, not a defect.

With redirected stdout (a log, a pipe, a captured CI run) there is nobody watching, so the install
takes the record/defaults instead of blocking on a terminal it does not own. A wrapper that
genuinely has a human sets `ORCHICON_INSTALL_INTERACTIVE=1`.

- A captured run **must ask nothing** (asserted), and it must still **say which ports it used**
  (asserted against the instance's own `ports.json`).
- On a **second** install for the same instance the ports come from the instance's record and the
  runtime-image prompt is not re-offered. A re-run that re-asks is a **defect**.
- If you intend to exercise the prompts, run in a real terminal (or set that variable) and state
  which mode was used in the evidence. The harness runs in **captured mode** and reports so.

### C4 — the footprint number needs its date and its contents beside it.

`scripts/disk-report.sh` answers a *different* question — how much a btrfs filesystem pins in
snapshots (`data_used − du(live)`) — and it reports "no snapshot attribution" on ext4, APFS and
NTFS, i.e. on two of the three platforms here. The footprint is therefore measured with
`scripts/e2e/footprint.sh` (`du`, per component) and **recorded with the measurement date and the
components that were installed**. Until the Grafana-removal work lands, the figure still includes
everything in the tree at the time of the run; a number without that context will be misread later.

---

## 1. What "clean machine" means (all platforms)

The harness enforces all of this in step 0 and refuses to run otherwise (exit 2, no product defect
filed — an unclean host accuses no product code).

- No Docker, Docker Desktop, Podman, Colima/Lima, nerdctl. The harness plants booby-trapped shims for
  every one of those names (`scripts/e2e/no-engine-path.sh`); each exits 127 and logs its argv, and
  the invocation log **must stay empty** for the whole run (AC5).
- No WSL on Windows, and no `wsl`/`wsl.exe` reachable on any platform.
- No previously installed `orchicon` on `PATH` (`command -v orchicon` must be empty).
- No Postgres running for this user (the plane starts its own host service).
- Download + verify + extract tooling: `curl`, `sha256sum`/`shasum`/`certutil`, `tar`/`unzip`,
  and `script` for the TUI pty.

A disposable VM per platform is the honest way to get this. A machine that has ever run Docker is
not a clean machine, even with Docker stopped.

---

## 2. Linux

```bash
# 0. name the version under test (C1)
scripts/published-archive-e2e.sh --version vX.Y.Z --platform linux --instance dev --preflight-only

# 1-11. the whole procedure
scripts/published-archive-e2e.sh --version vX.Y.Z --platform linux --instance dev
```

Evidence lands in `qa-evidence/published-archive-e2e/<UTC>-linux/`. Attach: `summary.md`,
`1-archive-listing.txt`, `3-install.log`, `4-healthz.txt`, `4-plane-status.txt`, `9-tui-render.txt`,
`10-shim-invocations.txt`, `11-footprint.txt`.

Footprint only (repeats step 11 on its own):

```bash
scripts/e2e/footprint.sh --instance dev                 # human report
scripts/e2e/footprint.sh --instance dev --raw            # key=value, what the harness parses
```

---

## 3. macOS (Gatekeeper must not be a dead end)

Apple Silicon downloads `orchicon_<version>_darwin_arm64.tar.gz`; Intel uses `_amd64`.

```bash
scripts/published-archive-e2e.sh --version vX.Y.Z --platform darwin --instance dev
```

Step 8 asks Gatekeeper directly (`spctl --assess --type execute -vv`, `codesign --verify --deep
--strict`) and reads `com.apple.quarantine`. There are exactly three outcomes:

1. **Accepted** — nothing for the user to get past. Done.
2. **Refused, with the exact instruction below present in this file** — not a dead end, because the
   download still runs. This is the tested instruction:

   ```bash
   # the archive was downloaded with a browser (quarantine xattr present):
   xattr -d com.apple.quarantine ~/Downloads/orchicon_<version>_darwin_arm64.tar.gz
   tar -xzf ~/Downloads/orchicon_<version>_darwin_arm64.tar.gz -C ~/Downloads
   xattr -d com.apple.quarantine ~/Downloads/orchicon_<version>_darwin_arm64/orchicon
   ~/Downloads/orchicon_<version>_darwin_arm64/orchicon --version
   ```

   If `spctl` still refuses the extracted binary, the **release** is the problem (unsigned or
   un-notarised): file the defect against the release-matrix / signing task. Do **not** patch the
   installer here (AC10).
3. **Refused with no instruction in this file** — a **DEAD END**, asserted as a failure. Fix this
   document or the release, in the task that owns it.

Note the quarantine xattr is set by *browsers*, not by `curl`. A `curl`-downloaded archive can pass
while the same URL clicked in Safari fails — so test the browser path too, and say which you used.

---

## 4. Windows (no WSL, no Docker, and an elevated shell must be told why)

Run in **Windows PowerShell 5.1 or 7**, **not** elevated (see step 7). The asset is
`orchicon_<version>_windows_amd64.zip` — the release matrix ships a zip for `windows/amd64` only
(`windows/arm64` runs the amd64 zip under emulation and is deliberately not published).

```powershell
# in WSL-free, Docker-free PowerShell
bash scripts/published-archive-e2e.sh --version vX.Y.Z --platform windows --instance dev --skip-tui:$false
```

If you are driving the steps by hand, they are the same shape:

```powershell
Expand-Archive -LiteralPath "$HOME\Downloads\orchicon_<version>_windows_amd64.zip" -DestinationPath "$HOME\Downloads\orchicon"
& "$HOME\Downloads\orchicon\orchicon_<version>_windows_amd64\orchicon.exe" --version   # must name <version>
& "$HOME\Downloads\orchicon\orchicon_<version>_windows_amd64\orchicon.exe" install --instance dev
& "$HOME\Downloads\orchicon\orchicon_<version>_windows_amd64\orchicon.exe" plane status dev
```

**SmartScreen is not a dead end.** Exactly one of:

- the published `.exe` carries a **valid Authenticode signature** (`Get-AuthenticodeSignature` →
  `Valid`), or
- the **tested zip-mark instruction** below is what the user follows:

  ```powershell
  # Windows marks the downloaded ARCHIVE as from the internet; mark it safe, then extract
  Unblock-File -LiteralPath "$HOME\Downloads\orchicon_<version>_windows_amd64.zip"
  Expand-Archive -LiteralPath "$HOME\Downloads\orchicon_<version>_windows_amd64.zip" -DestinationPath "$HOME\Downloads\orchicon" -Force
  Get-ChildItem "$HOME\Downloads\orchicon" -Recurse | Unblock-File
  ```

**WSL is not required for a local-mode install.** The plane and its host services are native Windows
processes; WSL is only ever about Linux *containers*, which this mode does not use. The harness
plants `wsl`/`wsl.exe` shims; a single invocation fails the run (AC3).

**The elevated-shell message (step 7).** Postgres refuses to run as Administrator, so an elevated
install must refuse **up front** with an actionable message — not fail minutes later inside `initdb`.
Run the procedure from a **normal** shell: step 7 launches a UAC-elevated child, captures its
transcript, and asserts all four parts of the message:

```
this PowerShell is running as Administrator, and Orchicon cannot install from an
  elevated shell: its Postgres host service REFUSES to run as Administrator ...
Next steps:
  1. Close this window.
  2. Open a NORMAL PowerShell (Start → PowerShell; NOT 'Run as administrator').
  3. Re-run:  irm https://orchicon.dev/install.ps1 | iex
  Nothing was downloaded and nothing was changed.
```

If UAC elevation is unavailable to the harness the step is a **SKIP with that reason** — never a pass.

---

## 5. No Docker, anywhere (AC5)

Step 10 asserts three separate things, and each has a different failure owner:

1. **Nothing invoked an engine or WSL** — the shim invocation log is empty. Owner: the installer (or
   whatever shelled out).
2. **No engine reference in any install-path output** — install log, re-install log, `plane status`,
   the TUI capture, and the plane's own log. Owner: the installer (this is where DEF-2 lives).
3. **The plane is host-resident** — `plane status` says so (step 4). Owner: the plane/host-services task.

A transcript that merely *mentions* Docker for a user who has no Docker is a defect even when nothing
executed: it is the instruction that sends a user to install a container engine to run a host-native
product.

---

## 6. The installed footprint (AC6)

```bash
scripts/e2e/footprint.sh --instance dev            # human
scripts/e2e/footprint.sh --instance dev --raw      # machine (this is what the harness parses)
scripts/e2e/footprint.sh --instance dev --require-all   # exit 1 if a component is absent
```

What is counted: the install dir (`orchicon`, `orch`), the host-services binaries (`<data>/bin`), and
one instance's data dir. What is **excluded**, and reported separately so the exclusion cannot be
quietly widened: `projects/` and `runtime/` **images** — exactly what the epic's claim excludes.

Record in the evidence, for each platform: the **measured total**, the **date**, and **which
components were installed**. If the total is at or above 1 GiB the claim is **refuted**: the harness
records a `FINDING` (not a failure) and the summary keeps it visible. A refutation is the finding
this procedure exists to produce — hiding it is the only wrong answer.

---

## 7. Evidence checklist (AC9)

Per platform, attach:

- [ ] the **tested version** and the exact **asset name** (`summary.md`)
- [ ] the archive's **SHA-256** and whether it was **listed in SHA256SUMS**
- [ ] `1-archive-listing.txt` (both binaries inside, no container assets)
- [ ] `2-version.txt` (tag vs. what the binary reports)
- [ ] `3-install.log` and `6-reinstall.log` (captured mode: nothing asked, ports named, re-run stable)
- [ ] `4-healthz.txt` and `4-plane-status.txt` (host-resident, current, healthy)
- [ ] `9-tui-screenshot.txt` (the TUI actually rendered) or the recorded reason it could not
- [ ] `10-shim-invocations.txt` (empty) and `10-engine-references.txt` (empty)
- [ ] `11-footprint.txt` (measured total, date, components)
- [ ] `summary.md` (per-step verdicts, defects, findings)

---

## 8. Known defects this procedure will report (file them, do not patch them here — AC10)

| id | what | owner | effect on the run |
|---|---|---|---|
| DEF-1 | `release.yml` builds `SHA256SUMS` from `*.tar.gz *.pkg` only, so the Windows `.zip` is published **unchecksummed** and `install.ps1` falls back to "skipping checksum verification". | release-matrix task | step 1 fails "the archive is LISTED in SHA256SUMS" on Windows; Linux/macOS verify normally |
| DEF-2 | `scripts/install.sh` help/teardown text still names a "Docker volume" and probes `docker`. | installer task | step 10 fails on any path that prints it |
| DEF-3 | `scripts/disk-report.sh` measures btrfs snapshot-pinned bytes, not an install footprint. | epic (footprint claim) | why step 11 uses `scripts/e2e/footprint.sh` instead |

A run that hits these is a **valid run with real findings**: record them, file them against the owner
named in `defects.tsv`, and do not "fix" the product from inside this procedure.

---

## 9. Exit codes

| code | meaning |
|---|---|
| 0 | every assertion passed on this platform |
| 1 | at least one assertion FAILED — each names the sibling task that owns it (`defects.tsv`) |
| 2 | **not a valid proof**: no explicit version (C1), this host is not a clean machine, or a required surface is absent |
