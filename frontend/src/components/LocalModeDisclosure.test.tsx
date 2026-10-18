import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { LocalModeDisclosure } from "./LocalModeDisclosure";

// The local-mode disclosure is a SAFETY surface, not styling. What is pinned here is that it
// names what is NOT protected before anything reassuring, enumerates the surviving layers
// honestly as defense-in-depth, and points at the only real boundary (runtime containers).
// A future edit that softens it into "sandboxed" fails these assertions, which is the point.
const html = renderToStaticMarkup(createElement(LocalModeDisclosure, {}));

describe("LocalModeDisclosure", () => {
  it("is announced as a note with a stable accessible name", () => {
    expect(html).toContain('role="note"');
    expect(html).toContain('aria-labelledby="local-mode-disclosure-title"');
    expect(html).toContain('id="local-mode-disclosure-title"');
    expect(html).toContain("Local mode is not contained.");
    expect(html).toContain('data-testid="local-mode-disclosure"');
  });

  it("says local mode is a host process with the operator's own filesystem access", () => {
    expect(html).toContain("host process with your own filesystem access");
    expect(html).toContain("read and write anything you can");
  });

  it("names what is NOT protected and calls the layers defense-in-depth", () => {
    expect(html).toContain("Not protected:");
    expect(html).toContain("defense-in-depth, not containment");
    // The two escape examples the acceptance criteria name.
    expect(html).toContain("/bin/rm");
    expect(html).toContain("absolute path");
    expect(html).toContain("writing its own");
  });

  it("enumerates the surviving protections, each as best-effort", () => {
    expect(html).toContain("Protected (best-effort):");
    expect(html).toContain("permission");
    expect(html).toContain("/tmp/orchicon/**");
    expect(html).toContain("never-allow binary class");
    expect(html).toContain("execution guard");
    expect(html).toContain("Safety rules");
    expect(html).toContain("All three are defense-in-depth.");
  });

  it("points at runtime containers as the real boundary", () => {
    expect(html).toContain("If isolation matters, use runtime containers.");
    expect(html).toContain("Execution mode → runtime");
    expect(html).toContain("root-free container");
  });

  it("is legible in every theme — design tokens, no hardcoded colour values", () => {
    expect(html).toContain("text-amber-700 dark:text-amber-300");
    expect(html).toContain("text-muted-foreground");
    // A hex/rgb literal would break the dark theme; tokens only.
    expect(html).not.toMatch(/#[0-9a-fA-F]{3,8}/);
  });

  it("contains nothing focusable, so it cannot trap or steal keyboard focus", () => {
    expect(html).not.toContain("<button");
    expect(html).not.toContain("<a ");
    expect(html).not.toContain("tabindex");
    expect(html).not.toContain("aria-expanded");
  });
});
