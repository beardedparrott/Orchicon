import { chromium } from "playwright";

const VIEWPORTS = [
  { name: "desktop", width: 1280, height: 800 },
  { name: "tablet", width: 768, height: 1024 },
  { name: "mobile", width: 375, height: 812 },
];

const browser = await chromium.launch({ args: ["--no-sandbox", "--disable-setuid-sandbox"] });

for (const vp of VIEWPORTS) {
  const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
  const page = await context.newPage();
  
  // Collect console errors
  const consoleErrors = [];
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text());
  });
  
  // Collect network failures
  const networkErrors = [];
  page.on("requestfailed", (req) => {
    networkErrors.push(`${req.url()} - ${req.failure()?.errorText}`);
  });

  try {
    await page.goto(`http://localhost:5173/work-items`, { waitUntil: "networkidle", timeout: 30000 });
    
    // Wait a bit for any late renders
    await page.waitForTimeout(2000);
    
    // Take screenshot
    await page.screenshot({ 
      path: `/tmp/orchicon/qa-screenshots/work-items-${vp.name}.png`, 
      fullPage: true 
    });
    
    console.log(`\n=== ${vp.name} (${vp.width}x${vp.height}) ===`);
    
    // Check page title
    const title = await page.title();
    console.log(`Title: ${title}`);
    
    // Check for main heading
    const heading = await page.locator("h1").textContent().catch(() => "N/A");
    console.log(`Heading: ${heading}`);
    
    // Check for key UI elements
    const hasTreeToggle = await page.locator("button[aria-pressed]").count();
    console.log(`View toggle buttons: ${hasTreeToggle}`);
    
    const hasSelectAll = await page.locator("input[aria-label*='Select all']").count();
    console.log(`Select-all checkbox: ${hasSelectAll}`);
    
    const hasSearchInput = await page.locator("input[placeholder*='Search']").count();
    console.log(`Search input: ${hasSearchInput}`);
    
    const hasProjectSelect = await page.locator("select[aria-label='Project']").count();
    console.log(`Project select: ${hasProjectSelect}`);
    
    const hasStatusFilter = await page.locator("select[aria-label*='status']").count();
    console.log(`Status filter: ${hasStatusFilter}`);
    
    const hasKindFilter = await page.locator("select[aria-label*='type']").count();
    console.log(`Kind filter: ${hasKindFilter}`);
    
    const hasSortBy = await page.locator("select[aria-label*='Sort by']").count();
    console.log(`Sort by: ${hasSortBy}`);
    
    const hasNewButton = await page.locator("a[href*='work-items/new']").count();
    console.log(`New Work Item button: ${hasNewButton}`);
    
    const hasLiveIndicator = await page.locator("[role='timer']").count();
    console.log(`Live refresh indicator: ${hasLiveIndicator}`);
    
    // Check board columns if visible
    const boardColumns = await page.locator("section[aria-label*='column']").count();
    console.log(`Board columns: ${boardColumns}`);
    
    // Check for read-only column
    const readOnlyColumns = await page.locator("section[aria-label*='read-only']").count();
    console.log(`Read-only columns: ${readOnlyColumns}`);
    
    // Check accessibility: focus ring visibility
    const focusableElements = await page.locator("[tabindex], button, a, input, select").count();
    console.log(`Focusable elements: ${focusableElements}`);
    
    // Check for empty state or error state
    const emptyState = await page.locator("text=No work items").count();
    console.log(`Empty state visible: ${emptyState > 0}`);
    
    const errorState = await page.locator(".text-destructive").count();
    console.log(`Error state visible: ${errorState > 0}`);
    
    // Log console errors
    if (consoleErrors.length > 0) {
      console.log(`Console errors: ${consoleErrors.length}`);
      consoleErrors.forEach(e => console.log(`  - ${e}`));
    }
    
    // Log network errors (excluding expected 404s to API)
    const apiErrors = networkErrors.filter(e => !e.includes("/orchicon.api.v1/"));
    if (apiErrors.length > 0) {
      console.log(`Unexpected network errors: ${apiErrors.length}`);
      apiErrors.forEach(e => console.log(`  - ${e}`));
    }
    
    // Check viewport-specific layout issues
    const boardContainer = await page.locator(".flex.flex-1.gap-3.overflow-x-auto").first();
    if (await boardContainer.isVisible().catch(() => false)) {
      const box = await boardContainer.boundingBox();
      if (box) {
        console.log(`Board container: ${box.width}x${box.height} at (${box.x}, ${box.y})`);
        const overflowsViewport = box.x + box.width > vp.width;
        console.log(`Board overflows viewport width: ${overflowsViewport}`);
      }
    }
    
    // Check for the page root height calc
    const pageRoot = await page.locator("div[style*='calc(100vh']").first();
    if (await pageRoot.isVisible().catch(() => false)) {
      const rootBox = await pageRoot.boundingBox();
      if (rootBox) {
        console.log(`Page root: ${rootBox.width}x${rootBox.height}`);
      }
    }
    
  } catch (err) {
    console.error(`Error on ${vp.name}: ${err.message}`);
  }
  
  await context.close();
}

await browser.close();
console.log("\n=== QA screenshots saved to /tmp/orchicon/qa-screenshots/ ===");
