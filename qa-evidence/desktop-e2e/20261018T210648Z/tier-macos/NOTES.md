# What this evidence is — and what it is not

Produced by `scripts/desktop-e2e.sh` at 20261018T210649Z on `Linux 7.2.0-1-cachyos`.

Read `summary.md` for the verdict and `results.tsv` for every row. A SKIP is
UNPROVEN: most claims in this item are absence-shaped and are trivially true of a
capture that failed to run, which is why `make desktop-e2e-assertions-test` pins the
rule that an empty capture can never produce a PASS.

## Known limits of a harness run from inside this repo

- macOS and Windows tiers are manual evidence sheets; they are SKIP here.
- The session starts and owns ONE Xvfb display, so the app and every screenshot
  share it (`xvfb-run -a` would hide the display number from this shell).
- A webview exposes no DOM to the automation this repo has, so clicks are driven
  by `--click-map` coordinates; without one the click assertions are SKIP.
- Rendered-text assertions need an OCR engine (`tesseract`); without one the
  screenshot is evidence for a human, not a machine verdict.
