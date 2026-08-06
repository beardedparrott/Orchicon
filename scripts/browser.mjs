// Playwright launch helper for the Orchicon dev runtime.
//
// The runtime container has no root process, so Chromium's setuid
// sandbox cannot run: every launch MUST pass `--no-sandbox` or the
// browser fails to start. Import this helper instead of calling
// playwright directly (see AGENTS.md → Browser automation).
//
// playwright is installed globally in the dev image
// (/usr/local/lib/node_modules/playwright), not in the project's
// node_modules, so it is imported by absolute path (NODE_PATH is not
// honored for ESM).
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
let chromium;
try {
  // Local install wins if the project ever adds it as a dependency.
  ({ chromium } = require("playwright"));
} catch {
  ({ chromium } = require("/usr/local/lib/node_modules/playwright"));
}

export function launch(opts = {}) {
  return chromium.launch({ args: ["--no-sandbox", ...(opts.args ?? [])], ...opts });
}
