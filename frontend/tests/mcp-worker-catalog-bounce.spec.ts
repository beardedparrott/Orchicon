/**
 * mcp-worker-catalog-bounce.spec.ts — the CLICK-level repro for the reported defects on a WORKER page:
 *
 *   (a) "When adding an MCP server or one from the catalog on workers, it still bounces you back to the
 *        worker detail page out of the edit page."
 *   (b) "It is NOT allowing you to save MCP servers in the GUI currently even though it looks like you
 *        can. This is broken."
 *
 * WHY THIS SPEC EXISTS AT ALL — and this is the honest reason, not a boast.
 *
 * MCPServersPanel.formNesting.test.tsx asserts the MARKUP invariant (that no control inside the panel
 * renders a submit button). Its own header states the limit: the repo ships no jsdom, so the contract
 * is asserted on `renderToStaticMarkup` output and it NEVER CLICKS. That is how (a) survived a green
 * suite: "no button declares type=submit" is not the same claim as "a click does not submit the host
 * form", and only a real click in a real browser can carry the second one.
 *
 * And (b) is a claim about PERSISTENCE — "it looks like you can save but it doesn't" — which no
 * component test can carry at all, because the verdict lives in the plane's own record of the version.
 * So the second test's final assertion reads the version back from the API: the operator's view says
 * what it looked like, the plane's record says whether it happened.
 *
 * RUN IT (dev instance, against the bundle the plane itself serves — see playwright.mcp-repro.config.ts):
 *
 *   cd frontend
 *   E2E_WORKER_ID=<a SCRATCH worker with a DRAFT version> \
 *   E2E_USERNAME=orchicon E2E_PASSWORD=orchicon \
 *   PLAYWRIGHT_BASE_URL=http://localhost:8080 \
 *   npx playwright test --config playwright.mcp-repro.config.ts tests/mcp-worker-catalog-bounce.spec.ts
 *
 * THE SECOND TEST WRITES, so it must run against a scratch worker: it adds a catalog entry to the
 * version and SAVES the version (the only way a worker-version spec is persisted — see below).
 */
import { test, expect, type Page } from "@playwright/test";
import { writeFileSync } from "node:fs";

const BASE = (process.env.PLAYWRIGHT_BASE_URL || "http://localhost:8080").replace(/\/+$/, "");
const WORKER_ID = process.env.E2E_WORKER_ID ?? "";
const USERNAME = process.env.E2E_USERNAME ?? "";
const PASSWORD = process.env.E2E_PASSWORD ?? "";
// Optional evidence dump. The spec writes NOTHING unless this is set, so a normal run leaves no files
// behind; a diagnosis run points it at a scratch path and gets the attempt's payloads and its timing.
const OUT = process.env.E2E_REPRO_OUT ?? "";

/** The Connect method whose invocation IS defect (a) (the host form's onSubmit saves the version). */
const UPDATE_VERSION_RPC = "/orchicon.api.v1.WorkerService/UpdateWorkerVersion";
const LIST_VERSIONS_RPC = "/orchicon.api.v1.WorkerService/ListWorkerVersions";

/**
 * login seeds the SPA's own session the way its OIDC callback does (session.ts loadStashedToken), so
 * the token survives the full-page navigation. Returns the token so a test can also read the plane
 * directly. Mirrors tests/diff-surface-e2e.spec.ts.
 */
