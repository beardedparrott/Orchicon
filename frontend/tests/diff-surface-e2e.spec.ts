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
// The two mounts hold DIFFERENT owner tuples, so their ledgers carry different
// paths. Falling back to the Ask files keeps a same-file seed working.
const EXEC_FILE_A = process.env.E2E_EXEC_FILE_A ?? FILE_A;
const EXEC_FILE_B = process.env.E2E_EXEC_FILE_B ?? FILE_B;
// The live plane is local-auth by default; supply the credentials of a seeded
// local account so the spec can reach the authenticated SPA. Optional: when
// unset (e.g. an externally pre-authenticated deployment) the helper steps aside.
const USERNAME = process.env.E2E_USERNAME ?? "";
const PASSWORD = process.env.E2E_PASSWORD ?? "";

/**
 * loginIfNeeded authenticates the browser against the live plane's local IdP.
 * The SPA keeps its access token in memory only, so a real UI login is the
 * honest way to reach the mounts — and the HttpOnly refresh cookie it sets
 * then survives the reload the resize-persistence check performs.
 */
async function loginIfNeeded(page: Page): Promise<void> {
  if (!USERNAME || !PASSWORD) return;
  // Obtain a real access token from the live plane's local IdP. Using the API
  // (not the login form) lets us seed the SPA's own sessionStorage stash — the
  // exact mechanism its OIDC callback uses (session.ts loadStashedToken) — so
  // the token survives the full-page navigations and the reload the
  // resize-persistence check performs. The response also sets the HttpOnly
  // refresh cookie on the shared browser context, so the SPA bootstrap can
  // mint fresh tokens for the whole run.
  const res = await page.request.post(`${BASE}/auth/local-login`, {
    data: { username: USERNAME, password: PASSWORD },
  });
  if (!res.ok()) {
    throw new Error(`local-login failed: ${res.status()} ${await res.text()}`);
  }
  const { access_token } = (await res.json()) as { access_token: string };
  await page.addInitScript((t: string) => {
    sessionStorage.setItem("orchicon_access_token", t);
  }, access_token);
}

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
  // ...and the pane's header names the clicked path. (The header span is a
  // sibling of `.diff-scroll`, not inside it — the diff BODY holds the hunk
  // text, the header holds the path.)
  await expect(diffRail(page).getByText(path, { exact: true })).toBeVisible();
}

/**
 * diffRail scopes to the rail's OWN container. At >= 768px that is the inline
 * <aside>; below 768px the rail is the overlay drawer (role="dialog"), which
 * renders NO <aside> at all — so a bare `.first()` grabs the wrong node (the
 * Execution page also renders Context/Messages complementaries).
 */
function diffRail(page: Page) {
  const aside = page
    .locator("aside")
    .filter({ has: page.getByRole("button", { name: "Close diff sidebar" }) });
  const drawer = page.getByRole("dialog", { name: "Diff sidebar" });
  return aside.or(drawer);
}

/**
 * The mounts poll (ListMessages / execution status), so `networkidle` never
 * settles and hangs a `goto` to the test timeout. Every navigation in this
 * spec therefore waits on `domcontentloaded` (the app shell) instead.
 */

