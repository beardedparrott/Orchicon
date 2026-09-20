// ask-metrics.test.ts — AC 8, the CLIENT-side rule for the Ask composer's
// context-window lookup: THE ADAPTER SEGMENT PICKS THE SOURCE AND IS NEVER
// CROSSED.
//
// The real occurrence this pins: the composer strip's window lookup fell back
// from the native providers view to AIGatewayService.ListOpenCodeModels — which
// SHELLS OUT TO THE OPENCODE BINARY. On a box with no opencode that lookup
// cannot answer at all, so for an `orchicon` ref the cross-source fallback
// silently reintroduces the exact dependency the native adapter exists to
// remove. It was caught by the operator, not by any test.
//
// The two data hooks are MOCKED, so the assertions are about WHICH SOURCE the
// hook ENABLES for a given ref — the exact decision under audit. The probe is
// rendered with react-dom/server, which runs render synchronously with no DOM
// and never fires the (mocked) queries.
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

const spy = vi.hoisted(() => ({
  provider: [] as Array<{ providerId: string; enabled: boolean }>,
  cli: [] as Array<{ adapter?: string; provider?: string; enabled: boolean }>,
}));

vi.mock("@/api/clients", () => ({ aiGatewayClient: {} }));
vi.mock("@/api/providers", () => ({
  useProviderModels: (providerId: string, enabled = true) => {
    spy.provider.push({ providerId, enabled });
    return { data: { models: [{ id: "deepseek-flash", context: 128000 }] } };
  },
}));
vi.mock("@/api/aigateway", () => ({
  useListOpenCodeModels: (adapter?: string, provider?: string, enabled = true) => {
    spy.cli.push({ adapter, provider, enabled });
    return {
      data: [{ id: "claude-sonnet-4", modelRef: "anthropic/claude-sonnet-4", limits: { context: 200000 } }],
    };
  },
}));

import { useModelContextWindow } from "@/lib/ask-metrics";

function Probe({ modelRef }: { modelRef: string }) {
  useModelContextWindow(modelRef);
  return null;
}

function render(ref: string) {
  spy.provider.length = 0;
  spy.cli.length = 0;
  renderToStaticMarkup(createElement(Probe, { modelRef: ref }));
}

describe("useModelContextWindow source selection (AC 8)", () => {
  beforeEach(() => {
    spy.provider.length = 0;
    spy.cli.length = 0;
  });

  it("enables ONLY the native providers view for an orchicon ref — never the CLI", () => {
    render("orchicon/deepseek/deepseek-flash");
    // The native source is the one read, with the ref's own provider.
    expect(spy.provider).toEqual([{ providerId: "deepseek", enabled: true }]);
    // The CLI-backed source is DISABLED — an orchicon ref must never reach it.
    expect(spy.cli.every((c) => c.enabled === false)).toBe(true);
  });

  it("enables CLI discovery for a legacy opencode ref (the recorded exception)", () => {
    render("opencode/anthropic/claude-sonnet-4");
    // For the opencode adapter the CLI is the authority on its own models.
    expect(spy.cli).toEqual([{ adapter: "opencode", provider: "anthropic", enabled: true }]);
    // …and the native source is the one disabled.
    expect(spy.provider.every((c) => c.enabled === false)).toBe(true);
  });

  it("enables NEITHER source for a ref with no resolvable provider/model", () => {
    render("llama3");
    expect(spy.provider.every((c) => c.enabled === false)).toBe(true);
    expect(spy.cli.every((c) => c.enabled === false)).toBe(true);
  });
});
