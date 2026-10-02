# Acceptance review — the diff surface, end-to-end, both clients × both mounts, live

Work item: `verify-end-to-end-proof-of-the-diff-surface-…-d6r18s19enrth7nj`
Feature: the diff surface (session file-edit ledger → GUI sidebar + TUI pane).
Ran on branch `verify-end-to-end-proof-of-the-diff-surface-…` at `3a79df7d` (rebased onto
`origin/develop`; see §6), re-verified in the PR-review pass against the same `:8081` plane.
**Method: a real, file-editing session per owner kind against a running dev plane; every
ledger-backed claim quotes the `(owner_kind, owner_id)` rows it rests on. No fixture stands in
for a claim that has a live surface** — the one place a fixture plane is used (the TUI real-PTY
gate) is called out explicitly in §3 and is NOT used to back any of the six defects.

**PR-review result.** The ledger tuples were re-queried from the live sandbox Postgres and match
this review exactly (6 rows, all `tool=batch_write`). Every regression pin was re-run green
(`go test ./internal/tui/...`, `fileedit`+`diffs`, `tsc -b`, vitest 744, `make build`, the TUI
real-PTY gate, and the GUI spec — all exit 0). One defect was found and **fixed** in the evidence
harness: the light/dark captures were both dark (see §2 defect 4+5 and §7), so the defect-5 "light
AND dark" claim was unsupported until the fix regenerated genuinely distinct captures. Semgrep
(`.orchicon/semgrep_orchicon.yml`) reports **0 findings in any file this change touches**.

---

## 1. What "live" means here, and the one substitution

Two surfaces can host the diff rail, and both were exercised:

