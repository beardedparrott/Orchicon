import { chromium } from "playwright";

const browser = await chromium.launch({ args: ["--no-sandbox", "--disable-setuid-sandbox"] });
const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
const page = await context.newPage();

await page.goto("http://localhost:5173/work-items", { waitUntil: "networkidle", timeout: 30000 });
await page.waitForTimeout(2000);

console.log("=== ACCESSIBILITY AUDIT ===\n");

// 1. Check for ARIA landmarks
const landmarks = await page.evaluate(() => {
  const roles = ["banner", "navigation", "main", "contentinfo", "complementary"];
  return roles.map(role => ({
    role,
    found: document.querySelectorAll(`[role="${role}"], ${role === "navigation" ? "nav" : role === "banner" ? "header" : role === "main" ? "main" : role === "contentinfo" ? "footer" : role === "complementary" ? "aside" : ""}`).length
  }));
});
console.log("ARIA Landmarks:");
landmarks.forEach(l => console.log(`  ${l.role}: ${l.found > 0 ? "✅" : "❌ missing"}`));

// 2. Check for missing aria-labels on interactive elements
const unlabeledButtons = await page.evaluate(() => {
  const buttons = document.querySelectorAll("button");
  const unlabeled = [];
  buttons.forEach(btn => {
    const text = btn.textContent?.trim();
    const ariaLabel = btn.getAttribute("aria-label");
    if (!text && !ariaLabel) {
      unlabeled.push(btn.outerHTML.substring(0, 100));
    }
  });
  return unlabeled;
});
console.log(`\nButtons without text or aria-label: ${unlabeledButtons.length}`);
unlabeledButtons.forEach(html => console.log(`  - ${html}`));

// 3. Check for images/SVGs without alt
const unAltImages = await page.evaluate(() => {
  const imgs = document.querySelectorAll("img");
  return Array.from(imgs).filter(i => !i.getAttribute("alt") && !i.getAttribute("aria-hidden")).length;
});
console.log(`Images without alt: ${unAltImages}`);

// 4. Check focus styles
const focusableElements = await page.evaluate(() => {
  const els = document.querySelectorAll("button, a, input, select, [tabindex]");
  const results = [];
  els.forEach(el => {
    const computed = window.getComputedStyle(el);
    results.push({
      tag: el.tagName,
      text: (el.textContent || "").trim().substring(0, 30),
      outlineStyle: computed.outlineStyle,
      outlineWidth: computed.outlineWidth,
    });
  });
  return results.slice(0, 20);
});
console.log(`\nFocusable elements (first 20): ${focusableElements.length}`);
focusableElements.forEach(el => {
  const hasFocus = el.outlineStyle !== "none" && el.outlineWidth !== "0px";
  console.log(`  ${el.tag} "${el.text}" outline: ${el.outlineStyle}/${el.outlineWidth}`);
});

// 5. Check color contrast of key elements
const contrastChecks = await page.evaluate(() => {
  const checks = [];
  // Check heading
  const h1 = document.querySelector("h1");
  if (h1) {
    const style = window.getComputedStyle(h1);
    checks.push({ element: "h1", color: style.color, bg: style.backgroundColor });
  }
  // Check body text
  const p = document.querySelector("p");
  if (p) {
    const style = window.getComputedStyle(p);
    checks.push({ element: "p (subtitle)", color: style.color, bg: style.backgroundColor });
  }
  // Check select elements
  const sel = document.querySelector("select");
  if (sel) {
    const style = window.getComputedStyle(sel);
    checks.push({ element: "select", color: style.color, bg: style.backgroundColor });
  }
  // Check search input
  const input = document.querySelector("input[placeholder]");
  if (input) {
    const style = window.getComputedStyle(input);
    checks.push({ element: "search input", color: style.color, bg: style.backgroundColor });
  }
  return checks;
});
console.log("\nColor contrast samples:");
contrastChecks.forEach(c => console.log(`  ${c.element}: text=${c.color} bg=${c.bg}`));

// 6. Check for keyboard trap (tab through elements)
console.log("\nKeyboard navigation test:");
await page.keyboard.press("Tab");
for (let i = 0; i < 10; i++) {
  const focused = await page.evaluate(() => {
    const el = document.activeElement;
    return {
      tag: el?.tagName,
      text: (el?.textContent || "").trim().substring(0, 40),
      ariaLabel: el?.getAttribute("aria-label"),
      role: el?.getAttribute("role"),
    };
  });
  console.log(`  Tab ${i + 1}: <${focused.tag}> "${focused.text || focused.ariaLabel || focused.role || '?'}"`);
  await page.keyboard.press("Tab");
}

// 7. Check heading hierarchy
const headings = await page.evaluate(() => {
  const hs = document.querySelectorAll("h1, h2, h3, h4, h5, h6");
  return Array.from(hs).map(h => ({
    level: h.tagName,
    text: h.textContent?.trim().substring(0, 50),
  }));
});
console.log("\nHeading hierarchy:");
headings.forEach(h => console.log(`  ${h.level}: "${h.text}"`));

// 8. Check for required form labels
const formLabels = await page.evaluate(() => {
  const inputs = document.querySelectorAll("input, select, textarea");
  return Array.from(inputs).map(inp => ({
    type: inp.type,
    hasLabel: !!inp.getAttribute("aria-label") || !!inp.getAttribute("aria-labelledby"),
    hasFor: !!document.querySelector(`label[for="${inp.id}"]`),
    placeholder: inp.getAttribute("placeholder") || "",
  }));
});
console.log("\nForm field labels:");
formLabels.forEach(f => {
  const labeled = f.hasLabel || f.hasFor;
  console.log(`  ${f.type} "${f.placeholder}": ${labeled ? "✅ labeled" : "❌ no label"} aria-label=${f.hasLabel}, for=${f.hasFor}`);
});

// 9. Check viewport meta tag
const viewportMeta = await page.evaluate(() => {
  const meta = document.querySelector("meta[name='viewport']");
  return meta?.getAttribute("content");
});
console.log(`\nViewport meta: ${viewportMeta || "❌ missing"}`);

// 10. Check lang attribute
const lang = await page.evaluate(() => document.documentElement.lang);
console.log(`HTML lang: ${lang || "❌ missing"}`);

// 11. Check for live regions
const liveRegions = await page.evaluate(() => {
  const regions = document.querySelectorAll("[aria-live], [role='timer'], [role='status'], [role='alert']");
  return Array.from(regions).map(r => ({
    tag: r.tagName,
    role: r.getAttribute("role"),
    ariaLive: r.getAttribute("aria-live"),
    text: r.textContent?.trim().substring(0, 50),
  }));
});
console.log("\nLive regions:");
liveRegions.forEach(r => console.log(`  <${r.tag}> role=${r.role} aria-live=${r.ariaLive} "${r.text}"`));

await browser.close();
console.log("\n=== ACCESSIBILITY AUDIT COMPLETE ===");
