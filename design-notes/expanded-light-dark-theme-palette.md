# Design Notes — Expanded Light & Dark Theme Palette (incl. Gruvbox Light)

**Work item:** Expanded Light & Dark Theme Palette (incl. Gruvbox Light)
**Branch:** `feat/expanded-light-dark-theme-palette`
**Step:** 2 — UI Design Architect
**Date:** 2026-08-06

This document is the authoritative design spec for the theme palette work. The
implementation worker should transcribe the token values below mechanically —
they have been contrast-verified (WCAG 2.2 AA). See
`architecture-notes/expanded-light-dark-theme-palette.md` for the ADR.

---

## 1. Context / problem statement

Users report two lighting-condition problems with the current 20-theme palette
(10 light + 10 dark, `frontend/src/lib/themes.ts` + token blocks in
`frontend/src/index.css`):

1. **Light themes are too bright.** Every light theme sits at
   `--background` lightness 97–100 %. In bright rooms / direct light the
   canvas washes out: white workflow nodes and edges vanish against the white
   page, and card/border separation is nearly invisible (borders are ~86–90 %
   on ~98–100 % backgrounds ≈ 1.2–1.4:1). This is the "workflows hard to see"
   complaint.
2. **Dark themes are too dark.** Every dark theme sits at `--background`
   lightness 7–8 %. In bright conditions the screen glares and dim surfaces
   are illegible.

Additional ask: a **Gruvbox Light** theme with a warm parchment / off-white
background.

## 2. Scope of the change

- `frontend/src/index.css` — token blocks: adjust all 20 existing themes, add
  8 new themes (4 light incl. Gruvbox Light, 4 dark), adjust the global
  light-mode `--kind-*` accent set.
- `frontend/src/lib/themes.ts` — add 8 `Theme` entries with picker swatches.
- No changes to `theme-store.ts`, `theme-provider.tsx`, `tailwind.config.js`,
  or the Settings picker layout. New themes are additive; persisted theme ids
  stay valid; default stays `zinc` / dark. No migration.

## 3. Design decisions (summary — see ADR for full reasoning)

- **D1** Keep the existing token model (HSL triplets in CSS custom properties;
  light = `[data-theme="id"]`, dark = `[data-theme="id"].dark, .dark [data-theme="id"]`).
- **D2** Light themes: darken `--background` by ~3–6 lightness points; keep
  `card` == `background` (current pattern); darken borders/inputs by ~6–10
  points so surfaces separate; darken tinted-theme `--primary` where white
  button text would fail AA.
- **D3** Dark themes: lighten `--background` by ~2–3 points (existing) and add
  new "lighter dark" themes at 13–15 % (new).
- **D4** Gruvbox Light uses the canonical Gruvbox hex palette converted to HSL,
  with two deliberate deviations for AA: `--primary` is the deep Gruvbox blue
  `#2b6064` (not `#458588`, which is 3.7:1 on the parchment background), and
  `--input` uses Gruvbox `bg4` (`#a89984`) so fields read as fields.
- **D5** The global light-mode `--kind-*` set (canvas edge accents) is darkened
  so edges pass ≥3:1 non-text contrast on the darkest new light background
  (Gruvbox bg0). Dark-mode `--kind-*` set is unchanged; on the lightest new
  dark background (graphite) it measures 4.30:1–9.77:1 — indigo 4.30:1, violet
  4.70:1, rose 4.80:1, sky 7.02:1, amber 8.52:1, yellow 9.77:1, emerald 8.46:1,
  cyan 8.99:1 — all above the ≥3:1 non-text floor.
- **D6** Contrast floor: body text ≥ 4.5:1, muted text ≥ 4.5:1 (where muted is
  used for meaningful labels), UI components / kind accents ≥ 3:1. All values
  below verified.

---

## 4. Token specification

### 4.1 Kind accent colors (global, `index.css` lines ~15–23)

Change ONLY the light-mode set (`:root`). Keep the `.dark` set unchanged.

