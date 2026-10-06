import { describe, expect, it } from "vitest";

import {
  attachSecret,
  credentialKeys,
  inlineSpecIsHTTP,
  type InlineSpecLike,
} from "@/lib/mcpInlineCredential";

// The credential half of an inline MCP spec. A worker version has no definition row, so a credential is
// REFERENCED rather than stored: the picked secret becomes `${NAME}` in the spec's own env or headers.
//
// These are real assertions on real functions (no DOM needed — the repo ships no jsdom), because the two
// things that can silently go wrong here are both pure: writing the reference into the map the transport
// does NOT read, and mutating the array the caller owns.
describe("mcpInlineCredential", () => {
  it("decides which map a spec reads the way the server does", () => {
    // Type first, url as the fallback — internal/mcpsettings.specFromInline's own inference.
    expect(inlineSpecIsHTTP({ id: "a", type: "http" })).toBe(true);
    expect(inlineSpecIsHTTP({ id: "a", type: "streamable-http" })).toBe(true);
    expect(inlineSpecIsHTTP({ id: "a", type: "stdio" })).toBe(false);
    expect(inlineSpecIsHTTP({ id: "a" })).toBe(false);
    expect(inlineSpecIsHTTP({ id: "a", url: "https://mcp.example/sse" })).toBe(true);
  });

  it("offers the keys the spec already carries, sorted", () => {
    expect(credentialKeys({ id: "gh", type: "stdio", env: { B_TOKEN: "", A_TOKEN: "" } })).toEqual([
      "A_TOKEN",
      "B_TOKEN",
    ]);
    expect(
      credentialKeys({ id: "r", type: "http", headers: { Authorization: "Bearer x" }, env: { IGNORED: "y" } }),
    ).toEqual(["Authorization"]);
    // A catalog pick leaves a secret key out on purpose, so an empty list is the normal case.
    expect(credentialKeys({ id: "playwright", type: "stdio" })).toEqual([]);
    expect(credentialKeys(undefined)).toEqual([]);
  });

  it("writes the reference into env for stdio and headers for HTTP", () => {
    const stdio = attachSecret<InlineSpecLike>(
      [{ id: "gh", type: "stdio", env: { OTHER: "keep" } }],
      "gh",
      "GITHUB_PERSONAL_ACCESS_TOKEN",
      "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN",
    );
    expect(stdio[0].env).toEqual({
      OTHER: "keep",
      GITHUB_PERSONAL_ACCESS_TOKEN: "${MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN}",
    });
    expect(stdio[0].headers).toBeUndefined();

    const http = attachSecret<InlineSpecLike>(
      [{ id: "remote", type: "http", url: "https://mcp.example/sse", headers: {} }],
      "remote",
      "Authorization",
      "SLACK_BOT_TOKEN",
    );
    expect(http[0].headers).toEqual({ Authorization: "${SLACK_BOT_TOKEN}" });
    expect(http[0].env).toBeUndefined();
  });

  it("replaces an existing value at that key — that is the point of attaching", () => {
    const out = attachSecret(
      [{ id: "gh", type: "stdio", command: ["npx"], env: { GITHUB_TOKEN: "ghp_plaintext_was_here" } }],
      "gh",
      "GITHUB_TOKEN",
      "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN",
    );
    expect(out[0].env?.GITHUB_TOKEN).toBe("${MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN}");
  });

  it("touches nothing else: other specs and other keys survive", () => {
    const specs: InlineSpecLike[] = [
      { id: "gh", type: "stdio", env: { KEEP: "1" } },
      { id: "other", type: "stdio", env: { UNTOUCHED: "2" } },
    ];
    const out = attachSecret(specs, "gh", "TOKEN", "STORED");
    expect(out[1]).toEqual(specs[1]);
    expect(out[0].env).toEqual({ KEEP: "1", TOKEN: "${STORED}" });
  });

  it("returns NEW objects and leaves the caller's array alone", () => {
    // The panel writes through a form field, so the change has to be observable — a mutated nested map
    // would keep the same array identity and the version would look unmodified.
    const specs: InlineSpecLike[] = [{ id: "gh", type: "stdio", env: { A: "1" } }];
    const out = attachSecret(specs, "gh", "B", "STORED");
    expect(out).not.toBe(specs);
    expect(out[0]).not.toBe(specs[0]);
    expect(specs[0].env).toEqual({ A: "1" });
  });

  it("is a no-op without both a key and a secret name", () => {
    const specs: InlineSpecLike[] = [{ id: "gh", type: "stdio", env: {} }];
    expect(attachSecret(specs, "gh", "", "STORED")).toBe(specs);
    expect(attachSecret(specs, "gh", "TOKEN", "  ")).toBe(specs);
    // An id that is not in the array changes nothing either.
    expect(attachSecret(specs, "nope", "TOKEN", "STORED")[0].env).toEqual({});
  });
});
