import { test, expect } from "@playwright/test";
test("legacy redirects and deep links", async ({ page }) => {
  await page.goto("/ask-orchicon", { waitUntil: "networkidle" });
  await expect(page).toHaveURL(/\/ask-orchicon/);
  for (const route of ["/projects","/work-items","/workers","/workflows"]) {
    await page.goto(route, { waitUntil: "networkidle" });
    await expect(page.locator("body")).toBeVisible();
  }
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/ask-orchicon", { waitUntil: "networkidle" });
  const pulse = page.locator(".animate-pulse").first();
  if (await pulse.count() > 0) {
    const style = await pulse.evaluate((el) => getComputedStyle(el).animationName);
    expect(["none",""]).toContain(style === "none" ? "none" : style === "" ? "" : style);
  }
});
