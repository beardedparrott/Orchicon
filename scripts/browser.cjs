const { chromium } = require("playwright");
async function launch(opts = {}) {
  return chromium.launch({ args: ["--no-sandbox", ...(opts.args ?? [])], ...opts });
}
async function shot(page, name) {
  const path = `/tmp/orchicon/${name}.png`;
  await page.screenshot({ path, fullPage: false });
  return path;
}
module.exports = { chromium, launch, shot };