| Token | Current (light) | New (light) | Verified vs Gruvbox bg0 |
|---|---|---|---|
| `--kind-sky` | `199 89% 48%` | **`199 89% 38%`** | 3.80:1 |
| `--kind-amber` | `38 92% 50%` | **`38 92% 36%`** | 3.50:1 |
| `--kind-yellow` | `45 93% 47%` | **`45 93% 34%`** | 3.24:1 |
| `--kind-violet` | `263 70% 50%` | unchanged | 6.33:1 |
| `--kind-rose` | `346 77% 50%` | unchanged | 4.07:1 |
| `--kind-emerald` | `160 84% 39%` | **`160 84% 30%`** | 3.67:1 |
| `--kind-indigo` | `234 89% 60%` | unchanged | 5.02:1 |
| `--kind-cyan` | `188 86% 43%` | **`188 86% 32%`** | 3.81:1 |

Why: at current lightness the yellow/amber/sky/emerald/cyan edges are
1.7–2.5:1 on the parchment background — invisible on the canvas. The darkened
set is still vivid on the lighter themes (verified ≥3.0:1 on pure white).

### 4.2 Adjusted light themes (10)

Convention: `bg` L drops ~3–5; `card`/`popover` = `bg`; `secondary`/`muted` =
`bg` −2–4; `accent` = `bg` −4–6; `border`/`input` = `bg` −10–13; foreground
values unchanged unless noted; tinted primaries darkened where button text
failed AA. Only changed lines are listed; unlisted tokens stay as currently
defined.

**neutral**
- `--background` `0 0% 100%` → `0 0% 96%`; `--card`/`--popover` `0 0% 96%`
- `--secondary`/`--muted` `0 0% 96.1%` → `0 0% 93%`
- `--muted-foreground` `0 0% 45.1%` → `0 0% 42%`
- `--accent` `0 0% 94.1%` → `0 0% 91%`
- `--border` `0 0% 89.8%` → `0 0% 84%`; `--input` `0 0% 89.8%` → `0 0% 86%`

**zinc**
- `--background` `0 0% 100%` → `0 0% 96%`; `--card`/`--popover` `0 0% 96%`
- `--secondary`/`--muted` `240 4.8% 95.9%` → `240 4.8% 93%`
- `--muted-foreground` `240 3.8% 46.1%` → `240 3.8% 43%`
- `--accent` `240 4.8% 93%` → `240 4.8% 90%`
- `--border` `240 5.9% 90%` → `240 5.9% 84%`; `--input` `240 5.9% 90%` → `240 5.9% 86%`

**warm**
- `--background` `40 30% 98%` → `40 30% 94%`; `--card`/`--popover` `40 30% 94%`
- `--primary-foreground` `40 30% 98%` → `40 30% 96%`
- `--secondary`/`--muted` `40 20% 94%` → `40 20% 90%`
- `--muted-foreground` `30 8% 46%` → `30 8% 42%`
- `--accent` `30 15% 90%` → `30 15% 87%`
- `--border` `30 15% 88%` → `30 15% 82%`; `--input` `30 15% 88%` → `30 15% 84%`

**cool**
- `--background` `210 40% 98%` → `210 35% 94%`; `--card`/`--popover` `210 35% 94%`
- `--primary-foreground` `210 40% 98%` → `210 40% 96%`
- `--secondary` `210 40% 94%` → `210 35% 90%`
- `--muted` `210 30% 94%` → `210 25% 90%`
- `--muted-foreground` `215 16% 47%` → `215 16% 44%`
- `--accent` `210 30% 92%` → `210 25% 88%`
- `--border` `214 32% 89%` → `214 28% 83%`; `--input` `214 32% 89%` → `214 28% 85%`

**rose**
- `--background` `350 30% 98%` → `350 25% 94%`; `--card`/`--popover` `350 25% 94%`
- `--primary` `346 77% 50%` → `346 77% 46%`; `--ring` → `346 77% 46%`
- `--secondary` `350 30% 94%` → `350 30% 90%`
- `--muted` `350 20% 94%` → `350 20% 90%`
- `--muted-foreground` `340 8% 48%` → `340 8% 44%`
- `--accent` `350 25% 90%` → `350 25% 88%`
- `--border` `340 20% 88%` → `340 20% 83%`; `--input` `340 20% 88%` → `340 20% 85%`

