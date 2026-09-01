import { describe, expect, it } from "vitest";

import { DEFAULT_ADAPTER_KIND, formatModelRef, parseModelRef } from "@/lib/model-ref";

// Mirrors the Go table tests in internal/adapter/modelref_test.go
// (legacy, 3-seg, slashed model ids, malformed).
describe("parseModelRef", () => {
  it("parses legacy 2-segment refs with adapter inferred as opencode", () => {
    expect(parseModelRef("opencode/deepseek-v4-flash-free")).toEqual({
      adapter: "opencode",
      provider: "opencode",
      model: "deepseek-v4-flash-free",
      raw: "opencode/deepseek-v4-flash-free",
    });
    expect(parseModelRef("anthropic/claude-sonnet-4")).toEqual({
      adapter: "opencode",
      provider: "anthropic",
      model: "claude-sonnet-4",
      raw: "anthropic/claude-sonnet-4",
    });
    expect(parseModelRef("opencode-go/deepseek-v4-flash")).toEqual({
      adapter: "opencode",
      provider: "opencode-go",
      model: "deepseek-v4-flash",
      raw: "opencode-go/deepseek-v4-flash",
    });
  });

  it("parses 3-segment refs", () => {
    expect(parseModelRef("claude/anthropic/claude-sonnet-5")).toEqual({
      adapter: "claude",
      provider: "anthropic",
      model: "claude-sonnet-5",
      raw: "claude/anthropic/claude-sonnet-5",
    });
    expect(parseModelRef("opencode/opencode-go/deepseek-v4-flash")).toEqual({
      adapter: "opencode",
      provider: "opencode-go",
      model: "deepseek-v4-flash",
      raw: "opencode/opencode-go/deepseek-v4-flash",
    });
  });

  it("preserves slashed model ids verbatim (4+ segments, left-greedy)", () => {
    expect(parseModelRef("orchicon/command-code/deepseek/deepseek-v4-flash")).toEqual({
      adapter: "orchicon",
      provider: "command-code",
      model: "deepseek/deepseek-v4-flash",
      raw: "orchicon/command-code/deepseek/deepseek-v4-flash",
    });
    expect(parseModelRef("opencode/provider/a/b/c")).toEqual({
      adapter: "opencode",
      provider: "provider",
      model: "a/b/c",
      raw: "opencode/provider/a/b/c",
    });
  });

  it("parses 1-segment legacy bare model ids", () => {
    expect(parseModelRef("deepseek-v4-flash")).toEqual({
      adapter: DEFAULT_ADAPTER_KIND,
      provider: "",
      model: "deepseek-v4-flash",
      raw: "deepseek-v4-flash",
    });
  });

  it("returns null for empty or malformed refs", () => {
    expect(parseModelRef("")).toBeNull();
    expect(parseModelRef(undefined)).toBeNull();
    expect(parseModelRef(null)).toBeNull();
    expect(parseModelRef("   ")).toBeNull();
    expect(parseModelRef("/")).toBeNull();
    expect(parseModelRef("a//b")).toBeNull();
  });
});

describe("formatModelRef", () => {
  it("builds the canonical 3-segment ref", () => {
    expect(formatModelRef("opencode", "anthropic", "claude-sonnet-4")).toBe(
      "opencode/anthropic/claude-sonnet-4",
    );
    expect(formatModelRef("orchicon", "command-code", "deepseek/deepseek-v4-flash")).toBe(
      "orchicon/command-code/deepseek/deepseek-v4-flash",
    );
  });

  it("collapses gracefully when segments are absent", () => {
    expect(formatModelRef("", "", "claude-sonnet-4")).toBe("claude-sonnet-4");
    expect(formatModelRef("opencode", "", "")).toBe("opencode");
    expect(formatModelRef("opencode", "anthropic", "")).toBe("opencode/anthropic");
    expect(formatModelRef("", "", "")).toBe("");
  });
});
