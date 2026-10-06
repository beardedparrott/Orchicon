/**
 * activity-line-e2e.spec.ts — the GUI half of the activity-line end-to-end capstone.
 *
 * WHY A PLAYWRIGHT SPEC. This repo has no jsdom / @testing-library — component contracts are
 * asserted with `renderToStaticMarkup`, which is exactly the "satisfied on paper" evidence the
 * feature's own honesty check exists to prevent. A real-browser spec against the running SPA is the
 * only GUIsurface that can prove the line survives a REAL streaming turn, carries counters that
 * reconcile with the server's ledger, and yields to the watchdog's verdict.
 *
 * RUN IT LIVE (see qa-report-activity-line-e2e.md for the exact recipe):
 *
 *   # 1. the SHARED fixture plane (one process, both clients)
 *   go run ./internal/testfixtures/activitye2e/cmd/activitye2e -addr 127.0.0.1:8080 &
 *   # 2. the SPA dev server proxying /orchicon.api.v1 -> :8080
 *   cd frontend && npx vite --host 127.0.0.1 --port 5173 &
 *   # 3. the TUI leg (writes qa-evidence/activity-line-e2e/tui-cross-client.json)
 *   ORCH_ACTIVITY_E2E=1 ORCH_PTY_SMOKE=1 ... go test ./internal/tui -run TestActivityLineE2E
 *   # 4. this spec
 *   PLAYWRIGHT_BASE_URL=http://127.0.0.1:5173 npx playwright test tests/activity-line-e2e.spec.ts \
 *     --project=dark-desktop
 *
 * THE LOCATOR IS THE RENDERED DOM, NOT A HOOK. `[data-testid="ask-activity-line"]` is
 * ActivityLine.tsx's own contract (role="status", aria-live="polite"), so the assertion is on what a
 * browser paints rather than on a store's internal state.
 *
 * WHY NOT THE PLANE'S OTHER ROUTES. The SPA authenticates over plain HTTP (/auth/session,
 * /auth/local-login) — the fixture plane answers those, so the spec reaches the real authenticated
 * route without a login form.
 */
