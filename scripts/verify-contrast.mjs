#!/usr/bin/env node
// WCAG AA verification for 20 Orchicon themes
// Checks: foreground vs background ≥7:1, muted-foreground vs muted ≥4.5:1, primary-foreground vs primary ≥4.5:1
import fs from "fs";

function hslToRgb(h, s, l) {
  s /= 100; l /= 100;
  const k = n => (n + h / 30) % 12;
  const a = s * Math.min(l, 1 - l);
  const f = n => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
  return [Math.round(255 * f(0)), Math.round(255 * f(8)), Math.round(255 * f(4))];
}
function parseHsl(str) {
  const m = str.trim().match(/([\d.]+)\s+([\d.]+)%\s+([\d.]+)%/);
  if (!m) throw new Error(`bad hsl: ${str}`);
  return [parseFloat(m[1]), parseFloat(m[2]), parseFloat(m[3])];
}
function lum(rgb) {
  const [r,g,b] = rgb.map(v => { v/=255; return v <= 0.04045 ? v/12.92 : Math.pow((v+0.055)/1.055, 2.4); });
  return 0.2126*r + 0.7152*g + 0.0722*b;
}
function contrast(hslA, hslB) {
  const a = lum(hslToRgb(...parseHsl(hslA)));
  const b = lum(hslToRgb(...parseHsl(hslB)));
  const L1 = Math.max(a,b), L2 = Math.min(a,b);
  return (L1+0.05)/(L2+0.05);
}

// Extract theme blocks from index.css
const css = fs.readFileSync("frontend/src/index.css", "utf8");
// Find all [data-theme="..."] blocks
const themeRegex = /\[data-theme="([^"]+)"\][^{]*\{([^}]+)\}/g;
let match;
const themes = [];
while ((match = themeRegex.exec(css)) !== null) {
  const id = match[1];
  const body = match[2];
  const vars = {};
  for (const line of body.split(";")) {
    const vm = line.match(/--([a-z0-9-]+):\s*([^;]+)/);
    if (vm) vars[vm[1]] = vm[2].trim();
  }
  // deduplicate by id (dark has two selectors but same id; keep first)
  if (!themes.find(t=>t.id===id)) themes.push({ id, vars });
}

let failures = 0;
for (const t of themes) {
  const v = t.vars;
  const fg = v["foreground"], bg = v["background"];
  const mfg = v["muted-foreground"], m = v["muted"];
  const pfg = v["primary-foreground"], p = v["primary"];
  const checks = [];
  if (fg && bg) {
    const c = contrast(fg, bg);
    const ok = c >= 7;
    checks.push(`  fg/bg ${c.toFixed(2)}:1 ${ok?"PASS":"FAIL (need ≥7)"}`);
    if (!ok) failures++;
  }
  if (mfg && m) {
    const c = contrast(mfg, m);
    const ok = c >= 4.5;
    checks.push(`  muted-fg/muted ${c.toFixed(2)}:1 ${ok?"PASS":"FAIL (need ≥4.5)"}`);
    if (!ok) failures++;
    // also check muted-fg vs background (common usage)
    if (bg) {
      const c2 = contrast(mfg, bg);
      const ok2 = c2 >= 4.5;
      checks.push(`  muted-fg/bg ${c2.toFixed(2)}:1 ${ok2?"PASS":"FAIL (need ≥4.5)"}`);
      if (!ok2) failures++;
    }
  }
  if (pfg && p) {
    const c = contrast(pfg, p);
    const ok = c >= 4.5;
    checks.push(`  primary-fg/primary ${c.toFixed(2)}:1 ${ok?"PASS":"FAIL (need ≥4.5)"}`);
    if (!ok) failures++;
  }
  // destructive check
  if (v["destructive-foreground"] && v["destructive"]) {
    const c = contrast(v["destructive-foreground"], v["destructive"]);
    const ok = c >= 4.5;
    checks.push(`  destructive-fg/destructive ${c.toFixed(2)}:1 ${ok?"PASS":"FAIL"}`);
    if (!ok) failures++;
  }
  // nav active checks — must be ≥4.5:1 on background (active tab readability)
  if (v["nav-active-fg"] && v["background"]) {
    const c = contrast(v["nav-active-fg"], v["background"]);
    const ok = c >= 4.5;
    checks.push(`  nav-active-fg/bg ${c.toFixed(2)}:1 ${ok?"PASS":"FAIL (need ≥4.5)"}`);
    if (!ok) failures++;
  }
  if (v["nav-inactive-cyan"] && v["background"]) {
    const c = contrast(v["nav-inactive-cyan"], v["background"]);
    const ok = c >= 4.5;
    checks.push(`  nav-inactive-cyan/bg ${c.toFixed(2)}:1 ${ok?"PASS":"FAIL (need ≥4.5)"}`);
    if (!ok) failures++;
  }
  console.log(`${t.id}:\n${checks.join("\n")}`);
}
console.log(`\nTotal themes: ${themes.length}`);
if (themes.length !== 20) { console.error(`FAIL: expected 20 themes, got ${themes.length}`); failures++; }
if (failures) { console.error(`\n${failures} failure(s)`); process.exit(1); }
else console.log("\nAll WCAG AA checks PASSED");
