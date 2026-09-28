import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

/**
 * THE PRE-HYDRATION THEME MUST MATCH THE STORE'S DEFAULT, and this is a SOURCE comparison because
 * the failure is a single FRAME: `index.html`'s data-theme is what the browser paints before the
 * theme store has run, so if the two disagree a fresh load flashes the old theme and then switches.
 * That one frame is the only moment the default is visible AS a change rather than as the default —
 * which makes it exactly the kind of thing that ships unnoticed.
 *
 * I introduced this drift myself: changing DEFAULT_DARK_THEME to Teal Depths left index.html naming
 * Forest Night, and nothing failed. The file's own comment says the two must agree; a comment cannot
 * enforce it.
 */
describe("pre-hydration theme", () => {
  const root = path.join(__dirname, "../..");
  const html = fs.readFileSync(path.join(root, "index.html"), "utf8");
  const store = fs.readFileSync(path.join(__dirname, "theme-store.ts"), "utf8");

  it("names the same theme as the store's dark default", () => {
    const declared = /data-theme="([^"]+)"/.exec(html)?.[1];
    const fallback = /DEFAULT_DARK_THEME = "([^"]+)"/.exec(store)?.[1];
    expect(declared).toBeTruthy();
    expect(fallback).toBeTruthy();
    expect(declared).toBe(fallback);
  });

  // And the value must be a real theme id: an unknown one falls back silently, which would look
  // exactly like this drift — a flash of the wrong colours with nothing to blame.
  it("is an id the theme list actually defines", () => {
    const declared = /data-theme="([^"]+)"/.exec(html)?.[1];
    const themes = fs.readFileSync(path.join(__dirname, "themes.ts"), "utf8");
    const ids = [...themes.matchAll(/id: "([^"]+)"/g)].map((m) => m[1]);
    expect(ids).toContain(declared);
  });

  // The class attribute must still switch on dark styling, or the pre-hydration paint is the LIGHT
  // palette of that theme — a flash of the wrong mode, which is more jarring than the wrong accent.
  it("carries the dark class", () => {
    expect(/<html[^>]*class="[^"]*\bdark\b/.test(html)).toBe(true);
  });
});