import { test, expect, type Page } from "@playwright/test";
import { readFileSync, mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";

const BASE = (process.env.PLAYWRIGHT_BASE_URL || "").replace(/\/+$/, "");
// The fixture plane the TUI leg also talked to (host-side, not through the Vite proxy — the control
// surface is a harness route and is deliberately not proxied). It defaults to the port the dedicated
// playwright.activity-e2e.config.ts starts its plane on, so `--config playwright.activity-e2e.config.ts`
// is self-contained.
const PLANE = (process.env.E2E_PLANE_URL || "http://127.0.0.1:18080").replace(/\/+$/, "");

const CONV_ID = process.env.E2E_ACTIVITY_CONV_ID || "conv-activity-e2e";
const OUT_DIR = process.env.E2E_ACTIVITY_OUT || "qa-evidence/activity-line-e2e";
const CROSS = resolve(process.cwd(), "..", OUT_DIR, "tui-cross-client.json");

/** The line's own testid — ActivityLine.tsx's contract, not a hook. */
const LINE = '[data-testid="ask-activity-line"]';

const WITNESS_CONTENT = "E2EWITNESSCONTENT";

function ensureOut(): void {
  try {
    mkdirSync(resolve(process.cwd(), "..", OUT_DIR), { recursive: true });
  } catch {
    /* best effort */
  }
}

async function dump(page: Page, name: string): Promise<void> {
  ensureOut();
  try {
    await page.screenshot({
      path: resolve(process.cwd(), "..", OUT_DIR, `gui-${name}.png`),
      fullPage: false,
    });
  } catch {
    /* screenshot is evidence, not the assertion */
  }
}

/** phase drives the shared fixture plane's control surface. */
async function phase(p: string): Promise<void> {
  const res = await fetch(`${PLANE}/__e2e/phase?p=${p}`, { method: "POST" });
  if (!res.ok) throw new Error(`control surface refused phase ${p}: ${res.status}`);
}

async function reset(p: string): Promise<void> {
  const res = await fetch(`${PLANE}/__e2e/reset?p=${p}`, { method: "POST" });
  if (!res.ok) throw new Error(`control surface refused reset ${p}: ${res.status}`);
}

/** end marks the running turn over exactly as a completed reply does: turn_in_flight false and a
 *  Done on the open stream. It is the control the TUI leg drives too (Plane.EndTurn), so "the turn
 *  ends" is one observable fact and not two clients' opinions of it. */
async function endTurn(): Promise<void> {
  const res = await fetch(`${PLANE}/__e2e/end`, { method: "POST" });
  if (!res.ok) throw new Error(`control surface refused end: ${res.status}`);
}

/** reissue lands a FRESH burst of tool calls on the running turn (the browser's half of
 *  Plane.Reissue). Past a 30s stall the earlier calls have honestly aged out of the summarizer's
 *  rolling window, so a leg that needs the counters BACK must earn them with new work rather than
 *  rewind a clock — the same move the TUI leg makes. */
async function reissue(): Promise<void> {
  const res = await fetch(`${PLANE}/__e2e/reissue`, { method: "POST" });
  if (!res.ok) throw new Error(`control surface refused reissue: ${res.status}`);
}

/**
 * lineText returns the line's PAINTED text, or null when the line is absent.
 *
 * IT MUST NOT READ THE WHOLE REGION. ActivityLine.tsx puts the rotating verb + counters in an
 * aria-hidden VISIBLE span and the stable screen-reader string ("Orchicon is working · 1 modify")
 * in a separate `sr-only` span. `innerText` on the region folds BOTH in (sr-only is clipped, not
 * display:none), so a counter that the visible line correctly DROPPED would reappear from the
 * announcement and make AC5 fail on a line that is in fact correct. Reading the span that carries
 * "Orchicon is" is reading what the operator sees — which is the whole point of asserting on the DOM.
 */
async function lineText(page: Page): Promise<string | null> {
  const l = page.locator(LINE);
  if ((await l.count()) === 0) return null;
  if (!(await l.first().isVisible())) return null;
  const visible = l.first().locator('span[aria-hidden="true"]').filter({ hasText: "Orchicon is" });
  if ((await visible.count()) === 0) return null;
  return (await visible.first().innerText()).replace(/\s+/g, " ").trim();
}

/** The rotating word from a painted line ("Orchicon is <word>…"). */
function verbWord(line: string): string {
  const m = /Orchicon is ([a-z]+)/.exec(line);
  return m ? m[1] : "";
}

/** The count half, dropping the clients' own trailing "newest call Ns ago" age. */
function countHalf(line: string): string {
  const i = line.indexOf("…");
  if (i < 0) return "";
  let rest = line.slice(i + 1).replace(/^\s*·\s*/, "").trim();
  if (rest.startsWith("no output for") || rest.startsWith("last activity")) return "";
  const j = rest.lastIndexOf("· newest call ");
  if (j >= 0) rest = rest.slice(0, j).trim();
  return rest;
}

// The counter vocabulary is the summarizer's OWN bucket words (toolclass countLabel), kept in step
// with the shared contract: `other tools?` is the catch-all the under-report fix added, so a turn
// whose only counted work is product tools still reads as a counter here. Mirrored byte-for-byte by
// counterRE in internal/tui/activity_e2e_pty_test.go.
const COUNTER_RE = /\d+ (modif(?:y|ies)|reads?|bash|other tools?)/;

/** seedSession stashes a token on the app's OWN bootstrap path (session.ts loadStashedToken), so the
 *  router guard resolves an authenticated session and /ask-orchicon renders for real. */
async function seedSession(page: Page): Promise<void> {
  await page.addInitScript(() => {
    try {
      sessionStorage.setItem("orchicon_access_token", "e2e-access-token");
    } catch {
      /* storage may be unavailable; the refresh-cookie path covers it */
    }
  });
}

test.describe("activity line through the real GUI pane", () => {
  test("the line is present mid-reply, carries reconciled counters, rotates, and yields to escalation", async ({
    page,
  }) => {
    test.setTimeout(240_000);
    await reset("started");
    await seedSession(page);

    // NEVER networkidle: the pane polls while a turn runs (see the diff-surface recipe).
    await page.goto(`${BASE}/ask-orchicon?conversationId=${CONV_ID}`, { waitUntil: "domcontentloaded" });

    // THE CONVERSATION PANE IS UP AND THIS CONVERSATION IS OPEN. The GUI's own header is the h2 with
    // the conversation title (the TUI's "Conversation:" field row has no GUI counterpart), and the
    // transcript carries the fixture's user message. Both are the real DOM, not a hook.
    await expect(page.getByRole("heading", { name: "activity-e2e", level: 2 })).toBeVisible({
      timeout: 30_000,
    });
    await expect(page.getByText("E2EPROMPT", { exact: false }).first()).toBeVisible({
      timeout: 30_000,
    });

    // -----------------------------------------------------------------
    // OBSERVATION 1 — the turn has just started and counted nothing.
    //
    // THE TURN IS RESET TO `started` FIRST, whose scripted work is deliberately HELD until the phase
    // flips to `flight`. That makes "just started, nothing counted yet" a STEADY state instead of a
    // race against the scripted calls.
    // -----------------------------------------------------------------
    const composer = page.getByRole("textbox", { name: "Ask Orchicon Anything..." });
    await composer.click();
    await composer.fill("E2EPROMPT inspect files and run the tests");
    await composer.press("Enter");

    await expect(page.locator(LINE)).toHaveAttribute("role", "status");
    await expect(page.locator(LINE)).toContainText("Orchicon is", { timeout: 20_000 });
    const bare = await lineText(page);
    expect(bare, "the activity line must be present the moment the turn starts").toBeTruthy();
    await dump(page, "01-turn-started-bare-line");
    expect(bare!, "no counter may appear before a tool call lands").not.toMatch(COUNTER_RE);

    // -----------------------------------------------------------------
    // OBSERVATION 2/3 — counters land, grow, and the line is STILL there once content has arrived
    // (AC3: the regression this feature fixes).
    // -----------------------------------------------------------------
    // RELEASE THE TURN: the scripted work now lands, one call at a time.
    await phase("flight");
    await expect(page.locator(LINE)).toContainText(/\d+ read/, { timeout: 40_000 });
    await dump(page, "02-counter-first-call");

    await expect(page.getByText(WITNESS_CONTENT).first()).toBeVisible({ timeout: 30_000 });
    await expect(page.locator(LINE)).toContainText(/1 modify/, { timeout: 60_000 });
    await dump(page, "03-ac3-content-and-line-same-frame");

    // THE AC3 ASSERTION, from ONE observation: content is rendered AND the line is STILL there with
    // its counter. This is what the pre-change build failed.
    expect(await page.getByText(WITNESS_CONTENT).first().isVisible()).toBe(true);
    const mid = await lineText(page);
    expect(mid, "AC3: the reply body has content but the activity line VANISHED — the exact " +
      "regression the feature fixes").toBeTruthy();
    expect(mid!, "AC3: the surviving line lost its counter").toMatch(COUNTER_RE);

    // -----------------------------------------------------------------
    // OBSERVATION 4 — AC4: the counts reconcile with the SERVER's own ledger.
    // -----------------------------------------------------------------
    const state = await (await fetch(`${PLANE}/__e2e/state`)).json();
    const serverCount = countHalf(`… ${state.counter ?? ""}`);
    const guiCount = countHalf(mid!);
    expect(serverCount, "fixture: the server's own ledger render is empty").not.toBe("");
    expect(guiCount, `AC4: the GUI counter must equal the server's own render of the same ledger ` +
      `(${JSON.stringify(serverCount)})`).toBe(serverCount);
    // THE LEDGER ITSELF, recorded: AC4 is "verified by reading the same ledger/message data
    // independently", so the raw rows are evidence a reviewer can check by hand rather than an
    // assurance that the two strings matched.
    ensureOut();
    writeFileSync(
      resolve(process.cwd(), "..", OUT_DIR, "gui-server-ledger.json"),
      JSON.stringify(state, null, 2),
    );

    // -----------------------------------------------------------------
    // OBSERVATION 5 — AC7: the TUI and the GUI show the SAME word and the SAME counts for the same
    // turn (the TUI leg wrote its own observation to the handshake file).
    // -----------------------------------------------------------------
    // -----------------------------------------------------------------
    // OBSERVATION 5 — AC7: the TUI and the GUI show the SAME word and the SAME counts for the same
    // turn at the same SERVER CLOCK.
    //
    // THE CLOCK IS ALIGNED EXPLICITLY, and that is the contract rather than a workaround: the verb
    // is a pure function of the server's stamp (`VerbAt`), so "same word" is only a meaningful claim
    // AT THE SAME STAMP. The TUI leg recorded the stamp it drew against; this leg sets the fixture's
    // heartbeat to exactly that stamp and waits for the GUI to draw the word the rotation names for
    // it. Two clients, one server clock, one word — with no shared state between them.
    // -----------------------------------------------------------------
    let cross: { stamp: number; word: string; counter: string } | null = null;
    try {
      cross = JSON.parse(readFileSync(CROSS, "utf8"));
    } catch {
      cross = null; // the TUI leg did not run — recorded, not silently skipped
    }
    expect(cross, "the TUI leg must have written its observation to " + CROSS).not.toBeNull();

    // Align the server clock to the TUI's recorded stamp and wait for the GUI to draw that word.
    const stampRes = await fetch(`${PLANE}/__e2e/stamp?v=${cross!.stamp}`, { method: "POST" });
    expect(stampRes.ok, "the control surface refused the stamp sync").toBe(true);
    await expect(page.locator(LINE)).toContainText(` ${cross!.word}…`, { timeout: 30_000 });
    const aligned = await lineText(page);
    await dump(page, "03b-ac7-same-server-clock");
    expect(verbWord(aligned!), `AC7: at server stamp ${cross!.stamp} the GUI draws a different word ` +
      `than the TUI did (TUI saw ${JSON.stringify(cross!.word)})`).toBe(cross!.word);
    const alignedCount = countHalf(aligned!);
    expect(alignedCount, `AC7: the GUI and the TUI must show the same counts at the same turn (TUI ` +
      `saw ${JSON.stringify(cross!.counter)})`).toBe(cross!.counter);
    ensureOut();
    writeFileSync(
      resolve(process.cwd(), "..", OUT_DIR, "gui-activity-line.txt"),
      `stamp=${cross!.stamp}\nline=${aligned}\nword=${verbWord(aligned!)}\ncounter=${alignedCount}\n`,
    );

    // -----------------------------------------------------------------
    // OBSERVATION 6 — AC5: silence escalates and the counters GONE; a dead plane outranks both.
    // -----------------------------------------------------------------
    await phase("stalled");
    await expect(page.locator(LINE)).toContainText("no output for", { timeout: 60_000 });
    const stalled = await lineText(page);
    await dump(page, "04-ac5-stalled-no-output");
    expect(stalled, "AC5: the line must still be present while stalled").toBeTruthy();
    expect(stalled!, "AC5: the counter must be ABSENT beside the watchdog's verdict").not.toMatch(
      COUNTER_RE,
    );
    expect(stalled!).toContain("Orchicon is");

    // THE RE-DIAL BAND (35s), observed live in the SAME stall. It is a DISTINCT state from the 25s
    // warn band — the watchdog re-dials at 40s and the operator is told the stream will re-attach
    // BEFORE it happens — and the work item's observation table names it as its own row, so stopping
    // at the warn band would leave a named state unproven. Same real clock: the silence age is the
    // gap between the server's last-activity stamp and this repaint, so it escalates on its own.
    await expect(page.locator(LINE)).toContainText("will re-attach", { timeout: 40_000 });
    const redial = await lineText(page);
    await dump(page, "04c-ac5-redial-band");
    expect(redial, "AC5: the line must still be present at the re-dial band").toBeTruthy();
    expect(redial!, "AC5: the RE-DIAL line must keep the watchdog's own verdict").toContain(
      "no output for",
    );
    expect(redial!, "AC5: past the warn band the counters are ABSENT, not merely outranked").not.toMatch(
      COUNTER_RE,
    );

    // -----------------------------------------------------------------
    // OBSERVATION 7 — AC5's SAFETY PROPERTY, observed live: the plane STOPS mid-turn (the work
    // item's own "kill the connection (or stop the plane) mid-turn"). The fixture's PhaseDown ends
    // the open stream with an error — exactly what a plane that dies mid-turn does to a live
    // stream — so the client's own failure path (`fail()` -> `reconnecting`) runs for real and the
    // disconnected banner must outrank the activity line.
    //
    // WHY THE COUNTERS ARE RE-EARNED FIRST. Coming out of the escalation legs the counter is absent
    // for two honest reasons — the bands drop it, and the earlier calls have now aged out of the
    // summarizer's 30s rolling window. Stopping a counter-less row would "pass" while proving
    // nothing, so fresh work is issued (reissue) to put counters back on a row that is genuinely
    // claiming work; the stop then makes their disappearance an observation rather than an
    // inheritance. Same move the TUI leg's kill subtest makes.
    // -----------------------------------------------------------------
    await phase("flight");
    await reissue();
    await expect(page.locator(LINE)).toContainText(COUNTER_RE, { timeout: 40_000 });
    await dump(page, "04b-ac5-counters-before-the-kill");

    // STOPPING THE PLANE: every RPC — including the open stream and the re-dial — now fails. The
    // SPA's own auth routes keep answering in `rpc-down`, so the page stays mounted and the banner
    // is observable rather than the screen going blank.
    await phase("rpc-down");

    // THE BANNER OUTRANKS: the activity line yields its slot to the disconnected notice.
    await expect(page.getByText(/Connection interrupted|Turn stalled/)).toBeVisible({
      timeout: 60_000,
    });
    await expect(page.locator(LINE)).toHaveCount(0, { timeout: 30_000 });
    await dump(page, "05-ac5-plane-down-banner-outranks");
  });

  // THE TURN-ENDS OBSERVATION — the last row of the work item's table, and the state in which the
  // line must GET OUT OF THE WAY: it clears, and the transcript body gets its row back. A line that
  // outlived its turn would claim work that has stopped; that is why this is a second, separately
  // reset turn rather than an assertion bolted onto a leg that ends by stopping the plane.
  //
  // The end is driven by the SAME control the TUI leg uses (Plane.EndTurn via /__e2e/end), so "the
  // turn ends" is one server fact both clients observe, not two clients' opinions of it.
  test("the line clears when the turn ends", async ({ page }) => {
    test.setTimeout(180_000);
    await reset("flight");
    await seedSession(page);
    await page.goto(`${BASE}/ask-orchicon?conversationId=${CONV_ID}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByRole("heading", { name: "activity-e2e", level: 2 })).toBeVisible({
      timeout: 30_000,
    });

    const composer = page.getByRole("textbox", { name: "Ask Orchicon Anything..." });
    await composer.click();
    await composer.fill("E2EPROMPT inspect files and run the tests");
    await composer.press("Enter");

    // THE TURN IS GENUINELY LIVE, with a counter on the line, BEFORE it ends. Without this the
    // "cleared" assertion would be proving the absence of something that was never there.
    await expect(page.locator(LINE)).toContainText(COUNTER_RE, { timeout: 60_000 });
    await dump(page, "06-turn-live-before-end");

    await endTurn();

    // THE LINE CLEARS: the client's own completion effect sees turn_in_flight false plus the Done
    // on the stream and drops the slot, so the line's gate (turnInFlight) closes.
    await expect(page.locator(LINE)).toHaveCount(0, { timeout: 30_000 });
    await dump(page, "07-turn-end-line-cleared");

    // AND THE BODY KEEPS ITS CONTENT. The reply the turn produced is still rendered — clearing the
    // line must hand the body its rows back, never clear the reply with it.
    await expect(page.getByText(WITNESS_CONTENT).first()).toBeVisible({ timeout: 30_000 });
  });
});