**sky**
- `--background` `200 50% 98%` → `200 40% 94%`; `--card`/`--popover` `200 40% 94%`
- `--primary` `199 89% 48%` → `199 89% 34%`; `--ring` → `199 89% 34%`
- `--secondary` `200 40% 94%` → `200 40% 90%`
- `--muted` `200 30% 94%` → `200 30% 90%`
- `--muted-foreground` `210 20% 46%` → `210 20% 43%`
- `--accent` `200 35% 90%` → `200 35% 88%`
- `--border` `200 30% 86%` → `200 30% 82%`; `--input` `200 30% 86%` → `200 30% 84%`

**emerald**
- `--background` `150 30% 98%` → `150 25% 94%`; `--card`/`--popover` `150 25% 94%`
- `--primary` `160 84% 39%` → `160 84% 26%`; `--ring` → `160 84% 26%`
- `--secondary` `150 30% 94%` → `150 30% 90%`
- `--muted` `150 20% 94%` → `150 20% 90%`
- `--muted-foreground` `160 12% 45%` → `160 12% 43%`
- `--accent` `150 25% 90%` → `150 25% 88%`
- `--border` `150 20% 86%` → `150 20% 83%`; `--input` `150 20% 86%` → `150 20% 85%`

**violet**
- `--background` `260 30% 98%` → `260 25% 94%`; `--card`/`--popover` `260 25% 94%`
- `--secondary` `260 30% 94%` → `260 30% 90%`
- `--muted` `260 20% 94%` → `260 20% 90%`
- `--muted-foreground` `270 12% 46%` → `270 12% 44%`
- `--accent` `260 25% 90%` → `260 25% 88%`
- `--border` `260 20% 86%` → `260 20% 83%`; `--input` `260 20% 86%` → `260 20% 85%`

**amber**
- `--background` `40 40% 97%` → `40 35% 93%`; `--card`/`--popover` `40 35% 93%`
- `--primary` `38 92% 50%` → `38 92% 30%`; `--ring` → `38 92% 30%`
- `--primary-foreground` `40 40% 97%` → `40 40% 98%`
- `--secondary` `40 35% 93%` → `40 35% 89%`
- `--muted` `40 25% 93%` → `40 25% 89%`
- `--muted-foreground` `30 15% 46%` → `30 15% 40%`
- `--accent` `38 30% 88%` → `38 30% 85%`
- `--border` `38 20% 85%` → `38 20% 80%`; `--input` `38 20% 85%` → `38 20% 82%`

**teal**
- `--background` `170 40% 98%` → `170 30% 94%`; `--card`/`--popover` `170 30% 94%`
- `--primary` `173 80% 40%` → `173 80% 26%`; `--ring` → `173 80% 26%`
- `--secondary` `170 35% 94%` → `170 35% 90%`
- `--muted` `170 25% 94%` → `170 25% 90%`
- `--muted-foreground` `180 15% 44%` → `180 15% 42%`
- `--accent` `170 30% 90%` → `170 30% 88%`
- `--border` `170 25% 86%` → `170 25% 83%`; `--input` `170 25% 86%` → `170 25% 85%`

### 4.3 New light themes (4)

Add `[data-theme="..."]` blocks in the Light Themes section and matching
`LIGHT_THEMES` entries (full token sets below).

**gruvbox** — name `"Gruvbox Light"`, id `gruvbox`. Parchment look, canonical
Gruvbox hex → HSL. Place it first in `LIGHT_THEMES` (marquee theme).

