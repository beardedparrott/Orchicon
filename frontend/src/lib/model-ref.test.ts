import { describe, expect, it } from "vitest";

import {
  DEFAULT_ADAPTER_KIND,
  catalogModelMatches,
  formatModelRef,
  parseModelRef,
} from "@/lib/model-ref";

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

describe("catalogModelMatches (BUG-1: segment matching, not raw-value)", () => {
  const catalog = [
    { id: "claude-sonnet-4", providerId: "anthropic", modelRef: "anthropic/claude-sonnet-4" },
    { id: "deepseek-v4-flash", providerId: "command-code", modelRef: "command-code/deepseek-v4-flash" },
    { id: "legacy-model", providerId: "anthropic" }, // catalog entry without modelRef
  ];

  it("matches freshly-written 3-segment refs by segments", () => {
    const parsed = parseModelRef("opencode/anthropic/claude-sonnet-4");
    expect(parsed && catalogModelMatches(parsed, catalog[0])).toBe(true);
    // A catalog entry under a DIFFERENT provider never matches, regardless
    // of the ref's adapter segment (the adapter is not catalog identity).
    expect(parsed && catalogModelMatches(parsed, { id: "claude-sonnet-4", providerId: "opencode" })).toBe(false);
    // wrong provider must not match
    expect(parsed && catalogModelMatches(parsed, catalog[1])).toBe(false);
  });

  it("matches stored 3-segment refs whose adapter differs from the provider", () => {
    const parsed = parseModelRef("claude/anthropic/claude-sonnet-5");
    expect(parsed && catalogModelMatches(parsed, catalog[0])).toBe(false); // model id differs
    const parsed2 = parseModelRef("claude/anthropic/claude-sonnet-4");
    expect(parsed2 && catalogModelMatches(parsed2, catalog[0])).toBe(true);
  });

  it("matches slashed model ids (4+ segment refs)", () => {
    const parsed = parseModelRef("orchicon/command-code/deepseek/deepseek-v4-flash");
    // Catalog id carries the internal slashes verbatim.
    expect(parsed && catalogModelMatches(parsed, { id: "deepseek/deepseek-v4-flash", providerId: "command-code" })).toBe(true);
    // The bare-id entry does not match the slashed ref.
    expect(parsed && catalogModelMatches(parsed, catalog[1])).toBe(false);
    expect(parsed && catalogModelMatches(parsed, catalog[0])).toBe(false);
  });

  it("matches legacy 2-segment refs via provider+model segments", () => {
    const parsed = parseModelRef("anthropic/claude-sonnet-4");
    expect(parsed && catalogModelMatches(parsed, catalog[0])).toBe(true);
    expect(parsed && catalogModelMatches(parsed, catalog[1])).toBe(false);
  });

  it("matches legacy 1-segment refs by model id alone", () => {
    const parsed = parseModelRef("claude-sonnet-4");
    expect(parsed && catalogModelMatches(parsed, catalog[0])).toBe(true);
    expect(parsed && catalogModelMatches(parsed, catalog[1])).toBe(false);
  });

  it("handles catalog entries without a legacy modelRef", () => {
    const parsed = parseModelRef("opencode/anthropic/legacy-model");
    expect(parsed && catalogModelMatches(parsed, catalog[2])).toBe(true);
  });

  it("falls back to the legacy modelRef when segments are unavailable", () => {
    const parsed = parseModelRef("anthropic/legacy-model");
    // A provider-less entry still matches its legacy 2-seg modelRef string.
    expect(parsed && catalogModelMatches(parsed, { id: "", providerId: "", modelRef: "anthropic/legacy-model" })).toBe(true);
  });
});