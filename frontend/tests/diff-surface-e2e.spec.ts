/**
 * diff-surface-e2e.spec.ts — the GUI half of the terminal end-to-end proof for
 * the diff-surface feature (both mounts, live, in a real browser).
 *
 * WHY A PLAYWRIGHT SPEC. This repo has no jsdom / @testing-library — component
 * contracts are asserted with `renderToStaticMarkup`, which is exactly the
 * "satisfied on paper" evidence this feature's terminal verification item exists
 * to prevent. A real-browser spec against the running SPA + dev plane is the
 * only GUI surface that can prove click-through focus, fit, a grabbable
 * scrollbar, a drag-resizable rail, and the sub-768px drawer.
 *
 * RUN IT LIVE (see the acceptance review for the exact recipe):
 *
 *   E2E_ASK_CONV_ID=<conversation id>   # a conversation with >= 2 file-edit ledger rows
 *   E2E_EXEC_ID=<execution id>          # an execution with >= 2 file-edit ledger rows
 *   E2E_FILE_A=<path>  E2E_FILE_B=<path>  # the two changed files the ledger holds
 *   PLAYWRIGHT_BASE_URL=http://localhost:5173 npx playwright test tests/diff-surface-e2e.spec.ts
 *
 * The main claims run against the REAL ledger (no fixtures, no mocks). Only the
 * two "honest state" subtests intercept the Connect `GetSessionFileEdits`
 * response — that is how a genuinely empty ledger and a genuinely failed fetch
 * are produced deterministically, and the architecture note sanctions exactly
 * that interception for those two states.
 *
 * The six projects in playwright.config.ts give the light/dark (defect 5) and
 * the sub-768px viewport (the drawer) for free.
 */
import { test, expect, type Page } from "@playwright/test";

const RAW_BASE = process.env.PLAYWRIGHT_BASE_URL || "http://localhost:5173";
// A trailing slash makes url.resolve swallow the path; strip it.
const BASE = RAW_BASE.replace(/\/+$/, "");

const ASK_CONV_ID = process.env.E2E_ASK_CONV_ID ?? "";
const EXEC_ID = process.env.E2E_EXEC_ID ?? "";
const FILE_A = process.env.E2E_FILE_A ?? "";
const FILE_B = process.env.E2E_FILE_B ?? "";

/** The FileEditService Connect method suffix (matches the proto path). */
const GET_EDITS_SUFFIX = "/orchicon.api.v1.FileEditService/GetSessionFileEdits";

/**
 * requireLive skips the spec when the live ids are not supplied: the whole
 * point is a live run, and silently passing against an empty page would be the
 * "on paper" failure this item exists to prevent.
 */
function requireLive(test_: typeof test): void {
  test_.skip(
    !ASK_CONV_ID || !EXEC_ID || !FILE_A || !FILE_B,
    "live diff-surface run needs E2E_ASK_CONV_ID / E2E_EXEC_ID / E2E_FILE_A / E2E_FILE_B",
  );
}

/** openDiffRail opens the mount's diff rail via its trigger button. */
async function openDiffRail(page: Page, testid: string): Promise<void> {
  const trigger = page.locator(`[data-testid="${testid}"]`);
  await expect(trigger).toBeVisible();
  const open = await trigger.getAttribute("aria-expanded");
  if (open !== "true") {
    await trigger.click();
  }
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
}

/** diffTabButton returns the rail's Diff | Tree | Timeline tab button. */
function diffTabButton(page: Page, label: "Diff" | "Tree" | "Timeline") {
  return page.getByRole("button", { name: label, exact: true });
}

/** assertClickThrough clicks a Tree OR Timeline row and asserts the Diff tab
 * becomes active and renders that row's path.
 *
 * THE DEFECT THIS PINS (1+2): a row click in EITHER list tab must select the
 * file AND focus its diff. The two tabs call the same onSelect → onTabChange
 * ("diff") path in DiffTree.tsx / DiffTimeline.tsx, so both are exercised. */
async function assertClickThrough(page: Page, tab: "Tree" | "Timeline", path: string): Promise<void> {
  await diffTabButton(page, tab).click();
  // The row for `path` is a button whose accessible text contains the path.
  const row = page.getByRole("button").filter({ hasText: path }).first();
  await expect(row).toBeVisible();
  await row.click();
  // The Diff tab is now active...
  await expect(diffTabButton(page, "Diff")).toHaveAttribute("aria-pressed", "true");
  // ...and the diff body names the clicked path (DiffView's header span).
  const body = page.locator(".diff-scroll");
  await expect(body.filter({ hasText: path.split("/").pop()! }).first()).toBeVisible();
}

