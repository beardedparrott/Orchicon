// runtimeImageDockerfile.ts — THE Dockerfile preview generator, in ONE place.
//
// There were two copies (the create route and the edit route) and they had already drifted: only one of them
// knew about the adapter install. A preview is what the operator judges the image by, so two previews that
// disagree about the image is how "it looked right in the console" happens.
//
// It mirrors internal/runtimeimage.generatedDockerfile, which is what actually builds the image. The ORDER is
// load-bearing and is stated once, here: apt packages, then the row's own toolchain lines, then environment,
// then the runtime CLI adapters.
//
// ADAPTERS LAST, because a toolchain line that replaces or removes node/npm would otherwise silently take the
// adapters with it — and the damage would appear at the first session spawn, far from the Dockerfile that
// caused it.

/** parseList splits one-per-line textarea input into trimmed, non-empty entries. */
export function parseList(s: string): string[] {
  return (s || "")
    .split("\n")
    .map((x) => x.trim())
    .filter(Boolean);
}

/**
 * generateDockerfile renders the Dockerfile the daemon would generate for a structured spec.
 *
 * includeAdapters adds opencode and Claude Code. It defaults to true at every call site because an image that
 * cannot run any work is not what "create a runtime image" means; switching it off is a deliberate choice.
 */
export function generateDockerfile(
  base: string,
  apt: string,
  toolchains: string,
  env: string,
  includeAdapters: boolean = true,
): string {
  const aptList = parseList(apt);
  const toolList = parseList(toolchains);
  const envPairs = parseList(env);

  const lines: string[] = [`FROM ${base}`, "USER root"];
  if (aptList.length > 0) {
    lines.push("RUN apt-get update && apt-get install -y --no-install-recommends \\");
    aptList.forEach((p, i) => lines.push(`      ${p} ${i < aptList.length - 1 ? "\\" : ""}`));
    lines.push("      && rm -rf /var/lib/apt/lists/*");
  }
  for (const t of toolList) lines.push(`RUN ${t}`);
  for (const e of envPairs) {
    const i = e.indexOf("=");
    if (i > 0) lines.push(`ENV ${e.slice(0, i)}=${e.slice(i + 1)}`);
  }
  if (includeAdapters) {
    lines.push("ARG ORCHICON_OPENCODE_VERSION=latest");
    lines.push("ARG ORCHICON_CLAUDE_VERSION=latest");
    lines.push("RUN npm install -g \\");
    lines.push('      "@opencode/cli@${ORCHICON_OPENCODE_VERSION}" \\');
    lines.push('      "@anthropic-ai/claude-code@${ORCHICON_CLAUDE_VERSION}" \\');
    lines.push(" && chown -R 1000:1000 /usr/local/lib/node_modules /usr/local/bin 2>/dev/null || true");
  }
  lines.push("RUN chown -R 1000:1000 /usr /opt /var 2>/dev/null || true");
  lines.push("USER orchicon");
  lines.push("WORKDIR /home/orchicon");
  return lines.join("\n");
}
