// capture-screenshots.mjs — capture the site's app screenshots from a RUNNING plane.
//
// WHY THIS EXISTS AS A SCRIPT RATHER THAN SOMETHING THE AGENT RUNS: every app screen sits behind
// authentication, and the platform deliberately has no anonymous bypass ("Authenticate to access
// the control plane"). So a capture has to happen in a session that has been logged in, and the
// login must be the OPERATOR'S — nobody else should be handling that credential.
//
// USAGE
//   cd frontend && node ../scripts/capture-screenshots.mjs [baseUrl]
//
// A browser window opens. Log in normally. The script notices the moment the app is up and then
// captures each route headlessly-sized at 1800px wide, converts to WebP at the quality the
// committed assets use, and writes them into site/assets/ with the EXISTING filenames so the page
// needs no edit.
//
//   baseUrl defaults to the dev plane (http://127.0.0.1:8080). Pass the prod URL to capture the
//   real data — and note that WHATEVER IS ON SCREEN IS PUBLISHED, so check for names you would not
//   put on a public site before committing the result.
import { chromium } from "playwright";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, "..");
const outDir = path.join(repo, "site", "assets");
const base = (process.argv[2] ?? "http://127.0.0.1:8080").replace(/\/$/, "");

// The routes behind the five committed web-*.webp assets. The FILENAMES are the contract: the page
// references these names, so a capture that invented its own names would leave the site unchanged.
const SHOTS = [
  { file: "web-work", route: "/work-items", wait: "the work items board" },
  { file: "web-workflows", route: "/workflows", wait: "the workflows list" },
  { file: "web-ask", route: "/ask-orchicon", wait: "a conversation" },
  { file: "web-ideacloud", route: "/idea-cloud", wait: "the idea cloud" },
  { file: "web-schedules", route: "/schedules", wait: "the schedules list" },
];
const VIEWPORT = { width: 1800, height: 1160 };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await chromium.launch({ args: ["--no-sandbox"], headless: false });
const page = await browser.newPage({ viewport: VIEWPORT });

console.log(`opening ${base} — log in, then leave the browser alone.`);
await page.goto(base, { waitUntil: "domcontentloaded", timeout: 60000 }).catch(() => {});

// Wait for a logged-in session: the login form is replaced once the app shell mounts. A generous
// window, because the operator is typing a password into it.
let ready = false;
for (let i = 0; i < 180; i++) {
  await sleep(1000);
  const url = page.url();
  const hasLoginForm = await page
    .locator('input[type="password"]')
    .count()
    .catch(() => 0);
  if (!url.includes("/login") && hasLoginForm === 0) {
    ready = true;
    break;
  }
}
if (!ready) {
  console.error("timed out waiting for a logged-in session — nothing was captured.");
  await browser.close();
  process.exit(1);
}
console.log("session detected — capturing.");

fs.mkdirSync(outDir, { recursive: true });
let failures = 0;
for (const shot of SHOTS) {
  const png = path.join(outDir, `${shot.file}.png`);
  const webp = path.join(outDir, `${shot.file}.webp`);
  try {
    await page.goto(base + shot.route, { waitUntil: "networkidle", timeout: 60000 });
    // The app fills in asynchronously; a screenshot taken on networkidle alone can catch skeletons.
    await sleep(2500);
    await page.screenshot({ path: png });
    // ImageMagick writes the WebP the same way the committed assets were made: 1800px wide, q82.
    execFileSync("magick", [png, "-quality", "82", webp]);
    fs.unlinkSync(png);
    const bytes = fs.statSync(webp).size;
    console.log(`  ${shot.file}.webp  ${bytes} bytes`);
  } catch (e) {
    failures++;
    console.error(`  ${shot.file}: FAILED ${String(e).split("\n")[0]}`);
  }
}
await browser.close();
console.log(failures === 0 ? "done — all five captured." : `done with ${failures} failure(s).`);
process.exit(failures === 0 ? 0 : 1);