async function login(page: Page): Promise<string> {
  const res = await page.request.post(`${BASE}/auth/local-login`, {
    data: { username: USERNAME, password: PASSWORD },
  });
  expect(res.ok(), `local-login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const { access_token } = (await res.json()) as { access_token: string };
  await page.addInitScript((t: string) => {
    sessionStorage.setItem("orchicon_access_token", t);
  }, access_token);
  return access_token;
}

/** latestVersion reads the worker's versions from the PLANE — the record, not the screen. */
async function latestVersion(page: Page, token: string): Promise<Record<string, unknown>> {
  const res = await page.request.post(`${BASE}${LIST_VERSIONS_RPC}`, {
    headers: { Authorization: `Bearer ${token}` },
    data: { workerId: WORKER_ID },
  });
  expect(res.ok(), `ListWorkerVersions failed: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { versions?: Record<string, unknown>[] };
  const versions = body.versions ?? [];
  expect(versions.length, "the scratch worker has a version").toBeGreaterThan(0);
  return versions[versions.length - 1];
}

/** openEditMode enters the draft form, which is mounted only while editing. */
async function openEditMode(page: Page): Promise<void> {
  await page.goto(`${BASE}/workers/${WORKER_ID}`, { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Edit", exact: true }).first().click();
  await expect(page.locator("#draftForm")).toBeVisible();
}

test.describe("a worker version's MCP catalog Add", () => {
  test.skip(
    !WORKER_ID || !USERNAME || !PASSWORD,
    "needs E2E_WORKER_ID + E2E_USERNAME/E2E_PASSWORD (this is a live repro, not a fixture test)",
  );

  test("(a) does not submit the draft form and does not leave edit mode", async ({ page }) => {
    const updateCalls: string[] = [];
    page.on("request", (r) => {
      if (r.method() === "POST" && r.url().includes(UPDATE_VERSION_RPC)) updateCalls.push(r.url());
    });

    await login(page);
    await openEditMode(page);

    // The control from the report. The manual control is "Add server", so "Add" alone is the Registry
    // catalog's per-row Add (MCPServersPanel.tsx:748) — the one that bounced the operator.
    const catalogAdd = page.getByRole("button", { name: "Add", exact: true }).first();
    await expect(catalogAdd).toBeVisible();
    await catalogAdd.click();

    // (1) the click reached the handler: the inline add card opens (it renders only when showForm).
    await expect(page.getByText("Add MCP server", { exact: true })).toBeVisible();

    // (2) THE DEFECT: edit mode survives and the host form was not submitted.
    await expect(page.locator("#draftForm"), "the draft form unmounted — the catalog Add submitted it").toBeVisible();

    // The MANUAL path from the same report ("adding an MCP server OR one from the catalog"). It is a
    // different control ("Add server") and a different code path from the catalog Add, so it is
    // exercised on its own rather than assumed to be the same case.
    await page.getByRole("button", { name: "Add server", exact: true }).click();
    await expect(page.getByText("Add MCP server", { exact: true })).toBeVisible();
    await expect(page.locator("#draftForm"), "the manual Add server unmounted the draft form").toBeVisible();

    // (3) the mechanism, so a failure names the cause instead of describing the symptom.
    expect(updateCalls, "adding a server submitted the worker form").toEqual([]);
  });

  test("(b) a catalog-added spec is persisted by the version Save", async ({ page }) => {
    const token = await login(page);

    // The Save's request body is captured, because "the spec did not persist" has two very different
    // causes and they are separable here: the browser never SENT it (a client bug), or the plane
    // received it and did not keep it (a plane bug). Asserting on the body names which one it was
    // instead of leaving the next reader to guess.
    // EVERY save body is kept, not just the last. If the UI issues more than one UpdateWorkerVersion (or
    // one with an empty permissions string), the FINAL stored state is decided by whichever arrives last
    // — and a single "last body" would hide exactly that. The array is dumped for inspection.
    const saveBodies: string[] = [];
    page.on("request", (r) => {
      if (r.method() === "POST" && r.url().includes(UPDATE_VERSION_RPC)) saveBodies.push(r.postData() ?? "");
    });

    // THE BASELINE IS ESTABLISHED HERE, NOT ASSUMED. The first version of this test required a pristine
    // scratch worker and read whatever it found — which made it order-dependent (a previous run's save
    // left an entry behind and the precondition failed) and is why an earlier attempt appeared to show
    // the plane dropping the spec when the real cause was the test's own stale starting state. Clearing
    // permissions through the API here makes the test repeatable in any order, any number of times.
    await page.request.post(`${BASE}${UPDATE_VERSION_RPC}`, {
      headers: { Authorization: `Bearer ${token}` },
      data: { workerId: WORKER_ID, versionId: await latestVersion(page, token).then((v) => v.id), permissions: "{}" },
    });
    const before = await latestVersion(page, token);
    expect(String(before.permissions ?? ""), "cleared baseline is empty").not.toContain("Filesystem");

    await openEditMode(page);

    // The name is asserted by COUNT rather than by position, because the panel legitimately renders it
    // in more than one place (the catalog row AND the version's own entry). What matters is the
    // direction: an empty version names it once, and after the add it is named more than once.
    const nameMatches = () => page.getByText("Filesystem", { exact: true });
    await expect(nameMatches(), "an empty version names it only in the catalog row").toHaveCount(1);

    // A worker version has no definition row: the catalog Add PREFILLS the panel's add form, and the
    // panel's Create writes the spec into the VERSION's own array (client state). Only the host form's
    // Save persists it — which is why (b) is a persistence claim, not a form claim.
    await page.getByRole("button", { name: "Add", exact: true }).first().click();
    await expect(page.getByText("Add MCP server", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Create", exact: true }).click();

    await expect
      .poll(async () => nameMatches().count(), {
        message: "the catalog add did not land in the version's own list",
      })
      .toBeGreaterThan(1);

    // Save the VERSION (the host form's submit; the panel's own Save is not mounted now that append is
    // done). Edit mode closing is the client's signal that the save resolved.
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(
      page.getByRole("button", { name: "Save", exact: true }),
      "the version save did not resolve (edit mode stayed open — look for a save error on the page)",
    ).toBeHidden({ timeout: 20_000 });

    // THE SPLIT: did the browser actually send the spec? The message carries the body either way, so a
    // failure here points at the client, and a pass here points at the plane.
    expect(saveBodies.join("\n"), `UpdateWorkerVersion bodies were: ${JSON.stringify(saveBodies)}`).toContain("Filesystem");

    // THE VERDICT, from the plane's own record rather than the screen.
    //
    // IT IS POLLED WITH A TIMER, because the interesting quantity is not pass/fail — the spec does become
    // visible — but HOW LONG the gap is between the save resolving and the write being readable. That gap
    // is the operator's experience: they save, look, and the servers are not there. Measuring it turns
    // "it feels broken" into a number a fix can be judged against.
    const t0 = Date.now();
    let seen = "";
    for (;;) {
      seen = String((await latestVersion(page, token)).permissions ?? "");
      if (seen.includes("Filesystem")) break;
      if (Date.now() - t0 > 15_000) break;
      await page.waitForTimeout(250);
    }
    const windowMs = Date.now() - t0;
    if (OUT) writeFileSync(OUT, JSON.stringify({ saveBodies, windowMs }, null, 1), "utf8");
    expect(windowMs, `the saved spec never became visible; last read: ${seen}`).toBeLessThan(15_000);

    // And the operator's view agrees after a reload: the spec is still listed beside the catalog row.
    await openEditMode(page);
    await expect
      .poll(async () => page.getByText("Filesystem", { exact: true }).count(), {
        message: "the saved spec is not listed after a reload",
      })
      .toBeGreaterThan(1);
  });
});