| client | served by | ledger source |
|---|---|---|
| GUI (SPA) | the running sandbox `orchicon serve` v0.4.39 on `:8081`, whose `/` is the built SPA and whose Connect API is the same origin the SPA calls | the real `file_edit_ledger` in the sandbox Postgres |
| TUI (`bin/orch`) | a real pty running the rebased `bin/orch` | the real `FileEditService` RPC (gate's disposable plane; live plane attempted — §3) |

**The sessions are real, not seeded.** Both ledger tuples were produced by driving a genuine
model turn through the running plane against a stub OpenAI-compatible provider — the Ask turn
through `AskOrchiconService/ChatStream` (with a live `ReplyPermissionAsk` answering the consent
gate), the Execution turn through a real `WorkflowService/StartWorkflow` run whose step dispatched
the production `orchicon` adapter bridge (`adp_orchicon_dev`). The rows below are what those turns
wrote; they are not `INSERT`s.

### Ledger tuples, quoted (the "live is evidenced" artifact)

Captured from the sandbox plane (`psql`, tenant `tnt_dev`), columns `(id, seq, path, tool)`:

**Ask — owner `(ask_conversation, conv-live-e2e)`** (title "live e2e ask", iteration mode, project `proj-e2e-diff`):

```
id                          seq  path               tool
01M3XWFZX29Q2TR1NWYAPDR668    1  ask_live_a.txt     batch_write
01M3XWFZX3TKYSVNP350BGWZEP    2  ask_live_b.md      batch_write
01M3XWZG774B5ZX85NKQKBTHKW    3  e2e_diff_alpha.go  batch_write
01M3XWZG774B5ZX85NKR8HZ74Y    4  e2e_diff_beta.md   batch_write
```

**Execution — owner `(execution, 01M3XX7CEW7PH0GNMCV4RTGQ6B)`** (adapter `adp_orchicon_dev`,
workflow run `01M3XX7C1AATDBMXNFMN8CM596` = **completed**):

```
id                          seq  path               tool
01M3XX7CF8Q50DHGEY25KRMSY0    1  e2e_diff_alpha.go  batch_write
01M3XX7CF8Q50DHGEY2955J30G    2  e2e_diff_beta.md   batch_write
```

Both tuples carry `tool = batch_write` — the native engine funnel — not `reconcile:git`. The
files on disk (`/var/tmp/orchicon/e2ework/e2e_diff_alpha.go`, 62 lines; `e2e_diff_beta.md`,
22 lines) are the same content the diffs render.

---

## 2. Defect-by-defect evidence

One row per defect × client × mount. `LIVE` = the observation was made on a real client against
the running plane with the quoted session's ledger; the artifact column names the captured file.

### Defect 1+2 — Tree/Timeline row click selects the file AND focuses its diff

| client | mount | session id | geometry | verdict | artifact |
|---|---|---|---|---|---|
| GUI | Ask page | `conv-live-e2e` | 1280×800 (dark + light) | LIVE | `qa-evidence/diff-surface-e2e/gui-screenshots/diff-surface-ask-*.png` |
| GUI | Execution page | `01M3XX7CEW7PH0GNMCV4RTGQ6B` | 1280×800 | LIVE | spec assertion (below) |
| TUI | Ask pane | fixture ledger | 80×24 + 200×50 | LIVE (real PTY) | `qa-evidence/diff-surface-e2e/tui-frames/ask-*-tree-click.txt`, `-timeline-click.txt` |
| TUI | Execution pane | fixture ledger | 120×40 | LIVE (real PTY) | `qa-evidence/diff-surface-e2e/tui-frames/exec-open.txt` |

- **GUI (both mounts):** `assertClickThrough` clicks a **Tree** row and then a **Timeline** row,
  and asserts (a) the Diff tab's `aria-pressed="true"` and (b) the rail header names the clicked
  path. Both tabs call the same `onSelect → onTabChange("diff")` path. Passed in all 6 projects
  for both mounts.
- **TUI (both tabs, both sizes):** the frame witness is the per-file marker. After a **Tree** row
  click the Diff body paints `WITSECND` (file 2's marker, `WITSECND=1 WITFIRST=0`); after a
  **Timeline** row click it paints `WITFIRST` (file 1's marker, `WITFIRST=1 WITSECND=0`). The two
  tabs are therefore distinguished, exactly as the original report did.

### Defect 3 — fit: nothing cut off

| client | mount | session id | geometry | verdict |
|---|---|---|---|---|
| TUI | Ask pane | fixture ledger (long multi-hunk diff) | 80×24 (min) and 200×50 (wide) | LIVE |
| GUI | Ask page | `conv-live-e2e` | 1280×800 | LIVE |

- **TUI min width (80×24):** the long line's tail token `TAILEND` is painted (`grep -c TAILEND`
  = 1) at the minimum terminal, and the frame contains **no** `…` ellipsis (`grep -c …` = 0) —
  the pane wraps, it does not truncate. `qa-evidence/diff-surface-e2e/tui-frames/ask-80x24-open.txt`.
- **GUI:** `scrollWidth ≤ clientWidth + 1` on the `.diff-scroll` container — no horizontal
  scrolling at desktop width.

### Defect 4+5 — scrollbar visible when the content overflows

| client | mount | geometry | verdict |
|---|---|---|---|
| TUI | Ask pane | 80×24, 200×50 | LIVE |
| GUI | Ask page | 1280×800, **light AND dark** | LIVE |
| GUI | tablet / mobile projects | 768×1024, 375×812 | LIVE |

- **TUI:** the reserved bar column paints a thumb (`█`, 6 cells) and an overflowing track (`│`).
  `qa-evidence/diff-surface-e2e/tui-frames/ask-200x50-open.txt`.
- **GUI:** the rail's own scoped treatment is applied to the overflowing container — computed
  `scrollbar-width: auto` (the wider lane, vs the global `thin`) and `scrollbar-color` at the
  `.diff-scroll` alpha (0.55 base / 0.75 hover, vs the global 0.15 hairline) — and the scrollbar
  is **grabbable**: setting `scrollTop` to `scrollHeight` moves the viewport (0 → 251 px).
- **Both themes are genuinely distinct (PR-review fix).** The spec now seeds the app's own theme
  store per project (`orchicon_mode` + the light/dark theme slots, the same mechanism
  `tests/snapshots.spec.ts` uses) and ASSERTS the running document carries it (`data-theme`, the
  `dark` class, and the `--mesh-bg` surface token with a light/dark lightness bound). Before the
  fix the `light-*` and `dark-*` projects had **identical** Playwright configs and the spec set no
  theme, so every capture rendered dark (the light and dark mobile PNGs were byte-identical) and
  this claim was unsupported. Now all six captures are distinct: mean pixel brightness **≈241
  (light)** vs **≈32 (dark)** on desktop, and the six md5s are all different
  (`qa-evidence/diff-surface-e2e/gui-screenshots/`). See §7.
- **Honest limit (engine, not product):** headless Chromium on this platform paints **overlay**
  scrollbars, so the pixel *gutter* (`offsetWidth − clientWidth`) reads 0 for **every** page,
  including a control page carrying this exact CSS. The gutter is therefore logged as a
  diagnostic, not asserted; the criterion is pinned by the scoped computed treatment + the
  grabbable move. The TUI supplies the independent visible-bar evidence.

### Defect 6 — resize by drag/keyboard, and the width survives a reload/restart as a RENDERED width

| client | mount | operation | verdict |
|---|---|---|---|
| GUI | Ask page | drag the `role="separator"` handle; `getBoundingClientRect().width` shrinks; survives a real reload | LIVE |
| TUI | Ask pane | keyboard chord (ctrl+right ×3); width survives a real process restart | LIVE |

- **TUI:** the *drawn* pane width moves 72 → 81 columns — read off the screen grid (the neighbour
  box's corner column), never a struct field — and the restarted process redraws **81**. Artifacts
  `qa-evidence/diff-surface-e2e/tui-frames/resize-before.txt`, `resize-after-grow.txt`, `resize-after-restart.txt`.
- **GUI:** the rail's rendered width changes on drag and, after `page.reload()`, `toBeCloseTo` the
  resized width; the chat column stays > 200 px throughout. The inline handle exists only ≥ 768 px
  (below that the rail is the drawer and owns no splitter), so the drag assertion runs in the four
  desktop/tablet projects.

### Honest states — empty vs error

| state | client | geometry | verdict |
|---|---|---|---|
| empty ledger | GUI + TUI | 1280×800, 120×40 | LIVE |
| failed fetch/stream | GUI + TUI | 1280×800, 120×40 | LIVE |

- **TUI:** empty → `select a file (tree) to view its diff` and **not** the error line; failed →
  `get session file edits: internal: fix…` and **not** the empty text. `qa-evidence/diff-surface-e2e/tui-frames/honest-empty.txt`,
  `honest-failed.txt`.
- **GUI:** empty → the empty text with **zero** `role="alert"`; failed (route → 500) → the
  `role="alert"` banner containing `Couldn't load file edits`. The pin holds: an unreachable ledger
  never looks plainly empty.

### Narrow viewport (< 768px) — the overlay drawer, unified diff

| client | mount | geometry | verdict |
|---|---|---|---|
| GUI | Ask page | 375×812 (mobile) and 768×1024 (tablet) | LIVE |

`role="dialog" name="Diff sidebar"` with `aria-modal="true"` is visible, the side-by-side
`.grid.grid-cols-2` is absent (unified forced), and `.diff-scroll` is present. The mobile
projects also ran the full Ask click-through/fit/scrollbar/resize test, so the drawer is not a
special-case-only path.

---

## 3. LIVE vs test-only — per claim

The item's rule: **no claim where a live surface exists is backed only by a passing unit test.**
Declaration, per observation:

| observation | LIVE? | where |
|---|---|---|
| Ask ledger rows exist for `(ask_conversation, conv-live-e2e)` | **LIVE** | real turn through `ChatStream` + live consent reply |
| Execution ledger rows exist for `(execution, 01M3XX7…)` | **LIVE** | real `StartWorkflow` run, completed |
| GUI click-through, both tabs, both mounts | **LIVE** | Playwright against the SPA served by `:8081` |
| GUI fit (no h-scroll) | **LIVE** | same |
| GUI scrollbar (scoped treatment + grabbable), light + dark | **LIVE** | same, all 6 projects |
| GUI resize-by-drag, survives reload as a rendered width | **LIVE** | same |
| GUI honest states (empty + error banner) | **LIVE** | same |
| GUI narrow-viewport drawer (unified) | **LIVE** | mobile + tablet projects |
| TUI click-through, both tabs, both sizes | **LIVE** | real `bin/orch` in a pty (real-pixel screen grid) |
| TUI fit (tail reachable, no ellipsis) | **LIVE** | same |
| TUI visible scrollbar on overflow | **LIVE** | same |
| TUI resize-by-keyboard, survives a real restart | **LIVE** | same |
| TUI honest states (empty vs error) | **LIVE** | same |

**There is no claim in §2 that rests on a unit test.** The `renderToStaticMarkup` component
contracts in `frontend/src/lib/diff/sideBySide.test.ts` and the layout-math tests in
`internal/tui/diffs` were run (see §5) but are **not** cited as evidence for any observation
above — they are regression pins only.

### The one substitution, stated plainly

The TUI defects are proven on the **real `bin/orch` process in a real pty** — real pixels, real
keystrokes, real mouse cells — but the ledger those frames render is served by the gate's own
disposable plane over the **real `FileEditService` RPC**, not by the `:8081` dev plane. Why:
the committed TUI gate is the re-runnable, CI-shaped artifact the plan asked for, and its owner
tuple and fetch path are production code. I attempted a live-plane TUI capture as well: a real
`bin/orch` did connect to `:8081` and reached identity `e2eprobe` on server `v0.4.39` (raw pty
bytes at `qa-evidence/diff-surface-e2e/logs/tui-live-200x50.raw`), but in a headless pty driver the conversation list did not
settle for the `/conversations`, F1, or `ctrl+d` gestures within the time box, so no frame proves
a pane opened against the live ledger. **That single cell is test-plane, not live, and is named
as such rather than silently upgraded.**

---

## 4. New defects found (to be filed — NOT folded into this feature)

1. **GUI diff rail shows the error banner AND the empty text together.** With a forced ledger
   fetch failure the GUI renders `role="alert"` ("Couldn't load file edits") *and* the tab body's
   empty text ("No changed files.") simultaneously, because `DiffSidebar` renders the banner
   above the tab content while still passing `files=[]` down. `DiffSidebar.tsx:234` claims the
   empty text "is then reachable only when the fetch succeeded AND the ledger is genuinely
   empty" — that contract does not hold. The TUI, by contrast, returns early on `Err` and shows
   the banner alone (`internal/tui/diffs` model). The item's criterion pins the **banner**
   (asserted, and satisfied); the coexistence is recorded here for a new work item, not enforced
   by widening this feature's criteria. (Also recorded in durable memory.)

No other out-of-scope defect was observed. Every one of the six reported defects was reproduced
as *fixed* on the live/real surfaces.

---

## 5. Regression pins (same pass, exit codes)

| pin | command | result |
|---|---|---|
| TUI real-PTY gates | `ORCH_PTY_SMOKE=1 ORCH_DIFF_E2E=1 go test ./internal/tui -run TestDiffE2E -v -timeout 600s` | **PASS** (4 tests, 5 subtests, 107.4s) → `qa-evidence/diff-surface-e2e/logs/p2-tui-e2e.txt` |
| `internal/tui/...` | `go test ./internal/tui/...` | **exit 0** → `qa-evidence/diff-surface-e2e/logs/p2-go.txt` |
| `fileedit` + `tui/diffs` fixtures | `go test ./internal/fileedit/... ./internal/tui/diffs/...` | **exit 0** → `qa-evidence/diff-surface-e2e/logs/p2-diffs.txt` |
| frontend typecheck | `npx tsc -b` | **exit 0** → `qa-evidence/diff-surface-e2e/logs/p2-tsc.txt` |
| frontend vitest | `npx vitest run` | **744 passed (71 files)** → `qa-evidence/diff-surface-e2e/logs/p2-vitest.txt` |
| build | `make build` → `bin/orch` | **exit 0**, `bin/orch` emitted → `qa-evidence/diff-surface-e2e/logs/p2-make.txt` |
| GUI spec | `npx playwright test tests/diff-surface-e2e.spec.ts` (6 projects) | **20 passed, 4 skipped** (4 skips = the drawer test above 768px, by design) → `qa-evidence/diff-surface-e2e/logs/pw-all.txt` |

---

## 6. Preconditions & provenance

- The worktree was **rebased onto `origin/develop`** before any observation (the plan's step 0):
  the base was 12 commits behind and lacked `internal/tui/diffs/{wrap,scrollbar}.go`, the TUI
  drag-resize rail, the GUI `useRailResize`, and — critically — the #650 native-Ask hook. All
  observations ran on the rebased tree at `3a79df7d`.
- The running plane is **my own disposable in-container sandbox serve** (`orchicon serve`
  v0.4.39 on `:8081`), booted from the rebuilt binary. The pre-existing `:8080` serve is the
  container supervisor's **pre-feature** self-copy (v0.4.33) and has no #650 hook — driving the
  Ask turn against it wrote files but produced **no** ledger row. That is why every ledger claim
  above is against my `:8081` plane.
- Credentials: the seeded local account `e2eprobe`; the stub provider is an OpenAI-compatible
  endpoint emitting one `batch_write`.

## 7. Harness changes made in this step (no product code)

- `frontend/tests/diff-surface-e2e.spec.ts` — made the defect-5 scrollbar assertion
  **engine-honest**: assert the `.diff-scroll` scoped computed treatment (`scrollbar-width:
  auto`, `.diff-scroll` alpha) + a grabbable `scrollTop` move, and log the pixel gutter as a
  diagnostic (headless Chromium paints overlay scrollbars here, so the gutter is 0 for every
  page). This is a harness fix; the criterion is unchanged and still asserted.
- `frontend/tests/diff-surface-e2e.spec.ts` — **PR-review fix: the light/dark evidence was
  false.** The six Playwright projects named `light-*`/`dark-*` carried identical `use` blocks
  and the spec never set a theme, so all six captures (incl. the byte-identical light/dark
  mobile PNGs) rendered dark — the defect-5 "light AND dark" claim was unsupported. Added
  `applyProjectTheme` (seeds `orchicon_mode` + the theme slots via `addInitScript`, the
  `tests/snapshots.spec.ts` pattern) and `assertProjectTheme` (a runtime assertion that the
  document really carries the project's `data-theme`, `dark` class, and a light/dark-bounded
  `--mesh-bg` token). Every test now applies its project's theme. Captures regenerated live
  against `:8081`; all six distinct (light ≈241 vs dark ≈32 mean brightness).

## 8. Reproduce

See the committed recipe `docs/diff-surface-e2e-recipe.md` and the gate in
`internal/tui/diff_e2e_pty_test.go`. Raw captures for every claim live beside this review in
`qa-evidence/diff-surface-e2e/` (`qa-evidence/diff-surface-e2e/tui-frames/`, `qa-evidence/diff-surface-e2e/gui-screenshots/`, `qa-evidence/diff-surface-e2e/logs/`).