test.describe("diff surface — GUI", () => {
  requireLive(test);

  test("Ask mount: click-through, fit, scrollbar, resize, honest states, drawer", async ({ page }) => {
    await loginIfNeeded(page);
    await page.goto(`${BASE}/ask-orchicon?conversationId=${encodeURIComponent(ASK_CONV_ID)}`, {
      waitUntil: "domcontentloaded",
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
    // The inline rail (and its resize separator) exist only >= 768px; below that
    // the rail is the overlay drawer and owns no splitter.
    const rail = page.locator('[role="separator"][aria-label="Resize diff rail"]');
    const hasRail = await rail.isVisible().catch(() => false);
    if (hasRail) {
      const before = await diffRail(page).boundingBox();
      const hb = await rail.boundingBox();
      if (before && hb) {
        // SHRINK the rail: growing hits the clamp that protects the chat
        // column at narrow-but-inline widths (e.g. 768px tablet), which would
        // make a growth assertion fail for the wrong reason. A shrink always
        // has headroom above MIN_RAIL_WIDTH.
        await page.mouse.move(hb.x + hb.width / 2, hb.y + hb.height / 2);
        await page.mouse.down();
        await page.mouse.move(hb.x - 80, hb.y + hb.height / 2, { steps: 8 });
        await page.mouse.up();
        await expect
          .poll(async () => (await diffRail(page).boundingBox())?.width ?? 0)
          .toBeLessThan(before.width);
        const resized = (await diffRail(page).boundingBox())?.width ?? 0;

        await page.reload({ waitUntil: "domcontentloaded" });
        await openDiffRail(page, "ask-diff-sidebar-trigger");
        await expect
          .poll(async () => (await diffRail(page).boundingBox())?.width ?? 0)
          .toBeCloseTo(resized, 0);
      }
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

    // Evidence artifact for the acceptance review: the live rail (viewport +
    // theme are the Playwright project's, so the light/dark and sub-768px
    // claims cite a real capture).
    await page.screenshot({ path: `test-results/diff-surface-ask-${test.info().project.name}.png`, fullPage: false });
  });

  test("Execution mount: pane opens and renders the (execution, id) ledger", async ({ page }) => {
    await loginIfNeeded(page);
    await page.goto(`${BASE}/executions/${encodeURIComponent(EXEC_ID)}`, { waitUntil: "domcontentloaded" });
    await openDiffRail(page, "execution-diff-sidebar-trigger");
    await expect(page.locator(".diff-scroll").first()).toBeVisible();
    // Click-through on this mount too — with THIS mount's own ledger paths.
    await assertClickThrough(page, "Tree", EXEC_FILE_B);
    await assertClickThrough(page, "Timeline", EXEC_FILE_A);
  });

  test("honest states: empty ledger renders empty, failed fetch renders the error banner", async ({ page }) => {
    await loginIfNeeded(page);
    // EMPTY: the durable fetch returns a genuinely empty ledger.
    await page.route(`**${GET_EDITS_SUFFIX}*`, (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ edits: [], maxSeq: "0" }) }),
    );
    await page.goto(`${BASE}/ask-orchicon?conversationId=${encodeURIComponent(ASK_CONV_ID)}`, {
      waitUntil: "domcontentloaded",
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
    await page.reload({ waitUntil: "domcontentloaded" });
    await openDiffRail(page, "ask-diff-sidebar-trigger");
    // THE PIN: a failed ledger fetch renders the explicit alert banner — an
    // unreachable ledger must never look like a plainly-empty one.
    await expect(page.getByRole("alert")).toBeVisible();
    await expect(page.getByRole("alert")).toContainText(/Couldn't load file edits/);
    // NEW DEFECT (found live, outside the six — filed as a new work item, NOT
    // folded into this feature): DiffSidebar renders the banner ABOVE the tab
    // content and still feeds the empty tab `files=[]`, so the GUI shows the
    // banner AND "No changed files." together. DiffSidebar.tsx:234 claims the
    // empty text "is then reachable only when the fetch succeeded AND the
    // ledger is genuinely empty" — that contract does not hold (the TUI, by
    // contrast, returns early on Err and shows the banner alone, model.go:689).
    // The criterion pins the BANNER, which is asserted above; the coexistence
    // is recorded here rather than enforced, so the pin stays truthful.
  });

  test("narrow viewport (<768px): the drawer overlay forces the unified diff", async ({ page }) => {
    const vw = page.viewportSize()?.width ?? 0;
    test.skip(vw >= 768, "the overlay drawer exists only below 768px");
    await loginIfNeeded(page);
    await page.goto(`${BASE}/ask-orchicon?conversationId=${encodeURIComponent(ASK_CONV_ID)}`, {
      waitUntil: "domcontentloaded",
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
