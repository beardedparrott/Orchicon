/**
 * theme-contrast.test.ts — every theme's ERROR colour must be READABLE.
 *
 * The operator: "Error/warning messages inside the GUI are VERY hard to read (all throughout the
 * platform and also the card messages in conversations), especially on the default teal theme (any
 * dark theme). They are a heavy red which is just not easy to read."
 *
 * They were right, and the numbers were not marginal. Every dark theme carried
 *
 *     --destructive: 0 62% 30%
 *
 * — a LIGHTNESS of 30% — while their backgrounds sit at 7–14%. Measured contrast for the most
 * common use (error TEXT on the page, 137 `text-destructive` call sites):
 *
 *     orchicon-dark           1.83 : 1
 *     orchicon-dark-teal      1.52 : 1     <- the shipped default
 *     orchicon-dark-crimson   1.79 : 1
 *
 * WCAG AA for normal text is 4.5 : 1, so the error text was roughly a third of the way to legible.
 * Light themes were fine (4.5–5.5 : 1) which is why the problem only showed on dark ones.
 *
 * SO THE ASSERTION IS CONTRAST, NOT A LITERAL. Pinning "0 72% 72%" would pass while a future
 * background change made it unreadable again; measuring the ratio keeps the THEME honest. It covers
 * all three roles the one token serves, because each has a different pairing:
 *
 *   text-destructive on --background   the 137 text sites (the reported case)
 *   text-destructive on --card         the conversation CARD messages (also reported)
 *   --destructive-foreground on --destructive   the solid badge fill
 */

import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

/** HSL (h 0-360, s/l 0-100) to sRGB 0-1, the CSS Color 4 formula. */
function hslToRgb(h: number, s: number, l: number): [number, number, number] {
  const hh = h / 360;
  const ss = s / 100;
  const ll = l / 100;
  if (ss === 0) return [ll, ll, ll];
  const f = (p: number, q: number, t: number): number => {
    let tt = t;
    if (tt < 0) tt += 1;
    if (tt > 1) tt -= 1;
    if (tt < 1 / 6) return p + (q - p) * 6 * tt;
    if (tt < 1 / 2) return q;
    if (tt < 2 / 3) return p + (q - p) * (2 / 3 - tt) * 6;
    return p;
  };
  const q = ll < 0.5 ? ll * (1 + ss) : ll + ss - ll * ss;
  const p = 2 * ll - q;
  return [f(p, q, hh + 1 / 3), f(p, q, hh), f(p, q, hh - 1 / 3)];
}

/** WCAG relative luminance. */
function luminance([r, g, b]: [number, number, number]): number {
  const ch = (c: number) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * ch(r) + 0.7152 * ch(g) + 0.0722 * ch(b);
}

/** WCAG contrast ratio between two HSL colours. */
function contrast(a: [number, number, number], b: [number, number, number]): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

const AA_NORMAL_TEXT = 4.5;

interface ThemeVars {
  name: string;
  bg?: [number, number, number];
  card?: [number, number, number];
  destructive?: [number, number, number];
  destructiveForeground?: [number, number, number];
}

/** hsl() from a `--name: H S% L%` declaration. */
function readHsl(body: string, name: string): [number, number, number] | undefined {
  const m = body.match(new RegExp(`--${name}:\\s*([\\d.]+)\\s+([\\d.]+)%\\s+([\\d.]+)%`));
  if (!m) return undefined;
  return hslToRgb(Number(m[1]), Number(m[2]), Number(m[3]));
}

/**
 * Parse every `[data-theme="…"]` / `.dark` block out of index.css.
 *
 * A block only counts when it declares --background, which is what makes it a theme rather than a
 * component-scoped override.
 */
function readThemes(): ThemeVars[] {
  const css = readFileSync(resolve(__dirname, "../index.css"), "utf8");
  const out: ThemeVars[] = [];
  for (const m of css.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
    const header = m[1];
    const body = m[2];
    const bg = readHsl(body, "background");
    if (!bg) continue;
    const id = header.match(/data-theme="([^"]+)"/)?.[1] ?? header.trim().slice(0, 40);
    out.push({
      name: id,
      bg,
      card: readHsl(body, "card"),
      destructive: readHsl(body, "destructive"),
      destructiveForeground: readHsl(body, "destructive-foreground"),
    });
  }
  return out;
}

describe("theme error-colour contrast", () => {
  const themes = readThemes();

  it("finds every theme (guards against a silent parse failure passing the suite)", () => {
    // The suite is worthless if the parser matched nothing: assert a real population.
    expect(themes.length).toBeGreaterThanOrEqual(20);
    const ids = themes.map((t) => t.name);
    expect(ids).toContain("orchicon-dark-teal");
    expect(ids).toContain("orchicon-dark");
    expect(ids).toContain("orchicon-light");
  });

  it("every theme declares a destructive colour and its foreground", () => {
    for (const t of themes) {
      expect(t.destructive, `${t.name} has no --destructive`).toBeDefined();
      expect(t.destructiveForeground, `${t.name} has no --destructive-foreground`).toBeDefined();
    }
  });

  // THE REPORTED CASE: error text on the page background.
  it("error text is readable on every theme's background (>= 4.5:1)", () => {
    const failures: string[] = [];
    for (const t of themes) {
      const ratio = contrast(t.destructive!, t.bg!);
      if (ratio < AA_NORMAL_TEXT) {
        failures.push(`${t.name}: ${ratio.toFixed(2)}:1`);
      }
    }
    expect(
      failures,
      `error text is below WCAG AA (${AA_NORMAL_TEXT}:1) on:\n  ${failures.join("\n  ")}\n` +
        `  Error text uses --destructive; raise its LIGHTNESS on dark themes. A dark red on a dark\n` +
        `  background is the "very hard to read" the operator reported.`,
    ).toEqual([]);
  });

  // ALSO REPORTED: "the card messages in conversations".
  it("error text is readable on every theme's card surface (>= 4.5:1)", () => {
    const failures: string[] = [];
    for (const t of themes) {
      if (!t.card) continue;
      const ratio = contrast(t.destructive!, t.card);
      if (ratio < AA_NORMAL_TEXT) {
        failures.push(`${t.name}: ${ratio.toFixed(2)}:1`);
      }
    }
    expect(failures, `error text is below WCAG AA on card surfaces:\n  ${failures.join("\n  ")}`).toEqual([]);
  });

  // The one SOLID fill: a destructive badge is `bg-destructive text-destructive-foreground`.
  it("the destructive badge fill keeps its label readable (>= 4.5:1)", () => {
    const failures: string[] = [];
    for (const t of themes) {
      const ratio = contrast(t.destructiveForeground!, t.destructive!);
      if (ratio < AA_NORMAL_TEXT) {
        failures.push(`${t.name}: ${ratio.toFixed(2)}:1`);
      }
    }
    expect(
      failures,
      `the destructive badge's label is below WCAG AA on:\n  ${failures.join("\n  ")}\n` +
        `  --destructive is shared between error TEXT (which must be light on a dark theme) and this\n` +
        `  FILL (whose label must therefore be dark). Keep --destructive-foreground dark on dark themes.`,
    ).toEqual([]);
  });
});
