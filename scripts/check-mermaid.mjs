// check-mermaid.mjs — validate every Mermaid diagram in the docs with a REAL parser.
//
// WHY THIS EXISTS, and why it is not a hand-written syntax check: `DOCUMENTATION.md` shipped with a
// domain-model `erDiagram` that did not render on GitHub — eight entities declared `string id ULID`,
// which is THREE BARE WORDS. Mermaid's grammar wants `type name [PK|FK|UK] ["comment"]`, so the whole
// block failed and the reader got "Unable to render rich display / Parse error on line 19".
//
// A hand-rolled validator ("balanced braces, known keywords, a `PK` per entity") passed that file,
// because the mistake is a GRAMMAR error rather than a structural one. The only thing that catches it
// is the parser GitHub itself runs.
//
// USAGE
//   node scripts/check-mermaid.mjs [file.md ...]     # defaults to every *.md at the repo root
//
// THE DEFAULT IS THE WHOLE ROOT DOC SET, deliberately. It used to be DOCUMENTATION.md alone, which
// meant a diagram added to any other doc was never validated by anything — the same silent-pass
// failure this file exists to prevent, one directory level up. Every root *.md is a document a reader
// sees on GitHub, so every root *.md is checked. Naming files on the command line still overrides.
//
// DEPENDENCIES, deliberately NOT part of the product or the frontend toolchain: `mermaid` and `jsdom`
// are installed into a scratch prefix by CI (`npm install --prefix "$RUNNER_TEMP/mermaid-check"`),
// the same pattern the Go gate uses for the TS protoc plugins. Nothing here is imported by the app,
// and no dependency is added to package.json for a docs check.
//
// The prefix is found from ORCHICON_MERMAID_PREFIX when set (CI sets it), otherwise `frontend/node_modules`,
// otherwise the repo root — so it also works for anyone who happens to have them installed locally.

import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

// The Mermaid version GitHub renders with is ~10.x, and the GRAMMAR is what is being checked — a
// version that accepts more than GitHub does would pass a diagram a reader cannot see. Pinned in one
// place; ci.yml reads this value so the two cannot drift.
const MERMAID_VERSION = "10.9.8";

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, "..");

const files = process.argv.slice(2);
const targets = files.length
  ? files
  : fs
      .readdirSync(repo)
      .filter((f) => f.endsWith(".md"))
      .sort()
      .map((f) => path.join(repo, f));

function resolveDeps() {
  const prefixes = [
    process.env.ORCHICON_MERMAID_PREFIX,
    path.join(repo, "frontend", "node_modules"),
    path.join(repo, "node_modules"),
  ].filter(Boolean);

  for (const prefix of prefixes) {
    if (!fs.existsSync(prefix)) continue;
    const req = createRequire(pathToFileURL(path.join(prefix, "noop.js")));
    try {
      // RESOLVE ONLY — never require. `require` EXECUTES the module, and dompurify decides at
      // EXECUTION time whether it is an instance or a factory based on whether a global `window`
      // exists. Loading it here, before the DOM is set up below, made it a factory whose `sanitize`
      // is undefined — so the flowchart parser reported "DOMPurify.sanitize is not a function" for a
      // diagram that renders perfectly. Order is the whole game: resolve paths, build the window,
      // then import.
      return {
        paths: {
          mermaid: req.resolve("mermaid"),
          jsdom: req.resolve("jsdom"),
          dompurify: req.resolve("dompurify"),
        },
        from: prefix,
      };
    } catch {
      // Try the next prefix.
    }
  }
  return null;
}

const deps = resolveDeps();
if (!deps) {
  console.error(
    [
      "check-mermaid: mermaid/jsdom are not installed, so NOTHING was validated.",
      "",
      "This is a FAILURE rather than a skip: a docs check that silently passes when its parser is",
      "missing is how an unrenderable diagram reaches a reader. Install them into a scratch prefix:",
      "",
      `  npm install --prefix "$PWD/.mermaid-check" mermaid@${MERMAID_VERSION} jsdom`,
      `  ORCHICON_MERMAID_PREFIX="$PWD/.mermaid-check/node_modules" node scripts/check-mermaid.mjs`,
      "",
    ].join("\n"),
  );
  process.exit(2);
}

// Mermaid probes the DOM at import time, so a document must exist first. `navigator` is defined
// rather than assigned — on Node 22 it is a getter on globalThis and a plain assignment throws.
const { JSDOM } = await import(pathToFileURL(deps.paths.jsdom).href);
const dom = new JSDOM("<!doctype html><html><body></body></html>", { pretendToBeVisual: true });
globalThis.window = dom.window;
Object.defineProperty(globalThis, "navigator", {
  value: dom.window.navigator,
  configurable: true,
});
globalThis.document = dom.window.document;

// THE REAL DOMPURIFY, imported AFTER the window exists so it initialises as an INSTANCE rather than a
// factory, and installed globally because that is where mermaid looks for it. A stub that returns its
// input is not enough: the flowchart parser sanitises labels, so a stub made a perfectly good
// `graph TB` report a parse failure. A validator whose failures cannot be trusted is worse than none.
const createDOMPurify = (await import(pathToFileURL(deps.paths.dompurify).href)).default;
globalThis.DOMPurify = createDOMPurify(dom.window);

const mermaid = (await import(pathToFileURL(deps.paths.mermaid).href)).default;
mermaid.initialize({ startOnLoad: false, securityLevel: "loose" });

let failures = 0;
let checked = 0;

for (const file of targets) {
  const md = fs.readFileSync(file, "utf8");
  const blocks = [...md.matchAll(/```mermaid\r?\n([\s\S]*?)```/g)].map((m) => m[1]);
  const rel = path.relative(repo, file) || file;
  console.log(`${rel}: ${blocks.length} diagram(s)`);
  for (const [i, block] of blocks.entries()) {
    const kind = block.trim().split("\n")[0].trim();
    // The line the block starts on, so a failure names a location in the file rather than in a fence.
    const before = md.slice(0, md.indexOf(block));
    const line = before.split("\n").length;
    checked++;
    try {
      await mermaid.parse(block);
      console.log(`  ok   ${rel}:${line}  ${kind}`);
    } catch (e) {
      failures++;
      const msg = String(e?.message ?? e).trim();
      console.error(`  FAIL ${rel}:${line}  ${kind}`);
      for (const l of msg.split("\n").slice(0, 8)) console.error(`       ${l}`);
    }
  }
}

console.log(`\nmermaid ${MERMAID_VERSION}: ${checked} diagram(s) checked, ${failures} broken`);
process.exit(failures === 0 ? 0 : 1);