| Token | Value |
|---|---|
| background | `48 87% 88%` (#fbf1c7) |
| foreground | `20 5% 22%` (#3c3836) |
| card | `43 59% 81%` (#ebdbb2) |
| card-foreground | `20 5% 22%` |
| popover | `43 59% 81%` |
| popover-foreground | `20 5% 22%` |
| primary | `184 40% 28%` (#2b6064 — deep Gruvbox blue; 6.25:1 on bg) |
| primary-foreground | `48 87% 88%` |
| secondary | `43 59% 81%` |
| secondary-foreground | `20 5% 22%` |
| muted | `43 59% 81%` |
| muted-foreground | `27 10% 36%` (#665c54; 5.74:1 on bg, 4.75:1 on card) |
| accent | `40 38% 73%` (#d5c4a1) |
| accent-foreground | `20 5% 22%` |
| destructive | `2 75% 46%` (#cc241d; white fg 5.47:1) |
| destructive-foreground | `0 0% 100%` |
| border | `40 38% 73%` (#d5c4a1 — soft Gruvbox border by design) |
| input | `35 17% 59%` (#a89984 — darker so fields read) |
| ring | `184 40% 28%` (#2b6064 — strong focus indicator, 6.25:1) |

Swatches: `["#fbf1c7", "#ebdbb2", "#d5c4a1", "#3c3836", "#cc241d"]`

**paper** — id `paper`, name `Paper`. Warm, deeper than Warm; the
"office-paper" option between Warm and Gruvbox in warmth.

| Token | Value |
|---|---|
| background | `40 25% 93%` |
| foreground | `30 15% 15%` |
| card / card-foreground | `40 25% 95%` / `30 15% 15%` |
| popover / popover-foreground | `40 25% 95%` / `30 15% 15%` |
| primary / primary-foreground | `30 35% 26%` / `40 25% 95%` (8.14:1) |
| secondary / secondary-foreground | `40 20% 89%` / `30 20% 20%` |
| muted / muted-foreground | `40 18% 90%` / `30 10% 42%` (4.53:1 on bg) |
| accent / accent-foreground | `35 15% 86%` / `30 20% 20%` |
| destructive / destructive-foreground | `10 75% 50%` / `0 0% 100%` |
| border / input | `35 15% 82%` / `35 15% 82%` |
| ring | `30 35% 26%` |

Swatches: `["#f2efe9", "#f5f3ef", "#d8d2ca", "#2c2621", "#df4020"]`

**ash** — id `ash`, name `Ash`. Cool gray, deeper than Cool.

| Token | Value |
|---|---|
| background | `210 12% 92%` |
| foreground | `222 30% 14%` |
| card / card-foreground | `210 12% 94%` / `222 30% 14%` |
| popover / popover-foreground | `210 12% 94%` / `222 30% 14%` |
| primary / primary-foreground | `215 30% 24%` / `210 12% 94%` (9.62:1) |
| secondary / secondary-foreground | `210 15% 88%` / `222 25% 18%` |
| muted / muted-foreground | `210 12% 89%` / `215 12% 41%` (4.85:1 on bg) |
| accent / accent-foreground | `215 10% 85%` / `222 25% 18%` |
| destructive / destructive-foreground | `0 72% 50%` / `0 0% 100%` |
| border / input | `215 10% 80%` / `215 10% 80%` |
| ring | `215 30% 24%` |

Swatches: `["#e8ebed", "#eef0f2", "#c7cbd1", "#191f2e", "#db2424"]`

**porcelain** — id `porcelain`, name `Porcelain`. Neutral, deeper than Zinc.

| Token | Value |
|---|---|
| background | `240 6% 93%` |
| foreground | `240 10% 12%` |
| card / card-foreground | `240 6% 95%` / `240 10% 12%` |
| popover / popover-foreground | `240 6% 95%` / `240 10% 12%` |
| primary / primary-foreground | `240 8% 22%` / `240 6% 95%` (10.44:1) |
| secondary / secondary-foreground | `240 5% 89%` / `240 8% 18%` |
| muted / muted-foreground | `240 5% 90%` / `240 4% 42%` (4.75:1 on bg) |
| accent / accent-foreground | `240 5% 86%` / `240 8% 18%` |
| destructive / destructive-foreground | `0 72% 50%` / `0 0% 100%` |
| border / input | `240 6% 80%` / `240 6% 80%` |
| ring | `240 8% 22%` |

Swatches: `["#ececee", "#f1f1f3", "#c9c9cf", "#1c1c22", "#db2424"]`

### 4.4 Adjusted dark themes (10)

Convention: `bg` L rises 7–8 % → 10–11 %; `card`/`popover` = `bg` +2; `secondary`
/`muted` = `bg` +5; `accent` = `bg` +8–9; `border`/`input` = `bg` +9–10;
`muted-foreground` 58 % → 62 %; primary/ring L +2–3 for slightly brighter
accents. Foregrounds unchanged. Only changed lines listed.

**midnight**
- `--background` `225 20% 8%` → `225 20% 11%`
- `--card`/`--popover` `225 18% 10%` → `225 18% 13%`
- `--primary` `210 55% 60%` → `210 55% 62%`; `--ring` → `210 55% 62%`
- `--primary-foreground` `225 20% 8%` → `225 20% 11%`
- `--secondary`/`--muted` `225 15% 14%` → `225 15% 16%`
- `--muted-foreground` `215 10% 58%` → `215 10% 62%`
- `--accent` `225 14% 18%` → `225 14% 20%`
- `--border`/`--input` `225 12% 18%` → `225 12% 20%`
- `--destructive` `0 65% 45%` → `0 65% 48%`

**charcoal**
- `--background` `30 6% 8%` → `30 6% 11%`; `--card`/`--popover` `30 6% 10%` → `30 6% 13%`
- `--primary` `30 30% 72%` → `30 30% 74%`; `--ring` → `30 30% 74%`; `--primary-foreground` `30 6% 8%` → `30 6% 11%`
- `--secondary`/`--muted` `30 6% 14%` → `30 6% 16%`; `--muted-foreground` `30 6% 60%` → `30 6% 63%`
- `--accent` `30 6% 18%` → `30 6% 20%`; `--border`/`--input` `30 5% 18%` → `30 5% 20%`; `--destructive` `10 65% 45%` → `10 65% 48%`

**storm**
- `--background` `220 15% 8%` → `220 15% 11%`; `--card`/`--popover` `220 12% 10%` → `220 12% 13%`
- `--primary` `210 40% 65%` → `210 40% 68%`; `--ring` → `210 40% 68%`; `--primary-foreground` `220 15% 8%` → `220 15% 11%`
- `--secondary`/`--muted` `220 12% 14%` → `220 12% 16%`; `--muted-foreground` `215 8% 58%` → `215 8% 62%`
- `--accent` `220 12% 17%` → `220 12% 19%`; `--border`/`--input` `220 10% 18%` → `220 10% 20%`; `--destructive` `0 65% 45%` → `0 65% 48%`

**dim**
- `--background` `240 8% 8%` → `240 8% 11%`; `--card`/`--popover` `240 6% 10%` → `240 6% 13%`
- `--primary` `240 5% 85%` → `240 5% 88%`; `--ring` `240 4% 80%` → `240 4% 82%`; `--primary-foreground` `240 8% 8%` → `240 8% 11%`
- `--secondary`/`--muted` `240 6% 14%` → `240 6% 16%`; `--muted-foreground` `240 4% 58%` → `240 4% 62%`
- `--accent` `240 6% 17%` → `240 6% 19%`; `--border`/`--input` `240 5% 18%` → `240 5% 20%`; `--destructive` `0 65% 40%` → `0 65% 45%`

**mauve**
- `--background` `340 10% 8%` → `340 10% 11%`; `--card`/`--popover` `340 8% 10%` → `340 8% 13%`
- `--primary` `346 55% 65%` → `346 55% 68%`; `--ring` → `346 55% 68%`; `--primary-foreground` `340 10% 8%` → `340 10% 11%`
- `--secondary`/`--muted` `340 8% 14%` → `340 8% 16%`; `--muted-foreground` `340 4% 58%` → `340 4% 62%`
- `--accent` `340 8% 17%` → `340 8% 19%`; `--border`/`--input` `340 5% 18%` → `340 5% 20%`; `--destructive` `0 65% 45%` → `0 65% 48%`

**cobalt**
- `--background` `220 20% 8%` → `220 20% 11%`; `--card`/`--popover` `220 15% 10%` → `220 15% 13%`
- `--primary` `210 70% 55%` → `210 70% 60%`; `--ring` → `210 70% 60%`; `--primary-foreground` `220 20% 8%` → `220 20% 11%`
- `--secondary`/`--muted` `220 15% 14%` → `220 15% 16%`; `--muted-foreground` `220 8% 58%` → `220 8% 62%`
- `--accent` `220 15% 17%` → `220 15% 19%`; `--border`/`--input` `220 10% 18%` → `220 10% 20%`; `--destructive` `0 65% 45%` → `0 65% 48%`

**pine**
- `--background` `160 15% 7%` → `160 15% 10%`; `--card`/`--popover` `160 12% 9%` → `160 12% 12%`
- `--primary` `160 55% 52%` → `160 55% 55%`; `--ring` → `160 55% 55%`; `--primary-foreground` `160 15% 7%` → `160 15% 10%`
- `--secondary`/`--muted` `160 12% 13%` → `160 12% 15%`; `--muted-foreground` `160 6% 58%` → `160 6% 62%`
- `--accent` `160 12% 16%` → `160 12% 18%`; `--border`/`--input` `160 8% 17%` → `160 8% 19%`; `--destructive` `0 65% 45%` → `0 65% 48%`

**plum**
- `--background` `270 15% 8%` → `270 15% 11%`; `--card`/`--popover` `270 12% 10%` → `270 12% 13%`
- `--primary` `263 55% 60%` → `263 55% 63%`; `--ring` → `263 55% 63%`; `--primary-foreground` `270 15% 8%` → `270 15% 11%`
- `--secondary`/`--muted` `270 12% 14%` → `270 12% 16%`; `--muted-foreground` `270 5% 58%` → `270 5% 62%`
- `--accent` `270 12% 17%` → `270 12% 19%`; `--border`/`--input` `270 8% 18%` → `270 8% 20%`; `--destructive` `0 65% 45%` → `0 65% 48%`

**ember**
- `--background` `30 15% 8%` → `30 15% 11%`; `--card`/`--popover` `30 12% 10%` → `30 12% 13%`
- `--primary` `38 70% 55%` → `38 70% 58%`; `--ring` → `38 70% 58%`; `--primary-foreground` `30 15% 8%` → `30 15% 11%`
- `--secondary`/`--muted` `30 12% 14%` → `30 12% 16%`; `--muted-foreground` `30 6% 58%` → `30 6% 62%`
- `--accent` `30 12% 17%` → `30 12% 19%`; `--border`/`--input` `30 8% 18%` → `30 8% 20%`; `--destructive` `0 65% 45%` → `0 65% 48%`

**abyss**
- `--background` `180 15% 7%` → `180 15% 10%`; `--card`/`--popover` `180 12% 9%` → `180 12% 12%`
- `--primary` `173 60% 50%` → `173 60% 53%`; `--ring` → `173 60% 53%`; `--primary-foreground` `180 15% 7%` → `180 15% 10%`
- `--secondary`/`--muted` `180 12% 13%` → `180 12% 15%`; `--muted-foreground` `180 6% 58%` → `180 6% 62%`
- `--accent` `180 12% 16%` → `180 12% 18%`; `--border`/`--input` `180 8% 17%` → `180 8% 19%`; `--destructive` `0 65% 45%` → `0 65% 48%`

### 4.5 New dark themes (4) — "lighter dark" (13–15 % background)

Add `[data-theme="..."].dark, .dark [data-theme="..."]` blocks in the Dark
Themes section and matching `DARK_THEMES` entries. These are the bright-room
dark themes.

**graphite** — id `graphite`, name `Graphite`. Neutral.

| Token | Value |
|---|---|
| background | `240 5% 15%` |
| foreground | `240 6% 96%` |
| card / card-foreground | `240 5% 17%` / `240 6% 96%` |
| popover / popover-foreground | `240 5% 17%` / `240 6% 96%` |
| primary / primary-foreground | `240 5% 88%` / `240 5% 15%` (11.63:1) |
| secondary / secondary-foreground | `240 4% 19%` / `240 6% 96%` |
| muted / muted-foreground | `240 4% 19%` / `240 4% 64%` (5.57:1 on card) |
| accent / accent-foreground | `240 4% 22%` / `240 6% 96%` |
| destructive / destructive-foreground | `0 65% 50%` / `0 0% 100%` |
| border / input | `240 5% 23%` / `240 5% 23%` |
| ring | `240 5% 88%` |

Swatches: `["#242428", "#29292e", "#38383e", "#f4f4f5", "#d22d2d"]`

**dusk** — id `dusk`, name `Dusk`. Blue-gray.

| Token | Value |
|---|---|
| background | `220 15% 14%` |
| foreground | `210 15% 96%` |
| card / card-foreground | `220 12% 16%` / `210 15% 96%` |
| popover / popover-foreground | `220 12% 16%` / `210 15% 96%` |
| primary / primary-foreground | `210 45% 70%` / `220 15% 14%` |
| secondary / secondary-foreground | `220 12% 18%` / `210 15% 96%` |
| muted / muted-foreground | `220 12% 18%` / `215 10% 63%` |
| accent / accent-foreground | `220 12% 21%` / `210 15% 96%` |
| destructive / destructive-foreground | `0 65% 48%` / `0 0% 100%` |
| border / input | `220 10% 23%` / `220 10% 23%` |
| ring | `210 45% 70%` |

Swatches: `["#1e2229", "#24272e", "#353941", "#f3f5f6", "#ca2b2b"]`

**forest** — id `forest`, name `Forest`. Green.

| Token | Value |
|---|---|
| background | `160 12% 13%` |
| foreground | `160 8% 95%` |
| card / card-foreground | `160 10% 15%` / `160 8% 95%` |
| popover / popover-foreground | `160 10% 15%` / `160 8% 95%` |
| primary / primary-foreground | `160 55% 58%` / `160 12% 13%` |
| secondary / secondary-foreground | `160 10% 17%` / `160 8% 95%` |
| muted / muted-foreground | `160 10% 17%` / `160 6% 63%` |
| accent / accent-foreground | `160 10% 20%` / `160 8% 95%` |
| destructive / destructive-foreground | `0 65% 48%` / `0 0% 100%` |
| border / input | `160 8% 22%` / `160 8% 22%` |
| ring | `160 55% 58%` |

Swatches: `["#1d2522", "#222a28", "#343d3a", "#f1f3f3", "#ca2b2b"]`

**cocoa** — id `cocoa`, name `Cocoa`. Warm brown.

| Token | Value |
|---|---|
| background | `30 10% 14%` |
| foreground | `30 10% 95%` |
| card / card-foreground | `30 8% 16%` / `30 10% 95%` |
| popover / popover-foreground | `30 8% 16%` / `30 10% 95%` |
| primary / primary-foreground | `38 55% 62%` / `30 10% 14%` |
| secondary / secondary-foreground | `30 10% 18%` / `30 10% 95%` |
| muted / muted-foreground | `30 10% 18%` / `30 6% 63%` |
| accent / accent-foreground | `30 10% 21%` / `30 10% 95%` |
| destructive / destructive-foreground | `0 65% 48%` / `0 0% 100%` |
| border / input | `30 8% 23%` / `30 8% 23%` |
| ring | `38 55% 62%` |

Swatches: `["#272420", "#2c2926", "#3f3b36", "#f4f2f1", "#ca2b2b"]`

---

## 5. `themes.ts` changes

Append to the arrays (order matters for the picker grid — `lg:grid-cols-5`):

- `LIGHT_THEMES`: insert `gruvbox` **first**, then add `paper`, `ash`,
  `porcelain` (any order; suggestion: after `teal`).
- `DARK_THEMES`: append `graphite`, `dusk`, `forest`, `cocoa`.

`Theme` shape is unchanged (`id`, `name`, `mode`, `swatches[5]`). Swatch
arrays are specified per theme above. No change to `getTheme`/`themeClass`/
`ALL_THEMES`.

## 6. Verification checklist (implementation)

1. `make ci` passes (frontend typecheck/lint).
2. `npm run build` in `frontend/` succeeds.
3. Serve the app and verify in a browser (Playwright/Chrome):
   - Settings → Appearance shows 14 Light + 14 Dark cards.
   - Selecting every new theme (gruvbox, paper, ash, porcelain, graphite,
     dusk, forest, cocoa) renders: app shell, workflow canvas edges (check a
     workflow with multiple step kinds — edges must be visible on gruvbox and
     paper), cards/inputs/focus rings.
   - Toggle mode while a theme is selected; the theme's light/dark block
     applies (data-theme + .dark).
   - Persisted selection survives reload (localStorage `orchicon_theme`).
4. Spot-check contrast with a picker tool for: muted text on card, primary
   button text, destructive button text for the new themes (spec values above
   already verified; this is a regression check).
5. Confirm existing stored theme ids (e.g. `zinc`) still resolve.

## 7. Out of scope / notes

- Kind accents in dark mode unchanged.
- No API/proto/settings-server changes (appearance is client-side, persisted
  in localStorage — `settings_pb.ts` documents this).
- The Settings picker grid stays 5 columns; the section grows to 3 rows —
  acceptable, no layout change.
- Gruvbox border (`#d5c4a1`, ~1.5:1) is intentionally soft per the Gruvbox
  aesthetic; inputs use the darker `#a89984` and the focus ring is the
  verified 6.25:1 indicator.