test.describe("diff surface — GUI", () => {
  requireLive(test);

  test("Ask mount: click-through, fit, scrollbar, resize, honest states, drawer", async ({ page }) => {
    await page.goto(`${BASE}/ask-orchicon?conversationId=${encodeURIComponent(ASK_CONV_ID)}`, {
      waitUntil: "networkidle",
    });
    await openDiffRail(page, "ask-diff-sidebar-trigger");

    // The rail rendered the real ledger.
    await expect(page.locator(".diff-scroll").first()).toBeVisible();

    // --- 1+2 CLICK-THROUGH, both tabs, both clients' gesture.
    await assertClickThrough(page, "Tree", FILE_B);
    await assertClickThrough(page, "Timeline", FILE_A);

    // --- 3 FIT: no horizontal scrolling on the diff container.
    const overflow = await page.locator(".diff-scroll").first().evaluate(
      (el) => {
        const h = el as HTMLElement;
        return { sw: h.scrollWidth, cw: h.clientWidth };
      },
    );
    expect(overflow.sw, "diff container must not scroll horizontally (fit)").toBeLessThanOrEqual(
      overflow.cw + 1,
    );

    // --- 5 SCROLLBAR: present and grabbable (light AND dark projects run this).
    const scrollEl = page.locator(".diff-scroll").first();
    const metrics = await scrollEl.evaluate((el) => {
      const h = el as HTMLElement;
      return { sh: h.scrollHeight, ch: h.clientHeight, gutter: h.offsetWidth - h.clientWidth };
    });
    if (metrics.sh > metrics.ch) {
      // A visible scrollbar reserves a gutter (a scrollbar-less overlay would
      // report 0). Then confirm it is grabbable by scrolling and observing.
      expect(metrics.gutter, "an overflowing diff must show a visible scrollbar gutter").toBeGreaterThan(0);
      await scrollEl.evaluate((el) => {
        (el as HTMLElement).scrollTop = (el as HTMLElement).scrollHeight;
      });
      const moved = await scrollEl.evaluate((el) => (el as HTMLElement).scrollTop);
      expect(moved, "the diff scrollbar must move the viewport").toBeGreaterThan(0);
    }

    // --- 6 RESIZE: drag the handle; the RENDERED width changes; reload keeps it.
    const rail = page.locator('[role="separator"][aria-label="Resize diff rail"]');
    await expect(rail).toBeVisible();
    const before = await page.locator("aside").first().boundingBox();
    const hb = await rail.boundingBox();
    if (before && hb) {
      await page.mouse.move(hb.x + hb.width / 2, hb.y + hb.height / 2);
      await page.mouse.down();
      await page.mouse.move(hb.x + 120, hb.y + hb.height / 2, { steps: 8 });
      await page.mouse.up();
      await expect
        .poll(async () => (await page.locator("aside").first().boundingBox())?.width ?? 0)
        .toBeGreaterThan(before.width);
      const resized = (await page.locator("aside").first().boundingBox())?.width ?? 0;

      await page.reload({ waitUntil: "networkidle" });
      await openDiffRail(page, "ask-diff-sidebar-trigger");
      await expect
        .poll(async () => (await page.locator("aside").first().boundingBox())?.width ?? 0)
        .toBeCloseTo(resized, 0);
    }

    // --- The chat column stays usable throughout (>= the rail's min chat width).
    const chat = await page
      .locator("main, [data-testid='ask-chat-column']")
      .first()
      .boundingBox()
      .catch(() => null);
    if (chat) {
      expect(chat.width, "the chat column must stay usable beside the rail").toBeGreaterThan(200);
    }
  });

  test("Execution mount: pane opens and renders the (execution, id) ledger", async ({ page }) => {
    await page.goto(`${BASE}/executions/${encodeURIComponent(EXEC_ID)}`, { waitUntil: "networkidle" });
    await openDiffRail(page, "execution-diff-sidebar-trigger");
    await expect(page.locator(".diff-scroll").first()).toBeVisible();
    // Click-through on this mount too.
    await assertClickThrough(page, "Tree", FILE_B);
    await assertClickThrough(page, "Timeline", FILE_A);
  });

  test("honest states: empty ledger renders empty, failed fetch renders the error banner", async ({ page }) => {
    // EMPTY: the durable fetch returns a genuinely empty ledger.
    await page.route(`**${GET_EDITS_SUFFIX}*`, (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ edits: [], maxSeq: "0" }) }),
    );
    await page.goto(`${BASE}/ask-orchicon?conversationId=${encodeURIComponent(ASK_CONV_ID)}`, {
      waitUntil: "networkidle",
    });
    await openDiffRail(page, "ask-diff-sidebar-trigger");
    await diffTabButton(page, "Tree").click();
    await expect(page.getByText(/No changed files|No file edits for this session yet\./)).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);

    // FAILED: the durable fetch errors — the explicit banner, never the empty text.
    await page.unroute(`**${GET_EDITS_SUFFIX}*`);
    await page.route(`**${GET_EDITS_SUFFIX}*`, (route) =>
      route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ code: "internal", message: "ledger unavailable" }) }),
    );
    await page.reload({ waitUntil: "networkidle" });
    await openDiffRail(page, "ask-diff-sidebar-trigger");
    await expect(page.getByRole("alert")).toBeVisible();
    await expect(page.getByText(/No changed files|No file edits for this session yet\./)).toHaveCount(0);
  });

  test("narrow viewport (<768px): the drawer overlay forces the unified diff", async ({ page }) => {
    const vw = page.viewportSize()?.width ?? 0;
    test.skip(vw >= 768, "the overlay drawer exists only below 768px");
    await page.goto(`${BASE}/ask-orchicon?conversationId=${encodeURIComponent(ASK_CONV_ID)}`, {
      waitUntil: "networkidle",
    });
    await openDiffRail(page, "ask-diff-sidebar-trigger");
    const dialog = page.getByRole("dialog", { name: "Diff sidebar" });
    await expect(dialog).toBeVisible();
    await expect(dialog).toHaveAttribute("aria-modal", "true");
    // Forced unified: no side-by-side two-column grid inside the drawer.
    await expect(dialog.locator(".grid.grid-cols-2")).toHaveCount(0);
    await expect(dialog.locator(".diff-scroll").first()).toBeVisible();
  });
});
