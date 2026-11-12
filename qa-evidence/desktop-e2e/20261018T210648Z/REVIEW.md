# QA review — the packaged-desktop acceptance proof

This directory is this QA step's own record. It **supersedes** the counts in
`../20261018T205747Z/` (that record predates the review that took the offline gate from
109 to 127 assertions); that earlier directory is left intact as history.

## What was run here, and what it says

| Command | Exit | Meaning |
|---|---|---|
| `scripts/tests/desktop-e2e-assertions/run.sh` (`make desktop-e2e-assertions-test`) | 0 | **127/127 green** — see `00-gate.txt` |
| `scripts/desktop-e2e.sh --dry-run` | 0 | the plan for steps 0–8, nothing executed — `00-plan.txt` |
| `scripts/desktop-e2e.sh --preflight-only --control-port 18099` | 2 | **RUN INVALID**: no package, nothing installed, no packaging sources in this tree — `00-preflight-run.txt`, `step-0.log`, `results.tsv`, `summary.md` |
| `scripts/desktop-e2e.sh --preflight-only` (default port 8080) | 2 | **RUN INVALID**: a plane *is* answering `http://127.0.0.1:8080/healthz` on this host, so the no-plane premise is violated |
| `scripts/desktop-e2e.sh --platform windows` | 2 | refused the host **and emitted `tier-windows/8-manual-windows.md`** |
| `scripts/desktop-e2e.sh --platform macos` | 2 | refused the host **and emitted `tier-macos/8-manual-macos.md`** |

Verdict for steps 1–8 on this host: **SKIP — unproven**, and recorded as such rather
than manufactured. There is no installed package here, no `xdotool`, no ImageMagick
`import`, no `tesseract`, no WebKitGTK, and — decisively — **none of the desktop sources
are on this branch** (`cmd/orchicon-desktop/`, `internal/desktop/`, `deploy/desktop/`),
so this tree cannot produce a package to install.

## Defect found by this step and fixed in the harness

**The manual evidence sheets were unreachable in the only situation they are needed.**
Steps 0–1's checks are Linux's, so a Windows/macOS operator running the documented
`scripts/desktop-e2e.sh --platform windows` on a Linux host always hit the step-0 refusal
— and `main()` exited 2 **before** step 8, so no sheet was written. The runbook,
`NOTES.md`, and the AC1/AC8 manual tiers all depend on that command producing a sheet;
it produced nothing.

Fixed in `scripts/desktop-e2e.sh`: a refused step 0 now still emits the requested
platform's sheet (and records its SKIP row) before exiting 2, so the refusal is honest
*and* the operator leaves with the evidence sheet. `manual_sheet` is also memoised per
platform — eight steps previously wrote eight copies of the same claim.

Pinned by the gate's new **rule 9** (7 assertions, 120 → 127): a refused host exits 2,
the sheet exists anyway, the tier is a SKIP row and never a `Verdict: PASS`, macOS gets
its sheet too, and asking twice writes one file and one row.

## Verified against the acceptance criteria

- **AC1–AC8 (Linux automated tier):** the harness implements each with an
  *independent* probe (proxy 502 body naming the tried URL; `/healthz` + `/versionz` +
  listening PID; `__ORCHICON_DESKTOP__` in the shell and absent from the plane; a
  *forced* version mismatch; `WriteTimeout: 0` + `FlushInterval: -1` + window frames
  that must grow; plane counts vs rendered rows; Stop proven by `curl` failing **and**
  `kill -0` going away). Not executable on this host, and recorded SKIP.
- **AC8 (Windows 10 / WebKitGTK):** the sheets are now emitted from a refused host;
  the Linux launcher and Windows preflight assertions read the owning files when the
  packaging branch is merged (`src_contains`/`gated_contains` → SKIP, never PASS, when
  absent).
- **AC9 (evidence):** this directory, plus the per-tier subdirectories.
- **AC10 (failures filed, not patched):** the branch touches **no production code** —
  `git diff --stat develop...HEAD` is `Makefile` (target wiring), `docs/`, `scripts/`,
  `qa-evidence/` only. D1 (packaging packages `desktop/build/bin/orchicon-desktop` while
  the shell lives at `cmd/orchicon-desktop/`) is stated against the packaging task and
  is written into `defects.tsv` automatically by step 0 in a tree that contains
  `deploy/desktop/`.

## Still unproven (a human with clean machines must close it)

Linux with a built `.deb` + `xdotool`/`imagemagick`/`tesseract`; Windows 10 with
WebView2 absent; macOS. Commands are in `NOTES.md` and in
`docs/desktop-e2e-acceptance.md`. Nothing here may be read as a pass for any of them.
