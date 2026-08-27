import { test, expect } from "@playwright/test";
test("only /ask-orchicon shows conversation history panel", async ({ page }) => {
  const guarded = ["/dashboard","/projects","/work-items","/workers","/workflows","/executions","/telemetry","/cost-explorer","/settings"];
  for (const route of guarded) {
    await page.goto(route, { waitUntil: "networkidle" });
    await expect(page.locator('[data-testid="conversation-history-panel"]')).toHaveCount(0);
  }
  await page.goto("/ask-orchicon", { waitUntil: "networkidle" });
  await expect(page.locator('[data-testid="conversation-history-panel"]')).toBeVisible();
});
test("conversation panel collapse via keyboard", async ({ page }) => {
  await page.goto("/ask-orchicon", { waitUntil: "networkidle" });
  const collapse = page.getByLabel(/Collapse conversation history/i);
  await expect(collapse).toBeVisible();
  await collapse.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator('[data-testid="conversation-history-panel"]')).toBeHidden();
  const expand = page.getByLabel(/Expand conversation history/i);
  await expect(expand).toBeVisible();
  await expand.focus();
  await page.keyboard.press("Space");
  await expect(page.locator('[data-testid="conversation-history-panel"]')).toBeVisible();
});
