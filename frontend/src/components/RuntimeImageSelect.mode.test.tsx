import { describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { RuntimeImageSelect } from "./RuntimeImageSelect";

/**
 * RuntimeImageSelect.mode.test.tsx — THE IMAGE PICKER MUST NOT LIE ABOUT A LOCAL RUN.
 *
 * REPORTED: "I set it to local and it set it to 'default (base image)'. That doesn't seem
 * right."
 *
 * The STORAGE was correct (runtime_image empty, verified in the database); the CONTROL was
 * wrong, twice:
 *
 *   1. it coerced an empty value onto the default image (`value={value || defaultImage}`),
 *      so an item with NO image displayed a concrete one, and
 *   2. it submitted that coerced value — so every GUI save of a local item round-tripped
 *      an image back into a field the server had just cleared. That is how the
 *      server-side clear kept being undone.
 *
 * These assertions pin both halves: fixing only the display would leave the form re-arming
 * the bug.
 */
vi.mock("@/api/runtimeImages", () => ({
  useAvailableRuntimeImages: () => ({
    data: {
      stockImages: ["orchicon-runtime:orchicon-dev"],
      customImages: [],
      defaultImage: "orchicon-runtime:orchicon-dev",
    },
  }),
  useListRuntimeImages: () => ({ data: [] }),
}));

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: unknown }) => createElement("span", null, children as never),
}));

const render = (props: { value: string; mode?: "local" | "runtime" }) =>
  renderToStaticMarkup(createElement(RuntimeImageSelect, { ...props, onChange: () => {} }));

describe("RuntimeImageSelect, mode-aware", () => {
  it("offers no image control at all when the item is local", () => {
    const html = render({ value: "", mode: "local" });
    // No <select>: there is nothing to choose for a run with no container.
    expect(html).not.toContain("<select");
    expect(html).toContain("No image");
    expect(html).toContain("executes on the host");
  });

  it("never renders the default image label on a local item", () => {
    const html = render({ value: "", mode: "local" });
    // The exact symptom: an item with an empty image showed "default (base image)".
    expect(html).not.toContain("Default (");
    expect(html).not.toContain("orchicon-runtime:orchicon-dev");
  });

  it("keeps an empty value empty on a runtime item — no coercion", () => {
    const html = render({ value: "", mode: "runtime" });
    // The coercion was `value={value || defaultImage}`; a selected empty option proves the
    // control now reflects what the item actually holds.
    expect(html).toContain('<option value="" selected');
    expect(html).not.toContain('<option value="orchicon-runtime:orchicon-dev" selected');
  });

  it("still offers concrete images on a runtime item", () => {
    const html = render({ value: "", mode: "runtime" });
    expect(html).toContain("orchicon-runtime:orchicon-dev");
    expect(html).toContain("Default (");
  });
});
