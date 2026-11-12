# What this evidence is — and what it is not

Produced by `scripts/desktop-e2e.sh` from inside the workflow's runtime container on
Debian 12 (Linux), on the branch `e2e-proof-…-rmc1kyknhhekdk6s` (off `develop`
`e95924e91`).

**This is a step-0 (validity precondition) record, not an acceptance run.** The harness
refuses this host and says why; that refusal is the deliverable here, committed so the
first verdict is on the record.

- `00-plan.txt` — `--dry-run`: the plan for steps 0–8, nothing executed.
- `00-preflight-run.txt` + `step-0.log` — `--preflight-only`. Two refusals were observed
  on this host across the run:
  1. **a plane was already answering** `http://127.0.0.1:8080/healthz` — the claim is
     "on a clean machine with NO plane running", so the run exits **2** rather than
     measuring a machine that already violates the premise;
  2. with the control port free, **no packaged artifact and nothing installed** — and
     this tree contains none of the desktop sources (`cmd/orchicon-desktop/`,
     `internal/desktop/`, `deploy/desktop/` all absent, they live on unmerged branches),
     so it cannot produce a package either. Exit **2**, with the reason named.
- `results.tsv` — the honesty gate PASSed (109 offline assertions), and the run itself is
  a **SKIP**, never a PASS.
- `defects.tsv` — empty **by design** here: a host condition is not a defect against a
  sibling file. Defect **D1** (packaging expects the shell at repo-root `desktop/` while
  it lives at `cmd/orchicon-desktop/`) is recorded in `docs/desktop-e2e-acceptance.md`
  and is written into `defects.tsv` automatically by step 0 in a tree that actually
  contains `deploy/desktop/`.

## What is UNPROVEN on this host (all of it, by construction)

No installed package, no clean machine, no `xdotool`, no `import` (ImageMagick), no
`tesseract`, and no WebKitGTK here — so **no step 1–8 assertion could be run**. Every
one of them would be SKIP by the harness's own rules; manufacturing a PASS from an empty
capture is exactly what `make desktop-e2e-assertions-test` (109 assertions, part of
`make ci-go`) forbids.

## To actually perform the acceptance run (the QA step)

On a clean Linux VM with no plane running:

```bash
scripts/desktop-e2e.sh --preflight-only          # must exit 0 (not 2) on a genuinely clean host
sudo apt-get install -y xdotool imagemagick tesseract-ocr
scripts/desktop-e2e.sh --deb ./orchicon_desktop_<v>_linux_amd64.deb \
    --click-map 'start=720,560 stop=720,620' --workflow-id <ID>   # steps 1–8 into qa-evidence/desktop-e2e/<UTC>/
scripts/desktop-e2e.sh --platform windows        # emits the manual Windows 10 (WebView2) sheet
scripts/desktop-e2e.sh --platform macos          # emits the manual macOS sheet
```

The Windows 10 and macOS rows stay SKIP until a human attaches the screenshots (AC9).
